package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"regexp"
	"sort"
	"time"

	"dockback/internal/notify"
)

// Cancelable restores. A restore is long-running and can legitimately need
// stopping — most commonly while the post-restore health gate waits for a
// container that will never come up. Every restore entry point (single backup,
// stack, whole node) registers its context here under the SAME id the UI already
// streams its log on, so one endpoint cancels any of them:
//
//	single backup : the backup id
//	stack         : "stack:<project>"
//	whole node    : "node:<nodeID>"
//
// Cancelling is deliberately NOT a rollback: the engine stops where it is and
// reports exactly what state things are in (naming the pre-restore snapshot when
// one was taken), so nothing is silently thrown away.

type restoreRun struct {
	cancel    context.CancelFunc
	canceled  bool
	startedAt int64
	label     string // human label for logs/audit ("nextcloud", "stack blog", …)
	// nodeID is the node the archive streams INTO, so the dashboard can stop
	// polling a proxy this run is saturating (#40). Empty when the caller does
	// not know it, which simply leaves the polling as it was.
	nodeID string
	// executionID is this run's own identity, since id is reused by every restore of
	// the same backup or stack.
	executionID string
}

// executionIDPattern is what a client may propose: newRestoreExecutionID's shape.
var executionIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// restoreExecutionIDBytes is the random size of an execution id.
const restoreExecutionIDBytes = 16

// resolveExecutionID keeps a well-formed id a client chose, so its console can know
// which run is its own from before the run starts, and otherwise makes one.
func resolveExecutionID(proposed string) string {
	if executionIDPattern.MatchString(proposed) {
		return proposed
	}
	raw := make([]byte, restoreExecutionIDBytes)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

// beginRestoreRun registers a cancelable restore under id and returns its
// context plus a finish func the caller MUST defer. ok=false when a restore
// with that id is already registered (the stack/container locks normally
// prevent this; this is the last-resort guard). A restore writes a safety
// snapshot outside the job queue, so it is counted for key rotation, waiting
// first while a rotation is under way (beginOutsideQueueRun). proposedExecutionID may
// be empty. The run is recorded, and a run that ends without reporting an outcome is
// recorded as interrupted when finish runs.
func (s *Server) beginRestoreRun(parent context.Context, id, label, nodeID string, timeout time.Duration, proposedExecutionID string) (context.Context, func(), bool) {
	if s.isStopping() {
		return nil, nil, false
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	stopWithProcess := context.AfterFunc(s.rootContext(), cancel)
	s.restoreMu.Lock()
	if s.restores == nil {
		s.restores = map[string]*restoreRun{}
	}
	if _, dup := s.restores[id]; dup {
		s.restoreMu.Unlock()
		stopWithProcess()
		cancel()
		return nil, nil, false
	}
	executionID := resolveExecutionID(proposedExecutionID)
	s.restores[id] = &restoreRun{cancel: cancel, startedAt: time.Now().Unix(), label: label, nodeID: nodeID, executionID: executionID}
	s.restoreMu.Unlock()
	if err := s.store.BeginRestoreExecution(executionID, id, label); err != nil {
		log.Printf("restore %s: could not record its start: %v", id, err)
	}
	endRun := s.beginOutsideQueueRun(id)

	return ctx, func() {
		s.finishUnreportedExecution(id, executionID)
		stopWithProcess()
		endRun()
		s.restoreMu.Lock()
		delete(s.restores, id)
		s.restoreMu.Unlock()
		cancel()
		// #40: deregistered first, so the refresh this node is owed sees a node
		// that is no longer busy.
		s.refreshAfterStreaming(nodeID)
	}, true
}

// cancelRestoreRun signals a running restore to stop. Returns false when no
// restore is registered under that id.
func (s *Server) cancelRestoreRun(id string) (label string, ok bool) {
	s.restoreMu.Lock()
	defer s.restoreMu.Unlock()
	r, ok := s.restores[id]
	if !ok {
		return "", false
	}
	r.canceled = true
	r.cancel()
	return r.label, true
}

// restoreWasCanceled reports whether the run under id was canceled BY AN
// OPERATOR (as opposed to hitting its own deadline), so the finishing log line
// can say "canceled" instead of "failed".
func (s *Server) restoreWasCanceled(id string) bool {
	s.restoreMu.Lock()
	defer s.restoreMu.Unlock()
	r, ok := s.restores[id]
	return ok && r.canceled
}

// handleCancelRestore — POST /api/restores/{id}/cancel (auth + csrf, audited).
// The id is the run id the UI streams the restore's log on.
func (s *Server) handleCancelRestore(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	label, ok := s.cancelRestoreRun(id)
	if !ok {
		errJSON(w, http.StatusNotFound, "no restore is running with that id")
		return
	}
	_ = s.store.Audit(userFrom(r), "restore.cancel", id, label)
	s.logSink(id, "WARN", "Cancel requested by the operator — stopping the restore at the next safe point…")
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "canceling"})
}

// RunningRestore is one in-flight restore as the UI sees it (F100).
//
// Deliberately few fields. A page reloaded mid-restore needs to know WHICH run to
// re-attach to (ID, and ExecutionID since ID is reused), what to call it in a banner
// (Label), and how long it has been going (StartedAt) — and nothing else. The restore's target container,
// node and options stay server-side: this endpoint exists to let an operator
// find and stop a running restore, not to describe one.
type RunningRestore struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	StartedAt int64  `json:"started_at"`
	// ExecutionID tells this run apart from any other restore under the same ID.
	ExecutionID string `json:"execution_id"`
}

// runningRestores returns the restores currently in flight, so a UI that was
// reloaded mid-restore can re-attach and still offer Cancel.
//
// Sorted oldest-first so the list is stable across polls — an unsorted map walk
// would reorder the banner on every 15-second refresh.
func (s *Server) runningRestores() []RunningRestore {
	s.restoreMu.Lock()
	defer s.restoreMu.Unlock()
	out := make([]RunningRestore, 0, len(s.restores))
	for id, r := range s.restores {
		out = append(out, RunningRestore{ID: id, Label: r.label, StartedAt: r.startedAt, ExecutionID: r.executionID})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt != out[j].StartedAt {
			return out[i].StartedAt < out[j].StartedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// handleListRestores — GET /api/restores. Run id, label and start time only; no
// target, node or credential detail.
func (s *Server) handleListRestores(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"running": s.runningRestores()})
}

// handleRestoreStatus — GET /api/restores/{id}/status[?execution=<id>]. The authority
// on how a restore ended, for a console that missed run.done or reattached. It reports
// the named execution, or the newest under this id. 404 means no such record.
func (s *Server) handleRestoreStatus(w http.ResponseWriter, r *http.Request) {
	execution, err := s.store.GetRestoreExecution(r.PathValue("id"), r.URL.Query().Get("execution"))
	if errors.Is(err, sql.ErrNoRows) {
		errJSON(w, http.StatusNotFound, "no record of that restore")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "could not read restore status")
		return
	}
	writeJSON(w, http.StatusOK, execution)
}

// restoreRunExecution is the execution behind a registered run, or "" for none.
func (s *Server) restoreRunExecution(id string) string {
	s.restoreMu.Lock()
	defer s.restoreMu.Unlock()
	if r, ok := s.restores[id]; ok {
		return r.executionID
	}
	return ""
}

// finishRestoreExecution records how a restore ended and only then announces it, so
// a console that hears the event and asks gets the same answer. It is what every
// restore path ends through.
func (s *Server) finishRestoreExecution(id, outcome, message string) {
	executionID := s.restoreRunExecution(id)
	if executionID != "" {
		if _, err := s.store.FinishRestoreExecution(executionID, outcome, message); err != nil {
			log.Printf("restore %s: could not record its outcome: %v", id, err)
		}
	}
	s.publishExecutionDone(id, executionID, outcome, message)
}

// finishUnreportedExecution closes a run that returned without an outcome, which a
// panic does. Without this it would read as running until the next restart.
func (s *Server) finishUnreportedExecution(id, executionID string) {
	const message = "This restore ended without reporting an outcome. Check the log, and what state the container is in."
	finished, err := s.store.FinishRestoreExecution(executionID, runOutcomeInterrupted, message)
	if err != nil {
		log.Printf("restore %s: could not record its outcome: %v", id, err)
		return
	}
	if finished {
		s.publishExecutionDone(id, executionID, runOutcomeInterrupted, message)
	}
}

// interruptAbandonedRestores settles what the last process left running: at startup
// nothing can be, so a row that says so belongs to a run that died with it.
func (s *Server) interruptAbandonedRestores() {
	const message = "DockBack stopped before this restore finished, so it did not complete. Data already written stays written."
	count, err := s.store.InterruptRunningRestoreExecutions(runOutcomeInterrupted, message)
	if err != nil {
		log.Printf("startup: could not settle restores the last process left running: %v", err)
		return
	}
	if count > 0 {
		log.Printf("startup: %d restore(s) were running when DockBack last stopped, recorded as interrupted", count)
	}
}

// restoreOutcome logs a restore's terminal line, distinguishing an operator
// cancel from a genuine failure so the console (and the operator's memory of
// what happened) stays honest. Shared by every restore entry point.
// restoreLabel names what was being restored, for an alert title. Falls back to
// the run id when the row is gone (a node/stack run has no backup row of its own).
func (s *Server) restoreLabel(id string) string {
	if b, err := s.store.GetBackup(id); err == nil && b.TargetName != "" {
		return b.TargetName
	}
	return id
}

// restoreOutcome records how a restore ended — in the run log for the operator,
// and as a structured run.done event for the console (#N10), which used to have
// to guess from the wording of the line above it.
func (s *Server) restoreOutcome(id string, err error, doneMsg string) {
	switch {
	case err == nil:
		s.logSink(id, "INFO", doneMsg)
		s.finishRestoreExecution(id, runOutcomeOK, doneMsg)
	case s.restoreWasCanceled(id):
		s.logSink(id, "WARN", "Restore CANCELED by the operator: "+err.Error())
		s.finishRestoreExecution(id, runOutcomeCanceled, err.Error())
		// F92: a canceled restore is not a failure — the operator chose it — but a
		// DESTRUCTIVE operation was interrupted part-way, so the alert inbox keeps
		// a record of what state things were left in. Warning, not critical.
		s.notify(notify.KindRestoreCanceled, "Restore canceled: "+s.restoreLabel(id),
			"A restore was stopped by the operator before it finished. Data already written stays written — DockBack does not roll back behind your back. "+err.Error())
	case s.isStopping():
		msg := "Restore interrupted by DockBack stopping: " + err.Error()
		s.logSink(id, "ERR", msg)
		s.finishRestoreExecution(id, runOutcomeInterrupted, msg)
		s.notify(notify.KindRestoreFailed, "Restore interrupted: "+s.restoreLabel(id),
			"DockBack stopped during this restore, so it did not finish. Data already written stays written. "+err.Error())
	default:
		s.logSink(id, "ERR", "Restore failed: "+err.Error())
		s.finishRestoreExecution(id, runOutcomeFailed, err.Error())
	}
}
