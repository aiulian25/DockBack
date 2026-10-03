package backup

import (
	"strings"
	"testing"
)

// R4 §Issue 31 cost seven false table mismatches out of eight. These assertions
// are the two rules that came out of it, pinned as text so a future edit to the
// query cannot quietly reintroduce either.
func TestDBHashSQL(t *testing.T) {
	t.Run("rule 1: temporal and locale-formatted columns are never rendered as text", func(t *testing.T) {
		q := pgTableHashQuery
		if !strings.Contains(q, `'extract(epoch from ' || quote_ident(a.attname) || ')::text'`) {
			t.Error("timestamps must be hashed as epoch — text rendering follows the session TimeZone")
		}
		for _, typ := range []string{"timestamptz", "timestamp", "date", "time", "timetz"} {
			if !strings.Contains(q, "'"+typ+"'") {
				t.Errorf("%s renders differently under a different TimeZone or DateStyle and must be projected as epoch", typ)
			}
		}
		if !strings.Contains(q, `'money'::regtype`) || !strings.Contains(q, "::numeric::text") {
			t.Error("money renders under lc_monetary and must go through numeric")
		}
		// The exact thing R4 measured: `md5(string_agg(t::text …))`.
		if strings.Contains(q, "t::text") || strings.Contains(q, "(t.*)::text") {
			t.Error("whole-row ::text is the bug that produced 7 false mismatches out of 8")
		}
	})

	t.Run("rule 2: the aggregate's order is byte-order, not collation order", func(t *testing.T) {
		q := pgTableHashQuery
		if !strings.Contains(q, `ORDER BY r COLLATE "C"`) {
			t.Error("without an explicit C collation, a cluster initialised with a different collation hashes identical data differently")
		}
		// Ordering by ctid would differ on a restored table; ordering by a textual
		// primary key reintroduces exactly the collation dependence above.
		if strings.Contains(q, "ctid") {
			t.Error("ctid is not stable across a restore")
		}
		if strings.Contains(q, "indisprimary") {
			t.Error("a textual primary key orders by collation; the row text under COLLATE \"C\" needs no key at all")
		}
	})

	t.Run("the generated statement is per table, executed in one round trip", func(t *testing.T) {
		q := pgTableHashQuery
		for _, want := range []string{
			"pg_class", "pg_namespace", "pg_attribute",
			`c.relkind = 'r'`,
			`n.nspname NOT IN ('pg_catalog','information_schema')`,
			"NOT a.attisdropped",
			`\gexec`,
			tableHashPrefix,
		} {
			if !strings.Contains(q, want) {
				t.Errorf("query must contain %q", want)
			}
		}
		// Identifiers are quoted by the server, so a table called `weird"name`
		// can neither break nor inject into the generated statement.
		if strings.Count(q, "quote_ident(") < 3 {
			t.Error("every identifier interpolated into generated SQL must go through quote_ident")
		}
	})

	t.Run("the postgres pass is bounded and covers every reachable database", func(t *testing.T) {
		script := pgTableHashScript(nil)
		for _, want := range []string{
			"FROM pg_database WHERE datallowconn",
			"template0", "template1",
			"SET statement_timeout = '" + pgHashStatementTimeout + "'",
			"ON_ERROR_STOP=1",
			"<<'DOCKBACKHASH'",
		} {
			if !strings.Contains(script, want) {
				t.Errorf("script must contain %q", want)
			}
		}
		// The query is full of quotes and backslashes and \gexec is a psql
		// meta-command, so it must travel on stdin through a QUOTED heredoc.
		if strings.Contains(script, `-c "`+pgTableHashQuery) {
			t.Error("the query must not be passed with -c")
		}
	})

	t.Run("mysql asks the server for its own checksum", func(t *testing.T) {
		script := mysqlTableHashScript()
		if !strings.Contains(script, "CHECKSUM TABLE $q EXTENDED") {
			t.Error("EXTENDED reads every row; the fast form is only a live-table shortcut")
		}
		if !strings.Contains(script, "table_type='BASE TABLE'") {
			t.Error("views have no content of their own to hash")
		}
		for _, sys := range []string{"mysql", "information_schema", "performance_schema", "sys"} {
			if !strings.Contains(script, "'"+sys+"'") {
				t.Errorf("system schema %s must be excluded", sys)
			}
		}
	})

	// A table name read back from information_schema is attacker-chosen data:
	// anyone who can CREATE TABLE in any application database on the server picks
	// it. Interpolated bare into the next statement, it ran as the dump user —
	// root whenever the image supplies a root password.
	t.Run("mysql identifiers are quoted by the server, never interpolated bare", func(t *testing.T) {
		script := mysqlTableHashScript()

		// The checksum runs against the QUOTED column, not the display key.
		if strings.Contains(script, "CHECKSUM TABLE $t ") {
			t.Error("$t is the raw name from information_schema — running it as SQL is the injection")
		}
		// The server does the quoting, doubling any backtick inside a name.
		for _, col := range []string{"table_schema", "table_name"} {
			want := "replace(" + col + ",'\\`','\\`\\`')"
			if !strings.Contains(script, want) {
				t.Errorf("%s must be escaped by the server: missing %s", col, want)
			}
		}
		// Every backtick reaching the shell must be escaped, or the double-quoted
		// argument would run it as a command substitution instead of sending it.
		for i, r := range script {
			if r == '`' && (i == 0 || script[i-1] != '\\') {
				t.Fatalf("unescaped backtick at %d would become a shell command substitution: %s", i, script[max(0, i-40):i+10])
			}
		}
		// Both columns are read, split on the tab that batch mode emits.
		if !strings.Contains(script, `while IFS="$(printf '\t')" read -r t q; do`) {
			t.Error("the display key and the quoted identifier must be read as two tab-separated columns")
		}
		// The manifest key is unchanged, so old and new backups still compare.
		if !strings.Contains(script, "SELECT concat(table_schema,'.',table_name),") {
			t.Error("the first column must stay schema.table — it is the manifest key")
		}
	})

	t.Run("engines with no table concept have no builder", func(t *testing.T) {
		for _, engine := range []string{"postgres", "mysql"} {
			if _, ok := tableHashScriptFor(engine, nil); !ok {
				t.Errorf("%s must have a builder", engine)
			}
		}
		for _, engine := range []string{"mongodb", "redis", ""} {
			if _, ok := tableHashScriptFor(engine, nil); ok {
				t.Errorf("%s has no tables to hash", engine)
			}
		}
	})

	t.Run("parse", func(t *testing.T) {
		got := parseTableHashes("TBLHASH|app.public.docs|4fabeb373dbc2174fc3205078905ff83\n" +
			"TBLHASH|app.public.keyless|4ca6d764a2c397edbedd1615d980ead7\n" +
			"NOTICE: something\n" +
			"TBLHASH|app.public.empty|-\n")
		if len(got) != 2 {
			t.Fatalf("got %v", got)
		}
		if got["app.public.docs"] != "4fabeb373dbc2174fc3205078905ff83" {
			t.Errorf("docs = %q", got["app.public.docs"])
		}
		// '-' is the marker for a table with no rows to aggregate; recording it as
		// a hash would make an empty table compare unequal to another empty one.
		if _, ok := got["app.public.empty"]; ok {
			t.Error("an empty table must record no hash")
		}
		// A table name containing the separator still parses: the hash is taken
		// from the right.
		odd := parseTableHashes("TBLHASH|app.public.we|ird|deadbeef\n")
		if odd["app.public.we|ird"] != "deadbeef" {
			t.Errorf("a name containing the separator must survive: %v", odd)
		}
	})
}

func TestCompareTableHashes(t *testing.T) {
	captured := map[string]string{
		"app.public.documents_document": "aaa",
		"app.public.auth_user":          "bbb",
		"app.public.django_session":     "ccc",
	}

	t.Run("a perfect restore is clean and counts every table", func(t *testing.T) {
		v := CompareTableHashes(captured, map[string]string{
			"app.public.documents_document": "aaa",
			"app.public.auth_user":          "bbb",
			"app.public.django_session":     "ccc",
		})
		if !v.Clean() || len(v.Identical) != 3 || v.Compared() != 3 {
			t.Errorf("got %+v", v)
		}
	})

	t.Run("R4 §31's real outcome: runtime state differs, durable data does not", func(t *testing.T) {
		// The corrected comparison was 73/74, "the single remaining difference
		// being genuine runtime state". With the session table declared volatile
		// that must read as a clean restore; without the declaration it must still
		// be reported, just not as a failure.
		v := CompareTableHashes(captured, map[string]string{
			"app.public.documents_document": "aaa",
			"app.public.auth_user":          "bbb",
			"app.public.django_session":     "written-since",
		})
		if len(v.Identical) != 2 || len(v.Different) != 1 || v.Different[0] != "app.public.django_session" {
			t.Fatalf("got %+v", v)
		}
		// Declared volatile by bare name — a profile cannot know which database
		// the table landed in.
		if s := v.Split(map[string]bool{"django_session": true}); !s.Clean() || len(s.VolatileDifferent) != 1 {
			t.Errorf("a declared-volatile table is not a durable mismatch: %+v", s)
		}
		if s := v.Split(nil); s.Clean() || len(s.DurableDifferent) != 1 {
			t.Errorf("undeclared, it is still reported: %+v", s)
		}
	})

	t.Run("absence is reported apart from difference", func(t *testing.T) {
		v := CompareTableHashes(captured, map[string]string{
			"app.public.documents_document": "aaa",
			"app.public.auth_user":          "CHANGED",
		})
		if len(v.Identical) != 1 || len(v.Different) != 1 || len(v.OnlyInBackup) != 1 {
			t.Fatalf("got %+v", v)
		}
		if v.OnlyInBackup[0] != "app.public.django_session" {
			t.Errorf("only-in-backup = %v", v.OnlyInBackup)
		}
		if v.Clean() {
			t.Error("a table that did not come back is not a clean restore")
		}
	})

	t.Run("a table only the restored side has is named, never blocking", func(t *testing.T) {
		// A dump legitimately creates tables of its own — an extension's.
		v := CompareTableHashes(map[string]string{"a": "1"}, map[string]string{"a": "1", "vectors.items": "2"})
		if !v.Clean() {
			t.Error("an extra table must not make a restore unclean")
		}
		if len(v.OnlyInRestored) != 1 || v.OnlyInRestored[0] != "vectors.items" {
			t.Errorf("only-in-restored = %v", v.OnlyInRestored)
		}
		if !v.Split(nil).Clean() {
			t.Error("an extra table is not a mismatch")
		}
	})

	t.Run("every list is ordered, so two identical restores read identically", func(t *testing.T) {
		v := CompareTableHashes(
			map[string]string{"z": "1", "a": "2", "m": "3"},
			map[string]string{"z": "x", "a": "y", "m": "w"})
		if len(v.Different) != 3 || v.Different[0] != "a" || v.Different[2] != "z" {
			t.Errorf("different = %v, want sorted", v.Different)
		}
	})

	t.Run("no baseline yields no verdict", func(t *testing.T) {
		v := CompareTableHashes(nil, map[string]string{"a": "1"})
		if v.Compared() != 0 || len(v.Different) != 0 {
			t.Errorf("an older backup carries no baseline: %+v", v)
		}
	})

	t.Run("the report names tables up to the cap and counts the rest", func(t *testing.T) {
		many := []string{"t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8"}
		got := namedTables(many)
		if !strings.Contains(got, "t1") || !strings.Contains(got, "t6") {
			t.Errorf("the first %d must be named: %s", maxReportedShortTables, got)
		}
		if strings.Contains(got, "t7") || !strings.Contains(got, "and 2 more") {
			t.Errorf("the rest must be counted, not listed: %s", got)
		}
		if got := namedTables([]string{"only"}); got != "only" {
			t.Errorf("a short list is listed in full: %s", got)
		}
	})

	t.Run("the phase label distinguishes the two moments", func(t *testing.T) {
		// The same comparison means different things before and after the
		// application runs, so the verdict carries which it was.
		if HashPhasePreStart == HashPhasePostStart {
			t.Fatal("the phases must differ")
		}
		if !strings.Contains(HashPhasePostStart, "volatile drift expected") {
			t.Errorf("post-start = %q", HashPhasePostStart)
		}
	})
}

func TestQuiescentTables(t *testing.T) {
	hashes := map[string]string{
		"app.public.documents": "aaa",
		"app.public.sessions":  "bbb",
		"app.public.users":     "ccc",
	}

	t.Run("a table written to across the window is left out", func(t *testing.T) {
		// Its hash describes a moment AFTER the dump, so keeping it would
		// guarantee a false mismatch on every restore of this backup.
		before := map[string]string{"app.public.documents": "10", "app.public.sessions": "5", "app.public.users": "2"}
		after := map[string]string{"app.public.documents": "10", "app.public.sessions": "9", "app.public.users": "2"}
		kept, moved := quiescentTables(hashes, before, after)
		if len(kept) != 2 || kept["app.public.sessions"] != "" {
			t.Errorf("kept = %v", kept)
		}
		if len(moved) != 1 || moved[0] != "app.public.sessions" {
			t.Errorf("moved = %v", moved)
		}
	})

	t.Run("a quiescent database keeps its whole baseline", func(t *testing.T) {
		counts := map[string]string{"app.public.documents": "10", "app.public.sessions": "5", "app.public.users": "2"}
		kept, moved := quiescentTables(hashes, counts, counts)
		if len(kept) != 3 || len(moved) != 0 {
			t.Errorf("kept=%v moved=%v", kept, moved)
		}
	})

	t.Run("a counter that cannot be compared is not evidence of quiescence", func(t *testing.T) {
		// A table missing from either reading — created mid-window, or stats not
		// yet reported — must be dropped, or the invariant is not an invariant.
		kept, moved := quiescentTables(hashes,
			map[string]string{"app.public.documents": "10"},
			map[string]string{"app.public.documents": "10", "app.public.sessions": "5", "app.public.users": "2"})
		if len(kept) != 1 || kept["app.public.documents"] != "aaa" {
			t.Errorf("kept = %v", kept)
		}
		if len(moved) != 2 {
			t.Errorf("moved = %v", moved)
		}
	})

	t.Run("moved tables are named in a stable order", func(t *testing.T) {
		_, moved := quiescentTables(map[string]string{"z": "1", "a": "2"},
			map[string]string{"z": "1", "a": "1"}, map[string]string{"z": "2", "a": "2"})
		if len(moved) != 2 || moved[0] != "a" {
			t.Errorf("moved = %v, want sorted", moved)
		}
	})

	t.Run("parse", func(t *testing.T) {
		got := parseTableActivity("TBLMOD|app.public.docs|42\nTBLMOD|app.public.we|ird|7\nnoise\nTBLMOD|bad|x\n")
		if got["app.public.docs"] != "42" {
			t.Errorf("docs = %q", got["app.public.docs"])
		}
		if got["app.public.we|ird"] != "7" {
			t.Errorf("a name containing the separator must survive: %v", got)
		}
		if _, ok := got["bad"]; ok {
			t.Error("a non-numeric counter is not a count")
		}
	})

	t.Run("the query counts every kind of write and keys like the hashes", func(t *testing.T) {
		q := pgTableActivityQuery
		for _, want := range []string{"n_tup_ins", "n_tup_upd", "n_tup_del", "pg_stat_user_tables", "current_database()"} {
			if !strings.Contains(q, want) {
				t.Errorf("query must contain %q", want)
			}
		}
	})
}

// MariaDB 11 ships no `mysql` binary and puts its root password under a
// MARIADB_ prefix. The first version of this script assumed both, produced
// nothing on mariadb:11.4-noble, and recorded no baseline — which looks exactly
// like a backup that never had one.
func TestMySQLClientSelection(t *testing.T) {
	script := mysqlTableHashScript()

	t.Run("it finds the client MariaDB actually ships", func(t *testing.T) {
		if !strings.Contains(script, `command -v mariadb >/dev/null 2>&1 && CLI=mariadb`) {
			t.Error("MariaDB 11 has no `mysql` binary")
		}
		if strings.Contains(script, "mysql --defaults-file") {
			t.Error("the client must never be hardcoded to mysql")
		}
	})

	t.Run("it reads both vendors' credential names", func(t *testing.T) {
		for _, want := range []string{"MYSQL_ROOT_PASSWORD", "MARIADB_ROOT_PASSWORD", "MYSQL_USER", "MARIADB_USER"} {
			if !strings.Contains(script, want) {
				t.Errorf("script must consider %s", want)
			}
		}
		// Never on a command line, where it would be visible in the process list.
		if strings.Contains(script, `-p"$RP"`) || strings.Contains(script, "-p$RP") {
			t.Error("a password must go through MYSQL_PWD, never argv")
		}
		if !strings.Contains(script, "MYSQL_PWD") {
			t.Error("the password is passed through the environment")
		}
	})

	t.Run("it gives up quietly rather than failing a good dump", func(t *testing.T) {
		if !strings.Contains(script, `command -v "$CLI" >/dev/null 2>&1 || exit 0`) {
			t.Error("a container that is not a server must exit 0 with no output")
		}
		if !strings.Contains(script, "else exit 0; fi") {
			t.Error("failing to authenticate must not fail the backup")
		}
	})
}

func TestQuiescenceEvidencePerEngine(t *testing.T) {
	// Both engines that have a baseline need before-and-after evidence, or the
	// baseline may describe a moment after the dump.
	for _, engine := range []string{"postgres", "mysql"} {
		if !engineNeedsQuiescenceEvidence(engine) {
			t.Errorf("%s baseline is only trustworthy with evidence", engine)
		}
	}
	for _, engine := range []string{"mongodb", "redis", ""} {
		if engineNeedsQuiescenceEvidence(engine) {
			t.Errorf("%s has no baseline to guard", engine)
		}
	}
	// MySQL's evidence is its own checksum, so before/after compare like any
	// other pair of readings.
	kept, moved := quiescentTables(
		map[string]string{"nc.oc_filecache": "2686266596", "nc.oc_jobs": "83162917"},
		map[string]string{"nc.oc_filecache": "2686266596", "nc.oc_jobs": "4183707817"},
		map[string]string{"nc.oc_filecache": "2686266596", "nc.oc_jobs": "83162917"})
	if len(kept) != 1 || kept["nc.oc_filecache"] == "" {
		t.Errorf("kept = %v", kept)
	}
	if len(moved) != 1 || moved[0] != "nc.oc_jobs" {
		t.Errorf("moved = %v — the table written during the dump must be left out", moved)
	}
}
