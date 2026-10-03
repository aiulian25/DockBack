package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"dockback/internal/backup"
	"dockback/internal/store"
)

// Periodic re-verification ("scrub", PLAN §9.4). Creation-time verification
// can't catch bit-rot months later or a destination that silently went bad, so
// stored backups are re-verified on a cadence and flagged/alerted if they no
// longer verify. Throttled (a few per cycle) so a large catalog is swept over
// time without hammering disk/CPU.
const (
	scrubTickInterval    = 30 * time.Minute
	defaultScrubPerCycle = 3
)

// scrubIntervalDays is the configured re-verify cadence (0 = scrub disabled).
func (s *Server) scrubIntervalDays() int {
	v, _ := s.store.GetSetting("scrub.interval_days", "0")
	n, _ := strconv.Atoi(v)
	if n < 0 {
		n = 0
	}
	return n
}

// scrubPerCycle is how many overdue backups to re-verify each cycle (clamped
// 1..5, default 3 — F18), so capable hardware can sweep a large catalog faster
// without a load spike. Parity with the configurable drill-per-cycle knob.
func (s *Server) scrubPerCycle() int {
	n := s.settingInt("scrub.per_cycle", defaultScrubPerCycle)
	if n < 1 {
		n = 1
	}
	if n > 5 {
		n = 5
	}
	return n
}

// startScrub launches the periodic scrub loop (PLAN §9.4).
func (s *Server) startScrub() {
	go func() {
		t := time.NewTicker(scrubTickInterval)
		defer t.Stop()
		for range t.C {
			s.scrubTick()
		}
	}()
}

// scrubTick re-verifies the most-overdue successful backups (bounded per cycle).
func (s *Server) scrubTick() {
	days := s.scrubIntervalDays()
	if days <= 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	// Slim projection (perf Fix 7): batch selection reads only
	// status/locations/last-verified/created. The few selected backups are
	// re-fetched in FULL before verification — Scrub needs the real manifest/
	// cipher fields, never a slim row's empty ones.
	all, err := s.store.ListBackupScrubInfo(100000)
	if err != nil {
		return
	}
	for _, t := range selectScrubBatch(all, cutoff, s.scrubPerCycle()) {
		full, gerr := s.store.GetBackup(t.Backup.ID)
		if gerr != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		s.engine.Scrub(ctx, full, t.Source)
		cancel()
	}
}

// scrubTarget is one (backup, copy) pair the scrub should re-verify (F44). Source
// is ""/"local" for the local archive or a destination ID for an offsite copy.
type scrubTarget struct {
	Backup *store.Backup
	Source string
	// verifiedAt is when THIS copy was last verified (0 = never), used to sort.
	verifiedAt int64
}

// scrubLocations parses a backup's stored locations, falling back to a single
// local copy for legacy rows with none recorded.
func scrubLocations(b *store.Backup) []backup.Location {
	var locs []backup.Location
	if b.LocationsJSON != "" {
		_ = json.Unmarshal([]byte(b.LocationsJSON), &locs)
	}
	if len(locs) == 0 {
		locs = []backup.Location{{Kind: "local", Name: "local", Type: "local"}}
	}
	return locs
}

// selectScrubBatch picks up to max (backup, copy) pairs whose copy hasn't been
// verified since cutoff, least-recently-verified (incl. never) first — so scrubs
// rotate across BOTH the local archive and every destination copy (F44), and the
// worst-case staleness of ANY single copy always shrinks. A destination copy that
// never uploaded (failed/deferred) is skipped — there's nothing there to verify.
// A local copy with no per-copy timestamp falls back to the backup's global
// last-verified clock, so legacy behavior is preserved when nothing has drifted.
// Pure, for testability.
func selectScrubBatch(all []*store.Backup, cutoff int64, max int) []scrubTarget {
	var due []scrubTarget
	for _, b := range all {
		if b.Status != "success" {
			continue
		}
		for _, loc := range scrubLocations(b) {
			if loc.Status == "failed" || loc.Status == "deferred" {
				continue // no data at this copy
			}
			source := "local"
			if loc.Kind == "dest" {
				source = loc.DestID
			}
			at := loc.VerifiedAt
			if loc.Kind == "local" && at == 0 {
				at = b.LastVerifiedAt // legacy seed
			}
			if at < cutoff {
				due = append(due, scrubTarget{Backup: b, Source: source, verifiedAt: at})
			}
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].verifiedAt < due[j].verifiedAt })
	if len(due) > max {
		due = due[:max]
	}
	return due
}

// hasLocalCopy reports whether the backup keeps a local archive.
func hasLocalCopy(locs []backup.Location) bool {
	for _, l := range locs {
		if l.Kind == "local" {
			return true
		}
	}
	return false
}

// hasDestCopy reports whether a data-bearing copy exists on the given destination.
func hasDestCopy(locs []backup.Location, destID string) bool {
	for _, l := range locs {
		if l.Kind == "dest" && l.DestID == destID && l.Status != "failed" && l.Status != "deferred" {
			return true
		}
	}
	return false
}

// firstDataDestID returns the first destination copy that actually holds data, for
// defaulting an adopted (local-less) backup's on-demand verify to its real copy.
func firstDataDestID(locs []backup.Location) string {
	for _, l := range locs {
		if l.Kind == "dest" && l.Status != "failed" && l.Status != "deferred" {
			return l.DestID
		}
	}
	return ""
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// handleVerifyBackup re-verifies (scrubs) one backup on demand (PLAN §9.4). It
// runs asynchronously — the archive is re-read/decrypted — and the UI reflects
// the updated verified state + last-verified time on its next refresh.
func (s *Server) handleVerifyBackup(w http.ResponseWriter, r *http.Request) {
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
		errJSON(w, http.StatusBadRequest, "only successful backups can be re-verified")
		return
	}
	// Optional {"source": "<destID>"|"local"} selects which copy to re-verify (F44).
	// An empty body scrubs the local copy, or — for an adopted, destination-only
	// backup with no local copy — its first data-bearing destination copy, so
	// "Verify now" never fails spuriously on a missing local file.
	var body struct {
		Source string `json:"source"`
	}
	_ = readJSON(r, &body) // body is optional
	source := strings.TrimSpace(body.Source)
	locs := scrubLocations(b)
	if source != "" && source != "local" {
		if !hasDestCopy(locs, source) {
			errJSON(w, http.StatusBadRequest, "this backup has no copy on that destination")
			return
		}
	} else if source == "" && !hasLocalCopy(locs) {
		source = firstDataDestID(locs)
	}
	_ = s.store.Audit(userFrom(r), "backup.verify", b.ID, b.TargetName+" source="+firstNonEmptyStr(source, "local"))
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		s.engine.Scrub(ctx, b, source)
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "verifying"})
}
