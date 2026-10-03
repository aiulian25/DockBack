package api

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// Declarative label-driven backup policy (F19). A container (or compose service)
// opts into protection via `dockback.*` labels, so a GitOps homelab defines backup
// intent in its compose file and DockBack applies it automatically on discovery —
// no clicking. Labels are authoritative for the fields they set ("labels win"); the
// UI shows a read-only "Managed by labels" banner and disables those controls to
// avoid drift.
//
// Supported labels:
//
//	dockback.enable=true|false
//	dockback.schedule=<named-schedule-id-or-cron>
//	dockback.mounts.exclude=/path1,/path2
//	dockback.retention=daily:7,weekly:4,monthly:6,yearly:1[,generations:N][,autoprune:true]
//	dockback.pause-mode=pause|stop|none

const dockbackLabelPrefix = "dockback."

// dockbackPolicy is the parsed intent from a container's dockback.* labels.
type dockbackPolicy struct {
	Enable        *bool                 `json:"enable,omitempty"`
	Schedule      string                `json:"schedule,omitempty"`
	ExcludeMounts []string              `json:"exclude_mounts,omitempty"`
	Retention     *store.PolicyOverride `json:"retention,omitempty"`
	PauseMode     string                `json:"pause_mode,omitempty"`
}

// parseDockbackLabels parses the dockback.* label set. ok=false when NO dockback.
// key is present. A garbage/unrecognised value for a key is ignored (that field
// stays unset) rather than failing the whole parse — a typo in a compose file must
// never break discovery. Pure + unit-tested.
func parseDockbackLabels(labels map[string]string) (dockbackPolicy, bool) {
	var p dockbackPolicy
	seen := false
	for k, v := range labels {
		if !strings.HasPrefix(k, dockbackLabelPrefix) {
			continue
		}
		seen = true
		v = strings.TrimSpace(v)
		switch strings.TrimPrefix(k, dockbackLabelPrefix) {
		case "enable":
			if b, err := strconv.ParseBool(v); err == nil {
				p.Enable = &b
			}
		case "schedule":
			p.Schedule = v
		case "pause-mode":
			switch v {
			case backup.PauseNone, backup.PausePause, backup.PauseStop:
				p.PauseMode = v
			}
		case "mounts.exclude":
			p.ExcludeMounts = splitCleanList(v)
		case "retention":
			if ov, rok := parseRetentionSpec(v); rok {
				p.Retention = &ov
			}
		}
	}
	if !seen {
		return dockbackPolicy{}, false
	}
	return p, true
}

// splitCleanList splits a comma-separated label value into trimmed, non-empty items.
func splitCleanList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseRetentionSpec parses "daily:7,weekly:4,monthly:6,yearly:1[,generations:N]
// [,autoprune:true]" into a retention PolicyOverride. Negative/garbage counts are
// ignored; the same non-negative clamp as the UI policy path (policy.go) applies.
// ok=false when nothing usable was parsed.
func parseRetentionSpec(v string) (store.PolicyOverride, bool) {
	ov := store.PolicyOverride{Destinations: []string{}}
	any := false
	for _, part := range strings.Split(v, ",") {
		key, val, has := strings.Cut(strings.TrimSpace(part), ":")
		if !has {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		if key == "autoprune" {
			if b, err := strconv.ParseBool(val); err == nil {
				ov.Autoprune = b
				any = true
			}
			continue
		}
		n, err := strconv.Atoi(val)
		if err != nil || n < 0 { // clamp: ignore garbage / negatives (mirrors policy.go)
			continue
		}
		switch key {
		case "generations", "gen":
			ov.Generations, any = n, true
		case "daily":
			ov.KeepDaily, any = n, true
		case "weekly":
			ov.KeepWeekly, any = n, true
		case "monthly":
			ov.KeepMonthly, any = n, true
		case "yearly":
			ov.KeepYearly, any = n, true
		}
	}
	if !any {
		return ov, false
	}
	ov.OverrideRetention = true
	return ov, true
}

// labelMarkerKey stores the last-applied label policy JSON for a container, so the
// refresh loop can (a) apply only on a real change — no churn on every 30s tick —
// and (b) revert exactly the fields the labels set when they're removed.
func labelMarkerKey(nodeID, name string) string { return "labelpolicy:" + nodeID + "\x00" + name }

// applyLabelPolicies reconciles every container's dockback.* labels into stored
// policy/schedule state after an inventory refresh (F19). Idempotent: it writes
// only when the derived policy differs from what was last applied. Cheap in steady
// state — unlabeled containers short-circuit in parseDockbackLabels, and a labeled
// one only reads one marker setting unless its labels actually changed.
func (s *Server) applyLabelPolicies(nodeID string, cs []*dockercli.Container) {
	for _, c := range cs {
		if c == nil || c.Name == "" {
			continue
		}
		parsed, ok := parseDockbackLabels(c.Labels)
		key := labelMarkerKey(nodeID, c.Name)
		stored, _ := s.store.GetSetting(key, "")
		if ok {
			desired, _ := json.Marshal(parsed)
			if stored == string(desired) {
				continue // unchanged — nothing to do
			}
			s.applyOneLabelPolicy(nodeID, c, parsed)
			_ = s.store.SetSetting(key, string(desired))
			s.logSink("labels", "INFO", fmt.Sprintf("Applied dockback.* labels for %q on node %s", c.Name, nodeID))
			continue
		}
		if stored != "" {
			// Labels were removed — undo exactly what they had set, then drop the marker.
			var prev dockbackPolicy
			_ = json.Unmarshal([]byte(stored), &prev)
			s.revertLabelPolicy(nodeID, c.Name, prev)
			_ = s.store.DeleteSetting(key)
			s.logSink("labels", "INFO", fmt.Sprintf("Cleared label-managed policy for %q on node %s (dockback.* labels removed)", c.Name, nodeID))
		}
	}
}

// applyOneLabelPolicy writes the stored state a container's labels describe. It
// only touches the fields the labels actually set, so an unrelated manual override
// (e.g. backup frequency, which has no label) is preserved.
func (s *Server) applyOneLabelPolicy(nodeID string, c *dockercli.Container, p dockbackPolicy) {
	name := c.Name
	scope := store.ContainerScope(nodeID, name)

	// Retention override (labels own the retention fields; keep any frequency override).
	cur, _ := s.store.GetPolicyOverride(scope)
	if p.Retention != nil {
		r := p.Retention
		cur.OverrideRetention = true
		cur.Generations, cur.KeepDaily, cur.KeepWeekly, cur.KeepMonthly, cur.KeepYearly, cur.Autoprune =
			r.Generations, r.KeepDaily, r.KeepWeekly, r.KeepMonthly, r.KeepYearly, r.Autoprune
		_ = s.store.SetPolicyOverride(scope, cur)
	}

	// Pause mode.
	if p.PauseMode != "" {
		_ = s.store.SetSetting(backup.PauseModeKey(nodeID, name), p.PauseMode)
	}

	// Mount exclusions → an explicit selection (all candidates minus the excluded).
	if len(p.ExcludeMounts) > 0 {
		s.engine.SetLabelMountExclusion(nodeID, name, c.Mounts, p.ExcludeMounts)
	}

	// Enable/disable schedule coverage.
	if p.Enable != nil {
		if *p.Enable {
			s.labelEnsureScheduled(nodeID, name, p.Schedule)
		} else {
			s.removeFromAllSchedules(nodeID, name)
		}
	}
}

// revertLabelPolicy undoes the fields a container's (now-removed) labels had set,
// per the last-applied policy — so removing a label cleanly returns the container
// to inheriting defaults, without disturbing fields the labels never touched.
func (s *Server) revertLabelPolicy(nodeID, name string, prev dockbackPolicy) {
	scope := store.ContainerScope(nodeID, name)
	if prev.Retention != nil {
		if cur, ok := s.store.GetPolicyOverride(scope); ok {
			cur.OverrideRetention = false
			cur.Generations, cur.KeepDaily, cur.KeepWeekly, cur.KeepMonthly, cur.KeepYearly, cur.Autoprune = 0, 0, 0, 0, 0, false
			_ = s.store.SetPolicyOverride(scope, cur) // deletes the row if nothing else is set
		}
	}
	if prev.PauseMode != "" {
		_ = s.store.DeleteSetting(backup.PauseModeKey(nodeID, name))
	}
	if len(prev.ExcludeMounts) > 0 {
		s.engine.ClearMountSelection(nodeID, name)
	}
	if prev.Enable != nil && *prev.Enable {
		s.removeFromAllSchedules(nodeID, name)
	}
}

// labelEnsureScheduled makes sure a container is covered by a schedule: the named
// schedule when `sched` is an existing schedule id, otherwise DockBack's default
// coverage (append to an enabled schedule, or create one). A cron-string value is
// treated as "just protect it" — creating ad-hoc cron schedules from labels is
// deliberately out of scope to keep the schedule list predictable.
func (s *Server) labelEnsureScheduled(nodeID, name, sched string) {
	if sched != "" {
		if _, err := s.store.GetSchedule(sched); err == nil {
			s.addTargetToSchedule(sched, nodeID, name)
			return
		}
	}
	s.scheduleProtect(nodeID, name) // idempotent: no-op when already covered
}

// addTargetToSchedule adds (node,name) to a named schedule if not already present.
func (s *Server) addTargetToSchedule(scheduleID, nodeID, name string) {
	row, err := s.store.GetSchedule(scheduleID)
	if err != nil {
		return
	}
	sc := scheduleFromRow(row)
	for _, t := range sc.Targets {
		if t.NodeID == nodeID && (t.ContainerName == name || t.ContainerID == name) {
			return // already covered
		}
	}
	sc.Targets = append(sc.Targets, ScheduleTarget{NodeID: nodeID, ContainerName: name})
	if newRow, e := sc.toRow(); e == nil {
		newRow.LastRun = row.LastRun
		_ = s.store.UpsertSchedule(newRow)
	}
}

// removeFromAllSchedules drops (node,name) from every schedule it appears in — used
// when dockback.enable=false or the labels are removed (F19).
func (s *Server) removeFromAllSchedules(nodeID, name string) {
	rows, err := s.store.ListSchedules()
	if err != nil {
		return
	}
	drop := []ScheduleTarget{{NodeID: nodeID, ContainerName: name}}
	for _, row := range rows {
		s.removeScheduleTargets(row.ID, drop)
	}
}
