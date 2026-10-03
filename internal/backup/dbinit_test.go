package backup

import (
	"strings"
	"testing"
)

// F174 — the check that runs immediately before a restore empties a database's
// data directory.
//
// The failure it exists for was reported from a real cross-host Nextcloud
// restore: MariaDB came back as "database did not become ready within timeout",
// when in fact the container had exited seconds in. It exited because the
// restore had just deleted its data directory so the engine would re-initialise,
// and the image refuses to initialise an empty directory with no root password
// in its environment. That environment had been fine for the life of the
// container, because the directory was initialised once, long ago.
func TestDBInitPossible(t *testing.T) {
	cases := []struct {
		name   string
		engine string
		env    []string
		ok     bool
	}{
		// The reported failure: a Nextcloud compose file that sets the
		// application's database user and nothing else. Correct for a container
		// that already has a data directory, fatal for one that does not.
		{"mariadb with only the app user", "mysql",
			[]string{"MYSQL_DATABASE=nextcloud", "MYSQL_USER=nextcloud", "MYSQL_PASSWORD=hunter2"}, false},

		{"mysql root password", "mysql", []string{"MYSQL_ROOT_PASSWORD=x"}, true},
		{"mariadb root password", "mysql", []string{"MARIADB_ROOT_PASSWORD=x"}, true},
		{"random root password", "mysql", []string{"MYSQL_RANDOM_ROOT_PASSWORD=yes"}, true},
		// Named in the image's own refusal message, so it must be accepted here.
		{"root password hash", "mysql", []string{"MARIADB_ROOT_PASSWORD_HASH=*ABC"}, true},
		{"empty root password allowed", "mysql", []string{"MARIADB_ALLOW_EMPTY_ROOT_PASSWORD=1"}, true},
		// A _FILE variant points at a mounted secret; the entrypoint reads it the
		// same way, and refusing here would block the more secure configuration.
		{"root password from a file", "mysql", []string{"MYSQL_ROOT_PASSWORD_FILE=/run/secrets/db"}, true},
		// Present but empty is not set. The entrypoint treats it as absent, so
		// so must this — otherwise the check passes and the container still dies.
		{"root password present but empty", "mysql", []string{"MYSQL_ROOT_PASSWORD="}, false},
		{"root password only whitespace", "mysql", []string{"MYSQL_ROOT_PASSWORD=   "}, false},

		{"postgres password", "postgres", []string{"POSTGRES_PASSWORD=x"}, true},
		{"postgres trust auth", "postgres", []string{"POSTGRES_HOST_AUTH_METHOD=trust"}, true},
		{"postgres with only a db name", "postgres", []string{"POSTGRES_DB=app", "POSTGRES_USER=app"}, false},

		// Engines whose first-init rules are not encoded here must never be
		// refused. This check may only ever stop a restore it actually
		// understands; anything else proceeds exactly as before.
		{"mongodb needs nothing", "mongodb", nil, true},
		{"an unknown engine is not judged", "clickhouse", nil, true},
		{"no environment at all, unknown engine", "", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, missing := dbInitPossible(c.engine, c.env)
			if ok != c.ok {
				t.Fatalf("dbInitPossible(%q, %v) = %v, want %v", c.engine, c.env, ok, c.ok)
			}
			// A refusal has to name the variable that fixes it. "Cannot
			// initialise" with nothing to act on is worse than no check at all,
			// because it arrives at the moment the operator most needs to move.
			if !ok && missing == "" {
				t.Error("a refusal must name what is missing")
			}
			if ok && missing != "" {
				t.Errorf("a pass must claim nothing is missing, got %q", missing)
			}
		})
	}
}

// The variables this check accepts must be the same ones DockBack itself sets
// when it initialises a throwaway database for verification. If those two lists
// ever drift, one half of the app would be initialising empty data directories
// with variables the other half considers insufficient.
func TestDBInitPossibleAgreesWithVerificationEnv(t *testing.T) {
	for engine, env := range map[string][]string{
		"mysql":    {"MYSQL_ROOT_PASSWORD=secret", "MARIADB_ROOT_PASSWORD=secret"},
		"postgres": {"POSTGRES_PASSWORD=secret", "POSTGRES_HOST_AUTH_METHOD=trust"},
	} {
		if ok, missing := dbInitPossible(engine, env); !ok {
			t.Errorf("%s: the environment DockBack uses to initialise a fresh database "+
				"must satisfy this check, but it reports %q missing", engine, missing)
		}
	}
}

// DBInitBlock is the plan-time form: manifest only, keys only, and silent
// wherever it cannot be sure.
func TestDBInitBlock(t *testing.T) {
	dbMan := func(keys ...string) *Manifest {
		return &Manifest{
			Databases:        []DBDump{{Engine: "mysql"}},
			ContainerEnvKeys: keys,
		}
	}
	if got := DBInitBlock(dbMan("MYSQL_DATABASE", "MYSQL_USER", "MYSQL_PASSWORD")); got == "" {
		t.Error("the reported configuration should be flagged before the restore starts")
	} else if !strings.Contains(got, "MYSQL_ROOT_PASSWORD") {
		t.Errorf("the warning must name the variable to add: %q", got)
	}
	if got := DBInitBlock(dbMan("MARIADB_ROOT_PASSWORD")); got != "" {
		t.Errorf("a container that can initialize must produce nothing: %q", got)
	}

	// Silence wherever there is no evidence. A backup taken before env keys were
	// recorded, or one that is not a database at all, must not render as
	// misconfigured — "we do not know" and "it is broken" are different claims.
	for name, m := range map[string]*Manifest{
		"nil manifest":         nil,
		"no recorded env keys": {Databases: []DBDump{{Engine: "mysql"}}},
		"not a database":       {ContainerEnvKeys: []string{"TZ"}},
		"unknown engine":       {Databases: []DBDump{{Engine: "clickhouse"}}, ContainerEnvKeys: []string{"TZ"}},
	} {
		if got := DBInitBlock(m); got != "" {
			t.Errorf("%s should say nothing, got %q", name, got)
		}
	}

	// A per-database subset dump imports into the RUNNING engine and never
	// empties the data directory, so the whole question does not arise.
	subset := &Manifest{
		Databases:        []DBDump{{Engine: "mysql", Databases: []string{"nextcloud"}}},
		ContainerEnvKeys: []string{"MYSQL_USER"},
	}
	if got := DBInitBlock(subset); got != "" {
		t.Errorf("a subset dump wipes nothing, so it must not warn: %q", got)
	}
}
