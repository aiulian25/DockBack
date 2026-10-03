package backup

import (
	"strings"
	"testing"
)

// F8: a per-database selection narrows the dump; an empty selection preserves the
// full-cluster default (so existing behavior/tests are unchanged).
func TestDBDumpSubsetPostgres(t *testing.T) {
	env := []string{"POSTGRES_USER=admin", "POSTGRES_PASSWORD=secret"}

	full := strings.Join(dbDumpCommand("postgres", env, nil), " ")
	if !strings.Contains(full, "exec pg_dumpall -U 'admin'") {
		t.Errorf("empty selection must keep the full-cluster pg_dumpall:\n%s", full)
	}
	if strings.Contains(full, "pg_dump ") && strings.Contains(full, "--create") {
		t.Errorf("full-cluster dump must not use per-DB pg_dump --create:\n%s", full)
	}

	sub := strings.Join(dbDumpCommand("postgres", env, []string{"app1", "app2"}), " ")
	for _, want := range []string{
		"pg_dumpall -U 'admin' --globals-only",
		"pg_dump -U 'admin' --create --clean --if-exists -d 'app1'",
		"pg_dump -U 'admin' --create --clean --if-exists -d 'app2'",
	} {
		if !strings.Contains(sub, want) {
			t.Errorf("subset dump missing %q:\n%s", want, sub)
		}
	}
	if strings.Contains(sub, "exec pg_dumpall -U 'admin'") && !strings.Contains(sub, "--globals-only") {
		t.Errorf("subset dump must NOT run a full pg_dumpall of all data:\n%s", sub)
	}
}

func TestDBDumpSubsetMySQL(t *testing.T) {
	env := []string{"MYSQL_ROOT_PASSWORD=rootpw"}

	full := strings.Join(dbDumpCommand("mysql", env, nil), " ")
	if !strings.Contains(full, "SHOW DATABASES") {
		t.Errorf("empty selection must keep the full user-DB enumeration:\n%s", full)
	}
	if strings.Contains(full, "--add-drop-database") {
		t.Errorf("full-cluster mysqldump must not add --add-drop-database:\n%s", full)
	}

	sub := strings.Join(dbDumpCommand("mysql", env, []string{"app1"}), " ")
	for _, want := range []string{"--databases 'app1'", "--add-drop-database", `command -v "$CLI"`} {
		if !strings.Contains(sub, want) {
			t.Errorf("subset dump missing %q:\n%s", want, sub)
		}
	}
	if strings.Contains(sub, "SHOW DATABASES") {
		t.Errorf("subset dump must not enumerate all databases:\n%s", sub)
	}
}

// Empty/whitespace names are ignored, so a junk selection can't narrow to nothing
// (it falls back to the full cluster).
func TestDBDumpSubsetIgnoresBlankNames(t *testing.T) {
	if got := cleanDBNames([]string{"", "  ", "app1", " "}); len(got) != 1 || got[0] != "app1" {
		t.Fatalf("cleanDBNames should keep only real names, got %v", got)
	}
	full := strings.Join(dbDumpCommand("postgres", nil, []string{"", "  "}), " ")
	if !strings.Contains(full, "pg_dumpall -U") || strings.Contains(full, "--globals-only") {
		t.Errorf("a blank-only selection must fall back to the full-cluster dump:\n%s", full)
	}
}

// F9: the MongoDB dump must try root, then no-auth, then app credentials (in that
// order), exit 3 when none authenticate, and exit 127 when mongodump is absent —
// bringing it to parity with the MySQL auth ladder.
func TestDBDumpMongoAuthLadder(t *testing.T) {
	env := []string{
		"MONGO_INITDB_ROOT_USERNAME=root", "MONGO_INITDB_ROOT_PASSWORD=rootpw",
		"MONGODB_USERNAME=app", "MONGODB_PASSWORD=apppw", "MONGODB_DATABASE=appdb",
	}
	cmd := strings.Join(dbDumpCommand("mongodb", env, nil), " ")

	// Absent-tool fallback (exit 127) and loud auth failure (exit 3) with a clear msg.
	for _, want := range []string{
		"command -v mongodump",
		"exit 127",
		"exit 3",
		"could not authenticate to MongoDB",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("mongo dump script missing %q:\n%s", want, cmd)
		}
	}

	// The three methods, in order: root (admin) → no-auth → app (own db).
	root := strings.Index(cmd, `exec mongodump --archive --username="$RU" --password="$RP" --authenticationDatabase=admin`)
	noauth := strings.Index(cmd, `exec mongodump --archive; fi`)
	app := strings.Index(cmd, `--db="$ADB"`)
	if root < 0 || noauth < 0 || app < 0 {
		t.Fatalf("mongo dump script missing one of the three auth methods:\n%s", cmd)
	}
	if !(root < noauth && noauth < app) {
		t.Errorf("mongo auth methods out of order (want root < no-auth < app): root=%d noauth=%d app=%d", root, noauth, app)
	}

	// Version is captured for restore-compatibility (auth-free binary version).
	if v := strings.Join(MongoVersionCmd(), " "); !strings.Contains(v, "mongod --version") {
		t.Errorf("MongoVersionCmd should read the server version: %s", v)
	}
}

func TestDBListCommand(t *testing.T) {
	pg := strings.Join(DBListCommand("postgres", nil), " ")
	if !strings.Contains(pg, "pg_database") || !strings.Contains(pg, "datistemplate=false") {
		t.Errorf("postgres list command should query pg_database non-template:\n%s", pg)
	}
	my := strings.Join(DBListCommand("mysql", nil), " ")
	if !strings.Contains(my, "SHOW DATABASES") || !strings.Contains(my, "information_schema") {
		t.Errorf("mysql list command should SHOW DATABASES minus system schemas:\n%s", my)
	}
	if DBListCommand("mongodb", nil) != nil || DBListCommand("redis", nil) != nil || DBListCommand("", nil) != nil {
		t.Error("only postgres/mysql support per-database listing")
	}
}
