package backup

import (
	"strings"
	"testing"
)

// R2 §Issue 13: a declared MYSQL_ROOT_PASSWORD that had never applied, because
// the entrypoint only honours it when the data directory is empty at first
// start. The dump was taken as the application user instead — complete for its
// schema, and missing the server's accounts.
func TestCredentialLadder(t *testing.T) {
	t.Run("the ladder tries the declared root password, then the socket, then the app user", func(t *testing.T) {
		ladder := mysqlAuthLadder()
		order := []string{
			`MYSQL_PWD="$RP" "$CLI" -uroot`,                       // 1: declared root password
			`elif "$CLI" -uroot -N -e "SELECT 1"`,                 // 2: socket, no password
			`elif [ -n "$AU" ] && MYSQL_PWD="$AP" "$CLI" -u"$AU"`, // 3: the app user
		}
		at := -1
		for _, rung := range order {
			i := strings.Index(ladder, rung)
			if i < 0 {
				t.Fatalf("ladder is missing a rung: %s", rung)
			}
			if i <= at {
				t.Errorf("rung out of order: %s", rung)
			}
			at = i
		}
		// Content-asserted, per §7.5: an exit code alone is not authentication.
		if !strings.Contains(ladder, `-e "SELECT 1"`) {
			t.Error("each candidate must be proven with a query")
		}
		// Nothing outside the container's own environment is ever tried.
		if strings.Contains(ladder, "password") || strings.Contains(ladder, "admin123") {
			t.Error("no credential may be guessed")
		}
	})

	t.Run("the dump and the probe share one ladder, so they cannot disagree", func(t *testing.T) {
		// A probe reporting a different user than the dump used would put a wrong
		// privilege scope in the manifest.
		probe := MySQLAuthProbeCommand([]string{"MYSQL_ROOT_PASSWORD=rootpw", "MYSQL_USER=app", "MYSQL_PASSWORD=p", "MYSQL_DATABASE=appdb"})
		script := probe[len(probe)-1]
		if !strings.Contains(script, mysqlAuthLadder()) {
			t.Error("the probe must run the ladder verbatim")
		}
		if !strings.Contains(script, authProbePrefix) {
			t.Error("the probe must report which identity it reached")
		}
		// It answers "who got in", never "with what".
		if strings.Contains(script, `"$RP"'`) || strings.Contains(script, "printf '"+authProbePrefix+"%s|%s|%s") {
			t.Error("no password may be printed")
		}
	})

	t.Run("MARIADB_ prefixed variables are read too", func(t *testing.T) {
		// The script reads the container's own variables rather than carrying
		// their values, so what must be present is the FALLBACK to the MariaDB
		// names — not a password.
		script := MySQLAuthProbeCommand(nil)[2]
		for _, want := range []string{"MARIADB_ROOT_PASSWORD", "MARIADB_USER", "MARIADB_PASSWORD"} {
			if !strings.Contains(script, want) {
				t.Errorf("MariaDB's own variable names must be honoured: %s missing", want)
			}
		}
	})

	t.Run("scope: root reaches the server, an app user reaches its schemas", func(t *testing.T) {
		if got := dumpScopeFor("root", []string{"bookstack"}); got != DumpScopeServer {
			t.Errorf("root scope = %q", got)
		}
		if got := dumpScopeFor("postgres", nil); got != DumpScopeServer {
			t.Errorf("postgres scope = %q", got)
		}
		got := dumpScopeFor("bookstackuser", []string{"bookstack"})
		if got != "schema:bookstack" {
			t.Errorf("app-user scope = %q", got)
		}
		names, yes := SchemaScoped(got)
		if !yes || names != "bookstack" {
			t.Errorf("SchemaScoped(%q) = %q,%v", got, names, yes)
		}
		if _, yes := SchemaScoped(DumpScopeServer); yes {
			t.Error("a server dump is not schema-scoped")
		}
		// Absent means unknown, never "server".
		if _, yes := SchemaScoped(""); yes {
			t.Error("an empty scope claims nothing")
		}
	})

	t.Run("a declared root password that did not win is the finding", func(t *testing.T) {
		env := []string{"MYSQL_ROOT_PASSWORD=declared-but-inert", "MYSQL_USER=bookstackuser"}
		if !declaredRootRejected(env, "bookstackuser") {
			t.Error("R2 §13's exact case must be reported")
		}
		if declaredRootRejected(env, "root") {
			t.Error("root winning is not drift")
		}
		// No declared password, no claim: an image that never declares one has not
		// drifted from anything.
		if declaredRootRejected([]string{"MYSQL_USER=app"}, "app") {
			t.Error("nothing declared, nothing to contradict")
		}
		if declaredRootRejected([]string{"MARIADB_ROOT_PASSWORD=x"}, "app") != true {
			t.Error("the MariaDB variable counts as declared too")
		}
	})

	t.Run("parse", func(t *testing.T) {
		user, dbs, ok := parseAuthProbe("AUTH|bookstackuser|bookstack \n")
		if !ok || user != "bookstackuser" || len(dbs) != 1 || dbs[0] != "bookstack" {
			t.Errorf("got %q %v %v", user, dbs, ok)
		}
		if _, _, ok := parseAuthProbe("noise\n"); ok {
			t.Error("noise is not a verdict")
		}
		// Unknown must stay unknown: a probe that could not run must not record
		// "server" and let a restore believe it holds the grants.
		if _, _, ok := parseAuthProbe(""); ok {
			t.Error("no output is no identity")
		}
	})
}
