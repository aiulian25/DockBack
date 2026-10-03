package dockercli

import "testing"

// TestParseSQLiteRestoreChecks locks in the restore-side read-back parse (F109).
//
// The distinction that matters most: an UNPARSEABLE line is skipped entirely
// rather than guessed at, because a malformed line becoming either a false pass
// or a false failure is worse than no result at all.
func TestParseSQLiteRestoreChecks(t *testing.T) {
	out := "some unrelated sidecar noise\n" +
		"DBCHK\t/config/absdatabase.sqlite\tok\t41\t120345\n" +
		"DBCHK\t/config/big.db\tok\t9\t-1\n" +
		"DBCHK\t/config/bad.db\t*** in database main ***\t5\t10\n" +
		"DBCHK\t/config/unreadable.db\t\t-1\t-1\n" +
		"DBCHK\tmalformed-too-few-fields\n" +
		"DBCHK\t\tok\t1\t1\n"

	got := parseSQLiteRestoreChecks(out)
	if len(got) != 4 {
		t.Fatalf("expected 4 parsed checks, got %d: %+v", len(got), got)
	}

	if g := got[0]; g.Path != "/config/absdatabase.sqlite" || g.Integrity != "ok" || g.Tables != 41 || g.Rows != 120345 || !g.RowsKnown {
		t.Errorf("first check parsed wrong: %+v", g)
	}
	// -1 rows means "not counted" — RowsKnown must be false so a comparison is
	// skipped rather than run against a bogus zero.
	if g := got[1]; !(g.Tables == 9 && g.Rows == 0 && !g.RowsKnown) {
		t.Errorf("an uncounted row total must read as unknown, got %+v", g)
	}
	if g := got[2]; g.Integrity == "ok" {
		t.Errorf("a corrupt database must not report ok: %+v", g)
	}
	// Unreadable: empty integrity AND unknown counts — the caller turns this into
	// a warning, never a pass or a failure.
	if g := got[3]; g.Integrity != "" || g.Tables != -1 || g.RowsKnown {
		t.Errorf("an unreadable database must report nothing measured, got %+v", g)
	}

	if len(parseSQLiteRestoreChecks("")) != 0 {
		t.Error("empty output must parse to no checks")
	}
}

// TestSQLiteStatsScriptShape is a cheap guard on the generated shell: the row
// count must be size-capped, and table names must be double-quote-escaped so a
// table called `weird"name` cannot break (or inject into) the generated query.
func TestSQLiteStatsScriptShape(t *testing.T) {
	if !contains(sqliteStatsScript, maxSQLiteRowCountBytesStr) {
		t.Error("the row count must be capped by size — an uncapped full scan would add minutes per backup")
	}
	if !contains(sqliteStatsScript, `replace(name,'\"','\"\"')`) {
		t.Error("table names must be double-quote-escaped inside the generated query")
	}
	if !contains(sqliteStatsScript, "stats.txt") {
		t.Error("stats must be written to stats.txt, kept separate from index.txt so an older archive still parses")
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// TestParseTableRow covers the per-table line format (F124). A table name may
// legitimately contain a tab, so the COUNT is taken from the last field rather
// than the name from the first.
func TestParseTableRow(t *testing.T) {
	if n, c, ok := parseTableRow("TBLROW\tapplications\t9"); !ok || n != "applications" || c != 9 {
		t.Errorf("got (%q,%d,%v)", n, c, ok)
	}
	if n, c, ok := parseTableRow("TBLROW\tweird\"name\t1"); !ok || n != `weird"name` || c != 1 {
		t.Errorf("a quoted table name must survive: (%q,%d,%v)", n, c, ok)
	}
	if n, _, ok := parseTableRow("TBLROW\thas\ttab\t3"); !ok || n != "has\ttab" {
		t.Errorf("the count is the LAST field, so a tabbed name stays intact: (%q,%v)", n, ok)
	}
	for _, bad := range []string{"", "DBCHK\t/a\tok\t1\t1", "TBLROW\tonly", "TBLROW\t\t5", "TBLROW\tt\tx", "TBLROW\tt\t-1"} {
		if _, _, ok := parseTableRow(bad); ok {
			t.Errorf("%q must not parse", bad)
		}
	}
}

// TestParseSQLiteRestoreChecksAttachesTableRows: a TBLROW belongs to the DBCHK
// above it, so two databases in one stream never mix their tables.
func TestParseSQLiteRestoreChecksAttachesTableRows(t *testing.T) {
	got := parseSQLiteRestoreChecks(
		"TBLROW\torphan\t1\n" + // before any DBCHK — dropped, never guessed at
			"DBCHK\t/app/data/gotify.db\tok\t4\t60\n" +
			"TBLROW\tusers\t1\nTBLROW\tapplications\t9\n" +
			"DBCHK\t/config/other.db\tok\t1\t5\n" +
			"TBLROW\tstuff\t5\n")
	if len(got) != 2 {
		t.Fatalf("expected 2 checks, got %d: %+v", len(got), got)
	}
	if len(got[0].TableRows) != 2 || got[0].TableRows["applications"] != 9 {
		t.Errorf("first database's tables wrong: %+v", got[0].TableRows)
	}
	if _, leaked := got[0].TableRows["stuff"]; leaked {
		t.Error("a later database's table must not attach to an earlier one")
	}
	if len(got[1].TableRows) != 1 || got[1].TableRows["stuff"] != 5 {
		t.Errorf("second database's tables wrong: %+v", got[1].TableRows)
	}
	if _, leaked := got[0].TableRows["orphan"]; leaked {
		t.Error("a TBLROW with no preceding DBCHK must be dropped")
	}
}

func TestParseSQLiteTableRows(t *testing.T) {
	got := ParseSQLiteTableRows("TBLROW\tusers\t1\nTBLROW\tclients\t32\n\nnoise\n")
	if len(got) != 2 || got["users"] != 1 || got["clients"] != 32 {
		t.Errorf("got %+v", got)
	}
	if ParseSQLiteTableRows("") != nil {
		t.Error("empty input must produce nil, not an empty map")
	}
}

// TestSQLiteTableRowsScriptShape guards the generated SQL: table names are
// escaped, the work is capped, and the prefix comes from SQL rather than from
// whichever sed the sidecar image happens to ship.
func TestSQLiteTableRowsScriptShape(t *testing.T) {
	if !contains(sqliteTableRowsScript, `replace(name,'\"','\"\"')`) {
		t.Error("table names must be double-quote-escaped inside the generated query")
	}
	if !contains(sqliteTableRowsScript, maxSQLiteTablesStr) {
		t.Error("the per-table scan must be capped")
	}
	if !contains(sqliteTableRowsScript, "TBLROW") {
		t.Error("the prefix must be emitted by the SQL itself")
	}
	for _, tool := range []string{"sed ", "awk "} {
		if contains(sqliteTableRowsScript, tool) {
			t.Errorf("the script must not depend on %s being present in the sidecar", tool)
		}
	}
}

// TestTarWithExcludes (F126). The fallback is the point: --exclude is a GNU tar
// option that Alpine's busybox provides and other busybox builds reject
// outright, and the sidecar image is user-configurable. Passing it blindly would
// make tar exit with a usage error and take the ENTIRE volume capture with it —
// trading a redundant directory for no backup at all.
func TestTarWithExcludes(t *testing.T) {
	cmd := tarWithExcludes([]string{"/config"}, []string{"/config/postgres"})
	if len(cmd) != 3 || cmd[0] != "/bin/sh" {
		t.Fatalf("an exclusion needs a shell for the probe, got %v", cmd)
	}
	s := cmd[2]
	// Probe, both branches, and an honest message when it degrades.
	if !contains(s, "--exclude=probe") {
		t.Error("the option must be PROBED before it is relied on")
	}
	if !contains(s, "does not support --exclude") {
		t.Error("degrading to a full capture must say so — a silent difference in what was captured is the worst outcome")
	}
	if !contains(s, "--exclude='config/postgres'") || !contains(s, "--exclude='config/postgres/*'") {
		t.Errorf("both the directory and its contents must be excluded: %q", s)
	}
	// Leading slashes stripped so the patterns match the relative members.
	if contains(s, "--exclude='/config") {
		t.Errorf("exclude patterns must be relative, got %q", s)
	}
	if !contains(s, "'config'") {
		t.Errorf("the member list must still be present: %q", s)
	}

	// No excludes: the plain argv form, byte-identical to before this existed.
	plain := tarWithExcludes([]string{"/config"}, nil)
	if len(plain) != 3 || contains(plain[2], "--exclude") {
		t.Errorf("with nothing to exclude there must be no exclusion at all, got %v", plain)
	}
	// Blank entries are not excludes and must not produce an empty pattern that
	// would match everything.
	blank := tarWithExcludes([]string{"/config"}, []string{"", "   ", "/"})
	if contains(blank[2], "--exclude") {
		t.Errorf("blank exclusions must be dropped, not turned into a pattern: %v", blank)
	}
}

// TestParseSQLiteRestoreChecksAcceptsBothShapes (F134). The DBCHK line gained a
// checksum field; a sidecar image mid-upgrade still emits the four-field form,
// and dropping those checks entirely would be worse than the missing hash.
func TestParseSQLiteRestoreChecksAcceptsBothShapes(t *testing.T) {
	got := parseSQLiteRestoreChecks(
		"DBCHK\t/data/old.db\tok\t20\t8105\n" + // pre-F134: four fields
			"DBCHK\t/data/new.db\tok\t20\t8105\tdeadbeef\n" + // current: five
			"DBCHK\t/data/nohash.db\tok\t20\t8105\t-\n") // hashing failed
	if len(got) != 3 {
		t.Fatalf("all three shapes must parse, got %d: %+v", len(got), got)
	}
	if got[0].SHA256 != "" {
		t.Errorf("the four-field form carries no hash, got %q", got[0].SHA256)
	}
	if got[0].Tables != 20 || got[0].Rows != 8105 || !got[0].RowsKnown {
		t.Errorf("the four-field form must still yield its counts: %+v", got[0])
	}
	if got[1].SHA256 != "deadbeef" {
		t.Errorf("got %q", got[1].SHA256)
	}
	// "-" is the sidecar's marker for "could not hash" and must never be read as
	// a digest — comparing against it would fail every restore.
	if got[2].SHA256 != "" {
		t.Errorf(`"-" must not be treated as a hash, got %q`, got[2].SHA256)
	}
}
