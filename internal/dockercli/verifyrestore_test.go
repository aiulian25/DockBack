package dockercli

import (
	"strings"
	"testing"
)

func TestDBInitEnv(t *testing.T) {
	mysql := strings.Join(dbInitEnv("mysql", "secret"), " ")
	if !strings.Contains(mysql, "MYSQL_ROOT_PASSWORD=secret") || !strings.Contains(mysql, "MARIADB_ROOT_PASSWORD=secret") {
		t.Errorf("mysql init env = %q", mysql)
	}
	pg := strings.Join(dbInitEnv("postgres", "secret"), " ")
	if !strings.Contains(pg, "POSTGRES_PASSWORD=secret") || !strings.Contains(pg, "POSTGRES_HOST_AUTH_METHOD=trust") {
		t.Errorf("postgres init env = %q", pg)
	}
	if env := dbInitEnv("mongodb", "secret"); env != nil {
		t.Errorf("mongodb should need no init env, got %v", env)
	}
}

func TestDBImportAndSanityCommands(t *testing.T) {
	for _, eng := range []string{"postgres", "mysql", "mongodb"} {
		if imp := dbImportCommand(eng); len(imp) == 0 {
			t.Errorf("%s import command empty", eng)
		}
		if q := dbSanityQuery(eng); len(q) == 0 {
			t.Errorf("%s sanity query empty", eng)
		}
	}
	// Loopback-only (never reaches outside the throwaway container).
	if !strings.Contains(strings.Join(dbImportCommand("mysql"), " "), "127.0.0.1") {
		t.Error("mysql import should connect over loopback")
	}
	if !strings.Contains(strings.Join(dbSanityQuery("postgres"), " "), "information_schema.tables") {
		t.Error("postgres sanity query should count tables")
	}
}

func TestDBSanitySummary(t *testing.T) {
	if got := dbSanitySummary("mysql", []byte("12\n")); got != "12 tables" {
		t.Errorf("mysql summary = %q", got)
	}
	if got := dbSanitySummary("mongodb", []byte(" 3 ")); got != "3 databases" {
		t.Errorf("mongodb summary = %q", got)
	}
	if got := dbSanitySummary("postgres", []byte("")); got != "0 tables" {
		t.Errorf("empty summary = %q", got)
	}
}

// TestPostgresSanityQueryWalksEveryDatabase is the regression for the "0 tables"
// bug (F120), which made every Postgres backup on wikijs, immich, mealie and
// commafeed report a confident, permanent zero.
//
// The cause: information_schema in PostgreSQL is PER-DATABASE, and the query
// connected to `postgres` and counted there — while pg_dumpall restores each
// application into its own database. MySQL was unaffected because its
// information_schema is server-wide, which is exactly why the two engines
// disagreed and why the bug survived so long.
func TestPostgresSanityQueryWalksEveryDatabase(t *testing.T) {
	q := strings.Join(dbSanityQuery("postgres"), " ")

	// It must enumerate the databases rather than assume one.
	if !strings.Contains(q, "pg_database") {
		t.Error("the query must list the server's databases — counting only inside `postgres` is the bug this fixes")
	}
	if !strings.Contains(q, "datallowconn") {
		t.Error("only connectable databases can be counted")
	}
	// Templates would double-count the schema of every database made from them.
	if !strings.Contains(q, "template0") || !strings.Contains(q, "template1") {
		t.Error("template databases must be excluded")
	}
	// The per-database count has to be addressed with -d "$db", not -d postgres.
	if !strings.Contains(q, `-d "$db"`) {
		t.Error("the table count must run against each database in turn")
	}
	if !strings.Contains(q, "tables=") || !strings.Contains(q, "databases=") {
		t.Error("both figures must be reported so a zero is legible as a broken probe rather than a fact")
	}
	// Still loopback-only inside the throwaway container.
	if !strings.Contains(q, "127.0.0.1") {
		t.Error("the sanity query must stay on loopback")
	}
}

// TestPostgresSanitySummary covers the rendering, including the shape that made
// the old bug so convincing: a bare "0 tables" read as a statement about the
// backup, where "0 tables in 0 databases" reads as a probe that found nothing.
func TestPostgresSanitySummary(t *testing.T) {
	cases := map[string]string{
		"tables=13\ndatabases=1\n": "13 tables in 1 database",
		"tables=1\ndatabases=1":    "1 table in 1 database",
		"tables=0\ndatabases=0":    "0 tables in 0 databases",
		"tables=41\ndatabases=3":   "41 tables in 3 databases",
	}
	for in, want := range cases {
		if got := dbSanitySummary("postgres", []byte(in)); got != want {
			t.Errorf("dbSanitySummary(postgres, %q) = %q, want %q", in, got, want)
		}
	}
	// An unexpected shape falls back to the plain rendering rather than
	// inventing numbers.
	if got := dbSanitySummary("postgres", []byte("7")); got != "7 tables" {
		t.Errorf("a bare number must still render, got %q", got)
	}
	if got := dbSanitySummary("postgres", []byte("tables=5")); got != "tables=5 tables" {
		t.Errorf("a half-reported pair must not be treated as complete, got %q", got)
	}
}

func TestParseKeyedCounts(t *testing.T) {
	if tb, db, ok := parseKeyedCounts("tables=13\ndatabases=2"); !ok || tb != 13 || db != 2 {
		t.Errorf("got (%d,%d,%v)", tb, db, ok)
	}
	// Both keys are required: a partial read must not become a confident answer.
	for _, in := range []string{"", "tables=1", "databases=1", "tables=x\ndatabases=1", "tables=-1\ndatabases=1"} {
		if _, _, ok := parseKeyedCounts(in); ok {
			t.Errorf("%q must not parse as a complete result", in)
		}
	}
}

func TestPlural(t *testing.T) {
	cases := map[int]string{0: "0 tables", 1: "1 table", 2: "2 tables", 13: "13 tables"}
	for n, want := range cases {
		if got := plural(n, "table"); got != want {
			t.Errorf("plural(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestRandPasswordUnique(t *testing.T) {
	a, b := randPassword(), randPassword()
	if a == b || len(a) != 32 {
		t.Errorf("randPassword not unique/wrong length: %q %q", a, b)
	}
}

func TestRandHexUnique(t *testing.T) {
	a, b := randHex(6), randHex(6)
	if a == b || len(a) != 12 {
		t.Errorf("randHex not unique/wrong length: %q %q", a, b)
	}
}

// F5: bootVerdict is the pure decision behind the volume drill's isolated boot.
// Healthy is the strongest proof; still-running at the deadline is a pass (deps
// may be unavailable in isolation); an early non-zero exit is a real failure.
func TestBootVerdict(t *testing.T) {
	cases := []struct {
		name       string
		health     string
		running    bool
		restarting bool
		exitCode   int
		atDeadline bool
		wantDone   bool
		wantErr    bool
	}{
		{"healthy immediately", "healthy", true, false, 0, false, true, false},
		{"still starting keeps polling", "starting", true, false, 0, false, false, false},
		{"running no healthcheck keeps polling", "", true, false, 0, false, false, false},
		{"exited clean is a pass", "", false, false, 0, false, true, false},
		{"exited non-zero is a failure", "", false, false, 1, false, true, true},
		{"running at deadline (no healthcheck) passes", "", true, false, 0, true, true, false},
		{"running at deadline (unhealthy, deps missing) passes", "unhealthy", true, false, 0, true, true, false},
		{"stuck restarting at deadline fails", "", false, true, 1, true, true, true},
		{"restarting is not yet terminal", "", false, true, 1, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			summary, done, err := bootVerdict(c.health, c.running, c.restarting, c.exitCode, c.atDeadline)
			if done != c.wantDone {
				t.Fatalf("done = %v, want %v", done, c.wantDone)
			}
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if done && err == nil && summary == "" {
				t.Errorf("a successful terminal verdict must carry a non-empty summary")
			}
			if !done && summary != "" {
				t.Errorf("non-terminal verdict must not carry a summary, got %q", summary)
			}
		})
	}
}

// TestDBCommandsSocketFallback locks the fix that the throwaway import + sanity
// query cover every way root is exposed across MySQL/MariaDB images: TCP with the
// password (official), TCP with NO password (linuxserver/mariadb ignores the root
// password), and the local SOCKET — passing the password via MYSQL_PWD, never on
// the argv (where `ps` inside the container could read it).
func TestDBCommandsSocketFallback(t *testing.T) {
	for name, s := range map[string]string{
		"import": strings.Join(dbImportCommand("mysql"), " "),
		"sanity": strings.Join(dbSanityQuery("mysql"), " "),
	} {
		if !strings.Contains(s, "MYSQL_PWD=") {
			t.Errorf("%s: password should be passed via MYSQL_PWD env: %q", name, s)
		}
		if strings.Contains(s, `-p"`) || strings.Contains(s, "-p$") {
			t.Errorf("%s: password must not be on the argv (-p...): %q", name, s)
		}
		if !strings.Contains(s, `MYSQL_PWD="$RP" "$CLI" -h127.0.0.1 -P3306 -uroot`) {
			t.Errorf("%s: should try root over TCP with the password: %q", name, s)
		}
		if !strings.Contains(s, `if "$CLI" -h127.0.0.1 -P3306 -uroot -e 'SELECT 1'`) {
			t.Errorf("%s: should try root over TCP with NO password (linuxserver/mariadb): %q", name, s)
		}
		if !strings.Contains(s, `"$CLI" -uroot -e 'SELECT 1'`) {
			t.Errorf("%s: should fall back to the local socket: %q", name, s)
		}
	}
}

// The Wiki.js numbers, measured rather than imagined.
//
// That application's report named a 17.9 MB dump verifying as "0 tables" as its
// top risk, and the question was whether the CAPTURE was empty or the CHECK was
// wrong. Run against a real Wiki.js 2.5 database: the old form of the query —
// connected to the `postgres` maintenance database — returns 0, and the current
// one returns 30, which is the real schema. So it was the check, and it is
// fixed; the dumps were sound all along.
//
// Pinned here as the shape a real answer has, so a future change to the output
// format is caught against something that was actually observed.
func TestWikiJSSanityCountParses(t *testing.T) {
	tables, dbs, ok := parseKeyedCounts("tables=30\ndatabases=1\n")
	if !ok || tables != 30 || dbs != 1 {
		t.Fatalf("the line a real Wiki.js database produces must parse: %d/%d/%v", tables, dbs, ok)
	}
	if got := dbSanitySummary("postgres", []byte("tables=30\ndatabases=1\n")); !strings.Contains(got, "30 tables") {
		t.Errorf("the summary should read naturally: %q", got)
	}
}
