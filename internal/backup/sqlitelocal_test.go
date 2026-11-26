package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// SEC-004: a container's database file is opened as untrusted input.

func TestUntrustedSQLiteDSN(t *testing.T) {
	got := untrustedSQLiteDSN("/work/d", "query_only(1)")
	want := "/work/d?_pragma=trusted_schema(0)&_pragma=cell_size_check(1)&_pragma=query_only(1)"
	if got != want {
		t.Fatalf("dsn = %q, want %q", got, want)
	}
}

func TestContainerDatabasesAreOpenedAsUntrusted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d")
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`CREATE TABLE notes(body TEXT); INSERT INTO notes VALUES('a'),('b')`); err != nil {
		t.Fatal(err)
	}
	seed.Close()

	db, err := openSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for pragma, want := range map[string]int{"trusted_schema": 0, "cell_size_check": 1, "query_only": 1} {
		var got int
		if err := db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil || got != want {
			t.Errorf("PRAGMA %s = %d (%v), want %d", pragma, got, err, want)
		}
	}

	// An ordinary database still snapshots, checks and counts as before.
	snapshot := filepath.Join(t.TempDir(), "d.dbk")
	if err := vacuumInto(path, snapshot); err != nil {
		t.Fatalf("vacuum: %v", err)
	}
	if verdict := sqliteIntegrity(snapshot); verdict != sqliteIntegrityOK {
		t.Fatalf("integrity = %q", verdict)
	}
	if tables, rows, _ := countSQLite(snapshot); tables != 1 || rows != 2 {
		t.Fatalf("tables=%d rows=%d, want 1 and 2", tables, rows)
	}
}

// The time limits only mean something if the engine really stops when its time is
// up. A query that never ends by itself must come back at the deadline.
func TestAStatementStopsAtItsTimeLimit(t *testing.T) {
	db, err := openSnapshot(filepath.Join(t.TempDir(), "empty"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const limit = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	started := time.Now()
	var count int64
	err = db.QueryRowContext(ctx, `WITH RECURSIVE forever(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM forever) SELECT count(*) FROM forever`).Scan(&count)
	if err == nil {
		t.Fatal("an endless query must fail at its time limit")
	}
	if took := time.Since(started); took > 20*limit {
		t.Fatalf("the query ran %s past a %s limit", took, limit)
	}
}
