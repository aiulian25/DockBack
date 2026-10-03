package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dockback/internal/config"
	"dockback/internal/crypto"
	"dockback/internal/store"
)

// newTestServer builds a minimal Server backed by a temp store for auth tests.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	st := testStore(t)
	return &Server{store: st, cfg: &config.Config{}, guard: newLoginGuard(st)}
}

func TestChangePasswordRevokesOtherSessionsAndRotates(t *testing.T) {
	s := newTestServer(t)
	hash, _ := crypto.HashPassword("currentpass-123")
	uid, err := s.store.CreateUser("admin", hash)
	if err != nil {
		t.Fatal(err)
	}
	// Two live sessions: the caller's and another device's.
	_ = s.store.CreateSession("tok-current", uid, time.Hour)
	_ = s.store.SetSetting("csrf:"+store.SessionKey("tok-current"), "csrfA")
	_ = s.store.CreateSession("tok-other", uid, time.Hour)
	_ = s.store.SetSetting("csrf:"+store.SessionKey("tok-other"), "csrfB")

	body := `{"current":"currentpass-123","new":"brand-new-pass-456"}`
	r := httptest.NewRequest("POST", "/api/account/password", strings.NewReader(body))
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok-current"})
	w := httptest.NewRecorder()
	s.handleChangePassword(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	// The other device is revoked, including its CSRF entry.
	if _, _, err := s.store.SessionUser("tok-other"); err == nil {
		t.Fatal("other session should be revoked")
	}
	if v, _ := s.store.GetSetting("csrf:"+store.SessionKey("tok-other"), ""); v != "" {
		t.Fatal("other session CSRF entry should be deleted")
	}
	// The caller's token is rotated (old token no longer valid).
	if _, _, err := s.store.SessionUser("tok-current"); err == nil {
		t.Fatal("current token should be rotated (old token revoked)")
	}
	// A fresh session cookie was issued.
	var gotSession bool
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" && c.Value != "tok-current" {
			gotSession = true
		}
	}
	if !gotSession {
		t.Fatal("expected a fresh rotated session cookie")
	}
	// Password actually changed: old fails, new verifies.
	u, _ := s.store.GetUserByName("admin")
	if crypto.VerifyPassword("currentpass-123", u.PasswordHash) {
		t.Fatal("old password should no longer be valid")
	}
	if !crypto.VerifyPassword("brand-new-pass-456", u.PasswordHash) {
		t.Fatal("new password should be set")
	}
}

func TestChangePasswordRejectsWrongCurrent(t *testing.T) {
	s := newTestServer(t)
	hash, _ := crypto.HashPassword("currentpass-123")
	uid, _ := s.store.CreateUser("admin", hash)
	_ = s.store.CreateSession("tok", uid, time.Hour)

	r := httptest.NewRequest("POST", "/api/account/password", strings.NewReader(`{"current":"wrong","new":"brand-new-pass-456"}`))
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	w := httptest.NewRecorder()
	s.handleChangePassword(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
	// Password unchanged.
	u, _ := s.store.GetUserByName("admin")
	if !crypto.VerifyPassword("currentpass-123", u.PasswordHash) {
		t.Fatal("password must be unchanged after a failed attempt")
	}
}

func TestExtendSessionOnlyOnce(t *testing.T) {
	s := newTestServer(t)
	uid, _ := s.store.CreateUser("admin", "hash")
	_ = s.store.CreateSession("tok", uid, time.Hour)

	call := func() int {
		r := httptest.NewRequest("POST", "/api/session/extend", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
		w := httptest.NewRecorder()
		s.handleExtendSession(w, r)
		return w.Code
	}

	if code := call(); code != http.StatusOK {
		t.Fatalf("first extend: want 200, got %d", code)
	}
	_, _, extended, _ := s.store.SessionInfo("tok")
	if !extended {
		t.Fatal("session should be marked extended after first extend")
	}
	if code := call(); code != http.StatusConflict {
		t.Fatalf("second extend: want 409 (only one allowed), got %d", code)
	}
}

func TestRevokeOtherSessionsKeepsCurrent(t *testing.T) {
	s := newTestServer(t)
	hash, _ := crypto.HashPassword("currentpass-123")
	uid, _ := s.store.CreateUser("admin", hash)
	_ = s.store.CreateSession("tok-current", uid, time.Hour)
	_ = s.store.SetSetting("csrf:"+store.SessionKey("tok-current"), "csrfA")
	_ = s.store.CreateSession("tok-other1", uid, time.Hour)
	_ = s.store.SetSetting("csrf:"+store.SessionKey("tok-other1"), "csrfB")
	_ = s.store.CreateSession("tok-other2", uid, time.Hour)
	_ = s.store.SetSetting("csrf:"+store.SessionKey("tok-other2"), "csrfC")

	r := httptest.NewRequest("POST", "/api/session/revoke-others", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok-current"})
	w := httptest.NewRecorder()
	s.handleRevokeOtherSessions(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	// Current session survives.
	if _, _, err := s.store.SessionUser("tok-current"); err != nil {
		t.Fatal("current session should remain valid")
	}
	// Others (and their csrf entries) are gone.
	for _, tok := range []string{"tok-other1", "tok-other2"} {
		if _, _, err := s.store.SessionUser(tok); err == nil {
			t.Fatalf("%s should be revoked", tok)
		}
		if v, _ := s.store.GetSetting("csrf:"+store.SessionKey(tok), ""); v != "" {
			t.Fatalf("%s csrf entry should be deleted", tok)
		}
	}
}

func TestChangePasswordEnforcesPolicy(t *testing.T) {
	s := newTestServer(t)
	hash, _ := crypto.HashPassword("currentpass-123")
	uid, _ := s.store.CreateUser("admin", hash)
	_ = s.store.CreateSession("tok", uid, time.Hour)

	r := httptest.NewRequest("POST", "/api/account/password", strings.NewReader(`{"current":"currentpass-123","new":"short"}`))
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	w := httptest.NewRecorder()
	s.handleChangePassword(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for short password, got %d: %s", w.Code, w.Body.String())
	}
	// Original session must still be valid (no revocation on a rejected change).
	if _, _, err := s.store.SessionUser("tok"); err != nil {
		t.Fatal("session should remain valid after a rejected change")
	}
}
