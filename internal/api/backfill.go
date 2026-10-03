package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"dockback/internal/backup"
	"dockback/internal/notify"
	"dockback/internal/store"
)

// backfillState is the live progress of a per-destination backfill sweep (F51).
type backfillState struct {
	Total     int   `json:"total"`
	Done      int   `json:"done"`
	Failed    int   `json:"failed"`
	Running   bool  `json:"running"`
	StartedAt int64 `json:"started_at,omitempty"`
}

// hasHealthyDestCopy reports whether a backup already holds a GOOD copy on destID —
// a location of Kind "dest" for that destination with an empty (ok) status. A
// "failed"/"deferred" copy is NOT healthy, so backfill re-attempts it.
func hasHealthyDestCopy(b *store.Backup, destID string) bool {
	if b.LocationsJSON == "" {
		return false
	}
	var locs []backup.Location
	if json.Unmarshal([]byte(b.LocationsJSON), &locs) != nil {
		return false
	}
	for _, l := range locs {
		if l.Kind == "dest" && l.DestID == destID && l.Status == "" {
			return true
		}
	}
	return false
}

// backfillCandidates returns the successful backups that should be mirrored to
// destID: those with no healthy copy there AND whose node's effective policy
// includes destID (so a node that excludes this destination is skipped), ordered
// OLDEST-FIRST. Pure — the effective-destinations lookup is injected so it's
// testable without a Server, and it never touches Docker or the network.
func backfillCandidates(all []*store.Backup, destID string, effectiveDests func(nodeID string) []string) []*store.Backup {
	includes := map[string]bool{} // nodeID -> policy includes destID (memoized)
	includesDest := func(nodeID string) bool {
		if v, ok := includes[nodeID]; ok {
			return v
		}
		v := false
		for _, d := range effectiveDests(nodeID) {
			if d == destID {
				v = true
				break
			}
		}
		includes[nodeID] = v
		return v
	}

	var out []*store.Backup
	for _, b := range all {
		if b.Status != "success" {
			continue
		}
		if !includesDest(b.NodeID) {
			continue
		}
		if hasHealthyDestCopy(b, destID) {
			continue
		}
		out = append(out, b)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// backfillStart marks a destination's backfill running (F51). Returns false when one
// is already running for it (the caller answers 409), so only one runs at a time.
func (s *Server) backfillStart(destID string, total int) bool {
	s.backfillMu.Lock()
	defer s.backfillMu.Unlock()
	if s.backfills == nil {
		s.backfills = map[string]*backfillState{}
	}
	if st := s.backfills[destID]; st != nil && st.Running {
		return false
	}
	s.backfills[destID] = &backfillState{Total: total, Running: true, StartedAt: time.Now().Unix()}
	return true
}

func (s *Server) backfillBump(destID string, done, failed int) {
	s.backfillMu.Lock()
	defer s.backfillMu.Unlock()
	if st := s.backfills[destID]; st != nil {
		st.Done += done
		st.Failed += failed
	}
}

func (s *Server) backfillFinish(destID string) {
	s.backfillMu.Lock()
	defer s.backfillMu.Unlock()
	if st := s.backfills[destID]; st != nil {
		st.Running = false
	}
}

// backfillGet returns a snapshot of a destination's backfill progress (zero value
// when it has never run).
func (s *Server) backfillGet(destID string) backfillState {
	s.backfillMu.Lock()
	defer s.backfillMu.Unlock()
	if st := s.backfills[destID]; st != nil {
		return *st
	}
	return backfillState{}
}

// handleBackfillDestination starts a background sweep that mirrors every existing
// backup missing a healthy copy on this destination, oldest-first, ONE upload at a
// time through the untouched single-backup MirrorExisting path (F51). It honors the
// destination's upload window/throttle (F14) exactly as "Send offsite now" does.
func (s *Server) handleBackfillDestination(w http.ResponseWriter, r *http.Request) {
	destID := r.PathValue("id")
	d, err := s.store.GetDestination(destID)
	if err != nil {
		errJSON(w, http.StatusNotFound, "destination not found")
		return
	}
	all, err := s.store.ListBackups("", 100000)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	cands := backfillCandidates(all, destID, func(nodeID string) []string { return s.effectivePolicy(nodeID).Destinations })

	if !s.backfillStart(destID, len(cands)) {
		errJSON(w, http.StatusConflict, "a backfill for this destination is already running")
		return
	}
	_ = s.store.Audit(userFrom(r), "destination.backfill", d.Name, fmt.Sprintf("candidates=%d", len(cands)))

	go func() {
		defer s.backfillFinish(destID)
		defer guardPanic("backfill", destID, func() { s.logSink("queue", "ERR", "Backfill aborted: internal error (panic)") })

		s.logSink("queue", "INFO", fmt.Sprintf("Backfill of %q started — %d backup(s) with no healthy copy there", d.Name, len(cands)))
		for _, b := range cands {
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
			mErr := s.engine.MirrorExisting(ctx, b.ID, []string{destID})
			cancel()
			if mErr != nil {
				s.backfillBump(destID, 0, 1)
				s.logSink("queue", "ERR", fmt.Sprintf("Backfill: %s → %q failed: %v", b.TargetName, d.Name, mErr))
			} else {
				s.backfillBump(destID, 1, 0)
				s.logSink("queue", "INFO", fmt.Sprintf("Backfill: %s → %q done", b.TargetName, d.Name))
			}
		}

		st := s.backfillGet(destID)
		if st.Failed > 0 {
			s.notify(notify.KindNoOffsite, "Backfill finished: "+d.Name,
				fmt.Sprintf("Backfilled %q — %d mirrored, %d failed of %d. Re-run the backfill or check the destination.", d.Name, st.Done, st.Failed, st.Total))
		} else {
			s.logSink("queue", "INFO", fmt.Sprintf("Backfill of %q finished — %d mirrored.", d.Name, st.Done))
		}
		_ = s.store.Audit("backfill", "destination.backfill.done", d.Name, fmt.Sprintf("done=%d failed=%d total=%d", st.Done, st.Failed, st.Total))
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{"status": "started", "candidates": len(cands)})
}

// handleBackfillStatus reports a destination's backfill progress (F51).
func (s *Server) handleBackfillStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.backfillGet(r.PathValue("id")))
}
