package backup

import (
	"strings"
	"testing"
)

// TestGuacamoleEmbeddedProfile (F126). The all-in-one image bundles guacd,
// Tomcat AND a full PostgreSQL, and nothing in its NAME says so — which is why
// detectDBEngine saw an ordinary app and DockBack raw-copied the live data
// directory: a hot file copy of a running database.
func TestGuacamoleEmbeddedProfile(t *testing.T) {
	p := ProfileFor("jwetzell/guacamole")
	if p == nil || p.EmbeddedDump == nil {
		t.Fatalf("Guacamole bundles its own PostgreSQL — that must be declared, got %+v", p)
	}
	d := p.EmbeddedDump
	if d.Engine != "postgres" {
		t.Errorf("engine = %q, want postgres", d.Engine)
	}
	// The data directory is where the APP puts it, not the postgres image default.
	if d.DataDir != "/config/postgres" {
		t.Errorf("datadir = %q — an embedded server does not use the engine image's default", d.DataDir)
	}
	if d.DBName == "" || d.User == "" {
		t.Errorf("the dump target must be declared, got user=%q db=%q", d.User, d.DBName)
	}
	// Image-name detection must still find nothing — that is the whole premise.
	if detectDBEngine("jwetzell/guacamole", nil) != "" {
		t.Error("the image name says nothing about the bundled database; if this starts matching, the profile is redundant")
	}
	// Already in the credential-store class, and it must stay there.
	if p.CredentialStore == "" {
		t.Error("Guacamole stores connection passwords recoverably — it must stay in the credential-store class")
	}
}

// TestEmbeddedDumpCommandPostgres: the standalone command dumps the whole
// cluster with pg_dumpall, which needs superuser — and an embedded server's
// application user routinely is not one, so that would fail on the globals and
// take the backup with it.
func TestEmbeddedDumpCommandPostgres(t *testing.T) {
	cmd := embeddedDumpCommand(&EmbeddedDump{Engine: "postgres", User: "guacamole", DBName: "guacamole_db"}, nil)
	if len(cmd) == 0 {
		t.Fatal("expected a command")
	}
	s := strings.Join(cmd, " ")
	if strings.Contains(s, "pg_dumpall") {
		t.Error("must NOT use pg_dumpall — an embedded server's app user is not a superuser")
	}
	for _, want := range []string{"pg_dump", "guacamole_db", "--create", "--clean", "--if-exists"} {
		if !strings.Contains(s, want) {
			t.Errorf("the dump must contain %q so it re-imports over an initialised server, got %q", want, s)
		}
	}
	// A missing client must exit 127 so the engine falls back to a file capture
	// rather than failing the backup outright.
	if !strings.Contains(s, "exit 127") {
		t.Error("an absent pg_dump must exit 127 for the fallback path")
	}
	// No password on argv, ever.
	if strings.Contains(s, "--password") || strings.Contains(s, "-W ") {
		t.Errorf("no password may reach argv: %q", s)
	}

	// Falls back to the container's own environment when the profile is silent.
	envCmd := embeddedDumpCommand(&EmbeddedDump{Engine: "postgres"},
		[]string{"POSTGRES_USER=app", "POSTGRES_DB=appdb"})
	if es := strings.Join(envCmd, " "); !strings.Contains(es, "appdb") || !strings.Contains(es, "app") {
		t.Errorf("must fall back to the container environment, got %q", es)
	}
}

// TestEmbeddedDumpCommandMySQL covers the Uptime Kuma shape: embedded MariaDB,
// root over the local socket, which unix_socket trusts without a password.
func TestEmbeddedDumpCommandMySQL(t *testing.T) {
	s := strings.Join(embeddedDumpCommand(&EmbeddedDump{Engine: "mysql", DBName: "kuma"}, nil), " ")
	for _, want := range []string{"mariadb-dump", "kuma", "--single-transaction", "--add-drop-database"} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in %q", want, s)
		}
	}
	if strings.Contains(s, "MYSQL_PWD") {
		t.Error("the local socket needs no password; none should be plumbed")
	}
	// No database named = the whole server.
	if all := strings.Join(embeddedDumpCommand(&EmbeddedDump{Engine: "mysql"}, nil), " "); !strings.Contains(all, "--all-databases") {
		t.Errorf("an unnamed database must dump everything, got %q", all)
	}
}

func TestEmbeddedDumpCommandUnknownEngine(t *testing.T) {
	if cmd := embeddedDumpCommand(&EmbeddedDump{Engine: "cassandra"}, nil); cmd != nil {
		t.Errorf("an engine with no dump support must yield no command, got %v", cmd)
	}
	if cmd := embeddedDumpCommand(nil, nil); cmd != nil {
		t.Errorf("a nil description must yield no command, got %v", cmd)
	}
}

// TestExcludeSubPathsArmedOnlyWithADump is the safety property of the whole
// feature: excluding a data directory when no dump replaced it converts a
// wasteful backup into an EMPTY one.
func TestExcludeSubPathsArmedOnlyWithADump(t *testing.T) {
	var unarmed Options
	if got := unarmed.excludeSubPaths(); len(got) != 0 {
		t.Errorf("nothing may be excluded until a dump has succeeded, got %v", got)
	}
	armed := Options{embeddedDataDir: "/config/postgres"}
	if got := armed.excludeSubPaths(); len(got) != 1 || got[0] != "/config/postgres" {
		t.Errorf("got %v, want the declared data directory", got)
	}
}

// TestEmbeddedDumpFor resolves the description from a backup's manifest on the
// restore side, and stays silent for every ordinary app.
func TestEmbeddedDumpFor(t *testing.T) {
	if d := embeddedDumpFor(&Manifest{Image: "jwetzell/guacamole"}); d == nil || d.Engine != "postgres" {
		t.Fatalf("expected the Guacamole embedded description, got %+v", d)
	}
	for _, img := range []string{"nginx:alpine", "postgres:16", ""} {
		if d := embeddedDumpFor(&Manifest{Image: img}); d != nil {
			t.Errorf("%q must have no embedded description, got %+v", img, d)
		}
	}
	if embeddedDumpFor(nil) != nil {
		t.Error("a nil manifest must yield nothing")
	}
}
