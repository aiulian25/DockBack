package api

import (
	"encoding/json"
	"testing"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
)

// F80: a stack-panel row must report exactly the values the container's own
// page reads — same settings keys, same defaults — so the two can't disagree.
func TestStackServiceOptionsFor(t *testing.T) {
	st := testStore(t)
	s := &Server{store: st, engine: &backup.Engine{Store: st, Log: func(string, string, string) {}}}

	c := &dockercli.Container{
		ID: "c1", Name: "web", Service: "web", Stack: "blog", State: "running", Image: "nginx:1",
		Mounts: []dockercli.Mount{
			{Type: "volume", Name: "cfg", Source: "cfg", Destination: "/config", RW: true},
			{Type: "bind", Source: "/srv/blog/data", Destination: "/data", RW: true},
		},
	}

	// Nothing seeded: container-page defaults — pause, balanced, default selection.
	row := s.stackServiceOptionsFor("n1", c, nil)
	if row.PauseMode != backup.PausePause {
		t.Fatalf("default pause mode must match the container page (pause), got %q", row.PauseMode)
	}
	if row.BackupOptions.Compression != "balanced" || row.BackupOptions.SaveImage || row.BackupOptions.Incremental {
		t.Fatalf("default options must be balanced/off: %+v", row.BackupOptions)
	}
	if row.MountsSelected != -1 || row.MountsTotal != 2 {
		t.Fatalf("default selection must report -1 of 2, got %d of %d", row.MountsSelected, row.MountsTotal)
	}
	if row.IsDatabase || row.Engine != "" {
		t.Fatalf("nginx must not be a database: %+v", row)
	}

	// Seed the SAME settings the container page writes; the row must mirror them.
	js, _ := json.Marshal(backup.SavedBackupOptions{Compression: "xz", SaveImage: true, Incremental: true, IncrementalFullEvery: 5})
	if err := st.SetSetting(backup.BackupOptionsKey("n1", "web"), string(js)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(backup.PauseModeKey("n1", "web"), backup.PauseStop); err != nil {
		t.Fatal(err)
	}
	sel, _ := json.Marshal([]string{"/config"})
	_ = st.SetSetting("mounts.n1.web", string(sel))

	row = s.stackServiceOptionsFor("n1", c, nil)
	if row.PauseMode != backup.PauseStop {
		t.Fatalf("pause mode not mirrored: %q", row.PauseMode)
	}
	if row.BackupOptions.Compression != "xz" || !row.BackupOptions.SaveImage || !row.BackupOptions.Incremental || row.BackupOptions.IncrementalFullEvery != 5 {
		t.Fatalf("backup options not mirrored: %+v", row.BackupOptions)
	}
	if row.MountsSelected != 1 || row.MountsTotal != 2 {
		t.Fatalf("stored selection must report 1 of 2, got %d of %d", row.MountsSelected, row.MountsTotal)
	}

	// A database image gets the engine chip.
	db := &dockercli.Container{ID: "c2", Name: "db", Service: "db", Stack: "blog", State: "running", Image: "postgres:16"}
	if r := s.stackServiceOptionsFor("n1", db, nil); !r.IsDatabase || r.Engine != "postgres" {
		t.Fatalf("postgres must be flagged as a database: %+v", r)
	}
}
