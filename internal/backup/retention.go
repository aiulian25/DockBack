package backup

import (
	"context"
	"strconv"
	"strings"
	"time"

	"dockback/internal/store"
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
func SelectForRetention(backups []*store.Backup, c RetentionConfig) (keep, prune []*store.Backup) {
	normal := backups
	var keepAuto, pruneAuto []*store.Backup
	if c.AutoKeep > 0 {
		normal, keepAuto, pruneAuto = partitionAutoSnaps(backups, c.AutoKeep)
	}
	keep, prune = selectGFS(normal, c)
	keep = append(keep, keepAuto...)
	prune = append(prune, pruneAuto...)
	return keep, prune
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

// chainProtected returns the backup ids retention must keep even when GFS would
// prune them: every chain ancestor of a KEPT backup (F61). A delta is
// unrestorable without its whole ancestor chain, so protecting the keep-set's
// ancestors is the WORM-retain pattern applied to chain integrity. Callers fail
// CLOSED on an error — skip the target's prune rather than risk orphaning a chain.
func (e *Engine) chainProtected(nodeID, target string, keep []*store.Backup) (map[string]bool, error) {
	parentOf, err := e.Store.BackupParentMap(nodeID, target)
	if err != nil {
		return nil, err
	}
	keepIDs := make([]string, 0, len(keep))
	for _, kept := range keep {
		keepIDs = append(keepIDs, kept.ID)
	}
	return protectedAncestors(keepIDs, parentOf), nil
}

// PruneAll applies retention across every container (node+target), resolving the
// effective per-node policy for each group, and deletes pruned backups from all
// their locations. Returns count + bytes freed (PLAN §4.6/§4.13).
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
		// F69: a tripwire hold freezes this target's pruning until the operator
		// reviews — never age out the last clean generations mid-incident.
		if e.retentionHeld(key[0], key[1]) {
			e.logf("", "WARN", "Retention: hold active for %s (possible mass-change event) — skipping its prune until cleared", key[1])
			continue
		}
		cfg, _ := EffectiveRetentionFor(e.Store, key[0], key[1]) // key = [node id, target name]
		if !cfg.Active() {
			continue
		}
		keep, toPrune := SelectForRetention(list, cfg)
		if len(toPrune) == 0 {
			continue // nothing to prune — skip the chain-map work for this group
		}
		protected, perr := e.chainProtected(key[0], key[1], keep)
		if perr != nil {
			e.logf("", "WARN", "Retention: could not read chain parents for %s — skipping its prune this cycle: %v", key[1], perr)
			continue
		}
		for _, cand := range toPrune {
			if protected[cand.ID] { // a newer incremental backup still depends on this one
				e.logf(cand.ID, "INFO", "Retention: keeping %s — a newer incremental backup depends on it", cand.TargetName)
				continue
			}
			// Full row for the destructive path (locations/storage key).
			old, gerr := e.Store.GetBackup(cand.ID)
			if gerr != nil {
				e.logf(cand.ID, "ERR", "Retention prune skipped — could not load backup: %v", gerr)
				continue
			}
			if e.hasLiveImmutable(old) { // WORM copy still locked — retain (§9.1)
				e.logf(old.ID, "INFO", "Retention: keeping %s — immutable copy still WORM-locked", old.TargetName)
				continue
			}
			e.logf(old.ID, "INFO", "Retention: pruning old backup of %s (GFS policy)", old.TargetName)
			e.DeleteArtifacts(ctx, old)
			if derr := e.Store.DeleteBackup(old.ID); derr != nil {
				e.logf(old.ID, "ERR", "Retention prune failed: %v", derr)
				continue
			}
			pruned++
			freed += old.SizeBytes
		}
	}
	return pruned, freed
}
