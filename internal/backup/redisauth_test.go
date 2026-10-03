package backup

import (
	"errors"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
)

// F180 — finding the Redis password wherever it actually is.
//
// Reported as a PaperlessNGX-REDIS backup failing with the server's own words:
// "NOAUTH Authentication required". The lookup read $REDIS_PASSWORD and nothing
// else, so the most common arrangement of all — the password on the server's own
// command line — was invisible to it.
//
// It cannot be found from INSIDE the container either, and the reason is worth
// keeping: Redis rewrites its own argv to a process title ("redis-server
// *:6379"), so by the time anything reads /proc the flag is gone. Verified live —
// the in-container search alone still failed this case. It survives only in the
// container's recorded configuration, which is read from outside.
func TestRedisAuthFromArgv(t *testing.T) {
	cases := []struct {
		name string
		argv [][]string
		want string
	}{
		{"separate argument", [][]string{{"redis-server", "--requirepass", "s3cret"}}, "s3cret"},
		{"joined with =", [][]string{{"redis-server", "--requirepass=s3cret"}}, "s3cret"},
		{"among other flags", [][]string{{"redis-server", "--appendonly", "yes", "--requirepass", "s3cret", "--maxmemory", "1gb"}}, "s3cret"},
		{"in the entrypoint instead", [][]string{nil, {"redis-server", "--requirepass", "s3cret"}}, "s3cret"},
		// Redis takes the last occurrence of a repeated option; so does this.
		{"repeated takes the last", [][]string{{"redis-server", "--requirepass", "old", "--requirepass", "new"}}, "new"},
		{"no password", [][]string{{"redis-server", "--appendonly", "yes"}}, ""},
		{"nothing at all", [][]string{nil, nil}, ""},
		// A trailing --requirepass with no value is not a password; treating the
		// empty string as one would send redis-cli an AUTH with nothing in it.
		{"flag with no value", [][]string{{"redis-server", "--requirepass"}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := redisAuthFromArgv(c.argv...); got != c.want {
				t.Errorf("redisAuthFromArgv(%v) = %q, want %q", c.argv, got, c.want)
			}
		})
	}
}

// The password reaches redis-cli through the EXEC's environment and never on a
// command line, so nothing inside the container can read it out of the process
// list. That is the same rule the MySQL dump follows with MYSQL_PWD.
func TestRedisDumpExecEnv(t *testing.T) {
	insp := types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{},
		Config:            &container.Config{Cmd: []string{"redis-server", "--requirepass", "s3cret"}},
	}
	env := redisDumpExecEnv(insp)
	if len(env) != 1 || env[0] != "REDISCLI_AUTH=s3cret" {
		t.Fatalf("env = %v", env)
	}
	// Nothing to add when there is nothing there: the in-container search covers
	// every other way the password is configured, and an empty REDISCLI_AUTH
	// would override a value the container itself sets.
	for name, in := range map[string]types.ContainerJSON{
		"no password": {ContainerJSONBase: &types.ContainerJSONBase{}, Config: &container.Config{Cmd: []string{"redis-server"}}},
		"no config":   {ContainerJSONBase: &types.ContainerJSONBase{}},
	} {
		if got := redisDumpExecEnv(in); len(got) != 0 {
			t.Errorf("%s should add nothing, got %v", name, got)
		}
	}

	// The generated script must never put the password on the argv.
	script := strings.Join(redisDumpCommand(), " ")
	for _, bad := range []string{"-a ", "--pass", "AUTH "} {
		if strings.Contains(script, bad) {
			t.Errorf("the password must not reach the command line (%q): %s", bad, script)
		}
	}
	if !strings.Contains(script, "REDISCLI_AUTH") {
		t.Error("the password must be passed through the environment")
	}
	// A missing client is a fallback to a file backup, not a failure.
	if !strings.Contains(script, "exit 127") {
		t.Error("a missing redis-cli must exit 127 so the engine falls back")
	}
	// An unusable one IS a failure: a Redis we cannot dump must not look like one
	// we did.
	if !strings.Contains(script, "exit 3") {
		t.Error("an authentication failure must fail the backup")
	}
}

// F185 — the two additions after the fix shipped and the same deployment failed
// again: a password in a redis.conf, and what happens when there is genuinely
// none to be found.
func TestRedisConfFromArgv(t *testing.T) {
	cases := []struct {
		argv [][]string
		want string
	}{
		{[][]string{{"redis-server", "/etc/redis/redis.conf"}}, "/etc/redis/redis.conf"},
		{[][]string{{"redis-server", "/usr/local/etc/redis/redis.conf", "--appendonly", "yes"}}, "/usr/local/etc/redis/redis.conf"},
		{[][]string{{"docker-entrypoint.sh"}, {"redis-server", "/data/redis.conf"}}, "/data/redis.conf"},
		{[][]string{{"redis-server"}}, ""},
		// A flag that happens to end in .conf is a flag, not the config file.
		{[][]string{{"redis-server", "--include=/etc/other.conf"}}, ""},
	}
	for _, c := range cases {
		if got := redisConfFromArgv(c.argv...); got != c.want {
			t.Errorf("redisConfFromArgv(%v) = %q, want %q", c.argv, got, c.want)
		}
	}

	// The script has to look in the conf the server was started with AND in the
	// paths the images use, since the recorded command line may name none.
	script := strings.Join(redisDumpCommand(), " ")
	for _, want := range []string{"$REDIS_CONF", "/usr/local/etc/redis/redis.conf", "/etc/redis/redis.conf", "requirepass", "user"} {
		if !strings.Contains(script, want) {
			t.Errorf("the conf search must cover %q", want)
		}
	}
}

// Only Redis may fall back to a file capture when it cannot authenticate.
//
// The rule everywhere else is that a database we cannot dump fails loudly: a SQL
// data directory copied from under a running server is torn, and a torn copy
// that grades like a backup is worse than none. Redis is different in a way that
// matters — it writes a COMPLETE RDB to /data on its own schedule, and that file
// is exactly what a restore replays. The honest outcome is an older snapshot,
// not a broken one, and it is recorded on the archive's face either way.
func TestRedisAuthUnavailableIsRedisOnly(t *testing.T) {
	authErr := errors.New(`exited 3: DockBack: this Redis requires a password and none of the places DockBack looks had a working one …`)
	if !redisAuthUnavailable("redis", authErr) {
		t.Error("a Redis auth failure must be recognised")
	}
	for _, engine := range []string{"postgres", "mysql", "mongodb", ""} {
		if redisAuthUnavailable(engine, authErr) {
			t.Errorf("%s must never fall back to a file capture on an auth failure", engine)
		}
	}
	// Some other Redis failure is not this one, and must not be quietly downgraded.
	for _, other := range []error{
		nil,
		errors.New("exited 3: DockBack: Redis did not answer PING: Connection refused"),
		errors.New("exited 1: some other problem"),
	} {
		if redisAuthUnavailable("redis", other) {
			t.Errorf("must not be classified as an auth failure: %v", other)
		}
	}
	// The recorded note has to say a consistent snapshot was NOT taken — that is
	// what keeps it off the archive's face as an ordinary backup.
	if !strings.Contains(redisAuthFallbackNote, "no consistent snapshot") {
		t.Errorf("the fallback note must state what was lost: %q", redisAuthFallbackNote)
	}
}

// F191 — the fallback has to exist at BOTH dump call sites.
//
// F185 added it where an individual backup dumps; the app-consistent snapshot
// dumps through its own windowed path, and that one still hard-failed the
// service — reported as "App-consistent snapshot: service redis: database dump:
// … NOAUTH" — which is how "individual backup works fine, stack backup always
// fails" happens. This covers the piece the consistent path needs that the
// ordinary one does not: putting the data directory back into the capture set
// after the dump that was going to replace it is off the table.
func TestReaddDataDirForFallback(t *testing.T) {
	e := &Engine{Log: func(string, string, string) {}}
	logs := []string{}
	e.Log = func(_, _, msg string) { logs = append(logs, msg) }

	mk := func(dests ...string) *serviceCapture {
		mounts := make([]types.MountPoint, 0, len(dests))
		for _, d := range dests {
			mounts = append(mounts, types.MountPoint{Type: "volume", Name: "redis_data", Destination: d, Source: "/var/lib/docker/volumes/redis_data/_data", Driver: "local"})
		}
		return &serviceCapture{
			id:   "b1",
			name: "PaperlessNGX-REDIS",
			man:  &Manifest{},
			insp: types.ContainerJSON{Mounts: mounts},
		}
	}

	// The normal fallback: /data was excluded because the dump would replace it,
	// the dump did not run, so it comes back — in the tar list AND the manifest.
	sc := mk("/data")
	e.readdDataDirForFallback(sc, "/data")
	if len(sc.volDests) != 1 || sc.volDests[0] != "/data" {
		t.Fatalf("the data dir must rejoin the capture set: %v", sc.volDests)
	}
	if len(sc.man.Volumes) != 1 || sc.man.Volumes[0].Destination != "/data" || sc.man.Volumes[0].Name != "redis_data" {
		t.Fatalf("the manifest must record the mount a restore will need: %+v", sc.man.Volumes)
	}

	// Already selected (the operator opted the raw dir in): nothing is doubled.
	sc = mk("/data")
	sc.volDests = []string{"/data"}
	sc.man.Volumes = []VolumeRef{{Destination: "/data"}}
	e.readdDataDirForFallback(sc, "/data")
	if len(sc.volDests) != 1 || len(sc.man.Volumes) != 1 {
		t.Errorf("an explicit selection must not be duplicated: %v / %v", sc.volDests, sc.man.Volumes)
	}

	// No such mount at all: nothing to fall back TO, and that is said out loud —
	// a backup of a database container holding no database must not be silent
	// about it.
	sc = mk() // no mounts
	logs = logs[:0]
	e.readdDataDirForFallback(sc, "/data")
	if len(sc.volDests) != 0 {
		t.Errorf("nothing should be added when the mount does not exist: %v", sc.volDests)
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l, "no database data") {
			found = true
		}
	}
	if !found {
		t.Error("a fallback with nothing to capture must say so")
	}
}
