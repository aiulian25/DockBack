package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// Deleting a backup removes three things: the catalog row, its drill result and
// its persisted run log. Done as three separate statements, a failure part-way
// left the row gone and its dependants behind — orphans keyed to a backup
// nothing can look up — or a row still listed for an archive whose dependants
// were already removed.
func TestDeleteBackupIsAllOrNothing(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	b := &Backup{ID: "b1", NodeID: "n1", TargetName: "app", Status: "success", CreatedAt: 1000}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDrill("b1", true, "clean", 1000); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendRunLog("b1", "INFO", "started"); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteBackup("b1"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// All three gone together.
	if _, err := st.GetBackup("b1"); err == nil {
		t.Error("the catalog row must be gone")
	}
	var drills, logs int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM restore_drills WHERE backup_id=?`, "b1").Scan(&drills); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM run_logs WHERE backup_id=?`, "b1").Scan(&logs); err != nil {
		t.Fatal(err)
	}
	if drills != 0 {
		t.Errorf("%d drill row(s) left pointing at a backup that no longer exists", drills)
	}
	if logs != 0 {
		t.Errorf("%d run-log line(s) left pointing at a backup that no longer exists", logs)
	}

	// Deleting something that is not there is not an error, and must not disturb
	// anything else.
	other := &Backup{ID: "b2", NodeID: "n1", TargetName: "app", Status: "success", CreatedAt: 2000}
	if err := st.CreateBackup(other); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteBackup("nope"); err != nil {
		t.Errorf("deleting an unknown backup should be a no-op: %v", err)
	}
	if _, err := st.GetBackup("b2"); err != nil {
		t.Error("an unrelated backup must survive")
	}
}

// A schema change that cannot be applied must stop startup, not be discarded.
// The old behaviour surfaced hours later as an unrelated "no such column" from
// whatever query needed it next.
func TestMigrateReportsWhatItCouldNotDo(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// A second run over an already-migrated database is clean: every column is
	// present, so nothing is attempted and nothing fails.
	if err := st.migrate(); err != nil {
		t.Fatalf("re-running the migration on an up-to-date database must be clean: %v", err)
	}

	// The one benign failure is a column that already exists — which is what a
	// race between two openers produces, and must not stop either of them.
	_, err = st.db.Exec(`ALTER TABLE backups ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0`)
	if err == nil {
		t.Fatal("expected the duplicate-column error this tolerance exists for")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
		t.Fatalf("the tolerated message changed: %v — the migration would now abort on a benign race", err)
	}
}
