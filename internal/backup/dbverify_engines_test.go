package backup

import (
	"io"
	"strings"
	"testing"
)

// Post-import verification for MySQL, MongoDB and Redis (F99).
//
// Postgres has been held to a did-it-actually-land check since the incident that
// motivated it; the other three engines were trusted on their exit code alone.
// A truncated mysqldump imports "successfully" in exactly the same way the
// Postgres one did. These tests pin each engine's own cheap invariant — and,
// just as importantly, that a restore which genuinely worked is never failed.

func TestDumpScannerTalliesMySQL(t *testing.T) {
	dump := strings.Join([]string{
		"-- MySQL dump 10.13  Distrib 8.0.36, for Linux (x86_64)",
		"CREATE TABLE `documents` (",
		"  `id` int NOT NULL",
		") ENGINE=InnoDB;",
		"INSERT INTO `documents` VALUES (1,'a'),(2,'b'),(3,'c');",
		"CREATE TABLE IF NOT EXISTS `tags` (`id` int);",
		"INSERT INTO `tags` VALUES (1,'x');",
		"-- Dump completed on 2026-07-26 10:00:00",
	}, "\n")

	sc := newDumpScannerFor("mysql", strings.NewReader(dump))
	got, err := io.ReadAll(sc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != dump {
		t.Fatal("the scanner must pass every byte through unchanged")
	}
	exp := sc.Expect()
	if exp.Tables != 2 {
		t.Fatalf("tables = %d, want 2", exp.Tables)
	}
	// INSERT STATEMENTS, not rows — the dump above holds 4 rows in 2 statements.
	if exp.Rows != 2 {
		t.Fatalf("insert statements = %d, want 2", exp.Rows)
	}
	if !exp.HeaderSeen || !exp.Complete {
		t.Fatalf("header=%v complete=%v, want both true", exp.HeaderSeen, exp.Complete)
	}
	if exp.PrimaryKeys != 0 || exp.ForeignKeys != 0 {
		t.Fatal("a MySQL dump must not be tallied with Postgres patterns")
	}
}

// A mysqldump cut mid-stream declares fewer tables and never reaches its
// trailer — the same shape the Postgres check already catches.
func TestDumpScannerDetectsMySQLTruncation(t *testing.T) {
	dump := "-- MySQL dump 10.13  Distrib 8.0.36\nCREATE TABLE `a` (`id` int);\nINSERT INTO `a` VALUES (1);\n"
	sc := newDumpScannerFor("mysql", strings.NewReader(dump))
	_, _ = io.Copy(io.Discard, sc)
	exp := sc.Expect()
	if exp.Complete {
		t.Fatal("a cut dump must not report a completion marker")
	}
	if !exp.Truncated() {
		t.Fatal("a dump that began and never finished must read as truncated")
	}
}

// A dump line inside INSERT data must not be miscounted as structure — a text
// column can legitimately contain "CREATE TABLE".
func TestMySQLDumpScanIgnoresDataText(t *testing.T) {
	var e DumpExpect
	for _, line := range []string{
		"INSERT INTO `notes` VALUES (1,'run CREATE TABLE foo to fix it');",
		"-- CREATE TABLE in a comment",
		"  CREATE TABLE without a backquoted name",
	} {
		mysqlDumpScan(line, &e)
	}
	if e.Tables != 0 {
		t.Fatalf("tables = %d, want 0 — only mysqldump's own backquoted form counts", e.Tables)
	}
	if e.Rows != 1 {
		t.Fatalf("insert statements = %d, want 1", e.Rows)
	}
}

// Binary dumps declare nothing scannable, so their tally must stay empty rather
// than pick up coincidental text — a false expectation would fail a good restore.
func TestDumpTallyIgnoresStructureInBinaryDumps(t *testing.T) {
	for _, engine := range []string{"mongodb", "redis"} {
		tally := NewDumpTallyFor(engine)
		_, _ = tally.Write([]byte("CREATE TABLE `x` (\nADD CONSTRAINT y PRIMARY KEY (id);\nINSERT INTO `x` VALUES (1);\n"))
		exp := tally.Expect()
		if exp.Tables != 0 || exp.PrimaryKeys != 0 || exp.Rows != 0 {
			t.Fatalf("%s: a binary dump must not be tallied for structure: %+v", engine, exp)
		}
		if exp.Bytes == 0 || exp.SHA256 == "" {
			t.Fatalf("%s: size and checksum must still be recorded", engine)
		}
	}
}

// THE acceptance case: a MySQL dump declaring 40 tables that imports 12 must
// fail, naming the shortfall.
func TestVerifyImportCountsMySQLShortfall(t *testing.T) {
	exp := DumpExpect{Tables: 40, Rows: 900, Complete: true}
	err := VerifyImportCounts("mysql", exp, 12)
	if err == nil {
		t.Fatal("a 12-of-40 import must fail")
	}
	if !strings.Contains(err.Error(), "12 of 40 tables") {
		t.Fatalf("the error must name the shortfall: %v", err)
	}

	// Identical counts pass, and so does MORE — a target legitimately holds other
	// databases, and failing on that would fail restores that worked.
	if err := VerifyImportCounts("mysql", exp, 40); err != nil {
		t.Fatalf("an exact match must pass: %v", err)
	}
	if err := VerifyImportCounts("mysql", exp, 57); err != nil {
		t.Fatalf("more than expected must pass: %v", err)
	}
}

// A cut dump fails even when the tables that survived all applied — the missing
// trailer is itself the evidence, exactly as on the Postgres path.
func TestVerifyImportCountsMySQLIncompleteStream(t *testing.T) {
	err := VerifyImportCounts("mysql", DumpExpect{Tables: 5, Complete: false}, 5)
	if err == nil || !strings.Contains(err.Error(), "cut short") {
		t.Fatalf("a dump with no completion marker must fail: %v", err)
	}
}

func TestVerifyImportCountsMongoAndRedis(t *testing.T) {
	mongo := DumpExpect{Collections: 18}
	if err := VerifyImportCounts("mongodb", mongo, 4); err == nil ||
		!strings.Contains(err.Error(), "4 of 18 collections") {
		t.Fatalf("a collection shortfall must be named: %v", err)
	}
	if err := VerifyImportCounts("mongodb", mongo, 18); err != nil {
		t.Fatalf("an exact match must pass: %v", err)
	}
	// A binary archive carries no completion marker, so Complete=false must NOT
	// fail these engines the way it does MySQL.
	if err := VerifyImportCounts("mongodb", DumpExpect{Collections: 3, Complete: false}, 3); err != nil {
		t.Fatalf("mongo has no completion marker to require: %v", err)
	}

	redis := DumpExpect{Keys: 120_000}
	if err := VerifyImportCounts("redis", redis, 0); err == nil ||
		!strings.Contains(err.Error(), "0 of 120000 keys") {
		t.Fatalf("an empty Redis after a restore must fail: %v", err)
	}
	if err := VerifyImportCounts("redis", redis, 120_001); err != nil {
		t.Fatalf("a key written since capture must not fail the restore: %v", err)
	}
}

// Nothing declared means nothing to check. An empty database is a legitimate
// thing to back up, and a pre-F99 backup recorded no counts at all — neither may
// be reported as a failed restore.
func TestVerifyImportCountsSilentWithoutAnExpectation(t *testing.T) {
	for _, engine := range []string{"mysql", "mongodb", "redis", "postgres", "cassandra"} {
		if err := VerifyImportCounts(engine, DumpExpect{}, 0); err != nil {
			t.Fatalf("%s: an absent expectation must never fail: %v", engine, err)
		}
	}
	// A MySQL dump with no completion marker AND no tables is not a mysqldump at
	// all (e.g. a legacy backup) — still silent.
	if err := VerifyImportCounts("mysql", DumpExpect{Complete: false}, 0); err != nil {
		t.Fatalf("a dump declaring nothing must not fail: %v", err)
	}
}

func TestExpectedForCarriesEveryEnginesContract(t *testing.T) {
	rec := &DBDump{DumpTables: 40, DumpRows: 900, DumpCollections: 18, DumpKeys: 120_000}
	got := expectedFor(rec, DumpExpect{Tables: 12, Complete: true})
	if got.Tables != 40 || got.Collections != 18 || got.Keys != 120_000 {
		t.Fatalf("the RECORDED contract must win over the re-derived one: %+v", got)
	}

	// A pre-F99 backup recorded nothing, so the streamed tally is the only bar
	// available and must be used unchanged.
	streamed := DumpExpect{Tables: 7, Complete: true}
	if got := expectedFor(&DBDump{}, streamed); got.Tables != 7 {
		t.Fatalf("a backup with no recorded contract must fall back to the stream: %+v", got)
	}
	if got := expectedFor(nil, streamed); got.Tables != 7 {
		t.Fatalf("no record at all must fall back to the stream: %+v", got)
	}
}

func TestParseNameCount(t *testing.T) {
	// mongosh and redis-cli both print banner noise around their output.
	out := "Current Mongosh Log ID: abc\nconnecting to: mongodb://127.0.0.1\ncollections|18\n"
	if n, ok := parseNameCount(out, "collections"); !ok || n != 18 {
		t.Fatalf("parseNameCount = %d,%v", n, ok)
	}
	if _, ok := parseNameCount(out, "keys"); ok {
		t.Fatal("a metric that isn't present must report not-found, not zero")
	}
	// Not-found and zero must stay distinguishable: zero keys is a real answer.
	if n, ok := parseNameCount("keys|0", "keys"); !ok || n != 0 {
		t.Fatalf("a genuine zero must be readable: %d,%v", n, ok)
	}
	if _, ok := parseNameCount("keys|not-a-number", "keys"); ok {
		t.Fatal("unparseable output must not be read as a count")
	}
}

// Every count command must be a self-contained /bin/sh script that reads its
// credentials from the container's OWN environment — never a value baked in by
// DockBack, which would put a secret in the manifest-adjacent code path and in
// the exec audit.
func TestCountCommandsShape(t *testing.T) {
	for engine, want := range map[string]string{"mysql": "tables", "mongodb": "collections", "redis": "keys"} {
		cmd := dbCountCmd(engine)
		if len(cmd) != 3 || cmd[0] != "/bin/sh" || cmd[1] != "-c" {
			t.Fatalf("%s: want a /bin/sh -c script, got %v", engine, cmd)
		}
		if !strings.Contains(cmd[2], want) {
			t.Fatalf("%s: the script must print a %q line", engine, want)
		}
	}
	// Where the client supports it, the password travels by ENVIRONMENT so it
	// never reaches the container's process list.
	if !strings.Contains(dbCountCmd("mysql")[2], "MYSQL_PWD=") {
		t.Fatal("mysql must pass its password via MYSQL_PWD, not argv")
	}
	if !strings.Contains(dbCountCmd("redis")[2], "REDISCLI_AUTH=") {
		t.Fatal("redis must pass its password via REDISCLI_AUTH, not argv")
	}
	// mongosh has no password environment variable, so it takes -p on the argv —
	// the same way the existing mongorestore import already does. The value comes
	// from, and stays inside, the container's own environment, so this exposes
	// nothing to a reader who could not already read /proc/1/environ there.
	if !strings.Contains(dbCountCmd("mongodb")[2], `MONGO_INITDB_ROOT_PASSWORD`) {
		t.Fatal("mongo must read its credential from the container's own environment")
	}
	if dbCountCmd("postgres") != nil {
		t.Fatal("postgres keeps its own constraint-count command")
	}
	if dbCountCmd("cassandra") != nil {
		t.Fatal("an unknown engine has no count command")
	}
}
