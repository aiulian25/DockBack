package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
	"dockback/internal/notify"
	"dockback/internal/store"
)

// Critical-database low-RPO protection.
//
// True continuous WAL archiving / physical PITR is deliberately NOT implemented:
// it would require restarting and persistently reconfiguring the user's database
// (archive_mode/wal_level are postmaster-context settings), a physical restore
// model that diverges from DockBack's verified logical-dump pipeline (§4.3/§9.2),
// and a long-lived replication connection the socket-proxy-only network model
// doesn't grant. See PLAN §9.7 for the full trade-off and the future
// privileged-helper path.
//
// Instead, marking a database "critical" drives FREQUENT consistent verified
// dumps so the worst-case data-loss window equals the chosen RPO rather than a
// full day — using only the existing, fully-verified backup pipeline (no DB
// restart, no config change, no divergent restore).

// criticalSetting holds every critical database in one JSON settings blob so the
// tick can iterate them without a prefix scan (mirrors how the schedule is
// stored).
const criticalSetting = "critical.databases"

// Critical-DB tier defaults (F31 — each is overridable in Settings → Performance
// & tuning, so the low-RPO tier matches the user's tolerance):
//   - the RPO FLOOR, so "low RPO" can never become a self-inflicted load problem
//     on the very database it protects (PLAN §9.7/§9.9);
//   - how often the low-RPO loop evaluates due dumps;
//   - how many consecutive verification failures pause a critical DB's automatic
//     low-RPO backups (circuit-breaker) — so a database that can't produce a
//     trustworthy dump isn't backed up on a loop forever; auto-resumes once one
//     backup verifies again (PLAN §9.7).
const (
	defaultRPOMinSeconds       = 300 // 5 min
	defaultCriticalTickSeconds = 60
	defaultLowRPOFailLimit     = 3
)

// criticalRPOMin is the effective RPO floor in SECONDS (setting
// critical.rpo_min_seconds, default 300, clamp 60..3600 — F31).
func (s *Server) criticalRPOMin() int {
	return clampInt(s.settingInt("critical.rpo_min_seconds", defaultRPOMinSeconds), 60, 3600)
}

// criticalTickSeconds is how often the low-RPO loop evaluates due dumps
// (setting critical.tick_seconds, default 60, clamp 30..600 — F31).
func (s *Server) criticalTickSeconds() int {
	return clampInt(s.settingInt("critical.tick_seconds", defaultCriticalTickSeconds), 30, 600)
}

// criticalFailLimit is the consecutive-verification-failure pause threshold
// (setting critical.fail_limit, default 3, clamp 1..10 — F31).
func (s *Server) criticalFailLimit() int {
	return clampInt(s.settingInt("critical.fail_limit", defaultLowRPOFailLimit), 1, 10)
}

// CriticalDB marks one database container for low-RPO protection. Keyed by node
// + container NAME (stable across recreates, unlike the container ID).
type CriticalDB struct {
	NodeID     string `json:"node_id"`
	Name       string `json:"name"`
	RPOSeconds int    `json:"rpo_seconds"`
}

func critKey(node, name string) string { return node + "\x00" + name }

func (s *Server) loadCriticalDBs() map[string]CriticalDB {
	out := map[string]CriticalDB{}
	js, _ := s.store.GetSetting(criticalSetting, "{}")
	_ = json.Unmarshal([]byte(js), &out)
	return out
}

func (s *Server) saveCriticalDBs(m map[string]CriticalDB) error {
	b, _ := json.Marshal(m)
	return s.store.SetSetting(criticalSetting, string(b))
}

// criticalStatus is the per-container response for the Critical-data card.
type criticalStatus struct {
	Enabled         bool   `json:"enabled"`
	RPOSeconds      int    `json:"rpo_seconds"`
	IsDatabase      bool   `json:"is_database"`
	Engine          string `json:"engine,omitempty"`
	MeasuredRPOSecs int64  `json:"measured_rpo_seconds"` // age of newest VERIFIED backup, -1 if none
	TargetMet       bool   `json:"target_met"`
	// Circuit-breaker state (PLAN §9.7): Paused is true when repeated verification
	// failures have paused automatic low-RPO backups; VerifyFailures is the current
	// consecutive-failure count.
	Paused         bool `json:"paused"`
	VerifyFailures int  `json:"verify_failures"`
	// Non-invasive Postgres PITR-readiness detection — read-only.
	PITRChecked   bool   `json:"pitr_checked"`
	WALLevel      string `json:"wal_level,omitempty"`
	ArchiveMode   string `json:"archive_mode,omitempty"`
	MaxWALSenders int    `json:"max_wal_senders,omitempty"`
	PITRReady     bool   `json:"pitr_ready"`
	// PITRDetail is the one-line verdict/remediation from the shared parser. It
	// travels with the reading rather than being re-derived in the browser, so
	// the wording that tells an operator what to put in postgresql.conf has one
	// definition (F49).
	PITRDetail string `json:"pitr_detail,omitempty"`
}

// newestVerifiedFor returns the timestamp of the most recent VERIFIED backup of
// a container (by stable name) on a node — the effective recovery point for RPO.
func (s *Server) newestVerifiedFor(nodeID, name string) (int64, bool) {
	// By target, not "the node's newest 500 filtered by name": a critical DB at a
	// 5-minute RPO fills a node-wide window in hours, and a container whose newest
	// verified backup falls outside it reads as an RPO breach that is not real.
	all, _ := s.store.ListBackupsForTarget(nodeID, name, 200)
	var newest int64
	found := false
	for _, b := range all {
		// A recovery point only counts if it actually VERIFIED — an empty/failed
		// dump that "succeeded" but failed verification is not a real recovery
		// point, so RPO must not treat it as one (PLAN §9.7/§4.3).
		if b.TargetName != name || b.Status != "success" || b.Verified != "verified" {
			continue
		}
		ts := b.CompletedAt
		if ts == 0 {
			ts = b.CreatedAt
		}
		if ts > newest {
			newest, found = ts, true
		}
	}
	return newest, found
}

// consecutiveVerifyFailures counts how many of a container's most recent backups
// failed (the backup failed, or completed but failed verification) before the
// newest VERIFIED one — the low-RPO circuit-breaker signal (PLAN §9.7). Backups
// are newest-first. In-flight and not-yet-verified backups are skipped so a
// pending verification never trips or resets the streak. Pure/testable.
func consecutiveVerifyFailures(backups []*store.Backup, name string) (fails int, hasVerified bool) {
	for _, b := range backups {
		if b.TargetName != name {
			continue
		}
		switch {
		case b.Status == "running" || b.Status == "pending":
			continue // in flight — ignore
		case b.Status == "success" && b.Verified == "verified":
			return fails, true // healthy recovery point — streak ends here
		case b.Status == "failed" || b.Verified == "failed":
			fails++
		default:
			continue // success but not yet verified — neutral
		}
	}
	return fails, false
}

// lowRPOHealth returns the current consecutive verification-failure count for a
// container and whether any recent backup verified.
func (s *Server) lowRPOHealth(nodeID, name string) (fails int, hasVerified bool) {
	all, _ := s.store.ListBackupsForTarget(nodeID, name, 100)
	return consecutiveVerifyFailures(all, name)
}

// handleGetCritical reports a container's critical-DB status: whether low-RPO is
// enabled, the measured RPO (age of the newest VERIFIED backup) vs target, the
// circuit-breaker (paused) state, and — for Postgres — non-invasive PITR readiness.
func (s *Server) handleGetCritical(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cid := r.PathValue("cid")
	cli, err := s.reg.Get(id)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	insp, err := cli.ContainerInspect(ctx, cid)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	name := strings.TrimPrefix(insp.Name, "/")
	engine := backup.DBEngine(insp.Config.Image)
	st := criticalStatus{IsDatabase: engine != "", Engine: engine, MeasuredRPOSecs: -1}

	if c, ok := s.loadCriticalDBs()[critKey(id, name)]; ok && c.RPOSeconds > 0 {
		st.Enabled = true
		st.RPOSeconds = c.RPOSeconds
		st.VerifyFailures, _ = s.lowRPOHealth(id, name)
		st.Paused = st.VerifyFailures >= s.criticalFailLimit()
	}
	if at, ok := s.newestVerifiedFor(id, name); ok {
		st.MeasuredRPOSecs = time.Now().Unix() - at
		if st.Enabled {
			st.TargetMet = st.MeasuredRPOSecs <= int64(st.RPOSeconds)
		}
	}
	// Read-only PITR-readiness probe (Postgres, running only) — changes nothing.
	if engine == "postgres" && insp.State != nil && insp.State.Running {
		if out, err := dockercli.ExecCapture(ctx, cli, cid, backup.PGReadinessCmd(insp.Config.Env)); err == nil {
			// Single source of truth (F49): the same parser the F34 chip uses, so
			// PITR readiness has ONE definition everywhere — wal_level replica/logical
			// AND archive_mode=on (a WAL archive is required for PITR; without it only
			// full dumps are possible).
			if rp := backup.ParsePGReadiness(string(out)); rp.WALLevel != "" {
				st.PITRChecked = true
				st.WALLevel, st.ArchiveMode = rp.WALLevel, rp.ArchiveMode
				st.MaxWALSenders, _ = strconv.Atoi(rp.MaxWALSenders)
				st.PITRReady = rp.Ready
				st.PITRDetail = rp.Detail
			}
		}
	}
	writeJSON(w, http.StatusOK, st)
}

// handleSetCritical enables/disables low-RPO protection for a database container.
func (s *Server) handleSetCritical(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cid := r.PathValue("cid")
	var body struct {
		Enabled    bool `json:"enabled"`
		RPOSeconds int  `json:"rpo_seconds"`
	}
	// readJSON, like every other handler: it caps the body and refuses unknown
	// fields, and a decoder wired by hand here got neither.
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid body")
		return
	}
	cli, err := s.reg.Get(id)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	insp, err := cli.ContainerInspect(ctx, cid)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	name := strings.TrimPrefix(insp.Name, "/")
	if backup.DBEngine(insp.Config.Image) == "" {
		errJSON(w, http.StatusBadRequest, "low-RPO protection applies to database containers only")
		return
	}

	m := s.loadCriticalDBs()
	k := critKey(id, name)
	if body.Enabled {
		rpo := body.RPOSeconds
		if min := s.criticalRPOMin(); rpo < min {
			rpo = min
		}
		m[k] = CriticalDB{NodeID: id, Name: name, RPOSeconds: rpo}
	} else {
		delete(m, k)
	}
	if err := s.saveCriticalDBs(m); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Reset the in-memory enqueue guard so a re-enable evaluates from scratch.
	s.critMu.Lock()
	delete(s.critLast, k)
	s.critMu.Unlock()

	_ = s.store.Audit(userFrom(r), "critical.set", name, fmt.Sprintf("enabled=%v rpo=%ds", body.Enabled, m[k].RPOSeconds))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// rpoDue reports whether a fresh low-RPO dump is due, given the last recovery
// point (newest successful backup), the last in-memory enqueue time (so we don't
// re-queue before the prior dump materializes), now, and the target RPO. Pure,
// so it is unit-testable.
func rpoDue(lastPoint, lastEnqueue, now time.Time, rpo time.Duration) bool {
	ref := lastPoint
	if lastEnqueue.After(ref) {
		ref = lastEnqueue
	}
	if ref.IsZero() {
		return true
	}
	return now.Sub(ref) >= rpo
}

// startCriticalLoop runs the low-RPO tick. The interval is re-read each cycle
// (setting critical.tick_seconds, F31), so a change in Settings applies on the
// next cycle without a restart.
func (s *Server) startCriticalLoop() {
	go func() {
		for {
			time.Sleep(time.Duration(s.criticalTickSeconds()) * time.Second)
			s.criticalTick()
		}
	}()
}

func (s *Server) criticalTick() {
	m := s.loadCriticalDBs()
	if len(m) == 0 {
		return
	}
	now := time.Now()
	for k, c := range m {
		if c.RPOSeconds <= 0 {
			continue
		}
		rpo := time.Duration(c.RPOSeconds) * time.Second

		// Circuit-breaker: if recent automatic backups keep failing verification,
		// stop re-queuing backups this DB can't produce trustworthily. Resumes
		// automatically once one verifies again (PLAN §9.7).
		fails, _ := s.lowRPOHealth(c.NodeID, c.Name)
		paused := fails >= s.criticalFailLimit()
		s.critMu.Lock()
		wasPaused := s.critPaused[k]
		s.critPaused[k] = paused
		s.critMu.Unlock()
		if paused {
			if !wasPaused {
				s.logSink("critical", "WARN", fmt.Sprintf("Low-RPO protection paused for %q — the last %d automatic backups failed verification; auto-backups are paused until one verifies", c.Name, fails))
			}
			s.notifyThrottled(notify.KindLowRPOPaused, k,
				"Low-RPO protection paused",
				fmt.Sprintf("Automatic low-RPO backups for %q are PAUSED — the last %d backups failed verification (do NOT trust them). They will resume automatically once a backup verifies. Investigate the database dump.", c.Name, fails),
				lowRPOPausedCooldown)
			continue
		}
		if wasPaused {
			s.logSink("critical", "INFO", fmt.Sprintf("Low-RPO protection resumed for %q — a backup verified again", c.Name))
		}

		var lastPoint time.Time
		if at, ok := s.newestVerifiedFor(c.NodeID, c.Name); ok {
			lastPoint = time.Unix(at, 0)
		}
		s.critMu.Lock()
		lastEnqueue := s.critLast[k]
		s.critMu.Unlock()
		if !rpoDue(lastPoint, lastEnqueue, now, rpo) {
			continue
		}

		// Resolve the live container by its stable name on the node.
		cli, err := s.reg.Get(c.NodeID)
		if err != nil {
			continue
		}
		lctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		cs, err := dockercli.ListContainers(lctx, cli)
		cancel()
		if err != nil {
			continue
		}
		var cid string
		var running bool
		for _, ct := range cs {
			if ct.Name == c.Name {
				cid, running = ct.ID, ct.State == "running"
				break
			}
		}
		if cid == "" || !running {
			// Can't protect a critical DB that isn't running — surface it; the
			// breach also shows up on the /metrics RPO gauge for external alerting.
			s.logSink("critical", "WARN", fmt.Sprintf("Critical database %q on node %s is not running — low-RPO protection cannot run", c.Name, c.NodeID))
			continue
		}
		node, err := s.store.GetNode(c.NodeID)
		if err != nil {
			continue
		}
		dests := s.effectivePolicy(c.NodeID).Destinations
		s.enqueueBackup(node.Name, backup.Options{
			NodeID: c.NodeID, ContainerID: cid, Compression: "balanced",
			Destinations: dests, DestinationsExplicit: true,
		}, prioScheduled)
		s.critMu.Lock()
		s.critLast[k] = now
		s.critMu.Unlock()
		s.logSink("critical", "INFO", fmt.Sprintf("Low-RPO backup queued for %q on %s (RPO %s)", c.Name, node.Name, rpo.Round(time.Second)))
	}
}

// criticalRPOBreaches counts critical databases whose newest successful backup is
// older than their target RPO (or have none) — exported on /metrics so an
// external monitor can alert (PLAN §9.7/§9.12).
func (s *Server) criticalRPOBreaches() (breaches, total int) {
	m := s.loadCriticalDBs()
	now := time.Now().Unix()
	for _, c := range m {
		if c.RPOSeconds <= 0 {
			continue
		}
		total++
		at, ok := s.newestVerifiedFor(c.NodeID, c.Name)
		if !ok || now-at > int64(c.RPOSeconds) {
			breaches++
		}
	}
	return breaches, total
}
