package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dockback/internal/config"
	"dockback/internal/crypto"
	"dockback/internal/store"
)

// Turning 2FA OFF used to need the password alone — the one action where the
// factor being removed is exactly the factor that would have stopped it. A
// stolen session plus a phished password silently stripped the second factor
// from the only admin account.

func twofaServer(t *testing.T) (*Server, *store.User, []string) {
	t.Helper()
	st := testStore(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 9)
	}
	s := &Server{store: st, guard: newLoginGuard(st), cfg: &config.Config{EncryptionKey: key, MinPasswordLen: 12}}

	hash, err := crypto.HashPassword(rsPassword)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := st.CreateUser("admin", hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession("tok", uid, time.Hour); err != nil {
		t.Fatal(err)
	}

	// Enrol 2FA exactly as the enable handler does.
	sealed, err := s.sealSecret(crypto.NewTOTPSecret())
	if err != nil {
		t.Fatal(err)
	}
	display, hashed := crypto.NewRecoveryCodes(3)
	recoveryJSON, _ := json.Marshal(hashed)
	if err := st.EnableTOTP(uid, sealed, string(recoveryJSON), 0); err != nil {
		t.Fatal(err)
	}
	u, err := st.GetUserByName("admin")
	if err != nil {
		t.Fatal(err)
	}
	return s, u, display
}

func postDisable2FA(s *Server, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/account/2fa/disable", strings.NewReader(body))
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	rec := httptest.NewRecorder()
	s.handleTOTPDisable(rec, r)
	return rec
}

func twofaStillOn(t *testing.T, s *Server) bool {
	t.Helper()
	u, err := s.store.GetUserByName("admin")
	if err != nil {
		t.Fatal(err)
	}
	return u.TOTPSecret != ""
}

// AC1 — the password alone is no longer enough, and 2FA stays on.
func TestDisable2FANeedsTheSecondFactor(t *testing.T) {
	s, _, _ := twofaServer(t)

	rec := postDisable2FA(s, `{"password":"`+rsPassword+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "two-factor code required") {
		t.Errorf("the refusal must say what is missing: %s", rec.Body.String())
	}
	if !twofaStillOn(t, s) {
		t.Fatal("two-factor must still be enabled after a refused disable")
	}
}

// AC2 — a wrong code is refused, counted against the shared lockout, and audited.
func TestDisable2FARejectsAWrongCode(t *testing.T) {
	s, _, _ := twofaServer(t)

	rec := postDisable2FA(s, `{"password":"`+rsPassword+`","code":"not-a-real-code"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid two-factor code") {
		t.Errorf("the refusal must name the failing factor: %s", rec.Body.String())
	}
	if !twofaStillOn(t, s) {
		t.Fatal("a wrong code must leave two-factor enabled")
	}
	// A wrong code counts as a failed attempt, so a six-digit space cannot be
	// walked without hitting the lockout.
	var audited bool
	for _, e := range mustAudit(t, s) {
		if e.Action == "2fa.disable_failed" && strings.Contains(e.Detail, "bad 2fa code") {
			audited = true
		}
	}
	if !audited {
		t.Error("a failed second factor on this endpoint must be audited")
	}
}

// AC3 — a lost authenticator still has a way through: a one-time recovery code
// is accepted, and the disable succeeds.
func TestDisable2FAAcceptsARecoveryCode(t *testing.T) {
	s, _, recovery := twofaServer(t)

	rec := postDisable2FA(s, `{"password":"`+rsPassword+`","code":"`+recovery[0]+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a recovery code must work: %d %s", rec.Code, rec.Body.String())
	}
	if twofaStillOn(t, s) {
		t.Fatal("two-factor should now be disabled")
	}
	var disabled bool
	for _, e := range mustAudit(t, s) {
		if e.Action == "2fa.disabled" {
			disabled = true
		}
	}
	if !disabled {
		t.Error("disabling two-factor must be audited")
	}
}

// The password check still comes first, and a wrong password must not let a
// code be tried or consumed.
func TestDisable2FAStillRequiresThePassword(t *testing.T) {
	s, _, recovery := twofaServer(t)

	rec := postDisable2FA(s, `{"password":"wrong-password","code":"`+recovery[0]+`"}`)
	if rec.Code == http.StatusOK {
		t.Fatal("a wrong password must never disable two-factor")
	}
	if !twofaStillOn(t, s) {
		t.Fatal("two-factor must still be enabled")
	}
	// The recovery code was never reached, so it still works.
	if rec := postDisable2FA(s, `{"password":"`+rsPassword+`","code":"`+recovery[0]+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("the unused recovery code must still be valid: %d %s", rec.Code, rec.Body.String())
	}
}
