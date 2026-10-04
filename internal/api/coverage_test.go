package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// F13: a stopped container that still holds a NAMED data volume and has no backup
// belongs in stopped_at_risk — but not one with only tmpfs/anonymous mounts, and a
// stopped-at-risk container must never inflate the running-unprotected number.
func TestCoverageStoppedAtRisk(t *testing.T) {
	st := testStore(t)
	if err := st.UpsertNode(&store.Node{ID: "n1", Name: "Node1"}); err != nil {
		t.Fatal(err)
	}
	// A stopped container that already has a successful backup must NOT be at-risk.
	if err := st.CreateBackup(&store.Backup{ID: "b1", NodeID: "n1", TargetName: "backed-up", Status: "success", CreatedAt: 100}); err != nil {
		t.Fatal(err)
	}
	anon := strings.Repeat("a", 64) // Docker-assigned anonymous-volume id

	s := &Server{store: st, stats: map[string]*nodeStat{}}
	s.setStat("n1", &nodeStat{Reachable: true, Containers: []*dockercli.Container{
		// Stopped, named volume, no backup → at risk.
		{ID: "c1", Name: "occasional-app", State: "exited", Mounts: []dockercli.Mount{{Type: "volume", Name: "occasional_data", Destination: "/data"}}},
		// Stopped, only tmpfs → not at risk.
		{ID: "c2", Name: "throwaway", State: "exited", Mounts: []dockercli.Mount{{Type: "tmpfs", Destination: "/tmp"}}},
		// Stopped, only an anonymous volume → not at risk.
		{ID: "c3", Name: "anon-only", State: "exited", Mounts: []dockercli.Mount{{Type: "volume", Name: anon, Destination: "/v"}}},
		// Stopped, named volume, but already backed up → not at risk.
		{ID: "c4", Name: "backed-up", State: "exited", Mounts: []dockercli.Mount{{Type: "volume", Name: "kept_data", Destination: "/data"}}},
		// Running, named volume, no backup → running-unprotected (NOT stopped_at_risk).
		{ID: "c5", Name: "running-app", State: "running", Mounts: []dockercli.Mount{{Type: "volume", Name: "run_data", Destination: "/data"}}},
	}})

	rec := httptest.NewRecorder()
	s.handleCoverage(rec, httptest.NewRequest("GET", "/api/coverage", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp coverageResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	if len(resp.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(resp.Nodes))
	}
	cn := resp.Nodes[0]

	atRisk := map[string]bool{}
	for _, c := range cn.StoppedAtRisk {
		atRisk[c.Name] = true
	}
	if !atRisk["occasional-app"] {
		t.Errorf("stopped container with a named volume + no backup should be at risk; got %v", names(cn.StoppedAtRisk))
	}
	for _, bad := range []string{"throwaway", "anon-only", "backed-up", "running-app"} {
		if atRisk[bad] {
			t.Errorf("%q must NOT be in stopped_at_risk", bad)
		}
	}
	if resp.StoppedAtRiskTotal != 1 {
		t.Errorf("stopped_at_risk_total = %d, want 1", resp.StoppedAtRiskTotal)
	}

	// The running-unprotected surface must be unchanged: only the running app,
	// and stopped-at-risk never inflates it.
	if cn.Running != 1 {
		t.Errorf("running = %d, want 1 (stopped containers excluded)", cn.Running)
	}
	if len(cn.Unprotected) != 1 || cn.Unprotected[0].Name != "running-app" {
		t.Errorf("unprotected = %v, want [running-app]", names(cn.Unprotected))
	}
	if resp.UnprotectedTotal != 1 {
		t.Errorf("unprotected_total = %d, want 1", resp.UnprotectedTotal)
	}
}

func names(cs []coverageContainer) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

// F13: the anonymous-volume-id detector must accept a 64-hex id and reject a
// human-chosen name (including short hex names).
func TestIsAnonymousVolumeName(t *testing.T) {
	if !isAnonymousVolumeName(strings.Repeat("0", 64)) {
		t.Error("64 hex chars should read as anonymous")
	}
	for _, named := range []string{"", "pgdata", "immich_data", "abc123", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("g", 64)} {
		if isAnonymousVolumeName(named) {
			t.Errorf("%q should read as a named (non-anonymous) volume", named)
		}
	}
}

// The 2026-10-04 recovery: stacks read "protected" on backups weeks old, and one
// scheduled stack marked the whole node as covered. Protected now means a
// backup recent for the schedule that covers it; older is stale, none is never.
func TestCoverageCountsOnlyRecentBackups(t *testing.T) {
	st := testStore(t)
	if err := st.UpsertNode(&store.Node{ID: "n1", Name: "Node1"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ago := func(d time.Duration) int64 { return now.Add(-d).Unix() }
	for i, b := range []struct {
		name string
		at   int64
	}{
		{"fresh-manual", ago(2 * 24 * time.Hour)},
		{"old-manual", ago(20 * 24 * time.Hour)},
		{"blog-app", ago(3 * 24 * time.Hour)}, // daily schedule: older than two runs
		{"blog-db", ago(time.Hour)},
	} {
		if err := st.CreateBackup(&store.Backup{ID: fmt.Sprintf("b%d", i), NodeID: "n1", TargetName: b.name, Status: "success", CreatedAt: b.at}); err != nil {
			t.Fatal(err)
		}
	}
	for _, sc := range []Schedule{
		{ID: "s-blog", Name: "blog", Enabled: true, Kind: "daily", Time: "03:00", Targets: []ScheduleTarget{{NodeID: "n1", Stack: "blog"}}},
		{ID: "s-node", Name: "node", Enabled: true, Kind: "weekly", Time: "03:00", Targets: []ScheduleTarget{{NodeID: "n2"}}},
	} {
		row, err := sc.toRow()
		if err != nil {
			t.Fatal(err)
		}
		if err := st.UpsertSchedule(row); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{store: st, stats: map[string]*nodeStat{}}
	s.setStat("n1", &nodeStat{Reachable: true, Containers: []*dockercli.Container{
		{ID: "c1", Name: "fresh-manual", State: "running"},
		{ID: "c2", Name: "old-manual", State: "running"},
		{ID: "c3", Name: "blog-app", Stack: "blog", State: "running"},
		{ID: "c4", Name: "blog-db", Stack: "blog", State: "running"},
		{ID: "c5", Name: "other", State: "running"},
	}})

	rec := httptest.NewRecorder()
	s.handleCoverage(rec, httptest.NewRequest("GET", "/api/coverage", nil))
	var resp coverageResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	cn := resp.Nodes[0]
	if cn.Protected != 2 {
		t.Errorf("protected = %d, want 2 (fresh-manual, blog-db)", cn.Protected)
	}
	if got := fmt.Sprint(names(cn.Stale)); got != "[old-manual blog-app]" {
		t.Fatalf("stale = %s, want [old-manual blog-app]", got)
	}
	if len(cn.Unprotected) != 1 || cn.Unprotected[0].Name != "other" || cn.Unprotected[0].Scheduled {
		t.Errorf("a scheduled stack must not cover the rest of the node: unprotected = %+v", cn.Unprotected)
	}
	if !cn.Stale[1].Scheduled || cn.Stale[1].LastBackupAt == 0 {
		t.Errorf("a stale row says when it was last backed up and that it is scheduled: %+v", cn.Stale[1])
	}
	if resp.StaleTotal != 2 || resp.UnprotectedTotal != 1 {
		t.Errorf("totals: stale %d unprotected %d, want 2 and 1", resp.StaleTotal, resp.UnprotectedTotal)
	}
}

// A whole-node schedule backs up stopped containers only when it says so, and
// coverage now agrees with what it actually does.
func TestCoverageHonoursIncludeStopped(t *testing.T) {
	st := testStore(t)
	if err := st.UpsertNode(&store.Node{ID: "n1", Name: "Node1"}); err != nil {
		t.Fatal(err)
	}
	row, err := Schedule{ID: "s1", Name: "node", Enabled: true, Kind: "daily", Time: "03:00", Targets: []ScheduleTarget{{NodeID: "n1"}}}.toRow()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSchedule(row); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st, stats: map[string]*nodeStat{}}
	s.setStat("n1", &nodeStat{Reachable: true, Containers: []*dockercli.Container{
		{ID: "c1", Name: "occasional-app", State: "exited", Mounts: []dockercli.Mount{{Type: "volume", Name: "occasional_data", Destination: "/data"}}},
	}})
	rec := httptest.NewRecorder()
	s.handleCoverage(rec, httptest.NewRequest("GET", "/api/coverage", nil))
	var resp coverageResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.StoppedAtRiskTotal != 1 {
		t.Errorf("a schedule without include-stopped never backs this up, so it is at risk: %+v", resp.Nodes[0].StoppedAtRisk)
	}
}

func TestRecencyOf(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	day := 24 * time.Hour
	cases := []struct {
		age       time.Duration
		interval  time.Duration
		scheduled bool
		want      backupRecency
	}{
		{7 * day, 0, false, backupRecent},
		{9 * day, 0, false, backupStale},
		{36 * time.Hour, day, true, backupRecent},
		{49 * time.Hour, day, true, backupStale},
		{13 * day, 7 * day, true, backupRecent},
		{9 * day, 0, true, backupStale}, // an unschedulable schedule falls back to the default
	}
	for _, c := range cases {
		if got := recencyOf(now.Add(-c.age).Unix(), now, c.interval, c.scheduled); got != c.want {
			t.Errorf("age %v interval %v scheduled %v = %v, want %v", c.age, c.interval, c.scheduled, got, c.want)
		}
	}
	if recencyOf(0, now, day, true) != backupNever {
		t.Error("no backup is never, whatever the schedule")
	}
}
