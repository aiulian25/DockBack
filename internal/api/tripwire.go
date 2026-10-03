package api

import (
	"fmt"
	"net/http"
	"strings"

	"dockback/internal/backup"
)

// Ransomware tripwire (F69) — API surface. Detection lives in the engine
// (internal/backup: AnalyzeDelta/SuspectDelta on incremental deltas); this file
// exposes the hold state and the operator's "reviewed — clear it" action.

// tripwireHoldReason returns the human reason of an active retention hold for a
// (node, target), "" when none. The stored value is "<reason>|<unix>".
func (s *Server) tripwireHoldReason(nodeID, target string) string {
	v, _ := s.store.GetSetting(backup.RetentionHoldKey(nodeID, target), "")
	if v == "" {
		return ""
	}
	if i := strings.LastIndexByte(v, '|'); i >= 0 {
		return v[:i]
	}
	return v
}

// handleClearTripwire clears a container's retention hold and resets the suspect
// flag on its rows — the operator has reviewed the event (F69). Audited.
func (s *Server) handleClearTripwire(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("id")
	name, err := s.resolveContainerName(nodeID, r.PathValue("cid"))
	if err != nil || name == "" {
		errJSON(w, http.StatusBadGateway, "could not resolve container name")
		return
	}
	_ = s.store.DeleteSetting(backup.RetentionHoldKey(nodeID, name))
	n, _ := s.store.ClearBackupSuspects(nodeID, name)
	_ = s.store.Audit(userFrom(r), "tripwire.clear", name, fmt.Sprintf("retention hold cleared; %d suspect row(s) reset", n))
	s.logSink("security", "INFO", fmt.Sprintf("Tripwire hold cleared for %s — retention resumes (%d suspect row(s) reset)", name, n))
	writeJSON(w, http.StatusOK, map[string]any{"status": "cleared", "rows_cleared": n})
}
