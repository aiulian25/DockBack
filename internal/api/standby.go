package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"

	"dockback/internal/backup"
	"dockback/internal/notify"
	"dockback/internal/store"
)

// Pilot-light standby rehearsals (F62). On a cadence, DockBack proves that a
// container's newest verified backup restores + boots on a DESIGNATED FALLBACK
// NODE — the question homelab DR actually hinges on, which same-node drills never
// answer. The loop mirrors drillTick: an hourly tick picks the most-overdue
// configured container (bounded one per cycle so there's no load spike), restores
// an isolated clone onto the standby node, health-gates it, records the result,
// and tears the clone down. Failures alert at drill severity.
const standbyTickInterval = 1 * time.Hour

// standbyReq is the PUT payload configuring a container's standby rehearsal.
type standbyReq struct {
	StandbyNode  string `json:"standby_node"`
	IntervalDays int    `json:"interval_days"`
}

// clampStandbyInterval keeps the cadence sane (a day floor, a year ceiling);
// 0/unset falls back to weekly.
func clampStandbyInterval(days int) int {
	if days <= 0 {
		return 7
	}
	if days > 365 {
		return 365
	}
	return days
}

// handleSetStandby configures (PUT) a container's cross-node standby rehearsal.
// Validates that the standby node exists and differs from the source node.
func (s *Server) handleSetStandby(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("id")
	cid := r.PathValue("cid")
	var req standbyReq
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.StandbyNode == "" {
		errJSON(w, http.StatusBadRequest, "standby_node is required")
		return
	}
	if req.StandbyNode == nodeID {
		errJSON(w, http.StatusBadRequest, "the standby node must be a DIFFERENT node than the source")
		return
	}
	if _, err := s.store.GetNode(req.StandbyNode); err != nil {
		errJSON(w, http.StatusBadRequest, "standby node not found")
		return
	}
	name, err := s.resolveContainerName(nodeID, cid)
	if err != nil || name == "" {
		errJSON(w, http.StatusBadGateway, "could not resolve container name")
		return
	}
	interval := clampStandbyInterval(req.IntervalDays)
	if err := s.store.SetStandby(nodeID, name, req.StandbyNode, interval); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "standby.set", nodeID+"/"+name, req.StandbyNode)
	sb, _ := s.store.GetStandby(nodeID, name)
	writeJSON(w, http.StatusOK, sb)
}

// handleDeleteStandby removes a container's standby rehearsal config.
func (s *Server) handleDeleteStandby(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("id")
	name, err := s.resolveContainerName(nodeID, r.PathValue("cid"))
	if err != nil || name == "" {
		errJSON(w, http.StatusBadGateway, "could not resolve container name")
		return
	}
	if err := s.store.DeleteStandby(nodeID, name); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "standby.delete", nodeID+"/"+name, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleGetStandby returns a container's standby config + last result (or null).
func (s *Server) handleGetStandby(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("id")
	name, err := s.resolveContainerName(nodeID, r.PathValue("cid"))
	if err != nil || name == "" {
		errJSON(w, http.StatusBadGateway, "could not resolve container name")
		return
	}
	sb, err := s.store.GetStandby(nodeID, name)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, nil)
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

// handleRunStandby triggers a standby rehearsal on demand (async). The result
// lands on the container's card / runbook on its next refresh.
func (s *Server) handleRunStandby(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("id")
	name, err := s.resolveContainerName(nodeID, r.PathValue("cid"))
	if err != nil || name == "" {
		errJSON(w, http.StatusBadGateway, "could not resolve container name")
		return
	}
	sb, err := s.store.GetStandby(nodeID, name)
	if errors.Is(err, store.ErrNotFound) {
		errJSON(w, http.StatusBadRequest, "standby is not configured for this container")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "standby.run", nodeID+"/"+name, sb.StandbyNode)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		s.runStandbyRehearsal(ctx, sb)
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "rehearsing"})
}

// startStandby launches the periodic standby-rehearsal loop (F62).
func (s *Server) startStandby() {
	go func() {
		t := time.NewTicker(standbyTickInterval)
		defer t.Stop()
		for range t.C {
			s.standbyTick()
		}
	}()
}

// overdueStandby returns the configured standbys due for a rehearsal (never run,
// or last run older than their interval), oldest-run first. Pure, so the
// scheduling decision is unit-testable.
func overdueStandby(list []*store.Standby, now int64) []*store.Standby {
	var due []*store.Standby
	for _, sb := range list {
		interval := clampStandbyInterval(sb.IntervalDays)
		cutoff := now - int64(interval)*24*3600
		if sb.LastRun < cutoff {
			due = append(due, sb)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].LastRun < due[j].LastRun })
	return due
}

// standbyTick rehearses the single most-overdue standby (bounded one per cycle so
// a fleet is swept gradually without a load spike). It rides the restore lock on
// the clone key so it never overlaps real work, and reschedules (skips) when busy.
func (s *Server) standbyTick() {
	list, err := s.store.ListStandby()
	if err != nil || len(list) == 0 {
		return
	}
	due := overdueStandby(list, time.Now().Unix())
	if len(due) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	s.runStandbyRehearsal(ctx, due[0])
}

// runStandbyRehearsal performs one rehearsal: pick the newest verified backup,
// take the clone's restore lock, rehearse on the standby node, record the result,
// and alert on failure. Guarded against panics like every background task.
func (s *Server) runStandbyRehearsal(ctx context.Context, sb *store.Standby) {
	label := sb.NodeID + "/" + sb.Target
	defer guardPanic("standby", label, func() {
		s.logSink("standby", "ERR", "Standby rehearsal failed: internal error (panic)")
	})

	b := s.newestVerifiedBackup(sb.NodeID, sb.Target)
	if b == nil {
		// Nothing proven to restore yet — don't record a (misleading) failure; leave it
		// "not yet rehearsed" so it runs as soon as a backup verifies. No alert.
		log.Printf("standby: %s has no verified backup to rehearse yet — skipping", label)
		return
	}
	// Ride the restore lock on the CLONE key (the throwaway clone name on the
	// standby node), exactly like clone restores, so a rehearsal never collides
	// with a real backup/restore. Skip (reschedule next cycle) when busy.
	cloneKey := stackKey(sb.StandbyNode, "", backup.StandbyCloneName(sb.Target))
	if !s.locks.acquireRestore(cloneKey) {
		log.Printf("standby: %s busy — skipping this cycle", label)
		return
	}
	defer s.releaseRestoreAndDispatch(cloneKey)

	ok, bootMs, detail := s.engine.RehearseStandby(ctx, b, sb.StandbyNode)
	if err := s.store.RecordStandbyResult(sb.NodeID, sb.Target, ok, bootMs, detail, time.Now().Unix()); err != nil {
		log.Printf("standby: could not record result for %s: %v", label, err)
	}
	targetName := s.nodeNameOr(sb.StandbyNode)
	if ok {
		log.Printf("standby OK: %s proven on %s (booted in %dms)", label, targetName, bootMs)
		return
	}
	log.Printf("standby FAILED: %s on %s — %s", label, targetName, detail)
	s.notify(notify.KindScrubFailed, "Standby rehearsal FAILED: "+sb.Target,
		fmt.Sprintf("A standby rehearsal of %s onto node %s did not come up — do NOT assume this container can fail over there. %s", sb.Target, targetName, detail))
}

// newestVerifiedBackup returns the most recent VERIFIED backup of a container on a
// node (the effective recovery point a rehearsal restores), or nil if none.
func (s *Server) newestVerifiedBackup(nodeID, name string) *store.Backup {
	// Slim projection (perf Fix 7): selection reads target/status/verified/
	// created; RehearseStandby reads only ID + TargetName from the row (the
	// restore itself re-fetches by BackupID inside the engine).
	all, _ := s.store.ListBackupSummaries(nodeID, 500)
	var best *store.Backup
	for _, b := range all {
		if b.TargetName != name || b.Status != "success" || b.Verified != "verified" {
			continue
		}
		if best == nil || b.CreatedAt > best.CreatedAt {
			best = b
		}
	}
	return best
}

// nodeNameOr returns a node's display name, falling back to its id.
func (s *Server) nodeNameOr(nodeID string) string {
	if n, err := s.store.GetNode(nodeID); err == nil && n.Name != "" {
		return n.Name
	}
	return nodeID
}
