package api

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// allFree is a nodeHasSlot predicate where every node has capacity.
func allFree(string) bool { return true }

// anyStack is a stackFree predicate where no stack is locked (PLAN §9.10).
func anyStack(*queuedJob) bool { return true }

func TestSelectJobPriority(t *testing.T) {
	now := time.Now()
	q := []*queuedJob{
		{id: "sched", nodeID: "n1", priority: prioScheduled, seq: 1, notBefore: now},
		{id: "manual", nodeID: "n1", priority: prioInteractive, seq: 2, notBefore: now},
	}
	idx, wait := selectJob(q, now, allFree, anyStack)
	if wait != 0 || idx < 0 || q[idx].id != "manual" {
		t.Fatalf("expected interactive job picked, got idx=%d wait=%v", idx, wait)
	}
}

func TestSelectJobFIFOWithinPriority(t *testing.T) {
	now := time.Now()
	q := []*queuedJob{
		{id: "b", nodeID: "n1", priority: prioScheduled, seq: 5, notBefore: now},
		{id: "a", nodeID: "n1", priority: prioScheduled, seq: 2, notBefore: now},
	}
	idx, _ := selectJob(q, now, allFree, anyStack)
	if q[idx].id != "a" {
		t.Fatalf("expected lowest seq (a) first, got %s", q[idx].id)
	}
}

func TestSelectJobJitterNotYetDue(t *testing.T) {
	now := time.Now()
	q := []*queuedJob{
		{id: "future", nodeID: "n1", priority: prioScheduled, seq: 1, notBefore: now.Add(10 * time.Second)},
	}
	idx, wait := selectJob(q, now, allFree, anyStack)
	if idx != -1 {
		t.Fatalf("future job should not be selected, got idx=%d", idx)
	}
	if wait <= 0 || wait > 10*time.Second {
		t.Fatalf("expected wait ~10s until due, got %v", wait)
	}
}

func TestSelectJobSkipsSaturatedNode(t *testing.T) {
	now := time.Now()
	q := []*queuedJob{
		{id: "onBusy", nodeID: "busy", priority: prioInteractive, seq: 1, notBefore: now},
		{id: "onFree", nodeID: "free", priority: prioScheduled, seq: 2, notBefore: now},
	}
	// "busy" is saturated even though its job is higher priority — must pick the
	// lower-priority job on the node that has a free slot (no wasted global slot).
	free := func(node string) bool { return node != "busy" }
	idx, wait := selectJob(q, now, free, anyStack)
	if wait != 0 || idx < 0 || q[idx].id != "onFree" {
		t.Fatalf("expected onFree picked, got idx=%d wait=%v", idx, wait)
	}
}

func TestSelectJobAllSaturated(t *testing.T) {
	now := time.Now()
	q := []*queuedJob{
		{id: "x", nodeID: "busy", priority: prioInteractive, seq: 1, notBefore: now},
	}
	idx, wait := selectJob(q, now, func(string) bool { return false }, anyStack)
	if idx != -1 || wait != 0 {
		t.Fatalf("all-saturated should return -1/0 (wait for completion), got idx=%d wait=%v", idx, wait)
	}
}

func TestSelectJobFutureAndSaturatedMix(t *testing.T) {
	now := time.Now()
	q := []*queuedJob{
		{id: "busyNow", nodeID: "busy", priority: prioInteractive, seq: 1, notBefore: now},
		{id: "later", nodeID: "free", priority: prioScheduled, seq: 2, notBefore: now.Add(5 * time.Second)},
	}
	// busyNow is due but saturated; later is free but not due → wait until later.
	idx, wait := selectJob(q, now, func(node string) bool { return node != "busy" }, anyStack)
	if idx != -1 {
		t.Fatalf("nothing should be runnable now, got idx=%d", idx)
	}
	if wait <= 0 || wait > 5*time.Second {
		t.Fatalf("expected wait ~5s, got %v", wait)
	}
}

func TestSelectJobEmpty(t *testing.T) {
	idx, wait := selectJob(nil, time.Now(), allFree, anyStack)
	if idx != -1 || wait != 0 {
		t.Fatalf("empty queue should return -1/0, got idx=%d wait=%v", idx, wait)
	}
}

// transientErr is a net.Error, which backup.IsTransient treats as retryable.
type transientErr struct{}

func (transientErr) Error() string   { return "dial tcp: connection refused" }
func (transientErr) Timeout() bool   { return false }
func (transientErr) Temporary() bool { return true }

var _ net.Error = transientErr{}

// TestBackupRetryPlan locks in the F26 decision: a transient failure retries with
// backoff until the attempt cap, and a permanent failure never does.
func TestBackupRetryPlan(t *testing.T) {
	transient := transientErr{}
	permanent := errors.New("no space left on device")

	// Transient, attempts remaining → retry with the scheduled backoff.
	if retry, delay := backupRetryPlan(transient, 0); !retry || delay != 1*time.Minute {
		t.Errorf("attempt 0 transient: retry=%v delay=%v, want true/1m", retry, delay)
	}
	if retry, delay := backupRetryPlan(transient, 1); !retry || delay != 5*time.Minute {
		t.Errorf("attempt 1 transient: retry=%v delay=%v, want true/5m", retry, delay)
	}
	// Third attempt (index 2) is the last of maxBackupAttempts=3 → no more retries.
	if retry, _ := backupRetryPlan(transient, 2); retry {
		t.Error("attempt 2 transient: expected no retry (attempt cap reached)")
	}
	// A permanent error is never retried, even on the first attempt.
	if retry, _ := backupRetryPlan(permanent, 0); retry {
		t.Error("permanent error must not be retried")
	}
	// A nil error (success) never retries.
	if retry, _ := backupRetryPlan(nil, 0); retry {
		t.Error("nil error must not be retried")
	}
}

// TestBackupRetryDelay checks the backoff schedule and its clamping.
func TestBackupRetryDelay(t *testing.T) {
	want := []time.Duration{1 * time.Minute, 5 * time.Minute, 15 * time.Minute}
	for i, w := range want {
		if got := backupRetryDelay(i); got != w {
			t.Errorf("backupRetryDelay(%d) = %v, want %v", i, got, w)
		}
	}
	// Out-of-range indices clamp to the ends (no panic).
	if got := backupRetryDelay(-1); got != 1*time.Minute {
		t.Errorf("backupRetryDelay(-1) = %v, want 1m", got)
	}
	if got := backupRetryDelay(99); got != 15*time.Minute {
		t.Errorf("backupRetryDelay(99) = %v, want 15m", got)
	}
}

// Cancelling a still-QUEUED backup must remove its registry entry, not just
// dequeue it: runQueued (the only other place s.jobs is pruned) never runs for
// a job cancelled before dispatch, so without this the map leaks one permanent
// entry per cancelled click, taxing every logSourceFor walk (Step 8).
func TestCancelQueuedBackupDropsJobEntry(t *testing.T) {
	s := &Server{store: testStore(t), jobs: map[string]*backupJob{}}

	// A queued job has no cancel func yet.
	s.jobs["b1"] = &backupJob{nodeID: "n1", containerID: "c1"}
	s.queue = append(s.queue, &queuedJob{id: "b1", nodeID: "n1", ctrKey: "n1\x00#c1"})

	if !s.cancelBackup("b1") {
		t.Fatal("cancelBackup should report success for a known job")
	}
	if _, ok := s.jobs["b1"]; ok {
		t.Fatal("a queued job's registry entry must be removed on cancel")
	}
	if len(s.jobs) != 0 {
		t.Fatalf("jobs map should be empty, got %d entries", len(s.jobs))
	}
	if len(s.queue) != 0 {
		t.Fatalf("queue should be empty, got %d", len(s.queue))
	}
}

// Cancelling a RUNNING backup must cancel its context but must NOT delete the
// registry entry — runQueued owns that cleanup, and deleting it here would race
// the deferred delete and could drop a future entry that reuses the id.
func TestCancelRunningBackupKeepsEntryForRunQueued(t *testing.T) {
	s := &Server{store: testStore(t), jobs: map[string]*backupJob{}}

	ctx, cancel := context.WithCancel(context.Background())
	s.jobs["r1"] = &backupJob{nodeID: "n1", containerID: "c2", cancel: cancel}

	if !s.cancelBackup("r1") {
		t.Fatal("cancelBackup should report success for a running job")
	}
	// The context was cancelled…
	select {
	case <-ctx.Done():
	default:
		t.Fatal("a running job's context should be cancelled")
	}
	// …but the entry stays for runQueued's deferred cleanup, marked cancelled.
	jb, ok := s.jobs["r1"]
	if !ok {
		t.Fatal("a running job's entry must remain for runQueued to clean up")
	}
	if !jb.canceled {
		t.Fatal("a running job should be marked cancelled")
	}
}

func TestCancelUnknownBackupIsFalse(t *testing.T) {
	s := &Server{store: testStore(t), jobs: map[string]*backupJob{}}
	if s.cancelBackup("nope") {
		t.Fatal("cancelling an unknown id must return false")
	}
}

func TestSelectJobSkipsLockedStack(t *testing.T) {
	now := time.Now()
	q := []*queuedJob{
		{id: "locked", nodeID: "n1", priority: prioInteractive, seq: 1, notBefore: now, stackKey: "n1@web", ctrKey: "c1"},
		{id: "free", nodeID: "n1", priority: prioScheduled, seq: 2, notBefore: now, stackKey: "n1@db", ctrKey: "c2"},
	}
	// The higher-priority job's stack is being restored — skip it, pick the other.
	stackFree := func(j *queuedJob) bool { return j.stackKey != "n1@web" }
	idx, wait := selectJob(q, now, allFree, stackFree)
	if wait != 0 || idx < 0 || q[idx].id != "free" {
		t.Fatalf("expected the unlocked job picked, got idx=%d wait=%v", idx, wait)
	}
}
