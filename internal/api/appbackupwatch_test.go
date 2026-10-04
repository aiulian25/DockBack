package api

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"dockback/internal/backup"
	"dockback/internal/config"
	"dockback/internal/notify"
	"dockback/internal/store"
)

func appBackupWatchServer(t *testing.T) *Server {
	t.Helper()
	st := testStore(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 7)
	}
	dir := t.TempDir()
	return &Server{
		store: st, bcast: newBroadcaster(16),
		cfg:      &config.Config{EncryptionKey: key, DataDir: dir, TmpDir: t.TempDir(), BackupsDir: dir},
		engine:   &backup.Engine{Store: st, Key: key, KeyFP: backup.KeyFingerprint(key), Log: func(string, string, string) {}},
		notifier: notify.New(func() (notify.Config, error) { return notify.Config{}, nil }, func(string, string) {}),
	}
}

// appBackupCount counts app backups taken, from the audit trail: two taken in
// the same second share a file name, which a test is fast enough to do.
func appBackupCount(t *testing.T, s *Server) int {
	t.Helper()
	rows, err := s.store.ListAudit(1000)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, row := range rows {
		if row.Action == "app.backup.create" {
			n++
		}
	}
	return n
}

// In the 2026-10-04 recovery DockBack's own backup was five weeks old: the
// schedule was off unless someone turned it on. It is now on unless someone
// turns it off, and the first tick backs up straight away.
func TestAppBackupIsOnByDefaultAndStartsAtOnce(t *testing.T) {
	s := appBackupWatchServer(t)
	a := s.loadAppBackupSchedule()
	if !a.Enabled || !a.PushExternal || a.Kind != "weekly" {
		t.Fatalf("with nothing saved the schedule is on, weekly, pushed off-machine: %+v", a)
	}
	s.appBackupTick(time.Now())
	if n := appBackupCount(t, s); n != 1 {
		t.Fatalf("the first activation takes a backup now, not in a week: %d backups", n)
	}

	off, _ := json.Marshal(appBackupSchedule{Enabled: false, Kind: "weekly", Time: "04:00"})
	if err := s.store.SetSetting(appBackupScheduleKey, string(off)); err != nil {
		t.Fatal(err)
	}
	if s.loadAppBackupSchedule().Enabled {
		t.Error("a schedule the operator switched off stays off")
	}
}

// A configuration change is backed up once it has settled for the quiet
// period, and the burst of edits behind it makes one backup.
func TestAppBackupFollowsConfigChanges(t *testing.T) {
	s := appBackupWatchServer(t)
	s.appBackupTick(time.Now()) // first activation: one backup, mark set
	a := s.loadAppBackupSchedule()
	start := appBackupCount(t, s)

	if err := s.store.Audit("admin", "backup.start", "x", ""); err != nil {
		t.Fatal(err)
	}
	s.appBackupAfterConfigChange(a, time.Now().Add(time.Hour))
	if appBackupCount(t, s) != start {
		t.Fatal("an action that changes no configuration must not trigger a backup")
	}

	for _, action := range []string{"schedule.update", "destination.add", "policy.update"} {
		if err := s.store.Audit("admin", action, "x", ""); err != nil {
			t.Fatal(err)
		}
	}
	s.appBackupAfterConfigChange(a, time.Now())
	if appBackupCount(t, s) != start {
		t.Fatal("a change still settling is not backed up yet")
	}
	s.appBackupTick(time.Now().Add(appBackupConfigQuiet + time.Minute))
	if got := appBackupCount(t, s); got != start+1 {
		t.Fatalf("a settled burst of changes is one backup: %d, want %d", got, start+1)
	}
	s.appBackupAfterConfigChange(a, time.Now().Add(2*appBackupConfigQuiet))
	if got := appBackupCount(t, s); got != start+1 {
		t.Fatalf("a covered change is not backed up twice: %d", got)
	}
}

func TestAppBackupAlerts(t *testing.T) {
	s := appBackupWatchServer(t)
	now := time.Now()
	s.checkAppBackupAlert(now)
	if n := len(alertsOfKind(t, s, notify.KindAppBackupStale)); n != 0 {
		t.Fatalf("with no container backups there is nothing to lose yet: %d alerts", n)
	}
	if err := s.store.CreateBackup(&store.Backup{ID: "b1", NodeID: "n1", TargetName: "app", Status: "success", CreatedAt: now.Unix()}); err != nil {
		t.Fatal(err)
	}
	s.checkAppBackupAlert(now)
	if n := len(alertsOfKind(t, s, notify.KindAppBackupStale)); n != 1 {
		t.Fatalf("no app backup at all must alert once there is something to lose: %d alerts", n)
	}

	if _, err := s.createAppBackup("test"); err != nil {
		t.Fatal(err)
	}
	_ = s.store.SetSetting(alertKey(notify.KindAppBackupStale, ""), strconv.FormatInt(0, 10)) // let it send again
	_ = s.store.AckAllAlerts()
	s.checkAppBackupAlert(now)
	if n := len(alertsOfKind(t, s, notify.KindAppBackupStale)); n != 1 {
		t.Fatalf("a fresh app backup is quiet: %d alerts", n)
	}
	s.checkAppBackupAlert(now.Add(9 * 24 * time.Hour))
	if n := len(alertsOfKind(t, s, notify.KindAppBackupStale)); n != 2 {
		t.Fatalf("an app backup older than eight days must alert: %d alerts", n)
	}
}

func TestIsConfigChange(t *testing.T) {
	for _, action := range []string{"settings.update", "schedule.create", "node.add", "destination.edit", "password.changed", "token.delete"} {
		if !isConfigChange(action) {
			t.Errorf("%s changes the configuration", action)
		}
	}
	for _, action := range []string{"backup.start", "schedule.runnow", "login.ok", "app.backup.create", "node.backup.all", "restore.volume"} {
		if isConfigChange(action) {
			t.Errorf("%s changes no configuration", action)
		}
	}
}
