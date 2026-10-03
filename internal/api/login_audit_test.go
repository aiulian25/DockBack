package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dockback/internal/crypto"
)

func postLogin(s *Server, ip, user, pass, code string) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"username":%q,"password":%q,"code":%q}`, user, pass, code)
	r := httptest.NewRequest("POST", "/api/login", strings.NewReader(body))
	r.RemoteAddr = ip + ":12345"
	w := httptest.NewRecorder()
	s.handleLogin(w, r)
	return w
}

func auditHas(t *testing.T, s *Server, action string) bool {
	t.Helper()
	es, err := s.store.ListAudit(1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		if e.Action == action {
			return true
		}
	}
	return false
}

// TestLoginAuthFailuresAuditedAndLocked locks in PLAN §3.9: failed logins are
// recorded in the audit trail, repeated failures lock the client (429 +
// Retry-After, audited), and a successful login is audited.
func TestLoginAuthFailuresAuditedAndLocked(t *testing.T) {
	s := newTestServer(t)
	hash, err := crypto.HashPassword("correct-pass-123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.CreateUser("admin", hash); err != nil {
		t.Fatal(err)
	}

	// A) Wrong password is rejected and audited as login.failed.
	if w := postLogin(s, "203.0.113.5", "admin", "wrong-pass", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: want 401, got %d", w.Code)
	}
	if !auditHas(t, s, "login.failed") {
		t.Fatal("a failed login must be written to the audit trail (§3.9)")
	}

	// B) Repeated failures from one IP lock it: 429 + Retry-After, audited.
	const ip = "203.0.113.9"
	for i := 0; i < 5; i++ { // ipLockPolicy.max = 5
		postLogin(s, ip, "admin", "wrong-pass", "")
	}
	w := postLogin(s, ip, "admin", "wrong-pass", "")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("after exceeding the threshold: want 429, got %d", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("a locked login should return a Retry-After header")
	}
	if !auditHas(t, s, "login.locked") {
		t.Fatal("a lockout must be written to the audit trail (§3.9)")
	}

	// C) Correct credentials from a fresh IP succeed and are audited.
	w = postLogin(s, "203.0.113.20", "admin", "correct-pass-123", "")
	if w.Code != http.StatusOK {
		t.Fatalf("correct login: want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !auditHas(t, s, "login.ok") {
		t.Fatal("a successful login should be audited")
	}
}
