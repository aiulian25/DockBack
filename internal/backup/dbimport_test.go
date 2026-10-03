package backup

import (
	"strings"
	"testing"
)

// A database client's exit code is not a verdict: psql (no ON_ERROR_STOP) and
// mysql --force both exit 0 after failed statements. These tests pin the
// classification that turns their OUTPUT into the verdict.

func TestClassifyImportPostgres(t *testing.T) {
	// The benign preamble: pg_dumpall --globals-only re-creating roles that the
	// target cluster already has. A correct restore looks exactly like this.
	benign := `SET
CREATE ROLE
ERROR:  role "postgres" already exists
ERROR:  database "paperless" already exists
NOTICE:  table "documents_document" does not exist, skipping
DROP DATABASE
CREATE DATABASE
ALTER TABLE
COPY 12345`
	o := ClassifyImportOutput("postgres", benign)
	if !o.OK() {
		t.Fatalf("a clean restore must not be flagged: %v", o.Fatal)
	}
	if o.Benign != 2 {
		t.Fatalf("benign count = %d, want 2", o.Benign)
	}
	if o.Summary() != "" {
		t.Fatalf("a clean outcome has no summary, got %q", o.Summary())
	}

	// The failure this whole feature exists for: constraint statements that did
	// not apply, while psql still exits 0.
	bad := `CREATE TABLE
ERROR:  relation "documents_workflowtrigger" does not exist
ALTER TABLE
ERROR:  there is no unique constraint matching given keys for referenced table "documents_workflowtrigger"
COPY 42`
	o = ClassifyImportOutput("postgres", bad)
	if o.OK() {
		t.Fatal("failing statements must make the import NOT ok")
	}
	if len(o.Fatal) != 2 {
		t.Fatalf("fatal count = %d, want 2: %v", len(o.Fatal), o.Fatal)
	}
	if !strings.Contains(o.Summary(), "no unique constraint") {
		t.Fatalf("summary must name the real error: %q", o.Summary())
	}

	// A NOTICE/WARNING is never an error (…"does not exist, skipping" is what
	// --if-exists produces on a fresh target and is completely normal).
	if o := ClassifyImportOutput("postgres", "NOTICE:  x\nWARNING:  y\nDETAIL:  z"); !o.OK() || o.Benign != 0 {
		t.Fatalf("notices/warnings must not count: %+v", o)
	}

	// An "already exists" on a TABLE is NOT benign — our dumps are
	// --clean --if-exists, so a surviving table means the drop didn't happen.
	if o := ClassifyImportOutput("postgres", `ERROR:  relation "documents_document" already exists`); o.OK() {
		t.Fatal("a table that already exists must be treated as a real failure")
	}
}

func TestClassifyImportMySQL(t *testing.T) {
	bad := "ERROR 1064 (42000) at line 812: You have an error in your SQL syntax\nERROR 1146 (42S02) at line 900: Table 'app.x' doesn't exist"
	o := ClassifyImportOutput("mysql", bad)
	if o.OK() || len(o.Fatal) != 2 {
		t.Fatalf("mysql errors must be fatal: %+v", o)
	}
	if o := ClassifyImportOutput("mysql", "ERROR 1007 (HY000) at line 1: Can't create database 'app'; database 'app' already exists"); !o.OK() || o.Benign != 1 {
		t.Fatalf("an already-exists database must be benign: %+v", o)
	}
	// Engines with no machine-readable error convention keep exit-code-only
	// semantics — never a false failure from noisy output.
	for _, eng := range []string{"mongodb", "redis", ""} {
		if o := ClassifyImportOutput(eng, "ERROR: something\nERROR 1064 at line 2: x"); !o.OK() {
			t.Fatalf("engine %q must rely on the exit code alone, got %v", eng, o.Fatal)
		}
	}
	if o := ClassifyImportOutput("postgres", ""); !o.OK() || o.Benign != 0 {
		t.Fatal("empty output is a clean outcome")
	}
}

// The reported summary is capped so a dump that fails wholesale can't dump a
// wall of text into the run log / error, but still says how many were hidden.
func TestImportSummaryCaps(t *testing.T) {
	var b strings.Builder
	for i := 0; i < maxReportedImportErrors+7; i++ {
		b.WriteString("ERROR:  statement " + string(rune('a'+i)) + " failed\n")
	}
	o := ClassifyImportOutput("postgres", b.String())
	if len(o.Fatal) != maxReportedImportErrors+7 {
		t.Fatalf("all errors must be counted, got %d", len(o.Fatal))
	}
	s := o.Summary()
	if !strings.Contains(s, "and 7 more") {
		t.Fatalf("summary must report the hidden remainder: %q", s)
	}
	if strings.Count(s, "ERROR:") != maxReportedImportErrors {
		t.Fatalf("summary must show exactly %d errors: %q", maxReportedImportErrors, s)
	}
}

// mongorestore exits 0 even when documents fail to restore, so its own count is
// the verdict — and it must be read as a NUMBER: every successful restore also
// prints "Failed: 0", and treating that as an error would fail every MongoDB
// restore (F99).
func TestClassifyImportMongo(t *testing.T) {
	bad := strings.Join([]string{
		"2026-07-26T10:00:00.000+0000\tpreparing collections to restore from",
		"2026-07-26T10:00:01.000+0000\trestoring app.documents from archive",
		"2026-07-26T10:00:02.000+0000\tFailed: 3",
		"2026-07-26T10:00:03.000+0000\t1200 document(s) restored successfully. 3 document(s) failed to restore.",
	}, "\n")
	o := ClassifyImportOutput("mongodb", bad)
	if o.OK() {
		t.Fatal("documents that failed to restore must be fatal")
	}
	if len(o.Fatal) != 2 {
		t.Fatalf("both the per-collection and summary failures should be caught, got %v", o.Fatal)
	}

	good := strings.Join([]string{
		"2026-07-26T10:00:00.000+0000\tpreparing collections to restore from",
		"2026-07-26T10:00:02.000+0000\tfinished restoring app.documents (1200 documents, 0 failures)",
		"2026-07-26T10:00:02.000+0000\tFailed: 0",
		"2026-07-26T10:00:03.000+0000\t1200 document(s) restored successfully. 0 document(s) failed to restore.",
		"2026-07-26T10:00:03.000+0000\tdone",
	}, "\n")
	if o := ClassifyImportOutput("mongodb", good); !o.OK() {
		t.Fatalf("a clean mongorestore must not be flagged: %v", o.Fatal)
	}
}

// redis-cli prefixes a server rejection with (error) or -ERR.
func TestClassifyImportRedis(t *testing.T) {
	o := ClassifyImportOutput("redis", "OK\n(error) ERR wrong number of arguments\nOK\n")
	if o.OK() || len(o.Fatal) != 1 {
		t.Fatalf("a redis error line must be fatal: %v", o.Fatal)
	}
	if o := ClassifyImportOutput("redis", "OK\nOK\nPONG\n"); !o.OK() {
		t.Fatalf("clean redis output must not be flagged: %v", o.Fatal)
	}
}

// An engine with no error convention still falls through untouched, so adding
// cases above cannot have changed what other engines report.
func TestClassifyImportUnknownEngineStaysExitCodeOnly(t *testing.T) {
	if o := ClassifyImportOutput("cassandra", "ERROR: everything is broken\n"); !o.OK() {
		t.Fatalf("an unhandled engine must stay exit-code-only: %v", o.Fatal)
	}
}
