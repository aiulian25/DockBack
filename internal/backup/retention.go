package backup

import (
	"context"
	"strconv"
	"strings"
	"time"

	"dockback/internal/store"
)

// The values of store.Backup.Verified that retention tells apart. Anything else is
// a backup with no verdict: still being verified, or adopted from an archive.
const (
	verifiedOK     = "verified"
	verifiedFailed = "failed"
)

// RetentionConfig is the GFS-style retention policy (PLAN §4.6): keep the newest
// N copies, plus the newest per day/week/month for the most recent buckets.
type RetentionConfig struct {
	Generations int // keep newest N regardless of age
	Daily       int // keep newest backup of each of the most recent N days
	Weekly      int // …N ISO weeks
	Monthly     int // …N months
	Yearly      int // …N calendar years
	// AutoKeep is the separate retention budget for automatic ("auto:") protective
	// snapshots (F48): keep the newest N per container, pruned BEFORE GFS so a flappy
	// container or an update night can't crowd out the scheduled generations the user
	// relies on. 0 = auto snapshots are treated like any other backup (prior behavior).
	AutoKeep int
}

// gfsActive reports whether any GFS bucket rule is set (drives the day/week/month/
// year selection; no GFS rule ⇒ keep every normal backup).
func (c RetentionConfig) gfsActive() bool {
	return c.Generations > 0 || c.Daily > 0 || c.Weekly > 0 || c.Monthly > 0 || c.Yearly > 0
}

// Active reports whether any retention rule is set — a GFS rule OR the auto-snapshot
// budget (F48) — so a target with only `retention.autosnap_keep` set is still swept.
// No rule ⇒ keep everything.
func (c RetentionConfig) Active() bool {
	return c.gfsActive() || c.AutoKeep > 0
}

// RetentionFromSettings builds the config from persisted settings via a getter
// (store.GetSetting), so the engine and API resolve it identically.
// DefaultGenerations is how many copies of a container are kept when the
// operator has set no retention policy.
//
// It lives here because this is the function that decides what actually gets
// pruned; every other reader is answering a question ABOUT that decision. Two of
// them used to answer with a different number — the settings endpoint said 10
// while the engine kept 3 — so the panel showed a policy the app was not
// applying, and an operator reading the page had no way to tell.
const DefaultGenerations = "3"

func RetentionFromSettings(get func(string, string) (string, error)) RetentionConfig {
	atoi := func(key, def string) int {
		v, _ := get(key, def)
		n, _ := strconv.Atoi(v)
		if n < 0 {
			n = 0
		}
		return n
	}
	return RetentionConfig{
		Generations: atoi("retention.generations", DefaultGenerations),
		Daily:       atoi("retention.daily", "0"),
		Weekly:      atoi("retention.weekly", "0"),
		Monthly:     atoi("retention.monthly", "0"),
		Yearly:      atoi("retention.yearly", "0"),
		AutoKeep:    atoi("retention.autosnap_keep", "3"),
	}
}

// partitionAutoSnaps splits newest-first backups into non-auto ("normal") rows, the
// automatic ("auto:") snapshots to KEEP (the newest `keep` of them, plus every
// pinned auto row, which never consume the quota), and the auto snapshots to PRUNE
// (F48). A backup is automatic when its label starts with "auto:" — the event-
// triggered pre-change/crash snapshots. Pure, and independent of GFS.
func partitionAutoSnaps(backups []*store.Backup, keep int) (normal, keepAuto, pruneAuto []*store.Backup) {
	kept := 0
	for _, b := range backups {
		if !strings.HasPrefix(b.Label, "auto:") {
			normal = append(normal, b)
			continue
		}
		if b.Pinned { // "keep forever" — always kept, outside the quota
			keepAuto = append(keepAuto, b)
			continue
		}
		if kept < keep {
			keepAuto = append(keepAuto, b)
			kept++
		} else {
			pruneAuto = append(pruneAuto, b)
		}
	}
	return normal, keepAuto, pruneAuto
}

// SelectForRetention partitions backups (which MUST be newest-first) into the set
// to keep and the set to prune. When AutoKeep > 0 (F48) it FIRST carves automatic
// snapshots into their own newest-N budget so they can never evict scheduled
// generations; the remaining "normal" backups then flow through the GFS rules
// exactly as before. AutoKeep == 0 reproduces the prior behavior byte-for-byte.
//
// A backup that failed verification spends no budget: it is not a generation, and
// a run of them must not push out the ones that are. With nothing else to judge,
// every failed backup stays, because one of them may be the only data left.
func SelectForRetention(backups []*store.Backup, c RetentionConfig) (keep, prune []*store.Backup) {
	countable, failed := splitFailedVerification(backups)
	if len(countable) == 0 {
		return backups, nil
	}
	normal := countable
	var keepAuto, pruneAuto []*store.Backup
	if c.AutoKeep > 0 {
		normal, keepAuto, pruneAuto = partitionAutoSnaps(countable, c.AutoKeep)
	}
	keep, prune = selectGFS(normal, c)
	keep = append(keep, keepAuto...)
	prune = append(prune, pruneAuto...)
	for _, b := range failed {
		if keepsFailedBackup(b, backups[0], c) {
			keep = append(keep, b)
		} else {
			prune = append(prune, b)
		}
	}
	return keep, prune
}

func splitFailedVerification(backups []*store.Backup) (countable, failed []*store.Backup) {
	for _, b := range backups {
		if b.Verified == verifiedFailed {
			failed = append(failed, b)
		} else {
			countable = append(countable, b)
		}
	}
	return countable, failed
}

// keepsFailedBackup keeps what a rule protects and the evidence of the latest run:
// the newest backup stays whatever it is, and an older failure has no further use
// once something newer exists. Without a rule that governs it, nothing is pruned.
func keepsFailedBackup(b, newest *store.Backup, c RetentionConfig) bool {
	governed := c.gfsActive() || (c.AutoKeep > 0 && strings.HasPrefix(b.Label, "auto:"))
	return !governed || b.Pinned || b.ID == newest.ID
}

// selectGFS is the GFS selection over an already-auto-partitioned list. The newest
// backup is always kept; if no GFS rule is active, everything here is kept.
func selectGFS(backups []*store.Backup, c RetentionConfig) (keep, prune []*store.Backup) {
	if !c.gfsActive() || len(backups) == 0 {
		return backups, nil
	}
	keepSet := map[string]bool{backups[0].ID: true} // always keep the newest (safety floor)

	for i := 0; i < c.Generations && i < len(backups); i++ {
		keepSet[backups[i].ID] = true
	}
	keepBuckets(backups, keepSet, c.Daily, func(t time.Time) string { return t.Format("2006-01-02") })
	keepBuckets(backups, keepSet, c.Weekly, func(t time.Time) string {
		y, w := t.ISOWeek()
		return strconv.Itoa(y) + "-W" + strconv.Itoa(w)
	})
	keepBuckets(backups, keepSet, c.Monthly, func(t time.Time) string { return t.Format("2006-01") })
	keepBuckets(backups, keepSet, c.Yearly, func(t time.Time) string { return t.Format("2006") })

	for _, b := range backups {
		// A pinned backup ("keep forever", F2) is never pruned, regardless of policy.
		if keepSet[b.ID] || b.Pinned {
			keep = append(keep, b)
		} else {
			prune = append(prune, b)
		}
	}
	return keep, prune
}

// keepBuckets keeps the newest backup in each of the most recent n distinct
// buckets (backups are newest-first, so the first seen per bucket is newest).
func keepBuckets(backups []*store.Backup, keepSet map[string]bool, n int, keyFn func(time.Time) string) {
	if n <= 0 {
		return
	}
	seen := map[string]bool{}
	kept := 0
	for _, b := range backups {
		if kept >= n {
			return
		}
		k := keyFn(time.Unix(b.CreatedAt, 0).UTC())
		if seen[k] {
			continue
		}
		seen[k] = true
		keepSet[b.ID] = true
		kept++
	}
}

// ChainParentMap builds id→parent-id from each backup's manifest (F61), used to
// protect the ancestor chain of retained incremental backups from pruning and by
// the manual-delete chain guard (F63). Only backups with a recorded Parent
// contribute an entry. Prefer Store.BackupParentMap (a json_extract
// micro-query) when the rows aren't already in hand.
func ChainParentMap(list []*store.Backup) map[string]string {
	pm := map[string]string{}
	for _, b := range list {
		if b.ManifestJSON == "" {
			continue
		}
		var m Manifest
		if unmarshal(b.ManifestJSON, &m) == nil && m.Parent != "" {
			pm[b.ID] = m.Parent
		}
	}
	return pm
}

// RetentionHoldKey names the settings key that freezes pruning for one
// (node, target) after a suspect mass-change delta (F69 ransomware tripwire).
// Value format: "<reason>|<unix>". While present, no retention path prunes that
// target — so clean pre-event generations can't be aged out underneath an
// attack. Cleared by the operator via the tripwire-clear endpoint.
func RetentionHoldKey(nodeID, target string) string {
	return "retention.hold." + nodeID + "." + target
}

// retentionHoldPrefix is what every hold key of one node starts with.
func retentionHoldPrefix(nodeID string) string {
	return RetentionHoldKey(nodeID, "")
}

// HeldTargets names the targets of a node that are under a tripwire hold (F69).
func HeldTargets(st *store.Store, nodeID string) []string {
	prefix := retentionHoldPrefix(nodeID)
	keys, err := st.SettingKeysWithPrefix(prefix)
	if err != nil {
		return nil
	}
	targets := make([]string, 0, len(keys))
	for _, key := range keys {
		targets = append(targets, strings.TrimPrefix(key, prefix))
	}
	return targets
}

// retentionHeld reports whether a tripwire hold is active for the target (F69).
func (e *Engine) retentionHeld(nodeID, target string) bool {
	v, _ := e.Store.GetSetting(RetentionHoldKey(nodeID, target), "")
	return v != ""
}

// tripwireEnabled reports whether mass-change detection is on (F69, default on).
func (e *Engine) tripwireEnabled() bool {
	v, _ := e.Store.GetSetting("tripwire.enabled", "true")
	return v != "false"
}

// settingIntClamped reads an integer setting with a default and clamps it into
// [lo, hi] — the same bounds coerceSetting enforces at write time, re-applied on
// read so a hand-edited DB value can't produce a nonsense threshold.
func (e *Engine) settingIntClamped(key string, def, lo, hi int) int {
	v, _ := e.Store.GetSetting(key, "")
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// successfulByTarget groups successful backups by (node, target), preserving the
// newest-first order within each group.
func successfulByTarget(all []*store.Backup) map[[2]string][]*store.Backup {
	groups := map[[2]string][]*store.Backup{}
	for _, b := range all {
		if b.Status == "success" {
			k := [2]string{b.NodeID, b.TargetName}
			groups[k] = append(groups[k], b)
		}
	}
	return groups
}

// applyRetentionOverride folds one scope's retention override into cfg. The
// auto-snapshot budget is global, so it is carried across every override tier
// (F48) rather than being reset to zero by a GFS override.
func applyRetentionOverride(cfg RetentionConfig, ov store.PolicyOverride) RetentionConfig {
	return RetentionConfig{
		Generations: ov.Generations, Daily: ov.KeepDaily, Weekly: ov.KeepWeekly,
		Monthly: ov.KeepMonthly, Yearly: ov.KeepYearly, AutoKeep: cfg.AutoKeep,
	}
}

// EffectiveRetention resolves the retention config + autoprune flag for a node:
// the global settings, then the node's CLUSTER override (F104), then the node's
// own override (PLAN §4.13). A node whose cluster and self both lack an override
// inherits the global, exactly as before clusters existed.
func EffectiveRetention(st *store.Store, nodeID string) (RetentionConfig, bool) {
	cfg := RetentionFromSettings(st.GetSetting)
	ap, _ := st.GetSetting("retention.autoprune", "false")
	autoprune := ap == "true"
	if scope := st.NodeClusterScope(nodeID); scope != "" {
		if ov, ok := st.GetPolicyOverride(scope); ok && ov.OverrideRetention {
			cfg = applyRetentionOverride(cfg, ov)
			autoprune = ov.Autoprune
		}
	}
	if ov, ok := st.GetPolicyOverride(store.NodeScope(nodeID)); ok && ov.OverrideRetention {
		cfg = applyRetentionOverride(cfg, ov)
		autoprune = ov.Autoprune
	}
	return cfg, autoprune
}

// EffectiveRetentionFor resolves retention + autoprune for a specific container
// (node + target name): a container-scope override wins, then the node override,
// then the cluster override, then the global settings. This lets a "large"
// container (Jellyfin, Plex, …) keep fewer/shorter copies than the fleet default
// (PLAN §4.2 granular control).
func EffectiveRetentionFor(st *store.Store, nodeID, target string) (RetentionConfig, bool) {
	cfg, autoprune := EffectiveRetention(st, nodeID) // global, then cluster, then node
	if target != "" {
		if ov, ok := st.GetPolicyOverride(store.ContainerScope(nodeID, target)); ok && ov.OverrideRetention {
			cfg = applyRetentionOverride(cfg, ov)
			autoprune = ov.Autoprune
		}
	}
	return cfg, autoprune
}

// Why a backup the policy would prune stays. Part of the retention preview's API.
const (
	RetainedLastHealthy = "last_healthy"
	RetainedChain       = "chain"
	RetainedImmutable   = "immutable"
	RetainedUnreadable  = "unreadable"
)

var retainedExplanation = map[string]string{
	RetainedLastHealthy: "it is the newest backup with a verified chain, and nothing newer can replace it yet",
	RetainedChain:       "a newer incremental backup depends on it",
	RetainedImmutable:   "immutable copy still WORM-locked",
	RetainedUnreadable:  "its catalog row could not be read",
}

// RetentionPlan is what retention will do to one target. The preview shows it and
// the sweeps carry it out, so the two cannot differ.
type RetentionPlan struct {
	Keep    []*store.Backup
	Prune   []*store.Backup   // full rows: deleting needs their real locations
	Reasons map[string]string // backup id → why the policy's prune was overruled
	// Held: a tripwire hold freezes this target (F69), so nothing is pruned.
	Held bool
	// NoKnownGood: nothing here is verified along its whole chain. Reported rather
	// than inferred. Only worked out when there is something to prune.
	NoKnownGood bool
}

// PlanRetention decides one target's retention. list is its successful backups,
// newest first. On top of the policy's selection it keeps, in this order:
//
//   - the last healthy point, when the policy kept none: the newest verified backup
//     whose whole chain is verified;
//   - anything with a live WORM lock (§9.1);
//   - every chain ancestor of what is kept (F61), since a delta is unrestorable
//     without them. An error here must fail CLOSED: the caller skips the target
//     rather than risk orphaning a chain.
func (e *Engine) PlanRetention(nodeID, target string, list []*store.Backup, cfg RetentionConfig) (RetentionPlan, error) {
	plan := RetentionPlan{Keep: list, Reasons: map[string]string{}}
	if !cfg.Active() {
		return plan, nil
	}
	if e.retentionHeld(nodeID, target) {
		plan.Held = true
		return plan, nil
	}
	keep, prune := SelectForRetention(list, cfg)
	if len(prune) == 0 {
		return plan, nil
	}
	parentOf, err := e.Store.BackupParentMap(nodeID, target)
	if err != nil {
		return plan, err
	}
	retain := func(id, reason string) {
		for i, b := range prune {
			if b.ID == id {
				keep = append(keep, b)
				prune = append(prune[:i:i], prune[i+1:]...)
				plan.Reasons[id] = reason
				return
			}
		}
	}

	healthy := healthyChainMembers(list, parentOf)
	if floor := newestIn(list, healthy); floor == nil {
		plan.NoKnownGood = true
	} else if !anyIn(keep, healthy) {
		retain(floor.ID, RetainedLastHealthy)
	}

	var pruneFull []*store.Backup
	for _, cand := range append([]*store.Backup(nil), prune...) {
		full, gerr := e.Store.GetBackup(cand.ID)
		switch {
		case gerr != nil:
			retain(cand.ID, RetainedUnreadable)
		case e.hasLiveImmutable(full):
			retain(cand.ID, RetainedImmutable)
		default:
			pruneFull = append(pruneFull, full)
		}
	}

	keepIDs := make([]string, 0, len(keep))
	for _, kept := range keep {
		keepIDs = append(keepIDs, kept.ID)
	}
	ancestors := protectedAncestors(keepIDs, parentOf)
	for _, full := range pruneFull {
		if ancestors[full.ID] {
			retain(full.ID, RetainedChain)
			continue
		}
		plan.Prune = append(plan.Prune, full)
	}
	plan.Keep = keep
	return plan, nil
}

// healthyChainMembers is the set of backups that are verified and whose ancestors
// all are, as far back as a full. A verified delta on a failed or missing parent
// cannot be restored, whatever its own badge says.
func healthyChainMembers(list []*store.Backup, parentOf map[string]string) map[string]bool {
	verified := map[string]bool{}
	for _, b := range list {
		if b.Verified == verifiedOK {
			verified[b.ID] = true
		}
	}
	healthy := map[string]bool{}
	for _, b := range list {
		id, walked := b.ID, map[string]bool{}
		for verified[id] && !walked[id] {
			walked[id] = true
			parent, hasParent := parentOf[id]
			if !hasParent {
				healthy[b.ID] = true
				break
			}
			id = parent
		}
	}
	return healthy
}

func newestIn(newestFirst []*store.Backup, members map[string]bool) *store.Backup {
	for _, b := range newestFirst {
		if members[b.ID] {
			return b
		}
	}
	return nil
}

func anyIn(backups []*store.Backup, members map[string]bool) bool {
	return newestIn(backups, members) != nil
}

// PruneAll applies retention across every container (node+target), resolving the
// effective per-node policy for each group, and deletes pruned backups from all
// their locations. Returns count + bytes freed (PLAN §4.6/§4.13): the size of
// each pruned backup whose every copy was confirmed removed (DBK-030), not the
// catalog's total, because a locked or versioned copy can outlive its row.
func (e *Engine) PruneAll(ctx context.Context) (pruned int, freed int64) {
	// Slim projection (perf Fix 7): grouping + GFS selection read only scalars
	// (id/node/target/status/pinned/label/created/size). Manifest parents come
	// from a json_extract micro-query per PRUNING group, and each actual prune
	// candidate is re-fetched in full so the WORM check and artifact deletion
	// see its real locations — never an empty slim field.
	all, err := e.Store.ListBackupSummaries("", 100000)
	if err != nil {
		return 0, 0
	}
	for key, list := range successfulByTarget(all) {
		cfg, _ := EffectiveRetentionFor(e.Store, key[0], key[1]) // key = [node id, target name]
		n, bytes := e.pruneTarget(ctx, key[0], key[1], list, cfg)
		pruned += n
		freed += bytes
	}
	return pruned, freed
}

// pruneTarget carries out one target's retention plan. list is its successful
// backups, newest first. Every prune path ends here, so none can decide differently.
func (e *Engine) pruneTarget(ctx context.Context, nodeID, target string, list []*store.Backup, cfg RetentionConfig) (pruned int, freed int64) {
	plan, err := e.PlanRetention(nodeID, target, list, cfg)
	if err != nil {
		e.logf("", "WARN", "Retention: could not read chain parents for %s — skipping its prune this cycle: %v", target, err)
		return 0, 0
	}
	if plan.Held {
		e.logf("", "WARN", "Retention: hold active for %s (possible mass-change event) — skipping its prune until cleared", target)
		return 0, 0
	}
	if plan.NoKnownGood {
		e.logf("", "WARN", "Retention: %s has no verified backup with a verified chain, so nothing here is known to restore", target)
	}
	for id, reason := range plan.Reasons {
		e.logf(id, "INFO", "Retention: keeping %s — %s", target, retainedExplanation[reason])
	}
	for _, old := range plan.Prune {
		e.logf(old.ID, "INFO", "Retention: pruning old backup of %s (GFS policy)", target)
		deletion, derr := e.DeleteBackup(ctx, old)
		if derr != nil {
			e.logf(old.ID, "ERR", "Retention prune failed: %v", derr)
			continue
		}
		if !deletion.Removed {
			e.logf(old.ID, "INFO", "Retention: a copy of this backup of %s could not be removed yet, so it stays listed as \"deletion incomplete\" until it can", target)
			continue
		}
		pruned++
		if !Reclaimed(deletion.Copies) {
			e.logf(old.ID, "INFO", "Retention: this backup of %s left the catalog, but not all of its storage is confirmed freed, so its size is not counted as freed", target)
			continue
		}
		freed += old.SizeBytes
	}
	return pruned, freed
}
