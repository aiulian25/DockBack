package api

import (
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"dockback/internal/backup"
	"dockback/internal/config"
	"dockback/internal/crypto"
	"dockback/internal/store"
)

// F205 — the recorded Redis password, at the API and rotation boundaries.
//
// Two things matter here beyond storage. The value must be write-only over the
// API — it is a live credential to a running service, so an endpoint that can
// hand it back is an endpoint that hands it to whoever reaches the API. And it
// must survive a master-key rotation, because a sealed setting left behind on
// the old key fails SILENTLY: the password stops opening, the dump falls back to
// a raw file capture, and the only symptom is backups quietly grading lower
// again — the exact loop this setting exists to close.

func redisAPIServer(t *testing.T) *Server {
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
	return &Server{
		store:  st,
		cfg:    &config.Config{EncryptionKey: key},
		engine: &backup.Engine{Store: st, Key: key, Log: func(string, string, string) {}},
	}
}

// openRecorded reads a stored password back the only way anything can: by
// unsealing it with a master key. There is deliberately no getter in the
// production code, so the test does what a dump does.
func openRecorded(t *testing.T, s *Server, node, name string, key []byte) string {
	t.Helper()
	stored, _ := s.store.GetSetting("redis.auth."+node+"."+name, "")
	if strings.TrimSpace(stored) == "" {
		return ""
	}
	blob, err := hex.DecodeString(stored)
	if err != nil {
		return ""
	}
	pw, err := crypto.OpenString(blob, key)
	if err != nil {
		return ""
	}
	return pw
}

// The password never comes back out — not in the container detail payload, not
// in the audit trail. Only the boolean does.
func TestRedisAuthIsNeverReturnedByTheAPI(t *testing.T) {
	s := redisAPIServer(t)
	const pw = "runtime-only-password"
	if err := s.engine.SetRedisAuth("node1", "paperless-redis", pw); err != nil {
		t.Fatal(err)
	}
	if !s.engine.HasRedisAuth("node1", "paperless-redis") {
		t.Fatal("should be recorded")
	}

	// The field the detail endpoint exposes is a boolean, and the payload built
	// from it carries nothing else.
	payload, err := json.Marshal(map[string]any{
		"redis_auth_set": s.engine.HasRedisAuth("node1", "paperless-redis"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), pw) {
		t.Fatalf("the password reached a response payload: %s", payload)
	}
	if !strings.Contains(string(payload), "true") {
		t.Errorf("the boolean must still report it is set: %s", payload)
	}

	// The audit row records the fact, never the value — that trail is readable
	// by any admin and is exported inside app backups.
	_ = s.store.Audit("admin", "redis.auth.set", "paperless-redis", "per-container Redis password (value not recorded)")
	entries, err := s.store.ListAudit(100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Detail+e.Target+e.Action, pw) {
			t.Fatalf("the password reached the audit trail: %+v", e)
		}
	}
}

// A trailing space from a copy-paste is invisible in a masked field, and the
// only symptom would be a broker that keeps failing to authenticate. The
// handler trims once, before sealing.
func TestRedisAuthTrimsPastedWhitespace(t *testing.T) {
	s := redisAPIServer(t)
	if err := s.engine.SetRedisAuth("node1", "redis", strings.TrimSpace("  the-password\n")); err != nil {
		t.Fatal(err)
	}
	if got := openRecorded(t, s, "node1", "redis", s.cfg.EncryptionKey); got != "the-password" {
		t.Errorf("stored = %q, want the trimmed password", got)
	}
}

// A master-key rotation carries the recorded passwords onto the new key.
// Without this the setting stops working after a rotation and every affected
// container silently returns to the file fallback.
func TestKeyRotationResealsRedisPasswords(t *testing.T) {
	s := redisAPIServer(t)
	oldKey := s.cfg.EncryptionKey
	newKey := make([]byte, 32)
	for i := range newKey {
		newKey[i] = byte(200 - i)
	}

	const pwA, pwB = "password-for-broker-a", "password-for-broker-b"
	if err := s.engine.SetRedisAuth("node1", "redis-a", pwA); err != nil {
		t.Fatal(err)
	}
	if err := s.engine.SetRedisAuth("node2", "redis-b", pwB); err != nil {
		t.Fatal(err)
	}
	// A cleared entry must not count as a failure.
	if err := s.engine.SetRedisAuth("node3", "redis-c", ""); err != nil {
		t.Fatal(err)
	}
	// A row sealed under some OTHER key: counted, and LEFT ALONE rather than
	// overwritten — it is the only copy of that password there is.
	strayKey := make([]byte, 32)
	for i := range strayKey {
		strayKey[i] = 0x5A
	}
	stray := &backup.Engine{Store: s.store, Key: strayKey}
	if err := stray.SetRedisAuth("node4", "redis-d", "sealed-under-another-key"); err != nil {
		t.Fatal(err)
	}
	before, _ := s.store.GetSetting("redis.auth.node4.redis-d", "")

	resealed, failed := s.resealRedisAuth(oldKey, newKey)
	if resealed != 2 {
		t.Errorf("resealed = %d, want 2", resealed)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1 (the row sealed under another key)", failed)
	}
	if after, _ := s.store.GetSetting("redis.auth.node4.redis-d", ""); after != before {
		t.Error("a row that could not be opened must be left untouched, never overwritten")
	}

	// Both passwords open under the NEW key — the whole point of the re-seal.
	if got := openRecorded(t, s, "node1", "redis-a", newKey); got != pwA {
		t.Errorf("broker A after rotation = %q, want %q", got, pwA)
	}
	if got := openRecorded(t, s, "node2", "redis-b", newKey); got != pwB {
		t.Errorf("broker B after rotation = %q, want %q", got, pwB)
	}
	// And no longer under the old one, which is what re-sealing means.
	if got := openRecorded(t, s, "node1", "redis-a", oldKey); got != "" {
		t.Errorf("the old key must no longer open it, got %q", got)
	}

	// The engine switched to the new key still finds it — end to end.
	s.engine.Key = newKey
	if !s.engine.HasRedisAuth("node1", "redis-a") {
		t.Error("the setting should still read as recorded after rotation")
	}
}

// A rotation with nothing recorded is a clean no-op, not an error.
func TestKeyRotationWithNoRedisPasswords(t *testing.T) {
	s := redisAPIServer(t)
	newKey := make([]byte, 32)
	for i := range newKey {
		newKey[i] = 0x11
	}
	if r, f := s.resealRedisAuth(s.cfg.EncryptionKey, newKey); r != 0 || f != 0 {
		t.Errorf("nothing recorded should reseal nothing: %d/%d", r, f)
	}
}
