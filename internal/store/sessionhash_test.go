package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// API tokens have always been hashed at rest. Sessions were not, so a copy of
// dockback.db handed whoever held it a set of live admin sessions — and that
// file travels: into an app-backup of the data volume, onto a NAS share, into
// whatever a support bundle sweeps up.
func TestSessionTokenIsNeverStoredInTheClear(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	uid, err := st.CreateUser("admin", "hash")
	if err != nil {
		t.Fatal(err)
	}

	const cookie = "a-very-secret-session-token-value"
	if err := st.CreateSessionFrom(cookie, uid, time.Hour, "203.0.113.7", "Firefox"); err != nil {
		t.Fatal(err)
	}

	// Nothing in the sessions table may be the cookie value.
	rows, err := st.db.Query(`SELECT token FROM sessions`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := 0
	for rows.Next() {
		var stored string
		if err := rows.Scan(&stored); err != nil {
			t.Fatal(err)
		}
		found++
		if stored == cookie {
			t.Error("the cookie value is in the database — a copy of the file is a set of live sessions")
		}
		if stored != SessionKey(cookie) {
			t.Errorf("stored %q, want the hash of the cookie value", stored)
		}
		if len(stored) != 64 {
			t.Errorf("stored key is %d chars, want a 64-char SHA-256 hex", len(stored))
		}
	}
	if found != 1 {
		t.Fatalf("want one session row, got %d", found)
	}

	// And the cookie value still works, which is the whole point.
	if _, name, err := st.SessionUser(cookie); err != nil || name != "admin" {
		t.Fatalf("the cookie must still authenticate: %v (%q)", err, name)
	}
	// The stored key is not a second credential: presenting it must not work.
	if _, _, err := st.SessionUser(SessionKey(cookie)); err == nil {
		t.Error("the stored key must not authenticate — otherwise hashing bought nothing")
	}
}

// Every path that identifies a session must agree on the stored form, or one of
// them silently stops matching.
func TestEverySessionPathAgreesOnTheStoredKey(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	uid, _ := st.CreateUser("admin", "hash")
	const cookie = "cookie-value"
	if err := st.CreateSession(cookie, uid, time.Hour); err != nil {
		t.Fatal(err)
	}

	if err := st.TouchSession(cookie); err != nil {
		t.Errorf("TouchSession must find the session: %v", err)
	}
	if err := st.TouchSessionFrom(cookie, "203.0.113.9"); err != nil {
		t.Errorf("TouchSessionFrom must find the session: %v", err)
	}
	if err := st.ExtendSession(cookie, time.Now().Add(2*time.Hour).Unix()); err != nil {
		t.Errorf("ExtendSession must find the session: %v", err)
	}

	// The list methods hand back the STORED key, never the cookie value.
	keys, err := st.ListUserSessionKeys(uid)
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListUserSessionKeys = %v, %v", keys, err)
	}
	if keys[0] != SessionKey(cookie) {
		t.Errorf("listed %q, want the stored key", keys[0])
	}
	devices, err := st.ListUserSessions(uid)
	if err != nil || len(devices) != 1 {
		t.Fatalf("ListUserSessions = %v, %v", devices, err)
	}
	if devices[0].Key != SessionKey(cookie) || strings.Contains(devices[0].Key, cookie) {
		t.Errorf("device key %q must be the stored key", devices[0].Key)
	}

	// Deleting by the listed key ends the session, and so does deleting by cookie.
	if err := st.DeleteSessionByKey(keys[0]); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SessionUser(cookie); err == nil {
		t.Error("the session should be gone")
	}
	if err := st.CreateSession(cookie, uid, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSession(cookie); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SessionUser(cookie); err == nil {
		t.Error("deleting by cookie value must end the session too")
	}
}
