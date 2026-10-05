package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/robfig/cron/v3"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// Named backup schedules (F6): several independent schedules, each with its own
// frequency and targets, all evaluated on the scheduler tick. Retention and
// destinations remain global (Policy); a schedule only picks WHAT and WHEN.

// scheduleView is a schedule enriched for the client with its persisted last-run
// and freshly-computed next-run times (flattened alongside the friendly fields).
type scheduleView struct {
	Schedule
	LastRun int64 `json:"last_run"`
	NextRun int64 `json:"next_run"`
}

// toRow converts a friendly schedule into its persisted row form, validating the
// cron spec up front. LastRun/NextRun are set by the caller (preserved on update).
func (sc Schedule) toRow() (*store.Schedule, error) {
	spec, err := sc.cronSpec()
	if err != nil {
		return nil, err
	}
	if _, err := cron.ParseStandard(spec); err != nil {
		return nil, fmt.Errorf("invalid cron expression: %w", err)
	}
	if err := sc.validateTargets(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(sc)
	if err != nil {
		return nil, err
	}
	return &store.Schedule{ID: sc.ID, OptionsJSON: string(b), Cron: spec, Enabled: sc.Enabled}, nil
}

// validateTargets enforces the F47 target rules: a target is EITHER a compose stack
// or a single container (never both), and app-consistent capture applies only to a
// stack. Pure, so it's unit-testable and runs on every create/update via toRow.
func (sc Schedule) validateTargets() error {
	for _, t := range sc.Targets {
		if t.Stack != "" && (t.ContainerID != "" || t.ContainerName != "") {
			return fmt.Errorf("a schedule target is either a stack or a container, not both")
		}
		if t.Consistent && t.Stack == "" {
			return fmt.Errorf("app-consistent capture only applies to a stack target")
		}
	}
	return nil
}

// scheduleFromRow rebuilds a friendly schedule from a stored row. The row's id and
// enabled column are authoritative (they drive scheduler queries).
func scheduleFromRow(row *store.Schedule) Schedule {
	var sc Schedule
	_ = json.Unmarshal([]byte(row.OptionsJSON), &sc)
	sc.ID = row.ID
	sc.Enabled = row.Enabled
	if sc.Targets == nil {
		sc.Targets = []ScheduleTarget{}
	}
	return sc
}

// enrichTargetNames fills in the current container name for legacy id-only targets
// (from the inventory cache) so the UI matches by name and re-saves it.
func (s *Server) enrichTargetNames(sc *Schedule) {
	for i, t := range sc.Targets {
		if t.ContainerName == "" && t.ContainerID != "" {
			if n := s.cachedContainerName(t.NodeID, t.ContainerID); n != "" {
				sc.Targets[i].ContainerName = n
			}
		}
	}
}

// viewFor packages a schedule row for the client with a freshly-computed next run.
func (s *Server) viewFor(row *store.Schedule) scheduleView {
	sc := scheduleFromRow(row)
	s.enrichTargetNames(&sc)
	var next int64
	if sc.Enabled {
		next = unixOrZero(sc.nextRun(time.Now()))
	}
	return scheduleView{Schedule: sc, LastRun: row.LastRun, NextRun: next}
}

// handleListSchedules returns all named schedules with last/next run times.
func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListSchedules()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []scheduleView{}
	for _, row := range rows {
		out = append(out, s.viewFor(row))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCreateSchedule creates a new named schedule. A fresh schedule starts with
// last_run=0 so the next tick baselines it (fires at the NEXT window, not now).
func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	var sc Schedule
	if err := readJSON(r, &sc); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	sc.ID = randToken()[:12] // server-assigned; any client id is ignored
	if sc.Name == "" {
		sc.Name = "Schedule"
	}
	row, err := sc.toRow()
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.UpsertSchedule(row); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "schedule.create", sc.Name, fmt.Sprintf("kind=%s targets=%d", sc.Kind, len(sc.Targets)))
	writeJSON(w, http.StatusOK, s.viewFor(row))
}

// handleUpdateSchedule edits an existing schedule in place, preserving its last_run
// baseline (a time change takes effect from the next window).
func (s *Server) handleUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.store.GetSchedule(id)
	if err != nil {
		errJSON(w, http.StatusNotFound, "schedule not found")
		return
	}
	var sc Schedule
	if err := readJSON(r, &sc); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	sc.ID = id
	if sc.Name == "" {
		sc.Name = "Schedule"
	}
	row, err := sc.toRow()
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	row.LastRun = existing.LastRun // keep the baseline across edits
	if err := s.store.UpsertSchedule(row); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "schedule.update", sc.Name, fmt.Sprintf("enabled=%v kind=%s targets=%d", sc.Enabled, sc.Kind, len(sc.Targets)))
	writeJSON(w, http.StatusOK, s.viewFor(row))
}

// handleDeleteSchedule removes a named schedule.
func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.store.GetSchedule(id)
	if err != nil {
		errJSON(w, http.StatusNotFound, "schedule not found")
		return
	}
	if err := s.store.DeleteSchedule(id); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "schedule.delete", scheduleFromRow(existing).Name, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleRunScheduleNow backs up one named schedule's targets immediately,
// regardless of whether it is enabled.
func (s *Server) handleRunScheduleNow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	row, err := s.store.GetSchedule(id)
	if err != nil {
		errJSON(w, http.StatusNotFound, "schedule not found")
		return
	}
	sc := scheduleFromRow(row)
	if len(sc.Targets) == 0 {
		errJSON(w, http.StatusBadRequest, "no targets selected — pick at least one node or container")
		return
	}
	_ = s.store.Audit(userFrom(r), "schedule.runnow", sc.Name, fmt.Sprintf("%d target(s)", len(sc.Targets)))
	go s.runScheduleTargets(sc)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

// scheduleCover is what the enabled schedules cover, each entry holding the
// shortest interval of the schedules that cover it, so coverage can judge how
// recent a backup has to be (F6). Keys are nodeID for whole-node targets,
// nodeID+"\x00"+project for stacks and nodeID+"\x00"+name for one container.
// Legacy id-only targets cannot be name-matched; such a container counts as
// covered once it has a backup, like any other.
type scheduleCover struct {
	wholeNode        map[string]time.Duration // running containers
	wholeNodeStopped map[string]time.Duration // stopped ones too: schedules with IncludeStopped
	stacks           map[string]time.Duration // every member, running or not
	named            map[string]time.Duration
}

// interval is the shortest interval of the schedules covering the container,
// and whether any does.
//
// A stack target covers its own project and nothing else. It once read as a
// whole-node target — its container fields are empty — so one scheduled stack
// marked every container on the node as covered, and "Protect" added nothing.
func (cover scheduleCover) interval(nodeID string, c *dockercli.Container) (time.Duration, bool) {
	var intervals []time.Duration
	if d, ok := cover.named[nodeID+"\x00"+c.Name]; ok {
		intervals = append(intervals, d)
	}
	if d, ok := cover.stacks[nodeID+"\x00"+c.Stack]; ok && c.Stack != "" {
		intervals = append(intervals, d)
	}
	wholeNode := cover.wholeNode
	if c.State != "running" {
		wholeNode = cover.wholeNodeStopped
	}
	if d, ok := wholeNode[nodeID]; ok {
		intervals = append(intervals, d)
	}
	if len(intervals) == 0 {
		return 0, false
	}
	return slices.Min(intervals), true
}

// scheduleCoverage collects what every ENABLED named schedule covers, so "is
// this container scheduled?" reflects every schedule, not just one (F6).
func (s *Server) scheduleCoverage(now time.Time) scheduleCover {
	cover := scheduleCover{
		wholeNode: map[string]time.Duration{}, wholeNodeStopped: map[string]time.Duration{},
		stacks: map[string]time.Duration{}, named: map[string]time.Duration{},
	}
	rows, err := s.store.ListSchedules()
	if err != nil {
		return cover
	}
	for _, row := range rows {
		if !row.Enabled {
			continue
		}
		sc := scheduleFromRow(row)
		interval := sc.interval(now)
		for _, t := range sc.Targets {
			switch {
			case t.Stack != "":
				keepShortest(cover.stacks, t.NodeID+"\x00"+t.Stack, interval)
			case t.ContainerName != "":
				keepShortest(cover.named, t.NodeID+"\x00"+t.ContainerName, interval)
			case t.ContainerID == "":
				keepShortest(cover.wholeNode, t.NodeID, interval)
				if sc.IncludeStopped {
					keepShortest(cover.wholeNodeStopped, t.NodeID, interval)
				}
			}
		}
	}
	return cover
}

// How a schedule reaches a container, as the container's page says it.
const (
	coveredByName  = "container"
	coveredByStack = "stack"
	coveredByNode  = "node"
)

// containerSchedule is a schedule that backs one container up, and how it
// reaches it.
type containerSchedule struct {
	scheduleView
	Covers string `json:"covers"`
}

// containerSchedules lists every schedule, on or off, that backs this
// container up — what its page shows as the schedule set for it. A container
// the inventory has not seen yet is matched by name alone.
func (s *Server) containerSchedules(nodeID, cid, name string) []containerSchedule {
	c := s.cachedContainer(nodeID, cid)
	if c == nil {
		c = &dockercli.Container{ID: cid, Name: name}
	}
	out := []containerSchedule{}
	rows, err := s.store.ListSchedules()
	if err != nil {
		return out
	}
	ignored := s.ignoredOn(nodeID)
	for _, row := range rows {
		if covers := scheduleCovers(scheduleFromRow(row), nodeID, c, ignored); covers != "" {
			out = append(out, containerSchedule{scheduleView: s.viewFor(row), Covers: covers})
		}
	}
	return out
}

// scheduleCovers says how a schedule backs container c on nodeID up — by its
// name, through its compose project, or with the rest of the server — or ""
// when it does not. The most specific target wins. Pure.
func scheduleCovers(sc Schedule, nodeID string, c *dockercli.Container, ignored ignoreSet) string {
	found := map[string]bool{}
	for _, t := range sc.Targets {
		found[targetCovers(t, sc.IncludeStopped, nodeID, c, ignored)] = true
	}
	for _, covers := range []string{coveredByName, coveredByStack, coveredByNode} {
		if found[covers] {
			return covers
		}
	}
	return ""
}

// targetCovers applies the scheduler's own matching (runScheduleTargets) to one
// target: a stack target takes its project's members, a named target its
// container, a legacy target its container id, and a whole-server target what
// wholeServerTakes takes. Pure.
func targetCovers(t ScheduleTarget, includeStopped bool, nodeID string, c *dockercli.Container, ignored ignoreSet) string {
	if t.NodeID != nodeID {
		return ""
	}
	if t.Stack != "" {
		return coveredWhen(c.Stack == t.Stack, coveredByStack)
	}
	if t.ContainerName != "" {
		return coveredWhen(c.Name == t.ContainerName, coveredByName)
	}
	if t.ContainerID != "" {
		return coveredWhen(c.ID == t.ContainerID, coveredByName)
	}
	return coveredWhen(wholeServerTakes(c, includeStopped, ignored), coveredByNode)
}

// coveredWhen is how when it matched, else "".
func coveredWhen(matched bool, how string) string {
	if !matched {
		return ""
	}
	return how
}

// stackSchedule is a schedule that backs a compose project up, and how it
// reaches it.
type stackSchedule struct {
	scheduleView
	Covers string `json:"covers"`
	// Consistent marks a stack target that snapshots every service in one
	// quiesce window.
	Consistent bool `json:"consistent,omitempty"`
	// Services names the members a schedule backs up by name.
	Services []string `json:"services,omitempty"`
	// Own marks the stack's own schedule, the one its page edits.
	Own bool `json:"own"`
}

// stackSchedules lists every schedule, on or off, that backs this compose
// project up, its own schedule marked.
func (s *Server) stackSchedules(nodeID, project string) []stackSchedule {
	out := []stackSchedule{}
	rows, err := s.store.ListSchedules()
	if err != nil {
		return out
	}
	members := s.cachedStackMembers(nodeID, project)
	ignored := s.ignoredOn(nodeID)
	_, own := ownStackSchedule(rows, nodeID, project)
	for _, row := range rows {
		entry, covered := stackCoverage(scheduleFromRow(row), nodeID, project, members, ignored)
		if !covered {
			continue
		}
		entry.scheduleView = s.viewFor(row)
		entry.Own = own != nil && row.ID == own.ID
		out = append(out, entry)
	}
	return out
}

// stackCoverage says how a schedule backs compose project `project` on nodeID
// up: as the stack itself, by naming some of its services, or with the rest of
// the server — the services matched the way the scheduler matches them. Pure.
func stackCoverage(sc Schedule, nodeID, project string, members []*dockercli.Container, ignored ignoreSet) (stackSchedule, bool) {
	for _, t := range sc.Targets {
		if t.NodeID == nodeID && t.Stack == project {
			return stackSchedule{Covers: coveredByStack, Consistent: t.Consistent}, true
		}
	}
	var named []string
	withServer := false
	for _, c := range members {
		covers := scheduleCovers(sc, nodeID, c, ignored)
		if covers == coveredByName {
			named = append(named, c.Name)
		}
		withServer = withServer || covers == coveredByNode
	}
	if len(named) > 0 {
		return stackSchedule{Covers: coveredByName, Services: named}, true
	}
	if withServer {
		return stackSchedule{Covers: coveredByNode}, true
	}
	return stackSchedule{}, false
}

// ownStackSchedule finds the stack's own schedule — the first whose only
// target is this stack — or nil when it has none.
func ownStackSchedule(rows []*store.Schedule, nodeID, project string) (Schedule, *store.Schedule) {
	for _, row := range rows {
		sc := scheduleFromRow(row)
		if isOwnStackSchedule(sc, nodeID, project) {
			return sc, row
		}
	}
	return Schedule{}, nil
}

// isOwnStackSchedule reports whether this stack is the schedule's only target.
func isOwnStackSchedule(sc Schedule, nodeID, project string) bool {
	return len(sc.Targets) == 1 && sc.Targets[0].NodeID == nodeID && sc.Targets[0].Stack == project
}

// cachedStackMembers returns a compose project's containers on a node from the
// inventory cache, without a Docker call.
func (s *Server) cachedStackMembers(nodeID, project string) []*dockercli.Container {
	s.statMu.RLock()
	defer s.statMu.RUnlock()
	st, ok := s.stats[nodeID]
	if !ok {
		return nil
	}
	var members []*dockercli.Container
	for _, c := range st.Containers {
		if c.Stack == project {
			members = append(members, c)
		}
	}
	return members
}

// stackScheduleTiming is when a stack's own schedule runs, as its page sets it.
type stackScheduleTiming struct {
	Enabled    bool   `json:"enabled"`
	Kind       string `json:"kind"`
	Time       string `json:"time"`
	Weekday    int    `json:"weekday"`
	Monthday   int    `json:"monthday"`
	Cron       string `json:"cron"`
	Consistent bool   `json:"consistent"`
}

// handleStackSchedules returns every schedule that backs a compose project up.
func (s *Server) handleStackSchedules(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetNode(id); err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	writeJSON(w, http.StatusOK, s.stackSchedules(id, r.PathValue("project")))
}

// handleSetStackSchedule gives a compose project its own schedule — one whose
// only target is the stack — or changes when the one it has runs. Other
// schedules are left as they are.
func (s *Server) handleSetStackSchedule(w http.ResponseWriter, r *http.Request) {
	id, project := r.PathValue("id"), r.PathValue("project")
	node, err := s.store.GetNode(id)
	if err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	var timing stackScheduleTiming
	if err := readJSON(r, &timing); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	rows, err := s.store.ListSchedules()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	sc, existing := ownStackSchedule(rows, id, project)
	if existing == nil {
		sc = Schedule{ID: randToken()[:12], Name: project + " on " + node.Name}
	}
	sc.Enabled, sc.Kind, sc.Time, sc.Weekday, sc.Monthday, sc.Cron = timing.Enabled, timing.Kind, timing.Time, timing.Weekday, timing.Monthday, timing.Cron
	sc.Targets = []ScheduleTarget{{NodeID: id, Stack: project, Consistent: timing.Consistent}}
	row, err := sc.toRow()
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	if existing != nil {
		row.LastRun = existing.LastRun // keep the baseline across edits
	}
	if err := s.store.UpsertSchedule(row); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "stack.schedule.update", project,
		fmt.Sprintf("node=%s schedule=%q enabled=%v kind=%s consistent=%v", node.Name, sc.Name, sc.Enabled, sc.Kind, timing.Consistent))
	writeJSON(w, http.StatusOK, s.stackSchedules(id, project))
}

// handleRemoveStackFromSchedule takes a compose project out of one schedule.
// The stack's own schedule, left with no target, is deleted.
func (s *Server) handleRemoveStackFromSchedule(w http.ResponseWriter, r *http.Request) {
	id, project := r.PathValue("id"), r.PathValue("project")
	row, err := s.store.GetSchedule(r.PathValue("sid"))
	if err != nil {
		errJSON(w, http.StatusNotFound, "schedule not found")
		return
	}
	sc := scheduleFromRow(row)
	target := ScheduleTarget{NodeID: id, Stack: project}
	if !scheduleTargetIn(sc.Targets, target) {
		errJSON(w, http.StatusNotFound, "this stack is not a target of that schedule")
		return
	}
	if isOwnStackSchedule(sc, id, project) {
		err = s.store.DeleteSchedule(row.ID)
	} else {
		s.removeScheduleTargets(row.ID, []ScheduleTarget{target})
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "stack.schedule.remove", project, fmt.Sprintf("node=%s schedule=%q", id, sc.Name))
	writeJSON(w, http.StatusOK, s.stackSchedules(id, project))
}

// keepShortest records d under key unless a shorter interval is already there.
func keepShortest(m map[string]time.Duration, key string, d time.Duration) {
	if prev, ok := m[key]; ok && prev <= d {
		return
	}
	m[key] = d
}

// scheduleProtect ensures a container is a target of some schedule (B5 "Protect").
// If it isn't covered by any schedule, it's appended to an enabled schedule (or,
// if none exists, a new enabled daily "Protected containers" schedule). Returns
// whether the container is now covered, whether a target was added, and whether a
// schedule covering it is enabled.
func (s *Server) scheduleProtect(nodeID, name string) (covered, added, enabled bool, scheduleID string) {
	rows, err := s.store.ListSchedules()
	if err != nil {
		return false, false, false, ""
	}
	// Already covered by any schedule (whole-node or this name)?
	for _, row := range rows {
		sc := scheduleFromRow(row)
		for _, t := range sc.Targets {
			if t.NodeID != nodeID {
				continue
			}
			if (t.ContainerName == "" && t.ContainerID == "") || t.ContainerName == name {
				return true, false, row.Enabled, row.ID
			}
		}
	}
	// Not covered — append to an enabled schedule (else the first existing one).
	target := firstEnabledSchedule(rows)
	if target != nil {
		sc := scheduleFromRow(target)
		sc.Targets = append(sc.Targets, ScheduleTarget{NodeID: nodeID, ContainerName: name})
		if newRow, e := sc.toRow(); e == nil {
			newRow.LastRun = target.LastRun // keep its baseline
			if s.store.UpsertSchedule(newRow) == nil {
				return true, true, sc.Enabled, target.ID
			}
		}
		return true, false, target.Enabled, target.ID
	}
	// No schedules at all — create one so protection actually automates.
	sc := Schedule{
		ID: randToken()[:12], Name: "Protected containers", Enabled: true,
		Kind: "daily", Time: "03:00", Monthday: 1,
		Targets: []ScheduleTarget{{NodeID: nodeID, ContainerName: name}},
	}
	if newRow, e := sc.toRow(); e == nil && s.store.UpsertSchedule(newRow) == nil {
		return true, true, true, sc.ID
	}
	return false, false, false, ""
}

// scheduleProtectStack ensures a whole compose PROJECT is a schedule target
// (F220), the stack-level counterpart of scheduleProtect.
//
// The difference that matters is not "one target instead of six" for its own
// sake — it is that a stack target can be app-CONSISTENT, and six container
// targets cannot. Six independent runs capture six moments; an application whose
// database and its files must agree is not restorable from that.
//
// So when the stack target is added, the per-container targets it now subsumes
// are REMOVED. Leaving them would back every member up twice per run: once by
// name, once as a member of the stack, which is worse than what the operator had
// before they clicked. The count is returned so the summary can say it out loud
// rather than silently editing someone's schedule.
func (s *Server) scheduleProtectStack(nodeID, project string, consistent bool, members []string) (covered, added bool, enabled bool, scheduleID string, replaced int) {
	rows, err := s.store.ListSchedules()
	if err != nil {
		return false, false, false, "", 0
	}
	isMember := map[string]bool{}
	for _, m := range members {
		isMember[m] = true
	}

	// Already covered? Two ways, and they mean different things.
	for _, row := range rows {
		sc := scheduleFromRow(row)
		for i, t := range sc.Targets {
			if t.NodeID != nodeID {
				continue
			}
			// A WHOLE-NODE target already backs up every member every run. Adding a
			// stack target beside it would double every capture, so nothing is
			// added — the stack is covered, just not as one consistent group. The
			// summary says so; the immediate run below is still app-consistent.
			if t.ContainerName == "" && t.ContainerID == "" && t.Stack == "" {
				return true, false, row.Enabled, row.ID, 0
			}
			if t.Stack != project {
				continue
			}
			// This stack is already a target. Upgrade it to app-consistent if it
			// isn't and the stack now has more than one service — a strict
			// improvement, and what "protect this stack" means. Never downgraded:
			// an operator who turned consistency OFF chose that.
			if consistent && !t.Consistent {
				sc.Targets[i].Consistent = true
				if newRow, e := sc.toRow(); e == nil {
					newRow.LastRun = row.LastRun
					_ = s.store.UpsertSchedule(newRow)
				}
			}
			return true, false, row.Enabled, row.ID, 0
		}
	}

	// Not covered — drop the per-container targets this stack target subsumes,
	// wherever they live, then append the stack target to an enabled schedule.
	for _, row := range rows {
		sc := scheduleFromRow(row)
		kept := make([]ScheduleTarget, 0, len(sc.Targets))
		for _, t := range sc.Targets {
			if t.NodeID == nodeID && t.Stack == "" && t.ContainerName != "" && isMember[t.ContainerName] {
				replaced++
				continue
			}
			kept = append(kept, t)
		}
		if len(kept) == len(sc.Targets) {
			continue
		}
		sc.Targets = kept
		if newRow, e := sc.toRow(); e == nil {
			newRow.LastRun = row.LastRun
			_ = s.store.UpsertSchedule(newRow)
		}
	}
	rows, err = s.store.ListSchedules() // re-read: the rows above were rewritten
	if err != nil {
		return false, false, false, "", replaced
	}

	target := firstEnabledSchedule(rows)
	if target != nil {
		sc := scheduleFromRow(target)
		sc.Targets = append(sc.Targets, ScheduleTarget{NodeID: nodeID, Stack: project, Consistent: consistent})
		if newRow, e := sc.toRow(); e == nil {
			newRow.LastRun = target.LastRun // keep its baseline
			if s.store.UpsertSchedule(newRow) == nil {
				return true, true, sc.Enabled, target.ID, replaced
			}
		}
		return true, false, target.Enabled, target.ID, replaced
	}
	// No schedules at all — create one, so protection actually automates.
	sc := Schedule{
		ID: randToken()[:12], Name: "Protected containers", Enabled: true,
		Kind: "daily", Time: "03:00", Monthday: 1,
		Targets: []ScheduleTarget{{NodeID: nodeID, Stack: project, Consistent: consistent}},
	}
	if newRow, e := sc.toRow(); e == nil && s.store.UpsertSchedule(newRow) == nil {
		return true, true, true, sc.ID, replaced
	}
	return false, false, false, "", replaced
}

// firstEnabledSchedule picks the schedule a new target should join: an enabled
// one, else the first that exists. Shared so container- and stack-protect never
// drift into appending to different schedules.
func firstEnabledSchedule(rows []*store.Schedule) *store.Schedule {
	for _, row := range rows {
		if row.Enabled {
			return row
		}
	}
	if len(rows) > 0 {
		return rows[0]
	}
	return nil
}

// migrateSchedules seeds the schedules table from the legacy single global
// schedule on first upgrade, so an existing install keeps automating backups
// (now shown as a "Default" schedule). The legacy settings key is left readable
// so a rollback still works. No-op once any schedule row exists.
func (s *Server) migrateSchedules() {
	rows, err := s.store.ListSchedules()
	if err != nil || len(rows) > 0 {
		return
	}
	sj, _ := s.store.GetSetting("schedule", "")
	if sj == "" {
		return
	}
	var sc Schedule
	if json.Unmarshal([]byte(sj), &sc) != nil || len(sc.Targets) == 0 {
		return // nothing meaningful to migrate
	}
	sc.ID = randToken()[:12]
	sc.Name = "Default"
	row, err := sc.toRow()
	if err != nil {
		return
	}
	// Carry over the legacy baseline so the migrated schedule doesn't fire a
	// spurious catch-up on the first tick after upgrade.
	lr, _ := s.store.GetSetting("schedule.last_run", "0")
	row.LastRun, _ = strconv.ParseInt(lr, 10, 64)
	if err := s.store.UpsertSchedule(row); err == nil {
		log.Printf("startup: migrated legacy schedule to named schedule %q", sc.Name)
	}
}
