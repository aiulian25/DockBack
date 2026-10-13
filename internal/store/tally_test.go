package store

import (
	"path/filepath"
	"testing"
)

// The status summary a dashboard widget shows is counted straight from the
// catalog: every entry the Backups page lists, the failures of the last day,
// and the targets whose newest failure no later success has made good.
func TestTallyAndFailingTargets(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, b := range []*Backup{
		{ID: "app-ok", NodeID: "n1", Stack: "blog", TargetName: "app", Status: "success", CreatedAt: 100, SizeBytes: 1000},
		{ID: "app-fail", NodeID: "n1", Stack: "blog", TargetName: "app", Status: "failed", CreatedAt: 200},
		{ID: "db-fail", NodeID: "n1", TargetName: "db", Status: "failed", CreatedAt: 150},
		{ID: "db-ok", NodeID: "n1", TargetName: "db", Status: "success", CreatedAt: 300, SizeBytes: 2000},
		{ID: "new-fail", NodeID: "n1", TargetName: "new", Status: "failed", CreatedAt: 250},
		{ID: "web-run", NodeID: "n2", TargetName: "web", Status: "running", CreatedAt: 400},
		{ID: "old-fail", NodeID: "n2", TargetName: "old", Status: "failed", CreatedAt: 50},
		{ID: "tie-ok", NodeID: "n2", TargetName: "tie", Status: "success", CreatedAt: 500, SizeBytes: 30},
		{ID: "tie-fail", NodeID: "n2", TargetName: "tie", Status: "failed", CreatedAt: 500},
	} {
		if err := st.CreateBackup(b); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateBackup(b); err != nil {
			t.Fatal(err)
		}
	}

	tally, err := st.TallyBackups(120)
	if err != nil {
		t.Fatal(err)
	}
	want := BackupTally{Total: 9, Bytes: 3030, FailedSince: 4, Running: 1, LastSuccess: 500}
	if tally != want {
		t.Errorf("tally %+v, want %+v", tally, want)
	}

	failing, err := st.FailingTargets()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range failing {
		names = append(names, f.NodeID+"/"+f.TargetName)
	}
	if len(failing) != 3 || names[0] != "n1/new" || names[1] != "n1/app" || names[2] != "n2/old" {
		t.Fatalf("failing targets, newest failure first, without the ones a later or same-second success made good: %v", names)
	}
	if failing[1].Stack != "blog" || failing[1].FailedAt != 200 {
		t.Errorf("a failing target carries its stack and when it failed: %+v", failing[1])
	}
}
