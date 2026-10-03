package api

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"dockback/internal/backup"
	"dockback/internal/notify"
	"dockback/internal/store"
)

// Daily summary digest (F15). On a fleet running a nightly window of dozens of
// containers, one on-success notification per backup becomes noise — which pushes
// users to disable success alerts entirely and lose the "it ran" signal. The
// digest condenses the day into a single message at a chosen time, assembled from
// the same SQLite catalog the Insights page uses (no Docker/network probes), so
// it's cheap. Failures and critical alerts are unaffected — they still notify
// immediately.

const digestLastSentKey = "digest.last_sent"

// aggregateBackupCounts counts finished backups created at/after `since`,
// mirroring the Insights catalog rules (a still-running backup is skipped;
// verified means verification passed; failed means the run failed) so the digest
// matches the Insights page for the same window. Pure.
func aggregateBackupCounts(list []*store.Backup, since int64) (total, verified, failed int) {
	for _, b := range list {
		if b.Status == "running" || b.CreatedAt < since {
			continue
		}
		total++
		if b.Verified == "verified" {
			verified++
		}
		if b.Status == "failed" {
			failed++
		}
	}
	return
}

// parseHHMM parses "HH:MM" (matching the schedule format), returning ok=false on
// anything malformed or out of range.
func parseHHMM(s string) (h, m int, ok bool) {
	if n, _ := fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &h, &m); n != 2 {
		return 0, 0, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// digestDue decides whether the daily digest should fire now and the last-sent
// baseline to persist. Pure, so it's unit-testable. It fires once per day at or
// after the configured HH:MM local time; the first evaluation (zero lastSent)
// establishes a baseline WITHOUT an immediate backfill send, so enabling the
// digest never fires one instantly. An unparseable time falls back to 09:00.
func digestDue(hhmm string, lastSent, now time.Time) (fire bool, newLast time.Time) {
	h, m, ok := parseHHMM(hhmm)
	if !ok {
		h, m = 9, 0
	}
	if lastSent.IsZero() {
		return false, now // establish baseline; fire at the next window
	}
	sched := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if now.Before(sched) {
		return false, lastSent // today's window not reached yet
	}
	if lastSent.Before(sched) {
		return true, now // window passed and we haven't sent for it
	}
	return false, lastSent // already sent for today's window
}

// startDigest runs the daily-summary loop: a lightweight minute ticker that fires
// the digest once per day at the configured time (F15). Mirrors startRetentionPrune.
func (s *Server) startDigest() {
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for range t.C {
			s.digestTick()
		}
	}()
}

// digestTick evaluates the digest schedule and sends the summary when due.
func (s *Server) digestTick() {
	cfg, err := s.loadNotifyConfig()
	if err != nil || !cfg.Digest.Enabled {
		return
	}
	var lastSent time.Time
	if raw, _ := s.store.GetSetting(digestLastSentKey, "0"); raw != "" {
		if v, _ := strconv.ParseInt(raw, 10, 64); v > 0 {
			lastSent = time.Unix(v, 0)
		}
	}
	fire, newLast := digestDue(cfg.Digest.Time, lastSent, time.Now())
	if !newLast.Equal(lastSent) {
		_ = s.store.SetSetting(digestLastSentKey, strconv.FormatInt(newLast.Unix(), 10))
	}
	if !fire {
		return
	}
	includeDR := cfg.Digest.IncludeDR == nil || *cfg.Digest.IncludeDR // absent = on (F60)
	title, body := s.composeDigest(time.Now(), includeDR)
	// KindBackupSuccess (info severity) so the digest routes to the same channels a
	// success ping would — it IS the aggregated success signal.
	s.notify(notify.KindBackupSuccess, title, body)
	s.logSink("digest", "INFO", "Sent daily summary: "+body)
}

// composeDigest builds the summary title/body from the SQLite catalog for the last
// 24h plus cheap RPO/destination context (F15). No Docker or network probes.
func (s *Server) composeDigest(now time.Time, includeDR bool) (title, body string) {
	since := now.Add(-24 * time.Hour).Unix()
	list, _ := s.store.ListBackups("", 5000)
	total, verified, failed := aggregateBackupCounts(list, since)
	breaches, rpoTotal := s.criticalRPOBreaches()
	destHealthy, destTotal := s.destinationsHealthy()

	var b strings.Builder
	fmt.Fprintf(&b, "Last 24h: %d backup%s, %d verified, %d failed.", total, plural(total), verified, failed)
	if destTotal > 0 {
		fmt.Fprintf(&b, " Destinations: %d of %d healthy.", destHealthy, destTotal)
	}
	if rpoTotal > 0 {
		if breaches == 0 {
			fmt.Fprintf(&b, " RPO: all %d critical database%s on target.", rpoTotal, plural(rpoTotal))
		} else {
			fmt.Fprintf(&b, " RPO: %d of %d critical database%s breaching target.", breaches, rpoTotal, plural(rpoTotal))
		}
	}
	// F60: DR-confidence block from the SAME catalog pass (no Docker/network probes).
	if includeDR {
		b.WriteString(drConfidenceLine(s.drConfidence(list, now.Unix())))
	}
	return "DockBack daily summary", b.String()
}

// drCounts holds the DR-confidence counters gathered from the catalog + settings
// (F60). Kept as a plain struct so drConfidenceLine is a pure, unit-testable
// composition with no store dependency.
type drCounts struct {
	Total        int   // targets with a newest successful backup
	DrilledOK    int   // …whose newest success has a PASSED restore drill
	Undrilled    int   // …with no drill at all
	Partial      int   // …whose newest success is a PARTIAL capture
	AppDrillAt   int64 // app-backup integrity drill: last run (0 = never)
	AppDrillOK   bool  // …and whether it passed (F58)
	KeyConfirmed bool  // master key confirmed backed up (escrow acknowledged)
	// F75: pilot-light standbys (F62). Proven = the last rehearsal ran and passed.
	StandbyTotal  int
	StandbyProven int
	Now           int64
}

// drConfidence gathers the DR-confidence counters from the ALREADY-FETCHED catalog
// list (newest-first) plus cheap store reads (drills map + settings) — no Docker,
// no network, one shared catalog pass with composeDigest.
func (s *Server) drConfidence(list []*store.Backup, now int64) drCounts {
	c := drCounts{Now: now}
	drills := map[string]*store.Drill{}
	if ds, err := s.store.ListDrills(); err == nil {
		for _, d := range ds {
			drills[d.BackupID] = d
		}
	}
	seen := map[string]bool{} // newest success per (node,target); list is newest-first
	for _, b := range list {
		if b.Status != "success" {
			continue
		}
		key := b.NodeID + "\x00" + b.TargetName
		if seen[key] {
			continue
		}
		seen[key] = true
		c.Total++
		if d := drills[b.ID]; d != nil {
			if d.OK {
				c.DrilledOK++
			}
		} else {
			c.Undrilled++
		}
		if b.ManifestJSON != "" {
			// F83: only UNCOVERED skips count — a shared bind captured by another
			// container's backups is intentional, not a gap.
			var m struct {
				SkippedMounts []backup.SkippedMount `json:"skipped_mounts"`
			}
			if json.Unmarshal([]byte(b.ManifestJSON), &m) == nil {
				for _, sk := range m.SkippedMounts {
					if sk.CoveredBy == "" {
						c.Partial++
						break
					}
				}
			}
		}
	}
	atStr, _ := s.store.GetSetting("appbackup.last_drill_at", "0")
	c.AppDrillAt, _ = strconv.ParseInt(atStr, 10, 64)
	okStr, _ := s.store.GetSetting("appbackup.last_drill_ok", "")
	c.AppDrillOK = okStr == "true"
	ack, _ := s.store.GetSetting("key.escrow_acknowledged", "false")
	c.KeyConfirmed = ack == "true"
	// F75: standby readiness from the same cheap store pass.
	if sbs, err := s.store.ListStandby(); err == nil {
		for _, sb := range sbs {
			c.StandbyTotal++
			if sb.LastRun > 0 && sb.LastOK {
				c.StandbyProven++
			}
		}
	}
	return c
}

// drConfidenceLine composes the DR-confidence sentence block (F60). Pure.
func drConfidenceLine(c drCounts) string {
	var b strings.Builder
	fmt.Fprintf(&b, " DR confidence: %d of %d stack%s drill-proven, %d undrilled, %d partial backup%s.",
		c.DrilledOK, c.Total, plural(c.Total), c.Undrilled, c.Partial, plural(c.Partial))
	switch {
	case c.AppDrillAt <= 0:
		b.WriteString(" App-backup not yet proven.")
	case c.AppDrillOK:
		fmt.Fprintf(&b, " App-backup proven %s.", digestAgo(c.Now-c.AppDrillAt))
	default:
		fmt.Fprintf(&b, " App-backup drill FAILED %s.", digestAgo(c.Now-c.AppDrillAt))
	}
	if c.KeyConfirmed {
		b.WriteString(" Key backup: confirmed.")
	} else {
		b.WriteString(" Key backup: NOT confirmed.")
	}
	// F75: only when any standbys exist — a fleet without them keeps the exact
	// pre-F75 digest body.
	if c.StandbyTotal > 0 {
		fmt.Fprintf(&b, " Standby: %d of %d proven.", c.StandbyProven, c.StandbyTotal)
	}
	return b.String()
}

// digestAgo is a compact relative-time string for the digest ("3d ago").
func digestAgo(secs int64) string {
	switch {
	case secs < 3600:
		return "recently"
	case secs < 86400:
		return fmt.Sprintf("%dh ago", secs/3600)
	default:
		return fmt.Sprintf("%dd ago", secs/86400)
	}
}

// destinationsHealthy counts enabled destinations and how many are within capacity
// (under the >90%-full threshold) from the newest CACHED sample — no network
// probe, so the digest stays cheap. A destination with no sample yet, or with no
// real quota, is counted as healthy (nothing indicates a problem).
func (s *Server) destinationsHealthy() (healthy, total int) {
	nowTs := time.Now().Unix()
	// count tallies one target as healthy unless its newest cached sample is over
	// the "almost full" threshold. Shared by the offsite destinations and the
	// primary backups volume (F42), so the daily summary's "N of M healthy"
	// includes the volume every backup lands on.
	count := func(id string) {
		total++
		samples, _ := s.store.DestSamples(id, nowTs-90*24*3600)
		if n := len(samples); n > 0 {
			t, u := samples[n-1].Total, samples[n-1].Used
			if t > 0 && float64(u)/float64(t) >= s.destFullThreshold() {
				return // over threshold — not counted as healthy
			}
		}
		healthy++
	}
	count(localPrimaryID)
	dests, _ := s.store.ListDestinations()
	for _, d := range dests {
		if !d.Enabled {
			continue
		}
		count(d.ID)
	}
	return
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
