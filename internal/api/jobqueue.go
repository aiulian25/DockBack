package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"runtime/debug"
	"time"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
	"dockback/internal/notify"
	"dockback/internal/store"
)

// guardPanic recovers a panicking background job so one bug can't crash the whole
// daemon and take down every OTHER in-flight backup/restore (PLAN §9.10 — "a
// crash mid-job must leave a clean, resumable state"). Register it as the FIRST
// defer in a job goroutine so the lock/slot/temp releases (registered after it)
// still run during unwind before this recovers; it then logs the panic with a
// stack trace and runs onPanic to mark the affected op failed.
func guardPanic(label, id string, onPanic func()) {
	if rec := recover(); rec != nil {
		log.Printf("panic in %s %s: %v\n%s", label, id, rec, debug.Stack())
		if onPanic != nil {
			onPanic()
		}
	}
}

// Job priorities (higher = dispatched first). Interactive backups (a user
// clicking "back up now" or a stack action) jump ahead of scheduled fleet-wide
// work so the UI stays responsive during a nightly window.
const (
	prioScheduled   = 0
	prioInteractive = 10
)

// Auto-retry of a transiently-failed backup (F26). A backup that fails on a
// transient error (node/DB/registry blip) is re-enqueued with a backing-off delay
// instead of waiting for the next schedule, up to maxBackupAttempts TOTAL tries.
// A permanent failure (bad selection, out of space) is never retried.
const maxBackupAttempts = 3

// backupRetryBackoffs is the delay before the next attempt, indexed by the
// just-failed 0-based attempt number (1m after the first failure, 5m after the
// second). The 15m tail keeps the schedule extensible past the current cap.
var backupRetryBackoffs = []time.Duration{1 * time.Minute, 5 * time.Minute, 15 * time.Minute}

// backupRetryDelay returns the backoff before retrying after failedAttempt (0-based)
// failed, clamped to the schedule bounds.
func backupRetryDelay(failedAttempt int) time.Duration {
	if failedAttempt < 0 {
		failedAttempt = 0
	}
	if failedAttempt >= len(backupRetryBackoffs) {
		failedAttempt = len(backupRetryBackoffs) - 1
	}
	return backupRetryBackoffs[failedAttempt]
}

// backupRetryPlan is the pure retry decision (F26): retry only a TRANSIENT error,
// and only while attempts remain (the just-run attempt is 0-based). Returns the
// backoff for the next attempt. Pure + testable — no clock, DB, or queue.
func backupRetryPlan(err error, attempt int) (retry bool, delay time.Duration) {
	if err == nil || !backup.IsTransient(err) {
		return false, 0
	}
	if attempt+1 >= maxBackupAttempts {
		return false, 0 // out of attempts — this is the terminal failure
	}
	return true, backupRetryDelay(attempt)
}

// queuedJob is one pending backup waiting for a concurrency slot.
type queuedJob struct {
	id        string
	nodeID    string
	nodeName  string
	opts      backup.Options
	priority  int
	seq       int64     // FIFO tiebreak within a priority
	notBefore time.Time // earliest dispatch time (scheduled jitter); zero = now
	// Stack-exclusion keys, resolved from the inventory cache at
	// enqueue: stackKey is the backup↔restore exclusion domain; ctrKey dedups
	// concurrent backups of the same container.
	stackKey string
	ctrKey   string
}

// enqueueBackup adds a backup to the priority queue and wakes the dispatcher.
// Scheduled jobs are given a randomized notBefore within the jitter window so a
// fleet-wide window doesn't start every backup at the same instant.
// Returns the backup id (registered immediately so it is cancelable while queued).
func (s *Server) enqueueBackup(nodeName string, opts backup.Options, priority int) string {
	notBefore := time.Now()
	// Jitter is read live (env value as default) so tuning it in the UI takes
	// effect on the next scheduled dispatch without a restart (F15).
	if jitter := s.scheduleJitter(); priority <= prioScheduled && jitter > 0 {
		notBefore = notBefore.Add(time.Duration(rand.Int63n(int64(jitter)+1)) * time.Second)
	}
	return s.enqueueBackupAt(nodeName, opts, priority, notBefore)
}

// enqueueBackupAt is enqueueBackup with an explicit earliest-dispatch time — used
// by the auto-retry path (F26) to schedule a fresh attempt at now+backoff, and by
// enqueueBackup after it computes the scheduled jitter window.
func (s *Server) enqueueBackupAt(nodeName string, opts backup.Options, priority int, notBefore time.Time) string {
	if opts.BackupID == "" {
		opts.BackupID = backup.NewID()
	}
	id := opts.BackupID

	// Register a cancel handle now (cancel func filled in when it actually runs),
	// so a queued backup can be canceled before it ever starts. NodeID/NodeName
	// let log lines for this backup be node-keyed.
	s.jobMu.Lock()
	s.jobs[id] = &backupJob{nodeID: opts.NodeID, nodeName: nodeName, containerID: opts.ContainerID}
	s.jobMu.Unlock()

	s.queueMu.Lock()
	s.queueSeq++
	seq := s.queueSeq
	sk, ck := s.backupExclusionKeys(opts)
	s.queue = append(s.queue, &queuedJob{
		id: id, nodeID: opts.NodeID, nodeName: nodeName, opts: opts,
		priority: priority, seq: seq, notBefore: notBefore,
		stackKey: sk,
		ctrKey:   ck,
	})
	s.queueMu.Unlock()

	// Persist so a restart resumes this job instead of dropping it.
	// Best-effort: a persistence failure must not block the backup.
	if b, err := json.Marshal(opts); err == nil {
		if perr := s.store.SaveQueuedJob(store.QueuedJobRow{
			ID: id, NodeID: opts.NodeID, NodeName: nodeName, OptsJSON: string(b),
			Priority: priority, Seq: seq, NotBefore: notBefore.Unix(),
		}); perr != nil {
			log.Printf("queue: persist job %s: %v", id, perr)
		}
	}

	s.signalQueue()
	return id
}

// activeBackup reports an already-pending backup of the same container: the id
// of a RUNNING job, or of a QUEUED interactive job. Manual "back up now" uses it
// to coalesce repeat clicks (each would otherwise queue a whole extra run —
// serialized by the per-container lock, so N clicks on a large container meant
// N sequential full backups). A queued SCHEDULED job deliberately does NOT
// match: a jittered nightly waiting in the queue must never swallow an explicit
// "now". Best-effort snapshot — the createMu in handleCreateBackup closes the
// double-submit race for the manual path.
func (s *Server) activeBackup(opts backup.Options) (id string, state string) {
	_, ck := s.backupExclusionKeys(opts)

	// Queue scan: collect every queued id for this container so the jobs-map scan
	// below can tell running from queued, and remember an interactive match.
	s.queueMu.Lock()
	queued := map[string]bool{}
	var queuedInteractive string
	for _, j := range s.queue {
		if j.ctrKey != ck {
			continue
		}
		queued[j.id] = true
		if j.priority >= prioInteractive && queuedInteractive == "" {
			queuedInteractive = j.id
		}
	}
	s.queueMu.Unlock()

	// Jobs map holds queued + running entries (removed at completion): a match
	// that is NOT in the queue is running right now.
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	for jid, jb := range s.jobs {
		if jb.nodeID == opts.NodeID && jb.containerID == opts.ContainerID &&
			opts.ContainerID != "" && !queued[jid] && !jb.canceled {
			return jid, "running"
		}
	}
	if queuedInteractive != "" {
		return queuedInteractive, "queued"
	}
	return "", ""
}

// resumeQueuedJobs re-enqueues jobs persisted by a previous run so a restart
// doesn't silently drop a nightly window. In-flight backups were
// already discarded by PurgeInterruptedBackups, so re-running is clean. Called
// once at startup before the dispatcher drains the queue.
func (s *Server) resumeQueuedJobs() {
	rows, err := s.store.ListQueuedJobs()
	if err != nil || len(rows) == 0 {
		return
	}
	var maxSeq int64
	for _, r := range rows {
		var opts backup.Options
		if json.Unmarshal([]byte(r.OptsJSON), &opts) != nil {
			_ = s.store.DeleteQueuedJob(r.ID) // unreadable row; drop it
			continue
		}
		opts.BackupID = r.ID
		s.jobMu.Lock()
		s.jobs[r.ID] = &backupJob{nodeID: r.NodeID, nodeName: r.NodeName, containerID: opts.ContainerID}
		s.jobMu.Unlock()
		sk, ck := s.backupExclusionKeys(opts)
		s.queueMu.Lock()
		s.queue = append(s.queue, &queuedJob{
			id: r.ID, nodeID: r.NodeID, nodeName: r.NodeName, opts: opts,
			priority: r.Priority, seq: r.Seq, notBefore: time.Unix(r.NotBefore, 0),
			stackKey: sk,
			ctrKey:   ck,
		})
		s.queueMu.Unlock()
		if r.Seq > maxSeq {
			maxSeq = r.Seq
		}
	}
	s.queueMu.Lock()
	if maxSeq > s.queueSeq {
		s.queueSeq = maxSeq
	}
	s.queueMu.Unlock()
	s.logSink("queue", "INFO", fmt.Sprintf("Resumed %d queued backup(s) from a previous run", len(rows)))
	s.signalQueue()
}

// signalQueue wakes the dispatcher without blocking (buffered, coalesced).
func (s *Server) signalQueue() {
	select {
	case s.queueSig <- struct{}{}:
	default:
	}
}

// removeFromQueue drops a still-queued job (on cancel) and its durable row.
// No-op on the in-memory queue once dispatched; the durable delete is idempotent.
func (s *Server) removeFromQueue(id string) {
	s.queueMu.Lock()
	for i, j := range s.queue {
		if j.id == id {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			break
		}
	}
	s.queueMu.Unlock()
	_ = s.store.DeleteQueuedJob(id)
}

// backupsInFlight reports how many backup jobs are queued or running. The jobs
// map holds both, and entries are removed at completion.
func (s *Server) backupsInFlight() int {
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	return len(s.jobs)
}

// holdBackupsForRotation stops the dispatcher from STARTING anything new, so a
// key rotation cannot begin re-wrapping the catalog while a backup is writing
// into it (F16). Queued jobs simply stay queued — the same thing that happens
// while a restore holds their stack — and the returned function releases the
// hold and wakes the dispatcher.
func (s *Server) holdBackupsForRotation() (release func()) {
	s.rotating.Store(true)
	return func() {
		s.rotating.Store(false)
		s.signalQueue()
	}
}

// queueDepth reports the number of jobs waiting for a slot (for /metrics).
func (s *Server) queueDepth() int {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	return len(s.queue)
}

// dispatchLoop is the single goroutine that drains the queue into the
// concurrency caps. It reserves a global slot, then picks the most urgent job
// whose node also has a free slot (so a saturated node never wastes the global
// slot), and launches it. When nothing is currently eligible it waits for the
// next job to become due or for a running job to finish.
func (s *Server) dispatchLoop() {
	for {
		s.backupSem.acquire() // reserve a global slot (blocks until one frees)
		j, nsem, wait := s.takeEligible()
		if j == nil {
			s.backupSem.release() // nothing runnable right now; release the global slot
			if wait > 0 {
				// A scheduled job's jitter window hasn't opened yet — wake then.
				select {
				case <-s.queueSig:
				case <-time.After(wait):
				}
			} else {
				<-s.queueSig // wait for an enqueue or a completion
			}
			continue
		}
		go s.runQueued(j, nsem)
	}
}

// takeEligible selects the most urgent dispatchable job under the queue lock,
// acquiring its per-node slot (guaranteed non-blocking — selection already
// verified a free slot). Returns (nil, nil, wait) when nothing is runnable now,
// where wait is the time until the next jittered job becomes due (0 = wait for a
// completion/enqueue signal instead).
func (s *Server) takeEligible() (*queuedJob, *dynSem, time.Duration) {
	// A key rotation is re-wrapping every stored data key; a backup started now
	// would be sealed with the outgoing key and never re-wrapped, leaving it
	// permanently "Key mismatch" (F16). Leave everything queued until it is done.
	if s.rotating.Load() {
		return nil, nil, 0
	}
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	idx, wait := selectJob(s.queue, time.Now(), func(nodeID string) bool {
		return s.nodeSem(nodeID).hasSlot()
	}, func(j *queuedJob) bool {
		// Skip (leave queued) a job whose stack is being restored or whose
		// container is already backing up — stack-exclusive locking.
		return s.locks.canBackup(j.stackKey, j.ctrKey)
	})
	if idx < 0 {
		return nil, nil, wait
	}
	j := s.queue[idx]
	// Acquire the stack/container lock atomically with the slot; if it lost a race
	// with a just-started restore, leave the job queued and retry on next signal.
	if !s.locks.acquireBackup(j.stackKey, j.ctrKey) {
		return nil, nil, 0
	}
	sem := s.nodeSem(j.nodeID)
	if !sem.tryAcquire() { // free slot confirmed by selectJob; should not block
		s.locks.releaseBackup(j.stackKey, j.ctrKey) // undo the lock we just took
		return nil, nil, 0                          // lost a rare race; retry on next signal
	}
	s.queue = append(s.queue[:idx], s.queue[idx+1:]...)
	return j, sem, 0
}

// selectJob is the pure scheduling decision: among queued jobs that are due
// (notBefore <= now) and whose node has a free slot, return the index of the
// most urgent (highest priority, then earliest notBefore, then FIFO). If none
// are runnable now it returns -1 and the duration until the soonest jittered job
// becomes due (0 when the only blockers are saturated nodes). Pure + testable.
func selectJob(q []*queuedJob, now time.Time, nodeHasSlot func(string) bool, stackFree func(*queuedJob) bool) (int, time.Duration) {
	best := -1
	var soonest time.Time
	for i, j := range q {
		if j.notBefore.After(now) {
			if soonest.IsZero() || j.notBefore.Before(soonest) {
				soonest = j.notBefore
			}
			continue
		}
		if !nodeHasSlot(j.nodeID) {
			continue
		}
		// Stack-exclusive lock: a job whose stack is being restored or
		// whose container is already backing up isn't runnable now — leave it queued.
		if stackFree != nil && !stackFree(j) {
			continue
		}
		if best == -1 || moreUrgent(j, q[best]) {
			best = i
		}
	}
	if best != -1 {
		return best, 0
	}
	if !soonest.IsZero() {
		return -1, time.Until(soonest)
	}
	return -1, 0
}

// moreUrgent orders jobs: higher priority first, then earliest notBefore, then
// the lowest sequence (FIFO) so equal jobs keep submission order.
func moreUrgent(a, b *queuedJob) bool {
	if a.priority != b.priority {
		return a.priority > b.priority
	}
	if !a.notBefore.Equal(b.notBefore) {
		return a.notBefore.Before(b.notBefore)
	}
	return a.seq < b.seq
}

// runQueued executes a dispatched job and releases its slots. The dispatcher has
// already reserved the global slot and acquired the per-node slot; both are
// released here. Cancellation + metrics match the previous direct-run behavior.
func (s *Server) runQueued(j *queuedJob, nsem *dynSem) {
	id := j.id
	// Registered first ⇒ runs last: the lock/slot/temp releases below unwind
	// cleanly, then this recovers so a panicking backup fails just itself instead
	// of crashing the daemon (PLAN §9.10).
	defer guardPanic("backup job", id, func() {
		_ = s.store.SetBackupStatus(id, "failed", "internal error (panic) during backup")
		s.metrics.recordBackup(false)
		s.pushBackupStatus(id, j.opts.NodeID, "failed")
	})
	defer s.signalQueue() // a slot freed — let the dispatcher re-evaluate
	defer s.backupSem.release()
	defer nsem.release()
	// Release the stack/container lock acquired in takeEligible.
	defer s.locks.releaseBackup(j.stackKey, j.ctrKey)
	// #40: the one refresh this node is owed once its transfer is over.
	// Registered BEFORE the deregistration below, so it unwinds AFTER it — a
	// refresh that ran while the job was still in the map would see the node as
	// busy and no-op, and the dashboard would stay frozen until the next tick.
	defer func() { s.refreshAfterStreaming(j.opts.NodeID) }()
	defer func() {
		s.jobMu.Lock()
		delete(s.jobs, id)
		s.jobMu.Unlock()
	}()
	// The job is no longer pending once it finishes (ran or canceled-while-queued):
	// drop its durable row so a restart doesn't re-run it.
	defer func() { _ = s.store.DeleteQueuedJob(id) }()

	// Generous safety cap so a large backup over a slow link can finish; the
	// upload has its own no-progress stall guard. Background context — a user
	// navigating away never affects an in-flight backup.
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	defer cancel()

	// Wire up cancellation; bail out if it was canceled while still queued.
	s.jobMu.Lock()
	jb := s.jobs[id]
	if jb == nil || jb.canceled {
		s.jobMu.Unlock()
		return
	}
	jb.cancel = cancel
	s.jobMu.Unlock()

	// Push a live "running" delta so the backup appears/updates immediately (A7).
	s.pushBackupStatus(id, j.opts.NodeID, "running")

	_, err := s.engine.Run(ctx, j.nodeName, j.opts)
	if s.wasCanceled(id) {
		_ = s.store.SetBackupStatus(id, "canceled", "canceled by user")
		s.logSink(id, "INFO", "Backup canceled by user")
		s.metrics.recordBackup(false)
		s.pushBackupStatus(id, j.opts.NodeID, "canceled")
		return
	}
	if err != nil {
		log.Printf("backup error: %v", err)
		// F26: auto-retry a TRANSIENT failure (node/DB/registry blip) with backoff
		// instead of waiting for the next schedule. A permanent failure, or one that
		// has exhausted its attempts, falls through to the terminal alert below.
		if retry, delay := backupRetryPlan(err, j.opts.Attempt); retry {
			s.metrics.recordBackup(false)                   // this attempt failed
			s.pushBackupStatus(id, j.opts.NodeID, "failed") // …a fresh attempt is queued
			s.scheduleBackupRetry(j, err, delay)
			return
		}
		// Terminal failure — the engine no longer notifies (the queue owns the
		// backup.failed alert so mid-retry attempts stay quiet), so alert now.
		s.notifyBackupFailed(id, j, err)
	}
	s.metrics.recordBackup(err == nil)
	// Emit the real terminal status (success/failed/…) read back from the row.
	if b, gerr := s.store.GetBackup(id); gerr == nil {
		s.pushBackupStatus(id, j.opts.NodeID, b.Status)
		// F28: on a success, check this run's duration/size for drift vs the
		// container's norm — async so it never delays the dispatcher.
		if b.Status == "success" {
			go s.checkBackupAnomaly(b)
			// F69 ransomware tripwire: the engine flagged this run's delta as a
			// mass-change event and froze the target's retention — alert loudly.
			if b.Suspect != "" {
				s.notify(notify.KindBackupAnomaly, "Possible mass-change/ransomware event: "+s.backupName(j),
					b.Suspect+" — retention for this container is ON HOLD until you review its backups and clear the hold on the container page.")
			}
		}
	} else if err != nil {
		s.pushBackupStatus(id, j.opts.NodeID, "failed")
	}
}

// scheduleBackupRetry enqueues a fresh backup attempt after a transient failure
// (F26). The retry is a NEW backup id (its own row + log) so the failed attempt
// stays in history; the incremented Attempt rides along in the persisted opts so a
// restart resumes mid-retry and the cap still applies.
func (s *Server) scheduleBackupRetry(j *queuedJob, cause error, delay time.Duration) {
	retryOpts := j.opts
	retryOpts.BackupID = "" // fresh attempt gets its own id
	retryOpts.Attempt = j.opts.Attempt + 1
	name := s.backupName(j)
	newID := s.enqueueBackupAt(j.nodeName, retryOpts, j.priority, time.Now().Add(delay))
	// Note it on the failed attempt's own log so its row explains itself, and on the
	// new attempt's log so the retry is traceable end to end.
	msg := fmt.Sprintf("Backup of %s failed transiently (%v) — auto-retrying (attempt %d of %d) in %s",
		name, cause, retryOpts.Attempt+1, maxBackupAttempts, delay)
	s.logSink(j.id, "INFO", msg)
	s.logSink(newID, "INFO", fmt.Sprintf("Auto-retry (attempt %d of %d) after a transient failure of %s",
		retryOpts.Attempt+1, maxBackupAttempts, name))
}

// notifyBackupFailed sends the terminal backup.failed alert (F26 moved this out of
// the engine's fail() so retried attempts don't alert). The copy matches what the
// engine used to send, so notifications are unchanged for the permanent/exhausted case.
func (s *Server) notifyBackupFailed(id string, j *queuedJob, cause error) {
	name := s.backupName(j)
	if b, gerr := s.store.GetBackup(id); gerr == nil && b.TargetName != "" {
		name = b.TargetName
	}
	// F88: a refused sidecar pin is a possible supply-chain event, not an ordinary
	// backup failure — it gets its OWN critical alert naming the image, deduped
	// per presented digest so a nightly schedule doesn't re-alert every run.
	var pinErr *dockercli.SidecarDigestChangedError
	if errors.As(cause, &pinErr) {
		nodeName := j.nodeID
		if n, nerr := s.store.GetNode(j.nodeID); nerr == nil {
			nodeName = n.Name
		}
		s.alertSidecarChanged(nodeName, j.nodeID, pinErr)
	}
	s.notify(notify.KindBackupFailed, "Backup FAILED: "+name,
		fmt.Sprintf("Backup of %s failed: %v", name, cause))
}

// backupName resolves a human name for a queued job's target from the inventory
// cache, falling back to a short backup id when the container isn't cached (e.g. a
// node-unreachable failure before it could be inspected).
func (s *Server) backupName(j *queuedJob) string {
	if st := s.getStat(j.nodeID); st != nil {
		for _, c := range st.Containers {
			if c != nil && c.ID == j.opts.ContainerID && c.Name != "" {
				return c.Name
			}
		}
	}
	if len(j.id) > 8 {
		return j.id[:8]
	}
	return j.id
}
