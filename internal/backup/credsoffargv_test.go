package backup

import (
	"strings"
	"testing"
)

// Every one of these commands is handed to `sh -c`, so the script text IS the
// exec's argv: it appears in `docker inspect` of the exec and in the container's
// own process list. A database password sitting there is readable by the very
// application the backup exists to protect — and by anything else that gets a
// shell in that container.
//
// Nothing is lost by referencing the variable instead. The values were read out
// of the CONTAINER's environment in the first place, so the script reads the
// same names and the password never leaves the container it already lives in.

// The values below must never appear in a generated script.
const (
	pgSecret    = "pg-p4ssw0rd-with a space"
	myRootPW    = "mysql-r00t-secret"
	myAppPW     = "mysql-app-secret"
	mongoRootPW = "mongo-r00t-secret"
	mongoAppPW  = "mongo-app-secret"
)

func dbEnv() []string {
	return []string{
		"POSTGRES_USER=app", "POSTGRES_PASSWORD=" + pgSecret, "POSTGRES_DB=appdb",
		"MYSQL_ROOT_PASSWORD=" + myRootPW, "MYSQL_USER=appuser", "MYSQL_PASSWORD=" + myAppPW, "MYSQL_DATABASE=appdb",
		"MONGO_INITDB_ROOT_USERNAME=root", "MONGO_INITDB_ROOT_PASSWORD=" + mongoRootPW,
		"MONGODB_USERNAME=appuser", "MONGODB_PASSWORD=" + mongoAppPW, "MONGODB_DATABASE=appdb",
	}
}

func TestNoDatabasePasswordEverReachesTheCommandLine(t *testing.T) {
	env := dbEnv()
	secrets := []string{pgSecret, myRootPW, myAppPW, mongoRootPW, mongoAppPW}

	commands := map[string][]string{
		"postgres dump":     dbDumpCommand("postgres", env, nil),
		"postgres subset":   dbDumpCommand("postgres", env, []string{"appdb"}),
		"mysql dump":        dbDumpCommand("mysql", env, nil),
		"mysql subset":      dbDumpCommand("mysql", env, []string{"appdb"}),
		"mongodb dump":      dbDumpCommand("mongodb", env, nil),
		"pg readiness":      PGReadinessCmd(env),
		"pg extensions":     PGExtensionsCmd(env),
		"db list postgres":  DBListCommand("postgres", env),
		"db list mysql":     DBListCommand("mysql", env),
		"mysql auth probe":  MySQLAuthProbeCommand(env),
		"embedded postgres": embeddedDumpCommand(&EmbeddedDump{Engine: "postgres", User: "app", DBName: "appdb"}, env),
	}

	for name, cmd := range commands {
		if len(cmd) == 0 {
			t.Fatalf("%s produced no command", name)
		}
		script := strings.Join(cmd, " ")
		for _, secret := range secrets {
			if strings.Contains(script, secret) {
				t.Errorf("%s puts a password in the command line, where the container can read it:\n%s", name, script)
			}
		}
	}
}

// The other half of the contract: the script must still REACH the credential, or
// the dump silently authenticates as nobody.
func TestEveryDumpStillReachesItsCredentialByName(t *testing.T) {
	env := dbEnv()
	cases := map[string]struct {
		cmd  []string
		want []string
	}{
		"postgres dump":    {dbDumpCommand("postgres", env, nil), []string{"POSTGRES_PASSWORD"}},
		"postgres subset":  {dbDumpCommand("postgres", env, []string{"appdb"}), []string{"POSTGRES_PASSWORD"}},
		"pg readiness":     {PGReadinessCmd(env), []string{"POSTGRES_PASSWORD"}},
		"pg extensions":    {PGExtensionsCmd(env), []string{"POSTGRES_PASSWORD"}},
		"db list postgres": {DBListCommand("postgres", env), []string{"POSTGRES_PASSWORD"}},
		"mysql dump": {dbDumpCommand("mysql", env, nil),
			[]string{"MYSQL_ROOT_PASSWORD", "MARIADB_ROOT_PASSWORD", "MYSQL_USER", "MYSQL_PASSWORD", "MYSQL_DATABASE"}},
		"mysql subset": {dbDumpCommand("mysql", env, []string{"appdb"}),
			[]string{"MYSQL_ROOT_PASSWORD", "MARIADB_ROOT_PASSWORD", "MYSQL_USER", "MYSQL_PASSWORD"}},
		"db list mysql": {DBListCommand("mysql", env),
			[]string{"MYSQL_ROOT_PASSWORD", "MARIADB_ROOT_PASSWORD"}},
		"mongodb dump": {dbDumpCommand("mongodb", env, nil),
			[]string{"MONGO_INITDB_ROOT_PASSWORD", "MONGODB_ROOT_PASSWORD", "MONGODB_PASSWORD", "MONGODB_DATABASE"}},
	}
	for name, c := range cases {
		script := strings.Join(c.cmd, " ")
		for _, want := range c.want {
			if !strings.Contains(script, want) {
				t.Errorf("%s no longer reaches %s — it would authenticate as nobody:\n%s", name, want, script)
			}
		}
	}
}

// Several of these scripts run under `set -u`, where a bare reference to an
// unset variable aborts the whole command. Every reference must carry a default.
func TestEveryCredentialReferenceHasADefault(t *testing.T) {
	env := dbEnv()
	scripts := [][]string{
		dbDumpCommand("postgres", env, nil), dbDumpCommand("postgres", env, []string{"appdb"}),
		dbDumpCommand("mysql", env, nil), dbDumpCommand("mysql", env, []string{"appdb"}),
		dbDumpCommand("mongodb", env, nil), PGReadinessCmd(env), PGExtensionsCmd(env),
		DBListCommand("postgres", env), DBListCommand("mysql", env), MySQLAuthProbeCommand(env),
	}
	for _, cmd := range scripts {
		script := strings.Join(cmd, " ")
		for _, name := range []string{
			"POSTGRES_PASSWORD", "MYSQL_ROOT_PASSWORD", "MYSQL_USER", "MYSQL_PASSWORD",
			"MYSQL_DATABASE", "MONGO_INITDB_ROOT_USERNAME", "MONGODB_USERNAME", "MONGODB_PASSWORD",
		} {
			// Every occurrence must be inside a ${…:-…} expansion, never a bare $VAR.
			if strings.Contains(script, "$"+name) && !strings.Contains(script, "${"+name+":-") {
				t.Errorf("%s is referenced without a default — under `set -u` that aborts the dump:\n%s", name, script)
			}
		}
	}
}
