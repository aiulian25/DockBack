package api

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"dockback/internal/backup"
	"dockback/internal/store"
)

// TestMissingTargetAction covers the F13 auto-clean threshold: a target is only
// removed once it has been missing for the configured grace period, the
// first-seen timestamp is recorded on first sighting, and days<=0 never removes.
func TestMissingTargetAction(t *testing.T) {
	const day = int64(86400)
	now := int64(1_000_000_000)

	// First sighting: record `now`, don't remove yet.
	if first, remove := missingTargetAction(0, 3, now); first != now || remove {
		t.Fatalf("first sighting: first=%d remove=%v; want first=%d remove=false", first, remove, now)
	}

	// Seen 2 days ago, 3-day grace: still within the window → keep.
	if first, remove := missingTargetAction(now-2*day, 3, now); remove || first != now-2*day {
		t.Fatalf("within grace: first=%d remove=%v; want keep", first, remove)
	}

	// Seen 3 days ago, 3-day grace: threshold reached → remove.
	if _, remove := missingTargetAction(now-3*day, 3, now); !remove {
		t.Fatal("at threshold: should auto-remove")
	}

	// Seen long ago but auto-clean disabled (0 days) → never remove.
	if _, remove := missingTargetAction(now-30*day, 0, now); remove {
		t.Fatal("days=0 must never auto-remove")
	}
}

// TestScheduledBackupOptions verifies a scheduled run reuses a container's
// remembered manual choices (compression / app-native export / save image),
// falling back to balanced/off when nothing is stored (F3).
func TestScheduledBackupOptions(t *testing.T) {
	s := &Server{store: testStore(t)}
	dests := []string{"d1", "d2"}

	// No stored options → defaults, destinations kept explicit.
	o := s.scheduledBackupOptions("n1", "cid1", "web", dests)
	if o.Compression != "balanced" || o.AppExport || o.SaveImage {
		t.Fatalf("defaults: got compression=%q appExport=%v saveImage=%v", o.Compression, o.AppExport, o.SaveImage)
	}
	if !o.DestinationsExplicit || len(o.Destinations) != 2 {
		t.Fatalf("destinations must stay explicit: explicit=%v n=%d", o.DestinationsExplicit, len(o.Destinations))
	}
	if o.NodeID != "n1" || o.ContainerID != "cid1" {
		t.Fatalf("node/container not set: %q/%q", o.NodeID, o.ContainerID)
	}
	if o.IncludeMounts != nil {
		t.Fatalf("IncludeMounts must be nil (remembered/default): %v", o.IncludeMounts)
	}

	// Persist a manual selection, then confirm the scheduler honors it.
	js, _ := json.Marshal(backup.SavedBackupOptions{Compression: "xz", AppExport: true, SaveImage: true})
	if err := s.store.SetSetting(backup.BackupOptionsKey("n1", "web"), string(js)); err != nil {
		t.Fatal(err)
	}
	o = s.scheduledBackupOptions("n1", "cid1", "web", dests)
	if o.Compression != "xz" || !o.AppExport || !o.SaveImage {
		t.Fatalf("remembered: got compression=%q appExport=%v saveImage=%v", o.Compression, o.AppExport, o.SaveImage)
	}

	// A different container on the same node is unaffected (keyed by name).
	if o2 := s.scheduledBackupOptions("n1", "cid2", "db", dests); o2.Compression != "balanced" || o2.AppExport || o2.SaveImage {
		t.Fatalf("unrelated container leaked options: %+v", o2)
	}

	// A stored record with an empty compression keeps the balanced default.
	js2, _ := json.Marshal(backup.SavedBackupOptions{Compression: "", AppExport: true})
	_ = s.store.SetSetting(backup.BackupOptionsKey("n1", "cache"), string(js2))
	if o3 := s.scheduledBackupOptions("n1", "cid3", "cache", dests); o3.Compression != "balanced" || !o3.AppExport {
		t.Fatalf("empty compression should keep balanced: %+v", o3)
	}
}

func TestScheduleDue(t *testing.T) {
	sc := Schedule{Enabled: true, Kind: "daily", Time: "03:00"}
	miss := 5 * time.Minute

	// lastRun yesterday 03:00; now is today 03:01 -> due, on time (not missed).
	lastRun := time.Date(2026, 6, 29, 3, 0, 0, 0, time.UTC)
	now := time.Date(2026, 6, 30, 3, 1, 0, 0, time.UTC)
	fire, at, missed := scheduleDue(sc, lastRun, now, miss)
	if !fire || missed {
		t.Fatalf("on-time daily: fire=%v missed=%v at=%v", fire, missed, at)
	}

	// App was down: now is today 09:00, well past the 03:00 window -> catch-up.
	now = time.Date(2026, 6, 30, 9, 0, 0, 0, time.UTC)
	fire, _, missed = scheduleDue(sc, lastRun, now, miss)
	if !fire || !missed {
		t.Fatalf("missed window should be a catch-up: fire=%v missed=%v", fire, missed)
	}

	// Not yet due: now is before the next scheduled time.
	now = time.Date(2026, 6, 29, 23, 0, 0, 0, time.UTC)
	if fire, _, _ := scheduleDue(sc, lastRun, now, miss); fire {
		t.Fatal("should not fire before the next scheduled time")
	}

	// Unschedulable (bad custom cron) never fires.
	if fire, _, _ := scheduleDue(Schedule{Kind: "custom", Cron: "not a cron"}, lastRun, now, miss); fire {
		t.Fatal("invalid schedule must not fire")
	}
}

// TestPrunePlan locks in the scheduled-prune decision (F17): a disabled schedule
// never prunes and never moves the baseline; the first evaluation establishes a
// baseline (no immediate backfill); a passed window fires; an unschedulable
// schedule never fires.
func TestPrunePlan(t *testing.T) {
	now := time.Date(2026, 6, 30, 9, 0, 0, 0, time.UTC)
	daily := Schedule{Enabled: true, Kind: "daily", Time: "03:00"}

	// Disabled: never fires, baseline untouched — even with a long-stale lastRun.
	stale := now.Add(-72 * time.Hour)
	off := daily
	off.Enabled = false
	if fire, nl := prunePlan(off, stale, now); fire || !nl.Equal(stale) {
		t.Fatalf("disabled must not prune or move baseline: fire=%v newLast=%v", fire, nl)
	}

	// First evaluation (zero lastRun): establish a baseline at now, do NOT fire.
	if fire, nl := prunePlan(daily, time.Time{}, now); fire || !nl.Equal(now) {
		t.Fatalf("first eval should set baseline=now without firing: fire=%v newLast=%v", fire, nl)
	}

	// Window passed since the last sweep: fire and advance the baseline to now.
	lastRun := time.Date(2026, 6, 29, 3, 0, 0, 0, time.UTC) // 03:00 yesterday
	if fire, nl := prunePlan(daily, lastRun, now); !fire || !nl.Equal(now) {
		t.Fatalf("due window should fire and advance baseline: fire=%v newLast=%v", fire, nl)
	}

	// Not yet due: keep the baseline, don't fire.
	soon := time.Date(2026, 6, 30, 3, 1, 0, 0, time.UTC) // just after this run's last fire
	future := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	if fire, nl := prunePlan(daily, soon, future); fire || !nl.Equal(soon) {
		t.Fatalf("before next window: fire=%v newLast=%v", fire, nl)
	}

	// Unschedulable (bad custom cron) never fires and never moves the baseline.
	bad := Schedule{Enabled: true, Kind: "custom", Cron: "not a cron"}
	if fire, nl := prunePlan(bad, stale, now); fire || !nl.Equal(stale) {
		t.Fatalf("invalid prune schedule must not fire: fire=%v newLast=%v", fire, nl)
	}
}

// TestScheduleRowRoundTrip verifies the friendly schedule survives the store's
// row form (name, kind, time, targets, enabled) and gets a valid computed cron.
func TestScheduleRowRoundTrip(t *testing.T) {
	sc := Schedule{
		Name: "DBs hourly", Enabled: true, Kind: "weekly", Time: "02:30", Weekday: 3,
		Targets: []ScheduleTarget{{NodeID: "n1", ContainerName: "pg"}}, IncludeStopped: true,
	}
	row, err := sc.toRow()
	if err != nil {
		t.Fatalf("toRow: %v", err)
	}
	if row.Cron != "30 2 * * 3" {
		t.Fatalf("computed cron = %q, want %q", row.Cron, "30 2 * * 3")
	}
	if !row.Enabled {
		t.Fatal("enabled column must mirror the schedule")
	}
	got := scheduleFromRow(row)
	if got.Name != sc.Name || got.Kind != sc.Kind || got.Time != sc.Time || got.Weekday != sc.Weekday {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if !got.IncludeStopped || len(got.Targets) != 1 || got.Targets[0].ContainerName != "pg" {
		t.Fatalf("targets/include-stopped lost: %+v", got)
	}

	// A custom schedule with an invalid cron is rejected up front.
	if _, err := (Schedule{Kind: "custom", Cron: "nope"}).toRow(); err == nil {
		t.Fatal("invalid custom cron must be rejected by toRow")
	}
}

// TestScheduleStackTarget covers F47: a stack target round-trips through storage
// (old-JSON compatibility) and the target-validation rules.
func TestScheduleStackTarget(t *testing.T) {
	// Round-trip a consistent stack target.
	sc := Schedule{
		Name: "myapp nightly", Enabled: true, Kind: "daily", Time: "03:00",
		Targets: []ScheduleTarget{{NodeID: "n1", Stack: "myapp", Consistent: true}},
	}
	row, err := sc.toRow()
	if err != nil {
		t.Fatalf("toRow: %v", err)
	}
	got := scheduleFromRow(row)
	if len(got.Targets) != 1 || got.Targets[0].Stack != "myapp" || !got.Targets[0].Consistent {
		t.Fatalf("stack target lost in round-trip: %+v", got.Targets)
	}

	// Legacy container JSON (no stack/consistent keys) must unmarshal with zero values.
	var legacy Schedule
	if err := json.Unmarshal([]byte(`{"targets":[{"node_id":"n1","container_name":"pg"}]}`), &legacy); err != nil {
		t.Fatalf("legacy unmarshal: %v", err)
	}
	if legacy.Targets[0].Stack != "" || legacy.Targets[0].Consistent {
		t.Fatalf("legacy target must have empty stack/consistent: %+v", legacy.Targets[0])
	}

	// Validation: a target can't be both a stack and a container.
	both := Schedule{Kind: "daily", Time: "03:00", Targets: []ScheduleTarget{{NodeID: "n1", Stack: "myapp", ContainerName: "pg"}}}
	if _, err := both.toRow(); err == nil {
		t.Fatal("a target set to both a stack and a container must be rejected")
	}
	// Validation: consistent only with a stack.
	consNoStack := Schedule{Kind: "daily", Time: "03:00", Targets: []ScheduleTarget{{NodeID: "n1", ContainerName: "pg", Consistent: true}}}
	if _, err := consNoStack.toRow(); err == nil {
		t.Fatal("app-consistent without a stack must be rejected")
	}
}

// TestSchedulePerDestination covers F27: a schedule can override its offsite
// destinations, an override to an empty list means local-only, a schedule without
// the override uses the effective policy, and the fields round-trip through storage.
func TestSchedulePerDestination(t *testing.T) {
	eff := []string{"offsiteA", "offsiteB"}

	// No override → effective policy destinations unchanged.
	if got := scheduleDests(Schedule{}, eff); len(got) != 2 || got[0] != "offsiteA" || got[1] != "offsiteB" {
		t.Fatalf("no override should use effective: %v", got)
	}
	// Override to a specific offsite set.
	if got := scheduleDests(Schedule{DestinationsExplicit: true, Destinations: []string{"onlyA"}}, eff); len(got) != 1 || got[0] != "onlyA" {
		t.Fatalf("override should use the schedule's set: %v", got)
	}
	// Override to an empty list = local-only (no offsite).
	if got := scheduleDests(Schedule{DestinationsExplicit: true, Destinations: []string{}}, eff); len(got) != 0 {
		t.Fatalf("empty override should be local-only: %v", got)
	}

	// The enqueued options carry EXACTLY the schedule's destinations, kept explicit.
	s := &Server{store: testStore(t)}
	over := Schedule{DestinationsExplicit: true, Destinations: []string{"onlyA"}}
	o := s.scheduledBackupOptions("n1", "cid1", "", scheduleDests(over, eff))
	if !o.DestinationsExplicit || len(o.Destinations) != 1 || o.Destinations[0] != "onlyA" {
		t.Fatalf("options must carry the schedule's exact destinations, explicit: %+v", o)
	}
	// Local-only override → explicit + empty.
	oLocal := s.scheduledBackupOptions("n1", "cid1", "", scheduleDests(Schedule{DestinationsExplicit: true, Destinations: []string{}}, eff))
	if !oLocal.DestinationsExplicit || len(oLocal.Destinations) != 0 {
		t.Fatalf("local-only must be explicit + empty: %+v", oLocal)
	}

	// GET /api/schedules round-trips the fields (persisted in OptionsJSON).
	sc := Schedule{
		Name: "Weekly offsite", Enabled: true, Kind: "weekly", Time: "03:00", Weekday: 0,
		DestinationsExplicit: true, Destinations: []string{"offsiteA"},
	}
	row, err := sc.toRow()
	if err != nil {
		t.Fatalf("toRow: %v", err)
	}
	back := scheduleFromRow(row)
	if !back.DestinationsExplicit || len(back.Destinations) != 1 || back.Destinations[0] != "offsiteA" {
		t.Fatalf("destinations lost on round-trip: %+v", back)
	}
	// A legacy schedule (no override) round-trips as not-explicit.
	if back2 := scheduleFromRow(mustRow(t, Schedule{Name: "n", Kind: "daily", Time: "02:00"})); back2.DestinationsExplicit {
		t.Fatalf("legacy schedule must not be destination-explicit: %+v", back2)
	}
}

func mustRow(t *testing.T, sc Schedule) *store.Schedule {
	t.Helper()
	row, err := sc.toRow()
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// TestNamedSchedulesIndependent persists two schedules and confirms they are
// evaluated independently: at one instant one is due (and a catch-up) while the
// other is not yet due / on-time, driven purely by their own last_run baselines.
func TestNamedSchedulesIndependent(t *testing.T) {
	st := testStore(t)

	nightly := Schedule{Name: "Nightly 02:00", Enabled: true, Kind: "daily", Time: "02:00",
		Targets: []ScheduleTarget{{NodeID: "n1"}}}
	late := Schedule{Name: "Nightly 04:00", Enabled: true, Kind: "daily", Time: "04:00",
		Targets: []ScheduleTarget{{NodeID: "n2"}}}
	for i, sc := range []Schedule{nightly, late} {
		row, err := sc.toRow()
		if err != nil {
			t.Fatal(err)
		}
		row.ID = "sched" + strconv.Itoa(i)
		if err := st.UpsertSchedule(row); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.ListSchedules()
	if err != nil || len(rows) != 2 {
		t.Fatalf("expected 2 schedule rows, got %d (err=%v)", len(rows), err)
	}

	miss := 5 * time.Minute

	// Independent firing: at 02:01 the 02:00 schedule is due, the 04:00 one isn't.
	now := time.Date(2026, 6, 30, 2, 1, 0, 0, time.UTC)
	aLast := time.Date(2026, 6, 29, 2, 0, 0, 0, time.UTC)
	bLast := time.Date(2026, 6, 29, 4, 0, 0, 0, time.UTC)
	if fire, _, _ := scheduleDue(nightly, aLast, now, miss); !fire {
		t.Fatal("02:00 schedule should be due at 02:01")
	}
	if fire, _, _ := scheduleDue(late, bLast, now, miss); fire {
		t.Fatal("04:00 schedule must NOT be due at 02:01 (independent)")
	}

	// Independent catch-up: at 04:01, the 02:00 schedule (baseline 2 days ago) is a
	// missed-window catch-up, while the 04:00 schedule (baseline today 03:00) fires
	// on time — neither influences the other.
	now = time.Date(2026, 6, 30, 4, 1, 0, 0, time.UTC)
	aStale := time.Date(2026, 6, 28, 2, 0, 0, 0, time.UTC)
	bFresh := time.Date(2026, 6, 30, 3, 0, 0, 0, time.UTC)
	if fire, _, missed := scheduleDue(nightly, aStale, now, miss); !fire || !missed {
		t.Fatalf("stale 02:00 schedule should be a catch-up: fire=%v missed=%v", fire, missed)
	}
	if fire, _, missed := scheduleDue(late, bFresh, now, miss); !fire || missed {
		t.Fatalf("fresh 04:00 schedule should fire on time: fire=%v missed=%v", fire, missed)
	}
}

// TestMigrateSchedules seeds the legacy single-schedule setting and confirms the
// startup migration creates one "Default" schedule carrying its baseline, and is
// idempotent (a second run doesn't duplicate it).
func TestMigrateSchedules(t *testing.T) {
	st := testStore(t)
	s := &Server{store: st}

	// No legacy setting → nothing migrated.
	s.migrateSchedules()
	if rows, _ := st.ListSchedules(); len(rows) != 0 {
		t.Fatalf("expected no schedules without a legacy setting, got %d", len(rows))
	}

	legacy := Schedule{Enabled: true, Kind: "weekly", Time: "03:00", Weekday: 0,
		Targets: []ScheduleTarget{{NodeID: "n1", ContainerName: "web"}}}
	lj, _ := json.Marshal(legacy)
	if err := st.SetSetting("schedule", string(lj)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("schedule.last_run", "1750000000"); err != nil {
		t.Fatal(err)
	}

	s.migrateSchedules()
	rows, _ := st.ListSchedules()
	if len(rows) != 1 {
		t.Fatalf("expected 1 migrated schedule, got %d", len(rows))
	}
	got := scheduleFromRow(rows[0])
	if got.Name != "Default" {
		t.Fatalf("migrated schedule name = %q, want Default", got.Name)
	}
	if rows[0].LastRun != 1750000000 {
		t.Fatalf("migrated last_run = %d, want the legacy baseline", rows[0].LastRun)
	}
	if len(got.Targets) != 1 || got.Targets[0].ContainerName != "web" {
		t.Fatalf("migrated targets lost: %+v", got.Targets)
	}

	// Idempotent: a second migration doesn't duplicate (table no longer empty).
	s.migrateSchedules()
	if rows, _ := st.ListSchedules(); len(rows) != 1 {
		t.Fatalf("migration must be idempotent, got %d schedules", len(rows))
	}
}
