package backup

import (
	"fmt"
	"strings"
	"testing"

	"dockback/internal/dockercli"
)

// TestParseSQLiteStats locks in the capture-side contract parse (F109), in
// particular that -1 ("too large to scan") is stored as 0 = NOT COUNTED and
// never confused with a genuine zero — the checks treat a real zero as a signal
// worth failing on.
func TestParseSQLiteStats(t *testing.T) {
	got := parseSQLiteStats("1\t41\t120345\n2\t7\t-1\n3\t0\t0\n\ngarbage\n4\ttwo\tthree\n")
	if len(got) != 4 {
		t.Fatalf("expected 4 parsed rows, got %d (%v)", len(got), got)
	}
	if got["1"].tables != 41 || got["1"].rows != 120345 {
		t.Errorf("row 1 = %+v, want 41 tables / 120345 rows", got["1"])
	}
	if got["2"].tables != 7 || got["2"].rows != 0 {
		t.Errorf("row 2 = %+v, want 7 tables and rows recorded as not-counted (0)", got["2"])
	}
	if got["3"].tables != 0 || got["3"].rows != 0 {
		t.Errorf("row 3 = %+v, want zeroes", got["3"])
	}
	// Unparseable numbers must degrade to "unknown" (0), never to a wrong value.
	if got["4"].tables != 0 || got["4"].rows != 0 {
		t.Errorf("row 4 = %+v, want zeroes for unparseable fields", got["4"])
	}
	if len(parseSQLiteStats("")) != 0 {
		t.Error("empty input must parse to no rows")
	}
}

// captureLog collects engine log lines so a test can assert on what the operator
// would actually be told.
func captureLog(lines *[]string) func(string, string, string) {
	return func(_, level, msg string) { *lines = append(*lines, level+" "+msg) }
}

// TestAssertSQLiteRestoredPass covers the happy path and the two legitimate
// non-failures: a count that came back HIGHER (a live container can gain rows
// between the overlay and the read-back) and a pre-F109 backup with no contract.
func TestAssertSQLiteRestoredPass(t *testing.T) {
	var lines []string
	e := &Engine{Log: captureLog(&lines)}
	man := &Manifest{SQLiteDumps: []SQLiteRef{
		{Source: "/config/absdatabase.sqlite", Tables: 41, Rows: 1000},
		{Source: "/config/legacy.db"}, // no contract recorded
	}}
	checks := []dockercli.SQLiteRestoreCheck{
		{Path: "/config/absdatabase.sqlite", Integrity: "ok", Tables: 41, Rows: 1200, RowsKnown: true},
		{Path: "/config/legacy.db", Integrity: "ok", Tables: 3},
	}
	if err := e.assertSQLiteRestored("b1", man, checks); err != nil {
		t.Fatalf("a matching (and a contract-free) restore must pass, got %v", err)
	}
}

// TestAssertSQLiteRestoredShortfall is the failure this feature exists for: a
// database that comes back with most of its rows missing, which an app will
// happily start on and which looks like a successful restore until someone opens
// it.
func TestAssertSQLiteRestoredShortfall(t *testing.T) {
	var lines []string
	e := &Engine{Log: captureLog(&lines)}
	man := &Manifest{SQLiteDumps: []SQLiteRef{{Source: "/config/absdatabase.sqlite", Tables: 41, Rows: 1000}}}

	err := e.assertSQLiteRestored("b1", man, []dockercli.SQLiteRestoreCheck{
		{Path: "/config/absdatabase.sqlite", Integrity: "ok", Tables: 41, Rows: 27, RowsKnown: true},
	})
	if err == nil || !strings.Contains(err.Error(), "27") {
		t.Fatalf("a row shortfall must fail the restore loudly, got %v", err)
	}
	// Loudly, and at the level the UI actually understands. This call site writes
	// "ERROR"; logf folds that to "ERR" (#N9), which is the only error level the
	// run consoles colour red and the only one that ends a run. Asserting the
	// spelling rather than the effect is what let 27 error lines render as
	// ordinary grey text for as long as they did.
	var loud bool
	for _, l := range lines {
		if strings.HasPrefix(l, "ERR ") {
			loud = true
		}
	}
	if !loud {
		t.Errorf("a failed verification must log at an error level the UI recognises, got %v", lines)
	}

	// A missing TABLE is the same class of failure.
	if err := e.assertSQLiteRestored("b1", man, []dockercli.SQLiteRestoreCheck{
		{Path: "/config/absdatabase.sqlite", Integrity: "ok", Tables: 12, Rows: 1000, RowsKnown: true},
	}); err == nil {
		t.Error("a table shortfall must fail the restore")
	}
}

// TestAssertSQLiteRestoredIntegrity: a non-"ok" integrity_check is a measured
// fact and always fatal, regardless of counts.
func TestAssertSQLiteRestoredIntegrity(t *testing.T) {
	var lines []string
	e := &Engine{Log: captureLog(&lines)}
	man := &Manifest{SQLiteDumps: []SQLiteRef{{Source: "/config/app.db", Tables: 5, Rows: 10}}}
	err := e.assertSQLiteRestored("b1", man, []dockercli.SQLiteRestoreCheck{
		{Path: "/config/app.db", Integrity: "*** in database main ***\nPage 42 is never used", Tables: 5, Rows: 10, RowsKnown: true},
	})
	if err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("a failed integrity check must fail the restore, got %v", err)
	}
}

// TestAssertSQLiteRestoredUnverifiable is the discipline that keeps the feature
// trustworthy: a check that could NOT run must never masquerade as a check that
// passed — nor as one that failed.
func TestAssertSQLiteRestoredUnverifiable(t *testing.T) {
	var lines []string
	e := &Engine{Log: captureLog(&lines)}
	man := &Manifest{SQLiteDumps: []SQLiteRef{{Source: "/config/app.db", Tables: 5, Rows: 10}}}

	// No sqlite3 in the sidecar at all: no checks come back.
	if err := e.assertSQLiteRestored("b1", man, nil); err != nil {
		t.Fatalf("an unreadable target must warn, not fail: %v", err)
	}
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "WARN ") {
		t.Errorf("expected a WARN that the restore was applied but not confirmed, got %v", lines)
	}

	// The file was found but integrity could not be read.
	lines = nil
	if err := e.assertSQLiteRestored("b1", man, []dockercli.SQLiteRestoreCheck{
		{Path: "/config/app.db", Integrity: "", Tables: -1},
	}); err != nil {
		t.Fatalf("an unreadable integrity check must warn, not fail: %v", err)
	}
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "WARN ") {
		t.Errorf("expected a WARN for the unreadable database, got %v", lines)
	}
}

// TestSQLiteLabel keeps check names short but never anonymous.
func TestSQLiteLabel(t *testing.T) {
	cases := map[string]string{
		"/config/absdatabase.sqlite": "absdatabase.sqlite",
		"app.db":                     "app.db",
		"":                           "database",
		"/trailing/":                 "/trailing/",
	}
	for in, want := range cases {
		if got := sqliteLabel(in); got != want {
			t.Errorf("sqliteLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestShortTables is the Gotify case (F124): ~115,000 rows almost entirely
// message history, so losing every one of the 32 `clients` rows — every device
// that receives notifications — is a 0.03% shortfall an aggregate can shrug off.
func TestShortTables(t *testing.T) {
	captured := map[string]int64{"users": 1, "applications": 9, "clients": 32, "messages": 115073}
	restored := map[string]int64{"users": 1, "applications": 9, "clients": 0, "messages": 115073}

	short := shortTables(captured, restored)
	if len(short) != 1 || !strings.Contains(short[0], "clients has 0 of 32") {
		t.Fatalf("the missing table must be named exactly, got %v", short)
	}

	// The case an aggregate cannot see AT ALL: one table loses rows while
	// another gains them, so the total is unchanged or higher.
	grew := map[string]int64{"users": 1, "applications": 9, "clients": 0, "messages": 115200}
	if short := shortTables(captured, grew); len(short) != 1 {
		t.Fatalf("a loss masked by growth elsewhere must still be caught, got %v", short)
	}
}

// TestShortTablesAsymmetry: MORE rows is legitimate (a live container gains them
// between the overlay and the read-back); FEWER means data is missing.
func TestShortTablesAsymmetry(t *testing.T) {
	captured := map[string]int64{"messages": 100, "users": 1}
	if short := shortTables(captured, map[string]int64{"messages": 140, "users": 1}); len(short) != 0 {
		t.Errorf("growth must never fail a restore, got %v", short)
	}
	if short := shortTables(captured, map[string]int64{"messages": 100, "users": 1}); len(short) != 0 {
		t.Errorf("an exact match must be silent, got %v", short)
	}
}

// TestShortTablesUnmeasured keeps the discipline the whole feature rests on: a
// thing that could NOT be measured must never become a failure.
func TestShortTablesUnmeasured(t *testing.T) {
	captured := map[string]int64{"users": 1, "clients": 32}
	// A pre-F124 backup, or a database too large to scan — fall through to the
	// aggregate check rather than reporting everything as missing.
	if short := shortTables(captured, nil); len(short) != 0 {
		t.Errorf("no read-back data must yield no failures, got %v", short)
	}
	if short := shortTables(nil, map[string]int64{"users": 1}); len(short) != 0 {
		t.Errorf("no captured contract must yield no failures, got %v", short)
	}
	// A table missing from the read-back is "not measured", not "zero rows".
	if short := shortTables(captured, map[string]int64{"users": 1}); len(short) != 0 {
		t.Errorf("an unmeasured table must not be reported as lost, got %v", short)
	}
	// A table captured as empty has nothing to come back short of.
	if short := shortTables(map[string]int64{"empty": 0}, map[string]int64{"empty": 0}); len(short) != 0 {
		t.Errorf("an empty table must be silent, got %v", short)
	}
}

// TestShortTablesBounded: a wholesale-wrong restore must not produce a wall of
// text, and the message must stay deterministic run to run.
func TestShortTablesBounded(t *testing.T) {
	captured, restored := map[string]int64{}, map[string]int64{}
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("t%02d", i)
		captured[name], restored[name] = 10, 0
	}
	short := shortTables(captured, restored)
	if len(short) != maxReportedShortTables+1 {
		t.Fatalf("expected %d named tables plus a summary line, got %d: %v", maxReportedShortTables, len(short), short)
	}
	if !strings.Contains(short[len(short)-1], "more table(s)") {
		t.Errorf("the overflow must be counted, not dropped: %v", short)
	}
	// Sorted, so the same failure reads the same way every time.
	if !strings.HasPrefix(short[0], "t00 ") {
		t.Errorf("the report must be deterministic (sorted), got %v", short)
	}
}

// TestAssertSQLiteRestoredNamesTheTable — the message an operator actually sees.
func TestAssertSQLiteRestoredNamesTheTable(t *testing.T) {
	var lines []string
	e := &Engine{Log: captureLog(&lines)}
	man := &Manifest{SQLiteDumps: []SQLiteRef{{
		Source: "/app/data/gotify.db", Tables: 4, Rows: 115115,
		TableRows: map[string]int64{"users": 1, "applications": 9, "clients": 32, "messages": 115073},
	}}}
	err := e.assertSQLiteRestored("b1", man, []dockercli.SQLiteRestoreCheck{{
		Path: "/app/data/gotify.db", Integrity: "ok", Tables: 4, Rows: 115115, RowsKnown: true,
		TableRows: map[string]int64{"users": 1, "applications": 9, "clients": 0, "messages": 115115},
	}})
	if err == nil {
		t.Fatal("losing every client token must fail the restore even though the total matches")
	}
	if !strings.Contains(err.Error(), "clients has 0 of 32") {
		t.Errorf("the error must name the table and the counts, got %v", err)
	}
}
