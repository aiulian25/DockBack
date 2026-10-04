package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"time"

	"dockback/internal/notify"
	"dockback/internal/store"
)

// Restore drills (PLAN §9.4). A drill re-restores a backup into an ISOLATED
// sandbox (throwaway DB container for database dumps, plus — for a volume/app
// backup — a fresh throwaway volume and an isolated boot of the container; the
// same primitives deep verification uses) and records a per-backup pass/fail. It's
// the real-world "can I actually restore this?" proof, at the same granularity
// as verification: per backup (per node + container), surfaced and triggered on
// each Backups row. Drills never touch the live stack or production containers.
// On a cadence, DockBack auto-drills backups per the configured scope; a large
// fleet is swept a bounded number per cycle (drill.per_cycle) so there's no spike.
const drillTickInterval = 1 * time.Hour // heavier than a scrub, so a slower cadence

// drillIntervalDays is the configured drill cadence (0 = disabled).
// drillIntervalKey holds how many days apart restore drills run; 0 is off.
const drillIntervalKey = "drill.interval_days"

func (s *Server) drillIntervalDays() int {
	v, _ := s.store.GetSetting(drillIntervalKey, "0")
	n, _ := strconv.Atoi(v)
	if n < 0 {
		n = 0
	}
	return n
}

// drillPerCycle is how many overdue backups to drill each cycle (clamped 1..5),
// so capable hardware can prove more of its history without a load spike (F12).
func (s *Server) drillPerCycle() int {
	v, _ := s.store.GetSetting("drill.per_cycle", "1")
	n, _ := strconv.Atoi(v)
	if n < 1 {
		n = 1
	}
	if n > 5 {
		n = 5
	}
	return n
}

// drillScope selects which backups are eligible for auto-drilling (F12):
// "newest" (default) = only the newest per container; "newest_per_week" also
// drills the newest of each ISO week, proving older generations still restore.
func (s *Server) drillScope() string {
	if v, _ := s.store.GetSetting("drill.scope", "newest"); v == "newest_per_week" {
		return v
	}
	return "newest"
}

// drillCandidates picks the auto-drill candidate set for a scope. "newest" keeps
// the newest successful backup per (node, target); "newest_per_week" keeps the
// newest per (node, target, ISO week), widening coverage across history. Pure so
// it's unit-testable.
func drillCandidates(all []*store.Backup, scope string) []*store.Backup {
	type gk struct {
		node, target, week string
	}
	best := map[gk]*store.Backup{}
	for _, b := range all {
		if b.Status != "success" {
			continue
		}
		k := gk{node: b.NodeID, target: b.TargetName}
		if scope == "newest_per_week" {
			y, w := time.Unix(b.CreatedAt, 0).UTC().ISOWeek()
			k.week = fmt.Sprintf("%d-%02d", y, w)
		}
		if cur := best[k]; cur == nil || b.CreatedAt > cur.CreatedAt {
			best[k] = b
		}
	}
	out := make([]*store.Backup, 0, len(best))
	for _, b := range best {
		out = append(out, b)
	}
	return out
}

// startDrills launches the periodic restore-drill loop (PLAN §9.4).
func (s *Server) startDrills() {
	go func() {
		t := time.NewTicker(drillTickInterval)
		defer t.Stop()
		for range t.C {
			s.drillTick()
		}
	}()
}

// drillTick drills the most-overdue backups (bounded to drill.per_cycle), from
// the candidate set selected by drill.scope (F12).
func (s *Server) drillTick() {
	days := s.drillIntervalDays()
	if days <= 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	// Slim projection (perf Fix 7): candidate selection reads only
	// node/target/status/created; the drilled backups are re-fetched in full.
	all, err := s.store.ListBackupSummaries("", 100000)
	if err != nil {
		return
	}
	candidates := drillCandidates(all, s.drillScope())
	// Those overdue for a drill (never drilled, or older than cutoff), oldest first.
	type due struct {
		b     *store.Backup
		ranAt int64
	}
	var pending []due
	for _, b := range candidates {
		ranAt := int64(0)
		if d, err := s.store.GetDrill(b.ID); err == nil {
			ranAt = d.RanAt
		}
		if ranAt < cutoff {
			pending = append(pending, due{b, ranAt})
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].ranAt < pending[j].ranAt })
	perCycle := s.drillPerCycle()
	for i := 0; i < perCycle && i < len(pending); i++ {
		// Full row for the drill itself — DrillBackup reads manifest/locations.
		full, gerr := s.store.GetBackup(pending[i].b.ID)
		if gerr != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		s.runBackupDrill(ctx, full)
		cancel()
	}
}

// recordDrillResult stores a drill's verdict and announces it.
//
// The two belong together. A drill runs for minutes, and its result changes
// what the Backups page shows for that row — the drill badge and the
// confidence grade gradeBackup derives from it. Without the announcement that
// page can only discover the verdict by polling fast for the whole time a tab
// is open, which is what it used to do.
func (s *Server) recordDrillResult(b *store.Backup, ok bool, summary string) error {
	if err := s.store.UpsertDrill(b.ID, ok, summary, time.Now().Unix()); err != nil {
		return err
	}
	s.pushBackupStatus(b.ID, b.NodeID, b.Status)
	return nil
}

// runBackupDrill test-restores one backup into a sandbox and records the result.
// Alerts on failure so a broken restore path is never silent.
func (s *Server) runBackupDrill(ctx context.Context, b *store.Backup) {
	label := b.TargetName
	if b.Stack != "" {
		label = b.Stack + "/" + b.TargetName
	}
	// F86: a write-only backup can't be opened by this instance, so a drill is
	// impossible — not failed. Recording it as failed would alert and downgrade a
	// perfectly good backup every cycle. Skip silently; the Backups page shows the
	// reason on the row instead.
	if reason := s.engine.DrillSkipReason(b); reason != "" {
		log.Printf("restore drill skipped: %s — %s", label, reason)
		return
	}
	ok, summary := s.engine.DrillBackup(ctx, b)
	if err := s.recordDrillResult(b, ok, summary); err != nil {
		log.Printf("restore drill: could not record result for %s: %v", b.ID, err)
		return
	}
	if ok {
		log.Printf("restore drill OK: %s restored cleanly into a sandbox (%s)", label, summary)
		return
	}
	log.Printf("restore drill FAILED: %s — %s", label, summary)
	// A failed drill is a data-at-risk signal — route it as a critical alert
	// (same band as a scrub regression), never silently.
	s.notify(notify.KindScrubFailed, "Restore drill FAILED: "+label,
		fmt.Sprintf("A restore drill of %s did not restore cleanly into a sandbox — do NOT assume this backup is recoverable. %s", label, summary))
}

// handleListDrills returns every recorded drill result (backup_id → outcome) so
// the Backups page can show each row's last drill state in one fetch (PLAN §9.4).
func (s *Server) handleListDrills(w http.ResponseWriter, r *http.Request) {
	ds, err := s.store.ListDrills()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ds == nil {
		ds = []*store.Drill{}
	}
	writeJSON(w, http.StatusOK, ds)
}

// handleDrillBackup runs a restore drill for one backup on demand (PLAN §9.4),
// mirroring the per-backup verify action. Async — the row reflects the result on
// its next refresh.
func (s *Server) handleDrillBackup(w http.ResponseWriter, r *http.Request) {
	b, err := s.store.GetBackup(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if b.Status != "success" {
		errJSON(w, http.StatusBadRequest, "only successful backups can be drilled")
		return
	}
	if reason := s.engine.DrillSkipReason(b); reason != "" {
		errJSON(w, http.StatusBadRequest, "this backup can't be drilled: "+reason)
		return
	}
	_ = s.store.Audit(userFrom(r), "backup.drill", b.ID, b.TargetName)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		s.runBackupDrill(ctx, b)
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "drilling"})
}
