package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
	"dockback/internal/notify"
	"dockback/internal/store"
)

// The settings keys the global policy is stored under. Named because every one
// of them is written by the save and read back by the load, and a typo on
// either side is a setting that silently stops working.
const (
	policyDestinationsKey     = "policy.destinations"
	retentionGenerationsKey   = "retention.generations"
	retentionDailyKey         = "retention.daily"
	retentionWeeklyKey        = "retention.weekly"
	retentionMonthlyKey       = "retention.monthly"
	retentionYearlyKey        = "retention.yearly"
	retentionAutopruneKey     = "retention.autoprune"
	autocleanMissingDaysKey   = "schedule.autoclean_missing_days"
	scheduleKey               = "schedule"
	retentionPruneScheduleKey = "retention.prune_schedule"
	retentionPruneLastRunKey  = "retention.prune_last_run"
)

// ScheduleTarget selects what a scheduled run backs up. Both ContainerID and
// ContainerName empty means "all running containers on this node".
//
// A specific container is targeted by NAME (resolved to its current ID at run
// time) so the target survives container recreation (a new ID, same name) —
// ContainerID is kept for legacy targets and display. If the named container no
// longer exists the run skips it, warns, and alerts once (PLAN §4.2/§4.7).
type ScheduleTarget struct {
	NodeID        string `json:"node_id"`
	ContainerID   string `json:"container_id,omitempty"`
	ContainerName string `json:"container_name,omitempty"`
	// Stack, when set, makes this target a whole compose PROJECT (F47): every
	// current member is backed up each run. Consistent additionally runs the single-
	// quiesce-window app-consistent snapshot (F33). Mutually exclusive with the
	// container fields (enforced on save). Old rows unmarshal with both empty.
	Stack      string `json:"stack,omitempty"`
	Consistent bool   `json:"consistent,omitempty"`
}

// Schedule is one named auto-backup schedule (F6). ID/Name identify a row in the
// schedules table; the remaining fields are the friendly frequency/target spec.
// (The same struct also serves the legacy single-schedule Policy.Schedule, where
// ID/Name are empty.)
type Schedule struct {
	ID       string           `json:"id,omitempty"`
	Name     string           `json:"name,omitempty"`
	Enabled  bool             `json:"enabled"`
	Kind     string           `json:"kind"`     // daily | weekly | monthly | custom
	Time     string           `json:"time"`     // "HH:MM" (local time)
	Weekday  int              `json:"weekday"`  // 0=Sun (weekly)
	Monthday int              `json:"monthday"` // 1-28 (monthly)
	Cron     string           `json:"cron"`     // custom 5-field cron
	Targets  []ScheduleTarget `json:"targets"`
	// IncludeStopped extends WHOLE-NODE targets to non-running containers too, so
	// an app kept off between uses is still backed up (F9). Off by default, so an
	// existing whole-node schedule keeps backing up only running containers.
	// Specific targets are always backed up regardless of state.
	IncludeStopped bool `json:"include_stopped"`
	// DestinationsExplicit + Destinations let a schedule OVERRIDE which offsite
	// destinations its runs mirror to (F27), so e.g. a nightly stays local-only
	// while a weekly pushes offsite — without touching the global/node/container
	// policy. When DestinationsExplicit is false (default), runs use the effective
	// policy destinations, exactly as before. When true, Destinations is the offsite
	// set (an empty list = local-only; Local is always written). Persisted in the
	// schedule's OptionsJSON — no schema change.
	DestinationsExplicit bool     `json:"destinations_explicit,omitempty"`
	Destinations         []string `json:"destinations,omitempty"`
}

// scheduleDests resolves the offsite destinations a schedule's runs mirror to
// (F27): the schedule's own explicit set when it overrides, otherwise the effective
// per-node policy destinations (unchanged behavior). Pure, so it is unit-testable.
func scheduleDests(sc Schedule, effective []string) []string {
	if sc.DestinationsExplicit {
		return sc.Destinations
	}
	return effective
}

// Policy is the global backup policy (PLAN §4.6/§4.7).
type Policy struct {
	Destinations []string `json:"destinations"` // default destination IDs (local always added)
	Generations  int      `json:"generations"`  // copies to keep (newest N)
	KeepDaily    int      `json:"keep_daily"`   // GFS: newest per day for the most recent N days
	KeepWeekly   int      `json:"keep_weekly"`  // GFS: newest per ISO week, N weeks
	KeepMonthly  int      `json:"keep_monthly"` // GFS: newest per month, N months
	KeepYearly   int      `json:"keep_yearly"`  // GFS: newest per calendar year, N years
	Autoprune    bool     `json:"autoprune"`
	Schedule     Schedule `json:"schedule"`
	// PruneSchedule runs the saved retention policy fleet-wide on its OWN cadence,
	// independent of any backup (F17) — reclaiming space from targets that aren't
	// being actively backed up. Only Enabled/Kind/Time/Weekday/Monthday/Cron are
	// used (it has no targets); reuses the Schedule cron machinery.
	PruneSchedule Schedule `json:"prune_schedule"`
	// AutocleanMissingDays auto-removes a scheduled target from its schedule after
	// its container has been missing this many days (0 = never), so a schedule
	// stops accumulating dead entries that alert forever (F13).
	AutocleanMissingDays int `json:"autoclean_missing_days"`
}

// retention maps the policy onto the engine's GFS retention config.
func (p Policy) retention() backup.RetentionConfig {
	return backup.RetentionConfig{
		Generations: p.Generations, Daily: p.KeepDaily, Weekly: p.KeepWeekly, Monthly: p.KeepMonthly, Yearly: p.KeepYearly,
	}
}

// cronSpec converts the schedule's friendly fields into a standard 5-field cron.
func (sc Schedule) cronSpec() (string, error) {
	hh, mm := 3, 0
	if sc.Time != "" {
		fmt.Sscanf(sc.Time, "%d:%d", &hh, &mm)
	}
	switch sc.Kind {
	case "daily":
		return fmt.Sprintf("%d %d * * *", mm, hh), nil
	case "weekly":
		return fmt.Sprintf("%d %d * * %d", mm, hh, sc.Weekday%7), nil
	case "monthly":
		d := sc.Monthday
		if d < 1 || d > 28 {
			d = 1
		}
		return fmt.Sprintf("%d %d %d * *", mm, hh, d), nil
	case "custom":
		return sc.Cron, nil
	}
	return "", fmt.Errorf("unknown schedule kind %q", sc.Kind)
}

// nextRun returns the next fire time after `from`, or zero if not schedulable.
func (sc Schedule) nextRun(from time.Time) time.Time {
	spec, err := sc.cronSpec()
	if err != nil {
		return time.Time{}
	}
	s, err := cron.ParseStandard(spec)
	if err != nil {
		return time.Time{}
	}
	return s.Next(from)
}

// loadPolicy reads the policy from settings.
func (s *Server) loadPolicy() Policy {
	var p Policy
	dj, _ := s.store.GetSetting(policyDestinationsKey, "[]")
	_ = json.Unmarshal([]byte(dj), &p.Destinations)
	g, _ := s.store.GetSetting(retentionGenerationsKey, backup.DefaultGenerations)
	p.Generations, _ = strconv.Atoi(g)
	d, _ := s.store.GetSetting(retentionDailyKey, "0")
	p.KeepDaily, _ = strconv.Atoi(d)
	wk, _ := s.store.GetSetting(retentionWeeklyKey, "0")
	p.KeepWeekly, _ = strconv.Atoi(wk)
	mo, _ := s.store.GetSetting(retentionMonthlyKey, "0")
	p.KeepMonthly, _ = strconv.Atoi(mo)
	yr, _ := s.store.GetSetting(retentionYearlyKey, "0")
	p.KeepYearly, _ = strconv.Atoi(yr)
	a, _ := s.store.GetSetting(retentionAutopruneKey, "false")
	p.Autoprune = a == "true"
	p.AutocleanMissingDays = s.autocleanMissingDays()
	sj, _ := s.store.GetSetting(scheduleKey, "{}")
	_ = json.Unmarshal([]byte(sj), &p.Schedule)
	if p.Schedule.Time == "" {
		p.Schedule.Time = "03:00"
	}
	if p.Schedule.Kind == "" {
		p.Schedule.Kind = "weekly"
	}
	// Never serialize a nil slice as JSON null — the UI reads .targets.length and
	// would crash on a fresh install where no schedule targets exist yet.
	if p.Schedule.Targets == nil {
		p.Schedule.Targets = []ScheduleTarget{}
	}
	psj, _ := s.store.GetSetting(retentionPruneScheduleKey, "{}")
	_ = json.Unmarshal([]byte(psj), &p.PruneSchedule)
	if p.PruneSchedule.Time == "" {
		p.PruneSchedule.Time = "03:00"
	}
	if p.PruneSchedule.Kind == "" {
		p.PruneSchedule.Kind = "weekly"
	}
	// The prune sweep has no per-container targets; keep the slice non-nil so the
	// UI (which shares the Schedule shape) never sees a null.
	p.PruneSchedule.Targets = []ScheduleTarget{}
	if p.Destinations == nil {
		p.Destinations = []string{}
	}
	return p
}

// applyOverride folds one scope's override into a policy. Split out so the
// cluster and node tiers apply identically — the tiers differ only in which
// scope they read, never in what an override means.
func applyOverride(p *Policy, ov store.PolicyOverride) {
	if ov.OverrideDestinations {
		p.Destinations = ov.Destinations
	}
	if ov.OverrideRetention {
		p.Generations, p.KeepDaily, p.KeepWeekly, p.KeepMonthly, p.KeepYearly, p.Autoprune =
			ov.Generations, ov.KeepDaily, ov.KeepWeekly, ov.KeepMonthly, ov.KeepYearly, ov.Autoprune
	}
}

// effectivePolicy resolves the global policy with the node's CLUSTER override
// and then the node's own override applied. Used for scheduled-backup
// destinations and the retention preview so a node configured once needn't be
// reconfigured per run.
//
// Precedence, least specific first: global -> cluster (F104) -> node. The
// cluster tier is skipped entirely when the node has no cluster or its cluster
// has no override, so an install that has never used clusters resolves to
// exactly the value it did before.
func (s *Server) effectivePolicy(nodeID string) Policy {
	p := s.loadPolicy()
	if scope := s.store.NodeClusterScope(nodeID); scope != "" {
		if ov, ok := s.store.GetPolicyOverride(scope); ok {
			applyOverride(&p, ov)
		}
	}
	if ov, ok := s.store.GetPolicyOverride(store.NodeScope(nodeID)); ok {
		applyOverride(&p, ov)
	}
	return p
}

// handleGetNodePolicy returns the global policy, this node's override (if any),
// and the resolved effective policy — so the UI can show inherited vs overridden.
func (s *Server) handleGetNodePolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetNode(id); err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	ov, _ := s.store.GetPolicyOverride(store.NodeScope(id))
	writeJSON(w, http.StatusOK, map[string]any{
		"global":    s.loadPolicy(),
		"override":  ov,
		"effective": s.effectivePolicy(id),
	})
}

// handleSetNodePolicy saves (or clears) a node's policy override.
func (s *Server) handleSetNodePolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetNode(id); err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	var ov store.PolicyOverride
	if err := readJSON(r, &ov); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	// Clamp negatives, matching the global policy validation.
	for _, v := range []*int{&ov.Generations, &ov.KeepDaily, &ov.KeepWeekly, &ov.KeepMonthly, &ov.KeepYearly} {
		if *v < 0 {
			*v = 0
		}
	}
	if err := s.store.SetPolicyOverride(store.NodeScope(id), ov); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "node.policy.update", id, fmt.Sprintf("dests=%v retention=%v", ov.OverrideDestinations, ov.OverrideRetention))
	s.handleGetNodePolicy(w, r)
}

// cachedContainerName resolves a container's name from the inventory cache
// (no Docker call); returns "" if not cached.
func (s *Server) cachedContainerName(nodeID, cid string) string {
	if c := s.cachedContainer(nodeID, cid); c != nil {
		return c.Name
	}
	return ""
}

// cachedContainer returns a node's cached container by id (name, image, mounts,
// …) without a Docker call — used by coverage/protect (B2/B5). nil if not cached.
func (s *Server) cachedContainer(nodeID, cid string) *dockercli.Container {
	s.statMu.RLock()
	defer s.statMu.RUnlock()
	if st, ok := s.stats[nodeID]; ok {
		for _, c := range st.Containers {
			if c.ID == cid {
				return c
			}
		}
	}
	return nil
}

// resolveContainerName maps a container ID to its name (the stable per-container
// override key), preferring the cache and falling back to a live inspect.
func (s *Server) resolveContainerName(nodeID, cid string) (string, error) {
	if n := s.cachedContainerName(nodeID, cid); n != "" {
		return n, nil
	}
	cli, err := s.reg.Get(nodeID)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	insp, err := cli.ContainerInspect(ctx, cid)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(insp.Name, "/"), nil
}

// resolveContainerID maps a container NAME to its current id — the reverse of
// resolveContainerName, and the reason F222 exists: an id dies on every
// `docker compose up -d`, a name does not. Cache first (every other read on
// these pages trusts it), live lookup as the fallback for a container started
// since the last poll.
func (s *Server) resolveContainerID(nodeID, name string) (string, error) {
	name = strings.TrimPrefix(strings.TrimSpace(name), "/")
	if name == "" {
		return "", errors.New("container name is empty")
	}
	s.statMu.RLock()
	if st, ok := s.stats[nodeID]; ok {
		for _, c := range st.Containers {
			if c.Name == name {
				s.statMu.RUnlock()
				return c.ID, nil
			}
		}
	}
	s.statMu.RUnlock()
	cli, err := s.reg.Get(nodeID)
	if err != nil {
		// Not "it does not exist" — we could not look. The message says which,
		// because a favorite that stopped working needs to distinguish "you
		// renamed it" from "the node is down".
		return "", fmt.Errorf("cannot reach this node to find a container named %q: %w", name, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if id, ok := dockercli.FindContainerByName(ctx, cli, name); ok {
		return id, nil
	}
	return "", fmt.Errorf("no container named %q on this node", name)
}

// frequencySkip reports whether a scheduled backup of this container should be
// skipped because its per-container minimum interval hasn't elapsed since the
// last successful backup (PLAN §4.2 granular control). Unknown name / no override
// / no prior success ⇒ never skip.
func (s *Server) frequencySkip(nodeID, name string) (bool, time.Duration) {
	if name == "" {
		return false, 0
	}
	ov, ok := s.store.GetPolicyOverride(store.ContainerScope(nodeID, name))
	if !ok || !ov.OverrideFrequency || ov.MinIntervalHours <= 0 {
		return false, 0
	}
	last, ok := s.store.LastSuccessfulBackupAt(nodeID, name)
	if !ok {
		return false, 0
	}
	minInterval := time.Duration(ov.MinIntervalHours) * time.Hour
	if elapsed := time.Since(time.Unix(last, 0)); elapsed < minInterval {
		return true, minInterval - elapsed
	}
	return false, 0
}

// handleGetContainerPolicy returns a container's override (if any) plus the
// inherited (node→global) retention it would use otherwise, so the UI can show
// inherited-vs-overridden per container (PLAN §4.2 granular control).
func (s *Server) handleGetContainerPolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cid := r.PathValue("cid")
	if _, err := s.store.GetNode(id); err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	name, err := s.resolveContainerName(id, cid)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	ov, _ := s.store.GetPolicyOverride(store.ContainerScope(id, name))
	// Inherited retention = node override then global, WITHOUT this container's own.
	inhCfg, inhAutoprune := backup.EffectiveRetention(s.store, id)
	writeJSON(w, http.StatusOK, map[string]any{
		"name":     name,
		"override": ov,
		"inherited": map[string]any{
			"generations": inhCfg.Generations, "keep_daily": inhCfg.Daily,
			"keep_weekly": inhCfg.Weekly, "keep_monthly": inhCfg.Monthly, "keep_yearly": inhCfg.Yearly, "autoprune": inhAutoprune,
		},
	})
}

// handleSetContainerPolicy saves (or clears) a container's policy override.
func (s *Server) handleSetContainerPolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cid := r.PathValue("cid")
	if _, err := s.store.GetNode(id); err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	name, err := s.resolveContainerName(id, cid)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	var ov store.PolicyOverride
	if err := readJSON(r, &ov); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	// Container overrides never change destinations — that's a node/global control.
	ov.OverrideDestinations = false
	ov.Destinations = nil
	for _, v := range []*int{&ov.Generations, &ov.KeepDaily, &ov.KeepWeekly, &ov.KeepMonthly, &ov.KeepYearly, &ov.MinIntervalHours} {
		if *v < 0 {
			*v = 0
		}
	}
	if err := s.store.SetPolicyOverride(store.ContainerScope(id, name), ov); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "container.policy.update", name, fmt.Sprintf("retention=%v frequency=%v", ov.OverrideRetention, ov.OverrideFrequency))
	s.handleGetContainerPolicy(w, r)
}

// handleGetPolicy returns the global policy + the schedule's next fire time.
func (s *Server) handleGetPolicy(w http.ResponseWriter, r *http.Request) {
	p := s.loadPolicy()
	// Enrich legacy specific targets (id only) with the current container name so
	// the UI matches by name (surviving recreation) and re-saves the name. A name
	// that no longer resolves is left as-is and flagged in the UI as missing.
	for i, t := range p.Schedule.Targets {
		if t.ContainerName == "" && t.ContainerID != "" {
			if n := s.cachedContainerName(t.NodeID, t.ContainerID); n != "" {
				p.Schedule.Targets[i].ContainerName = n
			}
		}
	}
	var next int64
	if p.Schedule.Enabled {
		if t := p.Schedule.nextRun(time.Now()); !t.IsZero() {
			next = t.Unix()
		}
	}
	lrStr, _ := s.store.GetSetting("schedule.last_run", "0")
	lastRun, _ := strconv.ParseInt(lrStr, 10, 64)
	caughtUp, _ := s.store.GetSetting("schedule.last_caught_up", "false")
	var pruneNext int64
	if p.PruneSchedule.Enabled {
		if t := p.PruneSchedule.nextRun(time.Now()); !t.IsZero() {
			pruneNext = t.Unix()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"policy": p, "next_run": next,
		"last_run": lastRun, "last_caught_up": caughtUp == "true",
		"prune_next_run": pruneNext,
	})
}

// handleSetPolicy saves the global policy.
func (s *Server) handleSetPolicy(w http.ResponseWriter, r *http.Request) {
	var p Policy
	if err := readJSON(r, &p); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	if p.Generations < 0 {
		p.Generations = 0
	}
	for _, v := range []*int{&p.KeepDaily, &p.KeepWeekly, &p.KeepMonthly, &p.KeepYearly, &p.AutocleanMissingDays} {
		if *v < 0 {
			*v = 0
		}
	}
	// Validate a custom cron up front so users get immediate feedback.
	if p.Schedule.Enabled {
		if spec, err := p.Schedule.cronSpec(); err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		} else if _, err := cron.ParseStandard(spec); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid cron expression: "+err.Error())
			return
		}
	}
	if p.PruneSchedule.Enabled {
		if spec, err := p.PruneSchedule.cronSpec(); err != nil {
			errJSON(w, http.StatusBadRequest, "prune schedule: "+err.Error())
			return
		} else if _, err := cron.ParseStandard(spec); err != nil {
			errJSON(w, http.StatusBadRequest, "prune schedule: invalid cron expression: "+err.Error())
			return
		}
	}
	dj, _ := json.Marshal(p.Destinations)
	sj, _ := json.Marshal(p.Schedule)
	// The prune sweep has no targets — drop any the client sent so the stored blob
	// stays lean and can't smuggle a target set into the prune path.
	p.PruneSchedule.Targets = nil
	psj, _ := json.Marshal(p.PruneSchedule)
	// (Re)establish the prune baseline on save so a freshly-enabled schedule fires
	// at its NEXT window, never immediately; disabling clears it so a later enable
	// starts fresh (no catch-up backfill on first enable — mirrors the scheduler).
	pruneBaseline := "0"
	if p.PruneSchedule.Enabled {
		pruneBaseline = strconv.FormatInt(time.Now().Unix(), 10)
	}
	// One unit: a crash between two of these would leave a retention policy the
	// operator never chose — a schedule enabled against last month's keep counts,
	// or a prune schedule with no baseline, which fires on the next tick.
	if err := s.store.SetSettings(map[string]string{
		policyDestinationsKey:     string(dj),
		retentionGenerationsKey:   strconv.Itoa(p.Generations),
		retentionDailyKey:         strconv.Itoa(p.KeepDaily),
		retentionWeeklyKey:        strconv.Itoa(p.KeepWeekly),
		retentionMonthlyKey:       strconv.Itoa(p.KeepMonthly),
		retentionYearlyKey:        strconv.Itoa(p.KeepYearly),
		retentionAutopruneKey:     strconv.FormatBool(p.Autoprune),
		autocleanMissingDaysKey:   strconv.Itoa(p.AutocleanMissingDays),
		scheduleKey:               string(sj),
		retentionPruneScheduleKey: string(psj),
		retentionPruneLastRunKey:  pruneBaseline,
	}); err != nil {
		errJSON(w, http.StatusInternalServerError, "could not save the policy: "+err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "policy.update", "", fmt.Sprintf("dests=%d keep=%d schedule=%v/%s prune=%v/%s", len(p.Destinations), p.Generations, p.Schedule.Enabled, p.Schedule.Kind, p.PruneSchedule.Enabled, p.PruneSchedule.Kind))
	s.handleGetPolicy(w, r)
}

// handleRetentionPreview is a DRY RUN of the saved retention policy: it reports,
// per container, which backups would be kept vs pruned — without deleting
// anything (PLAN §4.6 "dry-run preview before pruning ever deletes").
func (s *Server) handleRetentionPreview(w http.ResponseWriter, r *http.Request) {
	// Slim projection (perf Fix 7): the preview reads only scalars — grouping
	// keys, GFS inputs (created/pinned/label), and the item view fields.
	all, err := s.store.ListBackupSummaries("", 100000)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Group successful backups by (node, target), preserving newest-first order.
	type key struct{ node, target string }
	groups := map[key][]*store.Backup{}
	order := []key{}
	for _, b := range all {
		if b.Status != "success" {
			continue
		}
		k := key{b.NodeID, b.TargetName}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], b)
	}

	type itemView struct {
		ID        string `json:"id"`
		CreatedAt int64  `json:"created_at"`
		SizeBytes int64  `json:"size_bytes"`
		Verified  string `json:"verified"`
		Action    string `json:"action"`          // keep | prune
		Pinned    bool   `json:"pinned"`          // F2: kept because it is pinned ("keep forever")
		Label     string `json:"label,omitempty"` // F48: so the UI can badge "auto:" snapshots
	}
	type targetView struct {
		NodeID     string     `json:"node_id"`
		NodeName   string     `json:"node_name"`
		Target     string     `json:"target"`
		Keep       int        `json:"keep"`
		Prune      int        `json:"prune"`
		PruneBytes int64      `json:"prune_bytes"`
		Items      []itemView `json:"items"`
	}
	nodeName := map[string]string{}
	out := []targetView{}
	var totalPrune int
	var totalBytes int64
	var anyActive bool
	for _, k := range order {
		list := groups[k]
		cfg, _ := backup.EffectiveRetentionFor(s.store, k.node, k.target) // container → node → global
		if cfg.Active() {
			anyActive = true
		}
		keep, prune := backup.SelectForRetention(list, cfg)
		keepSet := map[string]bool{}
		for _, b := range keep {
			keepSet[b.ID] = true
		}
		nn, ok := nodeName[k.node]
		if !ok {
			if n, err := s.store.GetNode(k.node); err == nil {
				nn = n.Name
			}
			nodeName[k.node] = nn
		}
		tv := targetView{NodeID: k.node, NodeName: nn, Target: k.target, Keep: len(keep), Prune: len(prune)}
		for _, b := range list {
			action := "keep"
			if !keepSet[b.ID] {
				action = "prune"
				tv.PruneBytes += b.SizeBytes
			}
			tv.Items = append(tv.Items, itemView{ID: b.ID, CreatedAt: b.CreatedAt, SizeBytes: b.SizeBytes, Verified: b.Verified, Action: action, Pinned: b.Pinned, Label: b.Label})
		}
		totalPrune += tv.Prune
		totalBytes += tv.PruneBytes
		out = append(out, tv)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"active":            anyActive,
		"targets":           out,
		"total_prune":       totalPrune,
		"total_prune_bytes": totalBytes,
	})
}

// handleRetentionPrune applies the saved retention policy now, deleting pruned
// backups from every location. Destructive — auth + CSRF + audited.
func (s *Server) handleRetentionPrune(w http.ResponseWriter, r *http.Request) {
	if !s.anyRetentionActive() {
		errJSON(w, http.StatusBadRequest, "no retention rule set — nothing to prune")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	pruned, freed := s.engine.PruneAll(ctx)
	_ = s.store.Audit(userFrom(r), "retention.prune", "", fmt.Sprintf("pruned=%d freed=%d", pruned, freed))
	writeJSON(w, http.StatusOK, map[string]any{"pruned": pruned, "freed_bytes": freed})
}

// anyRetentionActive reports whether the global policy OR any node override has
// an active retention rule (so "prune now" isn't refused when only a node sets one).
func (s *Server) anyRetentionActive() bool {
	if s.loadPolicy().retention().Active() {
		return true
	}
	nodes, _ := s.store.ListNodes()
	for _, n := range nodes {
		if cfg, _ := backup.EffectiveRetention(s.store, n.ID); cfg.Active() {
			return true
		}
	}
	return false
}

// handleRunSchedule backs up the currently-saved schedule targets immediately
// (a starting point before the first scheduled run). Runs regardless of whether
// the schedule is enabled.
func (s *Server) handleRunSchedule(w http.ResponseWriter, r *http.Request) {
	p := s.loadPolicy()
	if len(p.Schedule.Targets) == 0 {
		errJSON(w, http.StatusBadRequest, "no targets selected — pick at least one node or container")
		return
	}
	_ = s.store.Audit(userFrom(r), "schedule.runnow", "", fmt.Sprintf("%d target(s)", len(p.Schedule.Targets)))
	go s.runSchedule(p)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

// startScheduler runs the global auto-backup schedule. It ticks
// often, fires when the next cron time has passed since the last run, and
// never backfills missed windows on first enable.
func (s *Server) startScheduler() {
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			s.schedulerTick()
		}
	}()
}

// startRetentionPrune runs the optional scheduled fleet-wide retention sweep
// (F17), independent of any backup. It ticks like startScheduler and fires when
// the prune schedule's next cron time has passed since the last sweep, so space
// is reclaimed from targets that aren't being actively backed up — not only as a
// side effect of a fresh backup (autoprune).
func (s *Server) startRetentionPrune() {
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			s.retentionPruneTick()
		}
	}()
}

// prunePlan decides whether a scheduled prune should fire and the last_run to
// persist. Pure, so it's unit-testable. A disabled or unschedulable schedule
// never fires and never moves the baseline; the first evaluation (zero lastRun)
// establishes a baseline at `now` (no immediate backfill); otherwise it fires
// once the cron window has passed.
func prunePlan(sc Schedule, lastRun, now time.Time) (fire bool, newLastRun time.Time) {
	if !sc.Enabled || sc.nextRun(now).IsZero() {
		return false, lastRun
	}
	if lastRun.IsZero() {
		return false, now // establish baseline; fire at the NEXT window
	}
	if due, _, _ := scheduleDue(sc, lastRun, now, scheduleMissThreshold); !due {
		return false, lastRun
	}
	return true, now
}

// retentionPruneTick evaluates the prune schedule and, when due, runs the same
// fleet-wide PruneAll path as "Prune now" — honoring WORM/immutable copies.
func (s *Server) retentionPruneTick() {
	sc := s.loadPolicy().PruneSchedule
	lrStr, _ := s.store.GetSetting(retentionPruneLastRunKey, "0")
	var lastRun time.Time
	if lr, _ := strconv.ParseInt(lrStr, 10, 64); lr > 0 {
		lastRun = time.Unix(lr, 0)
	}
	fire, newLast := prunePlan(sc, lastRun, time.Now())
	if !newLast.Equal(lastRun) {
		_ = s.store.SetSetting(retentionPruneLastRunKey, strconv.FormatInt(newLast.Unix(), 10))
	}
	if !fire {
		return
	}
	if !s.anyRetentionActive() {
		s.logSink("retention", "INFO", "Scheduled prune skipped — no active retention rule set")
		return
	}
	go s.runScheduledPrune()
}

// runScheduledPrune applies retention across the fleet and audits the sweep.
func (s *Server) runScheduledPrune() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	pruned, freed := s.engine.PruneAll(ctx)
	s.logSink("retention", "INFO", fmt.Sprintf("Scheduled retention prune removed %d backup(s), freed %s", pruned, humanBytes(freed)))
	_ = s.store.Audit("scheduler", "retention.prune.scheduled", "", fmt.Sprintf("pruned=%d freed=%d", pruned, freed))
}

// scheduleMissThreshold: a fire whose scheduled time is older than this was a
// MISSED window (the app was down / not ticking) being caught up, vs an on-time
// fire that's merely a few seconds late due to tick granularity.
const scheduleMissThreshold = 5 * time.Minute

// scheduleDue reports whether a scheduled run is due given the last run time, the
// scheduled time it would fire for, and whether that window was missed (a
// catch-up). Pure, so it's unit-testable.
func scheduleDue(sc Schedule, lastRun, now time.Time, missThreshold time.Duration) (fire bool, scheduledAt time.Time, missed bool) {
	next := sc.nextRun(lastRun)
	if next.IsZero() || now.Before(next) {
		return false, next, false
	}
	return true, next, now.Sub(next) > missThreshold
}

// schedulerTick evaluates every named schedule independently (F6): each keeps its
// own last_run baseline in its row and catches up at most one missed window.
func (s *Server) schedulerTick() {
	rows, err := s.store.ListSchedules()
	if err != nil {
		return
	}
	now := time.Now()
	for _, row := range rows {
		sc := scheduleFromRow(row)
		if !sc.Enabled || len(sc.Targets) == 0 {
			continue
		}
		if row.LastRun == 0 {
			// First evaluation: establish a baseline so we fire at the NEXT
			// scheduled time, not immediately.
			row.LastRun = now.Unix()
			row.NextRun = unixOrZero(sc.nextRun(now))
			_ = s.store.UpsertSchedule(row)
			continue
		}
		fire, scheduledAt, missed := scheduleDue(sc, time.Unix(row.LastRun, 0), now, scheduleMissThreshold)
		if !fire {
			continue
		}
		// Catch up at most ONE missed window: advance last_run to now so a long
		// outage doesn't replay every window (no stampede).
		row.LastRun = now.Unix()
		row.NextRun = unixOrZero(sc.nextRun(now))
		_ = s.store.UpsertSchedule(row)
		if missed {
			when := scheduledAt.Format("2006-01-02 15:04")
			s.logSink("schedule", "WARN", fmt.Sprintf("Missed scheduled window %q at %s (app was down) — running catch-up backup now", sc.Name, when))
			// Operational alert: a scheduled window was missed (the app was down
			// at backup time) — surface it, not just the log.
			s.notify(notify.KindMissedSchedule, "Missed scheduled backup",
				fmt.Sprintf("The scheduled backup %q window at %s was missed because DockBack wasn't running. A catch-up backup is running now, but check why the app was down.", sc.Name, when))
		}
		s.runScheduleTargets(sc)
	}
}

// unixOrZero returns t.Unix() or 0 for a zero time (unschedulable spec).
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// scheduledBackupOptions builds a scheduled run's Options for one container,
// applying the container's remembered manual choices — compression, app-native
// export, and save-image — so scheduled and whole-node runs match what the user
// set for a manual backup (F3). Defaults when nothing is remembered: balanced
// compression, export/image off. Mounts and pause mode are resolved separately
// (IncludeMounts left nil → the remembered/size-based default; pause via
// resolvePauseMode). Destinations are always explicit (the scheduler resolves
// them per node), so a stored empty list can't widen the target set.
func (s *Server) scheduledBackupOptions(nodeID, containerID, name string, dests []string) backup.Options {
	opts := backup.Options{
		NodeID: nodeID, ContainerID: containerID, Compression: "balanced",
		Destinations: dests, DestinationsExplicit: true,
	}
	if name == "" {
		return opts
	}
	if js, _ := s.store.GetSetting(backup.BackupOptionsKey(nodeID, name), ""); js != "" {
		var sb backup.SavedBackupOptions
		if json.Unmarshal([]byte(js), &sb) == nil {
			if sb.Compression != "" {
				opts.Compression = sb.Compression
				// F84: a saved non-default is a deliberate choice; a saved
				// "balanced" is just the manual panel's default carried along,
				// so it stays autotune-eligible.
				opts.CompressionExplicit = sb.Compression != "balanced"
			}
			opts.AppExport = sb.AppExport
			opts.SaveImage = sb.SaveImage
		}
	}
	return opts
}

// runSchedule backs up the legacy single-schedule targets (kept for the
// /api/policy/run route). Named schedules go through runScheduleTargets.
func (s *Server) runSchedule(p Policy) {
	s.runScheduleTargets(p.Schedule)
}

// autocleanMissingDays is the grace period before a scheduled target whose
// container has vanished is auto-removed (0 = never), from settings (F13).
func (s *Server) autocleanMissingDays() int {
	v, _ := s.store.GetSetting(autocleanMissingDaysKey, "0")
	n, _ := strconv.Atoi(v)
	if n < 0 {
		n = 0
	}
	return n
}

// scheduleMissingKey is the settings key that remembers when a scheduled target
// (node + container label) was first seen missing (F13 auto-clean bookkeeping).
func scheduleMissingKey(nodeID, label string) string {
	return "schedule.missing_since:" + nodeID + "/" + label
}

// missingTargetAction decides, for a scheduled target whose container is missing,
// the first-seen timestamp to persist and whether it should now be auto-removed:
// on first sighting it records `now`; once it has been missing for the grace
// period it is removed (days<=0 disables removal). Pure, so it's unit-testable.
func missingTargetAction(firstSeen int64, days int, now int64) (newFirstSeen int64, remove bool) {
	if firstSeen <= 0 {
		firstSeen = now
	}
	if days > 0 && now-firstSeen >= int64(days)*86400 {
		return firstSeen, true
	}
	return firstSeen, false
}

// removeScheduleTargets drops the given targets from a saved schedule row by id,
// preserving its run baseline (last_run). No-op for a legacy/unsaved schedule
// (empty id) so the deprecated single-schedule path is untouched (F13).
func (s *Server) removeScheduleTargets(scheduleID string, drop []ScheduleTarget) {
	if scheduleID == "" || len(drop) == 0 {
		return
	}
	row, err := s.store.GetSchedule(scheduleID)
	if err != nil {
		return
	}
	sc := scheduleFromRow(row)
	keep := make([]ScheduleTarget, 0, len(sc.Targets))
	for _, t := range sc.Targets {
		if scheduleTargetIn(drop, t) {
			continue
		}
		keep = append(keep, t)
	}
	sc.Targets = keep
	newRow, err := sc.toRow()
	if err != nil {
		return
	}
	newRow.LastRun = row.LastRun // keep the baseline so we don't re-fire/catch-up
	_ = s.store.UpsertSchedule(newRow)
}

// scheduleTargetIn reports whether t matches any target in list (by node +
// container identity).
func scheduleTargetIn(list []ScheduleTarget, t ScheduleTarget) bool {
	for _, x := range list {
		if x.NodeID == t.NodeID && x.ContainerName == t.ContainerName && x.ContainerID == t.ContainerID {
			return true
		}
	}
	return false
}

// runScheduleTargets backs up every targeted container of one schedule to the
// policy destinations (resolved per node). Specific targets are resolved by NAME
// to their current container (surviving recreation); a target whose container no
// longer exists is skipped with a WARN and a throttled alert, and — when
// schedule.autoclean_missing_days is set — auto-removed after that grace period
// (PLAN §4.2/§4.7, F13).
func (s *Server) runScheduleTargets(sc Schedule) {
	count := 0
	now := time.Now().Unix()
	autocleanDays := s.autocleanMissingDays()
	var toRemove []ScheduleTarget
	// Per-run cache of each node's container list (all states) so multiple targets
	// on one node don't re-list, and specific targets can be resolved by name.
	listCache := map[string][]*dockercli.Container{}
	nodeList := func(nodeID string) ([]*dockercli.Container, bool) {
		if cs, ok := listCache[nodeID]; ok {
			return cs, true
		}
		cli, err := s.reg.Get(nodeID)
		if err != nil {
			return nil, false
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		cs, err := dockercli.ListContainers(ctx, cli)
		cancel()
		if err != nil {
			return nil, false
		}
		listCache[nodeID] = cs
		return cs, true
	}

	for _, t := range sc.Targets {
		node, err := s.store.GetNode(t.NodeID)
		if err != nil {
			continue
		}
		cs, ok := nodeList(t.NodeID)
		if !ok {
			s.logSink("schedule", "WARN", fmt.Sprintf("Skipping node %q — unreachable this run", node.Name))
			continue
		}
		// F27: a schedule may override its offsite destinations; otherwise use the
		// effective per-node policy set. scheduledBackupOptions always marks them
		// explicit, so an override to an empty list means local-only.
		dests := scheduleDests(sc, s.effectivePolicy(t.NodeID).Destinations)

		// F47: a STACK target backs up a whole compose project. Handled before the
		// container/whole-node branch below (a stack target has empty container fields,
		// so it must not fall through to the whole-node path).
		if t.Stack != "" {
			var members []*dockercli.Container
			for _, c := range cs {
				if c.Stack == t.Stack {
					members = append(members, c)
				}
			}
			label := "stack:" + t.Stack
			if len(members) == 0 {
				// No current members — F13 auto-clean bookkeeping, keyed on the stack.
				key := scheduleMissingKey(t.NodeID, label)
				firstSeen := int64(0)
				if v, _ := s.store.GetSetting(key, "0"); v != "" {
					firstSeen, _ = strconv.ParseInt(v, 10, 64)
				}
				newFirst, remove := missingTargetAction(firstSeen, autocleanDays, now)
				if newFirst != firstSeen {
					_ = s.store.SetSetting(key, strconv.FormatInt(newFirst, 10))
				}
				if remove {
					toRemove = append(toRemove, t)
					_ = s.store.DeleteSetting(key)
					s.logSink("schedule", "INFO", fmt.Sprintf("Auto-removed scheduled stack %q on %q — no members for over %d day(s)", t.Stack, node.Name, autocleanDays))
					_ = s.store.Audit("scheduler", "schedule.target.autoremoved", t.NodeID+"/"+label, fmt.Sprintf("schedule=%q missing_days=%d", sc.Name, autocleanDays))
				} else {
					s.logSink("schedule", "WARN", fmt.Sprintf("Scheduled stack %q on %q has no current members — skipping", t.Stack, node.Name))
					s.notifyThrottled(notify.KindTargetMissing, t.NodeID+"/"+label,
						"Scheduled backup target missing",
						fmt.Sprintf("The stack %q on %s is scheduled for automatic backup but has no current members — it was skipped. Remove it from the schedule in Settings, or recreate the stack.", t.Stack, node.Name),
						targetMissingCooldown)
				}
				continue
			}
			// Members present — clear any stale missing marker.
			if v, _ := s.store.GetSetting(scheduleMissingKey(t.NodeID, label), ""); v != "" {
				_ = s.store.DeleteSetting(scheduleMissingKey(t.NodeID, label))
			}

			if t.Consistent {
				// One coherent app-consistent snapshot under a single stack-exclusive
				// lock — mirrors handleBackupStack's consistent goroutine.
				lockKey := stackKey(t.NodeID, t.Stack, "")
				if !s.locks.acquireRestore(lockKey) {
					s.logSink("schedule", "WARN", fmt.Sprintf("Skipping app-consistent snapshot of stack %q on %q — a backup or restore of it is already in progress", t.Stack, node.Name))
					continue
				}
				project := t.Stack
				s.logSink("stack:"+project, "INFO", "Scheduled app-consistent snapshot of the stack")
				go func(nodeID, project string, dests []string) {
					defer guardPanic("stack consistent backup", "stack:"+project, func() {
						s.logSink("stack:"+project, "ERR", "App-consistent snapshot failed: internal error (panic)")
					})
					defer s.releaseRestoreAndDispatch(lockKey)
					bctx, bcancel := context.WithTimeout(context.Background(), 6*time.Hour)
					defer bcancel()
					// F79: each service resolves its own remembered options, exactly
					// like a non-consistent scheduled run of the same container.
					if err := s.engine.BackupStackConsistent(bctx, nodeID, project, backup.Options{
						NodeID: nodeID, Compression: "balanced", Destinations: dests, DestinationsExplicit: true,
					}, func(cid, name string) backup.Options {
						return s.scheduledBackupOptions(nodeID, cid, name, dests)
					}); err != nil {
						s.logSink("stack:"+project, "ERR", "App-consistent snapshot: "+err.Error())
					}
				}(t.NodeID, project, dests)
				count++ // the stack counts as one scheduled unit
				continue
			}

			// Non-consistent stack: enqueue each current member (concurrent fan-out).
			for _, c := range members {
				if skip, remaining := s.frequencySkip(t.NodeID, c.Name); skip {
					s.logSink("schedule", "INFO", fmt.Sprintf("Skipping %q — min backup interval not yet elapsed (~%s remaining)", c.Name, remaining.Round(time.Minute)))
					continue
				}
				s.enqueueBackup(node.Name, s.scheduledBackupOptions(t.NodeID, c.ID, c.Name, dests), prioScheduled)
				count++
			}
			continue
		}

		// Build (id, name) pairs to back up for this target.
		type ct struct{ id, name string }
		var targets []ct
		if t.ContainerName == "" && t.ContainerID == "" {
			// Whole-node target: every running container, plus stopped ones when the
			// schedule opts in (F9). F219: never DockBack's own test clones — a copy
			// it created to prove a backup, and will delete tomorrow, is not a
			// workload to protect.
			for _, c := range cs {
				if isTestClone(c) {
					continue
				}
				if c.State == "running" || sc.IncludeStopped {
					targets = append(targets, ct{c.ID, c.Name})
				}
			}
		} else {
			// Specific target: resolve by name (preferred), else legacy id.
			var found *dockercli.Container
			for _, c := range cs {
				if (t.ContainerName != "" && c.Name == t.ContainerName) ||
					(t.ContainerName == "" && c.ID == t.ContainerID) {
					found = c
					break
				}
			}
			if found == nil {
				label := t.ContainerName
				if label == "" {
					label = t.ContainerID
				}
				// Auto-clean bookkeeping (F13): remember when the target first went
				// missing; after the grace period, drop it so it stops alerting forever.
				key := scheduleMissingKey(t.NodeID, label)
				firstSeen := int64(0)
				if v, _ := s.store.GetSetting(key, "0"); v != "" {
					firstSeen, _ = strconv.ParseInt(v, 10, 64)
				}
				newFirst, remove := missingTargetAction(firstSeen, autocleanDays, now)
				if newFirst != firstSeen {
					_ = s.store.SetSetting(key, strconv.FormatInt(newFirst, 10))
				}
				if remove {
					toRemove = append(toRemove, t)
					_ = s.store.DeleteSetting(key)
					s.logSink("schedule", "INFO", fmt.Sprintf("Auto-removed scheduled target %q on %q — missing for over %d day(s)", label, node.Name, autocleanDays))
					_ = s.store.Audit("scheduler", "schedule.target.autoremoved", t.NodeID+"/"+label, fmt.Sprintf("schedule=%q missing_days=%d", sc.Name, autocleanDays))
				} else {
					s.logSink("schedule", "WARN", fmt.Sprintf("Scheduled target %q on %q no longer exists — skipping (remove it from the schedule in Settings, or recreate the container)", label, node.Name))
					s.notifyThrottled(notify.KindTargetMissing, t.NodeID+"/"+label,
						"Scheduled backup target missing",
						fmt.Sprintf("The container %q on %s is scheduled for automatic backup but no longer exists — it was skipped. Remove it from the schedule in Settings, or restore/recreate the container.", label, node.Name),
						targetMissingCooldown)
				}
				continue
			}
			// Resolved — clear any stale missing marker so a recreated container
			// doesn't carry an old first-seen timestamp (F13).
			label := t.ContainerName
			if label == "" {
				label = t.ContainerID
			}
			if v, _ := s.store.GetSetting(scheduleMissingKey(t.NodeID, label), ""); v != "" {
				_ = s.store.DeleteSetting(scheduleMissingKey(t.NodeID, label))
			}
			targets = append(targets, ct{found.ID, found.Name})
		}

		for _, tg := range targets {
			// Per-container frequency throttle: skip a container whose min backup
			// interval hasn't elapsed since its last successful backup, so a "large"
			// container can back up less often than the global schedule (PLAN §4.2).
			if skip, remaining := s.frequencySkip(t.NodeID, tg.name); skip {
				s.logSink("schedule", "INFO", fmt.Sprintf("Skipping %q — min backup interval not yet elapsed (~%s remaining)", tg.name, remaining.Round(time.Minute)))
				continue
			}
			// Scheduled priority + jitter: a fleet-wide window is staggered and
			// yields to interactive backups. Options honor the container's remembered
			// manual choices (compression / app-native export / save image), F3.
			s.enqueueBackup(node.Name, s.scheduledBackupOptions(t.NodeID, tg.id, tg.name, dests), prioScheduled)
			count++
		}
	}
	// Persist any auto-removed targets to this schedule's row (once, F13).
	s.removeScheduleTargets(sc.ID, toRemove)

	label := sc.Name
	if label == "" {
		label = "schedule"
	}
	s.logSink("schedule", "INFO", fmt.Sprintf("Scheduled backup %q started for %d container(s)", label, count))
	_ = s.store.Audit("scheduler", "schedule.run", label, fmt.Sprintf("%d containers", count))
}
