package backup

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dockback/internal/dockercli"
)

// The shipped sidecar has no sqlite3, so no database was ever snapshotted,
// checked or counted by default. DockBack now does it with its own engine, from
// the raw files a paused app left — rows still in the write-ahead log included.
func TestDockBackSnapshotsSQLiteItself(t *testing.T) {
	src := filepath.Join(t.TempDir(), "app.db")
	live, err := sql.Open("sqlite", src+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	live.SetMaxOpenConns(1)
	for _, stmt := range []string{
		`CREATE TABLE users (name TEXT)`, `CREATE TABLE "odd ""table"" name" (x)`,
		`INSERT INTO users VALUES ('a'), ('b'), ('c')`, `INSERT INTO "odd ""table"" name" VALUES (1), (2)`,
	} {
		if _, err := live.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}

	dir := t.TempDir()
	copyFile(t, src, filepath.Join(dir, "raw-1.db"))
	copyFile(t, src+"-wal", filepath.Join(dir, "raw-1.db-wal")) // the rows are only here until a checkpoint
	live.Close()
	if err := os.WriteFile(filepath.Join(dir, "raw-2.db"), []byte("not a database at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sqliteRawIndex), []byte("1\t/data/app.db\n2\t/data/broken.db\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	snapshotSQLiteLocally(dir, true)

	if got := readFileString(filepath.Join(dir, "index.txt")); got != "1\t/data/app.db\n" {
		t.Fatalf("only the good database is indexed, got %q", got)
	}
	if got := readFileString(filepath.Join(dir, "stats.txt")); got != "1\t2\t5\n" {
		t.Errorf("two tables, five rows — the WAL's rows included: %q", got)
	}
	rows := dockercli.ParseSQLiteTableRows(readFileString(filepath.Join(dir, "rows-1.txt")))
	if rows["'users'"] != 3 || rows[`'odd "table" name'`] != 2 {
		t.Errorf("per-table rows keyed the way the sidecar keys them: %v", rows)
	}
	if verdict := sqliteIntegrity(filepath.Join(dir, "1.dbk")); verdict != sqliteIntegrityOK {
		t.Errorf("the snapshot must check clean: %s", verdict)
	}
	if got := readFileString(filepath.Join(dir, "failed.txt")); !strings.HasPrefix(got, "snapshot\t/data/broken.db\t") {
		t.Errorf("a file that is not a database is a failed snapshot, not a corrupt one: %q", got)
	}
	for _, leftover := range []string{"raw-1.db", "raw-1.db-wal", "raw-2.db", "stage-1", "2.dbk", sqliteRawIndex} {
		if _, err := os.Stat(filepath.Join(dir, leftover)); err == nil {
			t.Errorf("%s must not be left behind", leftover)
		}
	}
}

// A failed integrity check on a copy taken while the app was writing can be a
// torn copy, so it must not be reported as a corrupt database — that verdict
// can fail the backup. Pure.
func TestIntegrityFailureMeansCorruptOnlyWhenQuiesced(t *testing.T) {
	if integrityFailureKind(true) != "corrupt" || integrityFailureDetail("row 3 missing", true) != "row 3 missing" {
		t.Error("a copy nothing could write to that fails its check is a corrupt database")
	}
	if integrityFailureKind(false) != "snapshot" || !strings.Contains(integrityFailureDetail("row 3 missing", false), "says nothing about the database") {
		t.Error("a live copy that fails its check is only a failed snapshot")
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
