package backup

import (
	"path/filepath"
	"strings"
	"testing"

	"dockback/internal/store"
)

// F205 — the operator-recorded Redis password.
//
// A broker whose password was set with `CONFIG SET requirepass` hides it from
// every place the F180 search looks, so that container fell back to a raw file
// capture on EVERY run — losing point-in-time consistency each time, with no
// place to record the answer once. These tests are mostly about the two
// properties that make such a setting safe to have: it is sealed at rest, and
// one lookup serves both dump paths.

func redisAuthEngine(t *testing.T) *Engine {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return &Engine{Store: st, Key: key, Log: func(string, string, string) {}}
}

// AC2 — what lands in the settings table is ciphertext, not the password.
func TestRedisAuthIsSealedAtRest(t *testing.T) {
	e := redisAuthEngine(t)
	const pw = "s3cr3t-runtime-requirepass"

	if err := e.SetRedisAuth("node1", "paperless-redis", pw); err != nil {
		t.Fatal(err)
	}
	stored, err := e.Store.GetSetting("redis.auth.node1.paperless-redis", "")
	if err != nil {
		t.Fatal(err)
	}
	if stored == "" {
		t.Fatal("nothing was stored")
	}
	if strings.Contains(stored, pw) {
		t.Fatalf("the password is stored in the clear: %q", stored)
	}
	// Hex ciphertext, so a casual `SELECT value FROM settings WHERE key LIKE
	// 'redis.auth%'` shows nothing resembling a password.
	for _, c := range stored {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			t.Fatalf("stored value should be hex ciphertext, got %q", stored)
		}
	}
	if got := e.redisAuth("node1", "paperless-redis"); got != pw {
		t.Errorf("unsealed = %q, want the original password", got)
	}
}

// A password sealed under a DIFFERENT master key must read as "not usable", so
// the dump's own search and the file fallback still apply rather than the whole
// backup failing on a key somebody rotated wrong.
func TestRedisAuthUnderAnotherKeyReadsAsUnusable(t *testing.T) {
	e := redisAuthEngine(t)
	if err := e.SetRedisAuth("node1", "cache", "hunter2-hunter2"); err != nil {
		t.Fatal(err)
	}
	other := make([]byte, 32)
	for i := range other {
		other[i] = 0xAA
	}
	e.Key = other

	if got := e.redisAuth("node1", "cache"); got != "" {
		t.Errorf("a password sealed under another key must not open, got %q", got)
	}
	if env := e.redisAuthEnv("node1", "cache"); len(env) != 0 {
		t.Errorf("no usable password means no extra exec environment, got %v", env)
	}
	// HasRedisAuth still reports true — a row IS there, and a UI saying "not
	// set" would invite the operator to overwrite the only copy.
	if !e.HasRedisAuth("node1", "cache") {
		t.Error("the recorded row should still be visible as set")
	}
}

func TestRedisAuthSetAndClear(t *testing.T) {
	e := redisAuthEngine(t)
	if e.HasRedisAuth("node1", "redis") {
		t.Fatal("nothing is recorded yet")
	}
	if err := e.SetRedisAuth("node1", "redis", "a-real-password"); err != nil {
		t.Fatal(err)
	}
	if !e.HasRedisAuth("node1", "redis") {
		t.Fatal("should be recorded")
	}
	if err := e.SetRedisAuth("node1", "redis", ""); err != nil {
		t.Fatal(err)
	}
	if e.HasRedisAuth("node1", "redis") {
		t.Error("an empty value must CLEAR, not seal an empty string")
	}
	if got := e.redisAuth("node1", "redis"); got != "" {
		t.Errorf("cleared should read as absent, got %q", got)
	}
	// Whitespace-only is the same as empty: a stored seal of "   " would look
	// set and authenticate nothing.
	_ = e.SetRedisAuth("node1", "redis", "   ")
	if e.HasRedisAuth("node1", "redis") {
		t.Error("whitespace-only must clear too")
	}
}

// One broker's password must never reach another's dump.
func TestRedisAuthIsScopedToNodeAndContainer(t *testing.T) {
	e := redisAuthEngine(t)
	_ = e.SetRedisAuth("node1", "redis-a", "password-for-a")
	_ = e.SetRedisAuth("node2", "redis-a", "password-for-node-two")

	if got := e.redisAuth("node1", "redis-a"); got != "password-for-a" {
		t.Errorf("node1 = %q", got)
	}
	if got := e.redisAuth("node2", "redis-a"); got != "password-for-node-two" {
		t.Errorf("node2 = %q", got)
	}
	if got := e.redisAuth("node1", "redis-b"); got != "" {
		t.Errorf("a different container must not inherit a password, got %q", got)
	}
	if got := e.redisAuth("node3", "redis-a"); got != "" {
		t.Errorf("a different node must not inherit a password, got %q", got)
	}
}

// It travels as REDISCLI_AUTH — the channel F180 established, which keeps it
// off the argv and out of the container's process list.
func TestRedisAuthEnvUsesTheEnvironmentChannel(t *testing.T) {
	e := redisAuthEngine(t)
	const pw = "point-in-time-please"
	_ = e.SetRedisAuth("node1", "redis", pw)

	env := e.redisAuthEnv("node1", "redis")
	if len(env) != 1 || env[0] != "REDISCLI_AUTH="+pw {
		t.Fatalf("exec env = %v, want a single REDISCLI_AUTH entry", env)
	}
	if len(e.redisAuthEnv("node1", "nothing-here")) != 0 {
		t.Error("no recorded password means no extra environment")
	}
}

// A recorded password REPLACES one recovered from the container's configuration
// rather than sitting beside it: which of two duplicate entries an exec
// environment resolves to is not something to depend on.
func TestRecordedPasswordReplacesTheArgvOne(t *testing.T) {
	fromArgv := []string{"REDISCLI_AUTH=stale-from-compose", "REDIS_CONF=/etc/redis/redis.conf"}
	merged := append([]string{"REDISCLI_AUTH=what-the-operator-typed"}, withoutEnvKey(fromArgv, "REDISCLI_AUTH")...)

	auths := 0
	for _, kv := range merged {
		if strings.HasPrefix(kv, "REDISCLI_AUTH=") {
			auths++
			if kv != "REDISCLI_AUTH=what-the-operator-typed" {
				t.Errorf("the operator's password must be the surviving one, got %q", kv)
			}
		}
	}
	if auths != 1 {
		t.Fatalf("exactly one REDISCLI_AUTH must survive, got %d: %v", auths, merged)
	}
	// The config-file hint points at a path, not a credential, so it survives.
	found := false
	for _, kv := range merged {
		if kv == "REDIS_CONF=/etc/redis/redis.conf" {
			found = true
		}
	}
	if !found {
		t.Error("REDIS_CONF must be preserved — it is a path, not a password")
	}
	// withoutEnvKey must not mutate the caller's slice.
	if fromArgv[0] != "REDISCLI_AUTH=stale-from-compose" {
		t.Errorf("withoutEnvKey mutated its input: %v", fromArgv)
	}
}

// AC3 — one lookup serves both dump paths. The asymmetry F191 fixed was exactly
// this shape: a behaviour added to one dump call site and not the other.
func TestRedisAuthKeyIsSharedByBothDumpPaths(t *testing.T) {
	ordinary := ContainerRef{NodeID: "node1", Name: "paperless-redis"}
	consistent := ContainerRef{NodeID: "node1", Name: "paperless-redis"}
	if redisAuthKeyFor(ordinary.NodeID, ordinary.Name) != redisAuthKeyFor(consistent.NodeID, consistent.Name) {
		t.Fatal("the two dump paths must resolve the same setting")
	}
	// The compose SERVICE label ("redis") is a different string from the
	// container name ("paperless-redis"); keying on it would silently miss every
	// recorded password on a stack backup.
	if redisAuthKeyFor("node1", "redis") == redisAuthKeyFor("node1", "paperless-redis") {
		t.Fatal("service label and container name must not collide")
	}
	if !strings.HasPrefix(redisAuthKeyFor("node1", "x"), RedisAuthKeyPrefix) {
		t.Error("keys must live under the prefix key rotation enumerates")
	}
}

// Key rotation finds these by prefix. A missed one silently reverts a container
// to the file fallback, so the enumeration is worth asserting.
func TestRedisAuthKeysAreDiscoverableByPrefix(t *testing.T) {
	e := redisAuthEngine(t)
	_ = e.SetRedisAuth("node1", "redis-a", "aaa-password-here")
	_ = e.SetRedisAuth("node2", "redis-b", "bbb-password-here")
	_ = e.Store.SetSetting("backup.require_write_only.node1.redis-a", "true")

	keys, err := e.Store.SettingKeysWithPrefix(RedisAuthKeyPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("want both recorded passwords and nothing else, got %v", keys)
	}
	for _, k := range keys {
		if !strings.HasPrefix(k, RedisAuthKeyPrefix) {
			t.Errorf("unrelated setting %q matched the prefix", k)
		}
	}
}
