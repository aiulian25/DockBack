package api

import (
	"strings"
	"testing"
)

// F38: when Protect adds a container to a schedule that is DISABLED, the result
// must report schedule_enabled=false AND the id of that schedule, so the UI can
// offer a one-click "Enable the schedule" instead of silently leaving the
// container without automatic protection.
func TestScheduleProtectDisabledReturnsID(t *testing.T) {
	s := &Server{store: testStore(t)}

	// A single, DISABLED schedule that doesn't yet cover the container.
	sc := Schedule{Name: "Nightly", Enabled: false, Kind: "daily", Time: "03:00",
		Targets: []ScheduleTarget{{NodeID: "n1", ContainerName: "other"}}}
	row, err := sc.toRow()
	if err != nil {
		t.Fatal(err)
	}
	row.ID = "sched-off"
	if err := s.store.UpsertSchedule(row); err != nil {
		t.Fatal(err)
	}

	covered, added, enabled, schedID := s.scheduleProtect("n1", "paperless")
	if !covered {
		t.Error("container should be covered (added to the existing schedule)")
	}
	if !added {
		t.Error("a target should have been added")
	}
	if enabled {
		t.Error("the chosen schedule is disabled — enabled must be false")
	}
	if schedID != "sched-off" {
		t.Errorf("scheduleID = %q, want the disabled schedule's id \"sched-off\"", schedID)
	}
}

// F38: when the chosen schedule IS enabled, enabled=true and the id is still
// returned (so the UI never needs to guess and the summary is a plain success).
func TestScheduleProtectEnabledReturnsID(t *testing.T) {
	s := &Server{store: testStore(t)}
	sc := Schedule{Name: "Nightly", Enabled: true, Kind: "daily", Time: "03:00",
		Targets: []ScheduleTarget{{NodeID: "n1"}}} // whole-node target already covers it
	row, err := sc.toRow()
	if err != nil {
		t.Fatal(err)
	}
	row.ID = "sched-on"
	if err := s.store.UpsertSchedule(row); err != nil {
		t.Fatal(err)
	}

	covered, _, enabled, schedID := s.scheduleProtect("n1", "paperless")
	if !covered || !enabled {
		t.Errorf("covered=%v enabled=%v, want both true", covered, enabled)
	}
	if schedID != "sched-on" {
		t.Errorf("scheduleID = %q, want \"sched-on\"", schedID)
	}
}

// F38: with NO schedules at all, scheduleProtect creates an enabled one and
// returns its (non-empty) id — never a silent no-op.
func TestScheduleProtectCreatesEnabled(t *testing.T) {
	s := &Server{store: testStore(t)}
	covered, added, enabled, schedID := s.scheduleProtect("n1", "paperless")
	if !covered || !added || !enabled {
		t.Errorf("covered=%v added=%v enabled=%v, want all true", covered, added, enabled)
	}
	if schedID == "" {
		t.Error("a newly-created schedule must return a non-empty id")
	}
}

// F220 — protecting a whole compose project.
//
// The point is not "one target instead of six" for tidiness. Six container
// targets capture six separate moments; an application whose database and files
// must agree is not restorable from that. One stack target with Consistent set
// is, so these tests hold the shape of what gets written to the schedule.

// stackTargets returns every target across all schedules, for assertions about
// what protect actually wrote.
func stackTargets(t *testing.T, s *Server) []ScheduleTarget {
	t.Helper()
	rows, err := s.store.ListSchedules()
	if err != nil {
		t.Fatal(err)
	}
	var out []ScheduleTarget
	for _, row := range rows {
		out = append(out, scheduleFromRow(row).Targets...)
	}
	return out
}

// AC1 — a multi-service project becomes exactly ONE consistent stack target.
func TestScheduleProtectStackAddsOneConsistentTarget(t *testing.T) {
	s := &Server{store: testStore(t)}

	covered, added, enabled, id, replaced := s.scheduleProtectStack("n1", "blog", true, []string{"blog-db", "blog-app", "blog-cache"})
	if !covered || !added || !enabled || id == "" {
		t.Fatalf("covered=%v added=%v enabled=%v id=%q — want a target on a new enabled schedule", covered, added, enabled, id)
	}
	if replaced != 0 {
		t.Errorf("nothing existed to replace, got %d", replaced)
	}
	got := stackTargets(t, s)
	if len(got) != 1 {
		t.Fatalf("want exactly one target, got %d: %+v", len(got), got)
	}
	if got[0].Stack != "blog" || !got[0].Consistent || got[0].NodeID != "n1" {
		t.Errorf("target = %+v, want {n1 blog consistent}", got[0])
	}
	// A stack target must never also carry a container — the save-time validator
	// refuses that, so writing one would have made the schedule unsaveable.
	if got[0].ContainerName != "" || got[0].ContainerID != "" {
		t.Errorf("a stack target must carry no container fields: %+v", got[0])
	}
}

// AC2 — running it again changes nothing.
func TestScheduleProtectStackIsIdempotent(t *testing.T) {
	s := &Server{store: testStore(t)}
	members := []string{"blog-db", "blog-app"}
	s.scheduleProtectStack("n1", "blog", true, members)

	covered, added, _, _, replaced := s.scheduleProtectStack("n1", "blog", true, members)
	if !covered {
		t.Error("the stack is still covered")
	}
	if added || replaced != 0 {
		t.Errorf("a second run must add nothing: added=%v replaced=%d", added, replaced)
	}
	if got := stackTargets(t, s); len(got) != 1 {
		t.Errorf("want one target after two runs, got %d: %+v", len(got), got)
	}
	// A DIFFERENT stack on the same node is its own target, not a duplicate.
	s.scheduleProtectStack("n1", "shop", true, []string{"shop-db"})
	if got := stackTargets(t, s); len(got) != 2 {
		t.Errorf("a second project is a second target, got %d: %+v", len(got), got)
	}
}

// The per-container targets a stack target subsumes are REMOVED. Leaving them
// would back every member up twice per run — worse than before the click.
func TestScheduleProtectStackReplacesPerContainerTargets(t *testing.T) {
	s := &Server{store: testStore(t)}
	sc := Schedule{Name: "Nightly", Enabled: true, Kind: "daily", Time: "03:00", Targets: []ScheduleTarget{
		{NodeID: "n1", ContainerName: "blog-db"},
		{NodeID: "n1", ContainerName: "blog-app"},
		{NodeID: "n1", ContainerName: "unrelated"}, // another workload on the same node
		{NodeID: "n2", ContainerName: "blog-db"},   // same NAME, different node
	}}
	row, err := sc.toRow()
	if err != nil {
		t.Fatal(err)
	}
	row.ID = "nightly"
	if err := s.store.UpsertSchedule(row); err != nil {
		t.Fatal(err)
	}

	_, added, _, _, replaced := s.scheduleProtectStack("n1", "blog", true, []string{"blog-db", "blog-app"})
	if !added {
		t.Fatal("the stack target must be added")
	}
	if replaced != 2 {
		t.Errorf("replaced = %d, want the 2 members on this node", replaced)
	}
	got := stackTargets(t, s)
	if len(got) != 3 {
		t.Fatalf("want unrelated + n2's + the stack target, got %d: %+v", len(got), got)
	}
	for _, tg := range got {
		if tg.NodeID == "n1" && (tg.ContainerName == "blog-db" || tg.ContainerName == "blog-app") {
			t.Errorf("a subsumed member target survived: %+v", tg)
		}
	}
	// Everything that was NOT subsumed is untouched — this edits somebody's
	// schedule, so it must edit only what it claims to.
	var sawUnrelated, sawOtherNode bool
	for _, tg := range got {
		if tg.NodeID == "n1" && tg.ContainerName == "unrelated" {
			sawUnrelated = true
		}
		if tg.NodeID == "n2" && tg.ContainerName == "blog-db" {
			sawOtherNode = true
		}
	}
	if !sawUnrelated || !sawOtherNode {
		t.Errorf("unrelated targets must survive: unrelated=%v otherNode=%v (%+v)", sawUnrelated, sawOtherNode, got)
	}
}

// A WHOLE-NODE target already backs up every member every run. Adding a stack
// target beside it would double every capture, so nothing is added.
func TestScheduleProtectStackDoesNotDoubleAWholeNodeSchedule(t *testing.T) {
	s := &Server{store: testStore(t)}
	sc := Schedule{Name: "Everything", Enabled: true, Kind: "daily", Time: "03:00",
		Targets: []ScheduleTarget{{NodeID: "n1"}}}
	row, _ := sc.toRow()
	row.ID = "everything"
	if err := s.store.UpsertSchedule(row); err != nil {
		t.Fatal(err)
	}

	covered, added, enabled, id, _ := s.scheduleProtectStack("n1", "blog", true, []string{"blog-db"})
	if !covered || !enabled || id != "everything" {
		t.Errorf("covered=%v enabled=%v id=%q — the node-wide schedule already covers it", covered, enabled, id)
	}
	if added {
		t.Error("adding a stack target beside a whole-node target would back every member up twice")
	}
	if got := stackTargets(t, s); len(got) != 1 {
		t.Errorf("the schedule must be unchanged, got %+v", got)
	}
}

// A stack target that predates app-consistency is upgraded in place rather than
// duplicated — but consistency is never turned OFF, because an operator who
// unticked it chose that.
func TestScheduleProtectStackUpgradesConsistencyInPlace(t *testing.T) {
	s := &Server{store: testStore(t)}
	sc := Schedule{Name: "Nightly", Enabled: true, Kind: "daily", Time: "03:00",
		Targets: []ScheduleTarget{{NodeID: "n1", Stack: "blog"}}} // not consistent
	row, _ := sc.toRow()
	row.ID = "nightly"
	if err := s.store.UpsertSchedule(row); err != nil {
		t.Fatal(err)
	}

	if _, added, _, _, _ := s.scheduleProtectStack("n1", "blog", true, []string{"blog-db", "blog-app"}); added {
		t.Error("the stack is already a target — nothing to add")
	}
	got := stackTargets(t, s)
	if len(got) != 1 || !got[0].Consistent {
		t.Fatalf("the existing target must be upgraded in place: %+v", got)
	}
	// Now the same stack down to one service: consistency stays on.
	s.scheduleProtectStack("n1", "blog", false, []string{"blog-db"})
	if got := stackTargets(t, s); len(got) != 1 || !got[0].Consistent {
		t.Errorf("consistency must never be turned off behind the operator: %+v", got)
	}
}

// The summary is what the operator actually reads, so it has to state the two
// things they cannot see: that it is ONE consistent target, and that targets
// were removed to get there.
func TestProtectStackSummaryStatesWhatChanged(t *testing.T) {
	line := protectStackSummary(protectStackResp{
		Stack: "blog", Services: 3, Consistent: true, Databases: []string{"blog-db"},
		Destinations: []string{"Local", "NAS"}, Scheduled: true, ScheduleAdded: true,
		ScheduleEnabled: true, ReplacedTargets: 2, BackupStarted: true,
	})
	for _, want := range []string{"App-consistent", "3 services", "blog-db", "NAS", "ONE stack target", "2 per-container schedule targets were replaced", "First backup started"} {
		if !strings.Contains(line, want) {
			t.Errorf("summary is missing %q: %s", want, line)
		}
	}
	// A schedule that is switched off must say so — protection that does not run
	// is the one outcome that must never read as success.
	off := protectStackSummary(protectStackResp{
		Stack: "blog", Services: 2, Consistent: true, Destinations: []string{"Local"},
		Scheduled: true, ScheduleAdded: true, ScheduleEnabled: false,
	})
	if !strings.Contains(off, "currently OFF") {
		t.Errorf("a disabled schedule must be stated: %s", off)
	}
	// One service: no false claim of consistency across a group of one.
	solo := protectStackSummary(protectStackResp{Stack: "solo", Services: 1, Destinations: []string{"Local"}, Scheduled: true, ScheduleEnabled: true})
	if strings.Contains(solo, "App-consistent") {
		t.Errorf("a single-service project has nothing to be consistent across: %s", solo)
	}
}
