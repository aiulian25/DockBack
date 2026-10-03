package api

import (
	"context"
	"net/http"
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
}

// beginRestoreRun registers a cancelable restore under id and returns its
// context plus a finish func the caller MUST defer. ok=false when a restore
// with that id is already registered (the stack/container locks normally
// prevent this; this is the last-resort guard).
func (s *Server) beginRestoreRun(parent context.Context, id, label, nodeID string, timeout time.Duration) (context.Context, func(), bool) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	s.restoreMu.Lock()
	if s.restores == nil {
		s.restores = map[string]*restoreRun{}
	}
	if _, dup := s.restores[id]; dup {
		s.restoreMu.Unlock()
		cancel()
		return nil, nil, false
	}
	s.restores[id] = &restoreRun{cancel: cancel, startedAt: time.Now().Unix(), label: label, nodeID: nodeID}
	s.restoreMu.Unlock()

	return ctx, func() {
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
// Deliberately three fields. A page reloaded mid-restore needs to know WHICH run
// to re-attach to (ID), what to call it in a banner (Label), and how long it has
// been going (StartedAt) — and nothing else. The restore's target container,
// node and options stay server-side: this endpoint exists to let an operator
// find and stop a running restore, not to describe one.
type RunningRestore struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	StartedAt int64  `json:"started_at"`
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
		out = append(out, RunningRestore{ID: id, Label: r.label, StartedAt: r.startedAt})
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
		s.publishRunDone(id, runOutcomeOK, doneMsg)
	case s.restoreWasCanceled(id):
		s.logSink(id, "WARN", "Restore CANCELED by the operator: "+err.Error())
		s.publishRunDone(id, runOutcomeCanceled, err.Error())
		// F92: a canceled restore is not a failure — the operator chose it — but a
		// DESTRUCTIVE operation was interrupted part-way, so the alert inbox keeps
		// a record of what state things were left in. Warning, not critical.
		s.notify(notify.KindRestoreCanceled, "Restore canceled: "+s.restoreLabel(id),
			"A restore was stopped by the operator before it finished. Data already written stays written — DockBack does not roll back behind your back. "+err.Error())
	default:
		s.logSink(id, "ERR", "Restore failed: "+err.Error())
		s.publishRunDone(id, runOutcomeFailed, err.Error())
	}
}
