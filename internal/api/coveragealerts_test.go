package api

import (
	"strings"
	"testing"
	"time"

	"dockback/internal/dockercli"
	"dockback/internal/notify"
	"dockback/internal/store"
)

func coverageAlertServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st := testStore(t)
	if err := st.UpsertNode(&store.Node{ID: "n1", Name: "NUC"}); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st, stats: map[string]*nodeStat{},
		notifier: notify.New(func() (notify.Config, error) { return notify.Config{}, nil }, func(string, string) {})}
	return s, st
}

// A stack whose backups stop being recent is announced once, as one stack, and
// again only after it has recovered and lapsed a second time.
func TestStaleBackupsAreAnnouncedOnce(t *testing.T) {
	s, st := coverageAlertServer(t)
	now := time.Now()
	for _, name := range []string{"nextcloud-app", "nextcloud-db"} {
		if err := st.CreateBackup(&store.Backup{ID: "b-" + name, NodeID: "n1", TargetName: name, Status: "success", CreatedAt: now.Add(-20 * 24 * time.Hour).Unix()}); err != nil {
			t.Fatal(err)
		}
	}
	s.setStat("n1", &nodeStat{Reachable: true, Containers: []*dockercli.Container{
		{ID: "c1", Name: "nextcloud-app", Stack: "nextcloud", State: "running"},
		{ID: "c2", Name: "nextcloud-db", Stack: "nextcloud", State: "running"},
	}})

	s.checkCoverageAlerts(now)
	got := alertsOfKind(t, s, notify.KindCoverageStale)
	if len(got) != 1 || !strings.Contains(got[0].Message, "nextcloud on NUC (last backed up 20d ago)") {
		t.Fatalf("one alert naming the stack once with its age, got %+v", got)
	}
	s.checkCoverageAlerts(now.Add(time.Hour))
	if n := len(alertsOfKind(t, s, notify.KindCoverageStale)); n != 1 {
		t.Errorf("a stack still stale must not be announced again, got %d alerts", n)
	}

	// It recovers, then lapses again: announced again.
	for _, name := range []string{"nextcloud-app", "nextcloud-db"} {
		if err := st.CreateBackup(&store.Backup{ID: "fresh-" + name, NodeID: "n1", TargetName: name, Status: "success", CreatedAt: now.Unix()}); err != nil {
			t.Fatal(err)
		}
	}
	s.checkCoverageAlerts(now.Add(2 * time.Hour))
	s.checkCoverageAlerts(now.Add(30 * 24 * time.Hour))
	if n := len(alertsOfKind(t, s, notify.KindCoverageStale)); n != 2 {
		t.Errorf("a stack that recovered and lapsed again must be announced again, got %d alerts", n)
	}
}

// The first check only learns what exists. After that, a project nothing
// schedules is announced; a scheduled one is not.
func TestNewUnscheduledProjectsAreAnnounced(t *testing.T) {
	s, st := coverageAlertServer(t)
	now := time.Now()
	s.setStat("n1", &nodeStat{Reachable: true, Containers: []*dockercli.Container{
		{ID: "c1", Name: "old-app", Stack: "old", State: "running"},
	}})
	s.checkCoverageAlerts(now)
	if n := len(alertsOfKind(t, s, notify.KindProjectUnscheduled)); n != 0 {
		t.Fatalf("the first check records a baseline and announces nothing, got %d", n)
	}

	row, err := Schedule{ID: "s1", Name: "wiki", Enabled: true, Kind: "daily", Time: "03:00", Targets: []ScheduleTarget{{NodeID: "n1", Stack: "wiki"}}}.toRow()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSchedule(row); err != nil {
		t.Fatal(err)
	}
	s.setStat("n1", &nodeStat{Reachable: true, Containers: []*dockercli.Container{
		{ID: "c1", Name: "old-app", Stack: "old", State: "running"},
		{ID: "c2", Name: "commafeed-app", Stack: "commafeed", State: "running"},
		{ID: "c3", Name: "commafeed-db", Stack: "commafeed", State: "running"},
		{ID: "c4", Name: "wiki-app", Stack: "wiki", State: "running"},
	}})
	s.checkCoverageAlerts(now.Add(time.Hour))
	got := alertsOfKind(t, s, notify.KindProjectUnscheduled)
	if len(got) != 1 || !strings.Contains(got[0].Message, "commafeed on NUC") {
		t.Fatalf("the new unscheduled project must be announced once, by name, got %+v", got)
	}
	for _, quiet := range []string{"old on", "wiki on"} {
		if strings.Contains(got[0].Message, quiet) {
			t.Errorf("%q is known or scheduled and must not be named: %s", quiet, got[0].Message)
		}
	}
	s.checkCoverageAlerts(now.Add(2 * time.Hour))
	if n := len(alertsOfKind(t, s, notify.KindProjectUnscheduled)); n != 1 {
		t.Errorf("a project is new once, got %d alerts", n)
	}
}

func TestCoverageDigestLine(t *testing.T) {
	now := int64(1_000_000_000)
	cov := coverageResp{RunningTotal: 4, Nodes: []coverageNode{{
		NodeID: "n1", NodeName: "NUC",
		Stale: []coverageContainer{
			{Name: "arr-sonarr", Stack: "arr", LastBackupAt: now - 9*86400},
			{Name: "arr-radarr", Stack: "arr", LastBackupAt: now - 12*86400},
		},
		Unprotected: []coverageContainer{{Name: "searxng"}},
	}}}
	line := coverageDigestLine(cov, now)
	for _, want := range []string{"stale backup — arr on NUC (last backed up 12d ago)", "never backed up — searxng on NUC"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %q", want, line)
		}
	}
	if strings.Count(line, "arr on NUC") != 1 {
		t.Errorf("a stack is named once, however many members are stale: %q", line)
	}
	if got := coverageDigestLine(coverageResp{RunningTotal: 3, Nodes: []coverageNode{{NodeName: "NUC"}}}, now); !strings.Contains(got, "all 3 running containers have a recent backup") {
		t.Errorf("all clear must say so: %q", got)
	}
	if got := coverageDigestLine(coverageResp{}, now); got != "" {
		t.Errorf("nothing running, nothing to say: %q", got)
	}
}

func TestListWithCap(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	if got := listWithCap(names); got != "a, b, c, d, e, f, g, h and 2 more" {
		t.Errorf("got %q", got)
	}
	if got := listWithCap(names[:2]); got != "a, b" {
		t.Errorf("got %q", got)
	}
}

// One disk failure takes the backups with the machine when every copy is on it.
// A "local" destination is a folder on this machine, so it does not count.
func TestOffMachineDestinations(t *testing.T) {
	dests := []*store.Destination{
		{Type: "local", Enabled: true},
		{Type: "s3", Enabled: false},
		{Type: "sftp", Enabled: true},
	}
	if got := offMachineDestinations(dests); got != 1 {
		t.Errorf("only the enabled remote one leaves the machine: %d", got)
	}
	if got := offMachineDestinations(dests[:2]); got != 0 {
		t.Errorf("a local folder and a disabled remote leave nothing off the machine: %d", got)
	}
}

func TestOffMachineDigestLine(t *testing.T) {
	backedUp := []coverageNode{{LastBackupAt: 1}}
	if got := offMachineDigestLine(coverageResp{Nodes: backedUp}); !strings.Contains(got, "every backup copy is on this machine") {
		t.Errorf("no off-machine destination must be said plainly: %q", got)
	}
	if got := offMachineDigestLine(coverageResp{Nodes: backedUp, OffMachineDestinations: 1}); !strings.Contains(got, "DockBack's own backup") {
		t.Errorf("an app backup kept only here must be named: %q", got)
	}
	if got := offMachineDigestLine(coverageResp{Nodes: backedUp, OffMachineDestinations: 1, AppOffMachineDestinations: 1}); got != "" {
		t.Errorf("both off the machine: nothing to say, got %q", got)
	}
	if got := offMachineDigestLine(coverageResp{Nodes: []coverageNode{{}}}); got != "" {
		t.Errorf("nothing backed up yet, nothing to lose: %q", got)
	}
}
