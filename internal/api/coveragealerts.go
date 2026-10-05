package api

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"dockback/internal/notify"
)

// Coverage alerts. In the 2026-10-04 recovery seven stacks had no backup, others
// were weeks old, and nothing had said so. The hourly alert check now says when
// a backup stops being recent and when a compose project appears that no
// schedule covers, and the daily digest lists both. All of it comes from the
// cached inventory and the catalog, never from Docker.

const (
	// coverageStaleNotifiedKey remembers which stale units were already
	// announced, so each is announced once and again only after it recovers.
	coverageStaleNotifiedKey = "coverage.stale_notified"
	// coverageKnownProjectsKey remembers every compose project seen, so only a
	// project that is new gets the "no schedule" notice.
	coverageKnownProjectsKey = "coverage.known_projects"
	// coverageListCap is how many names one message lists before "and N more".
	coverageListCap = 8
)

// coverageUnit is one thing a coverage message names: a compose project, or a
// standalone container.
type coverageUnit struct {
	Key        string // node/project or node/container
	Label      string // "nextcloud on NUC"
	LastBackup int64  // the oldest last backup among its members, 0 for none
}

// coverageUnits groups one bucket of every node's rows by project, so a
// five-service stack is one name. Sorted by key. Pure.
func coverageUnits(cov coverageResp, bucket func(coverageNode) []coverageContainer) []coverageUnit {
	byKey := map[string]*coverageUnit{}
	for _, n := range cov.Nodes {
		for _, c := range bucket(n) {
			name := c.Name
			if c.Stack != "" {
				name = c.Stack
			}
			key := n.NodeID + "/" + name
			unit, seen := byKey[key]
			if !seen {
				unit = &coverageUnit{Key: key, Label: name + " on " + n.NodeName, LastBackup: c.LastBackupAt}
				byKey[key] = unit
			}
			unit.LastBackup = min(unit.LastBackup, c.LastBackupAt)
		}
	}
	units := make([]coverageUnit, 0, len(byKey))
	for _, unit := range byKey {
		units = append(units, *unit)
	}
	slices.SortFunc(units, func(a, b coverageUnit) int { return strings.Compare(a.Key, b.Key) })
	return units
}

func staleBucket(n coverageNode) []coverageContainer { return n.Stale }

func neverBucket(n coverageNode) []coverageContainer { return n.Unprotected }

// staleLabel names a stale unit with the age of its backup.
func staleLabel(unit coverageUnit, now int64) string {
	return fmt.Sprintf("%s (last backed up %s)", unit.Label, digestAgo(now-unit.LastBackup))
}

// listWithCap joins names, stopping at coverageListCap with a count of the
// rest. Pure.
func listWithCap(names []string) string {
	if len(names) <= coverageListCap {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:coverageListCap], ", "), len(names)-coverageListCap)
}

// coverageDigestLine is the digest's coverage sentence. Empty when nothing is
// running anywhere, so a fresh install's digest is unchanged. Pure.
func coverageDigestLine(cov coverageResp, now int64) string {
	if cov.RunningTotal == 0 {
		return ""
	}
	stale := coverageUnits(cov, staleBucket)
	never := coverageUnits(cov, neverBucket)
	if len(stale) == 0 && len(never) == 0 {
		return fmt.Sprintf(" Coverage: all %d running container%s have a recent backup.", cov.RunningTotal, plural(cov.RunningTotal))
	}
	var b strings.Builder
	b.WriteString(" Coverage:")
	if len(stale) > 0 {
		labels := make([]string, 0, len(stale))
		for _, unit := range stale {
			labels = append(labels, staleLabel(unit, now))
		}
		fmt.Fprintf(&b, " stale backup — %s.", listWithCap(labels))
	}
	if len(never) > 0 {
		labels := make([]string, 0, len(never))
		for _, unit := range never {
			labels = append(labels, unit.Label)
		}
		fmt.Fprintf(&b, " never backed up — %s.", listWithCap(labels))
	}
	return b.String()
}

// offMachineDigestLine warns when every copy is on this machine. Silent until
// there is a backup to lose. Pure.
func offMachineDigestLine(cov coverageResp) string {
	backedUp := slices.ContainsFunc(cov.Nodes, func(n coverageNode) bool { return n.LastBackupAt > 0 })
	switch {
	case !backedUp:
		return ""
	case cov.OffMachineDestinations == 0:
		return " Off-site: NONE — every backup copy is on this machine, so one disk failure takes the backups with it."
	case cov.AppOffMachineDestinations == 0:
		return " Off-site: DockBack's own backup has no destination off this machine."
	}
	return ""
}

// checkCoverageAlerts runs on the hourly alert tick.
func (s *Server) checkCoverageAlerts(now time.Time) {
	cov, err := s.computeCoverage(now)
	if err != nil {
		return
	}
	s.alertNewlyStale(cov, now)
	s.alertNewUnscheduledProjects(now)
}

// alertNewlyStale announces the units that went stale since the last check, in
// one message. Right after an upgrade that is every stale unit at once, which
// is the message the 2026-10-04 recovery needed.
func (s *Server) alertNewlyStale(cov coverageResp, now time.Time) {
	announced, _ := s.loadKeySet(coverageStaleNotifiedKey)
	units := coverageUnits(cov, staleBucket)
	current := make([]string, 0, len(units))
	var fresh []string
	for _, unit := range units {
		current = append(current, unit.Key)
		if !announced[unit.Key] {
			fresh = append(fresh, staleLabel(unit, now.Unix()))
		}
	}
	s.saveKeySet(coverageStaleNotifiedKey, current)
	if len(fresh) == 0 {
		return
	}
	s.notify(notify.KindCoverageStale,
		fmt.Sprintf("%d backup%s no longer recent", len(fresh), plural(len(fresh))),
		fmt.Sprintf("Older than twice their schedule's interval (8 days with no schedule): %s. Run a backup, or check why the schedule did not.", listWithCap(fresh)))
}

// alertNewUnscheduledProjects announces compose projects seen for the first
// time that no enabled schedule covers. The first check only records what is
// there, so turning this on never floods. Projects are never forgotten, so a
// node that drops offline and returns does not make its stacks look new.
func (s *Server) alertNewUnscheduledProjects(now time.Time) {
	known, existed := s.loadKeySet(coverageKnownProjectsKey)
	nodes, err := s.store.ListNodes()
	if err != nil {
		return
	}
	cover := s.scheduleCoverage(now)
	scheduled := map[string]bool{}
	labelOf := map[string]string{}
	var unscheduled []string
	for _, n := range nodes {
		st := s.getStat(n.ID)
		if st == nil {
			continue
		}
		ignored := s.ignoredOn(n.ID)
		for _, c := range st.Containers {
			if c == nil || c.Stack == "" || isTestClone(c) || ignored.has(c) {
				continue
			}
			key := n.ID + "/" + c.Stack
			if _, covered := cover.interval(n.ID, c); covered {
				scheduled[key] = true
			}
			if known[key] {
				continue
			}
			known[key] = true
			labelOf[key] = c.Stack + " on " + n.Name
			unscheduled = append(unscheduled, key)
		}
	}
	s.saveKeySet(coverageKnownProjectsKey, sortedKeys(known))
	if !existed {
		return
	}
	labels := make([]string, 0, len(unscheduled))
	for _, key := range unscheduled {
		if !scheduled[key] {
			labels = append(labels, labelOf[key])
		}
	}
	if len(labels) == 0 {
		return
	}
	s.notify(notify.KindProjectUnscheduled,
		fmt.Sprintf("%d new stack%s with no backup schedule", len(labels), plural(len(labels))),
		fmt.Sprintf("New compose project%s that no schedule covers: %s. Protect %s from its node's page.", plural(len(labels)), listWithCap(labels), pronounFor(len(labels))))
}

func pronounFor(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// loadKeySet reads a remembered set, and whether one was stored at all.
func (s *Server) loadKeySet(settingKey string) (map[string]bool, bool) {
	raw, err := s.store.GetSetting(settingKey, "")
	set := map[string]bool{}
	if err != nil || raw == "" {
		return set, false
	}
	var keys []string
	if json.Unmarshal([]byte(raw), &keys) != nil {
		return set, false
	}
	for _, k := range keys {
		set[k] = true
	}
	return set, true
}

func (s *Server) saveKeySet(settingKey string, keys []string) {
	if keys == nil {
		keys = []string{}
	}
	if raw, err := json.Marshal(keys); err == nil {
		_ = s.store.SetSetting(settingKey, string(raw))
	}
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
