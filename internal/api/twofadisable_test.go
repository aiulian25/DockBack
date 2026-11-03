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

const (
	ipLockThreshold = 5
	testClientIP    = "192.0.2.1"
)

func ipFailures(s *Server) int { return s.guard.load(lockKey("ip", testClientIP)).Fails }

func wrongCodeDisable(s *Server) *httptest.ResponseRecorder {
	return postDisable2FA(s, `{"password":"`+rsPassword+`","code":"000000x"}`)
}

// failBelowThreshold spends every wrong code the address has short of locking.
func failBelowThreshold(t *testing.T, s *Server) {
	t.Helper()
	for attempt := range ipLockThreshold - 1 {
		if rec := wrongCodeDisable(s); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d %s", attempt+1, rec.Code, rec.Body.String())
		}
	}
}

// The finding: with the password known, each retry wiped the failure before it, so the
// six-digit space could be walked for ever and no number of tries ever reached 429.
func TestCorrectPasswordRetriesReachTheLockout(t *testing.T) {
	s, u, recovery := twofaServer(t)
	const requests = 12
	statuses := make([]int, 0, requests)
	for range requests {
		statuses = append(statuses, wrongCodeDisable(s).Code)
	}
	for i, status := range statuses {
		want := http.StatusUnauthorized
		if i >= ipLockThreshold {
			want = http.StatusTooManyRequests
		}
		if status != want {
			t.Fatalf("request %d = %d, want %d (all: %v)", i+1, status, want, statuses)
		}
	}

	// Locked means locked: the right code does nothing now, and is not spent.
	if rec := postDisable2FA(s, `{"password":"`+rsPassword+`","code":"`+recovery[0]+`"}`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the right code while locked: %d %s", rec.Code, rec.Body.String())
	}
	if !twofaStillOn(t, s) {
		t.Fatal("2FA was removed during a lockout")
	}
	if left := recoveryLeft(t, s); left != len(recovery) {
		t.Fatalf("a lockout spent a recovery code: %d left", left)
	}
	if login := postLogin(s, testClientIP, u.Username, rsPassword, recovery[0]); login.Code != http.StatusTooManyRequests {
		t.Fatalf("the lockout is shared with login: %d", login.Code)
	}
}

func TestACorrectPasswordAloneClearsNoFailures(t *testing.T) {
	cases := map[string]func(*Server) *httptest.ResponseRecorder{
		"a disable with no code": func(s *Server) *httptest.ResponseRecorder {
			return postDisable2FA(s, `{"password":"`+rsPassword+`"}`)
		},
		"a password change the policy refuses": func(s *Server) *httptest.ResponseRecorder {
			return postChangePassword(s, `{"current":"`+rsPassword+`","new":"short"}`)
		},
	}
	for name, attempt := range cases {
		t.Run(name, func(t *testing.T) {
			s, _, _ := twofaServer(t)
			failBelowThreshold(t, s)
			if rec := attempt(s); rec.Code != http.StatusBadRequest {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			if got := ipFailures(s); got != ipLockThreshold-1 {
				t.Fatalf("failures = %d, want them kept", got)
			}
			if rec := wrongCodeDisable(s); rec.Code != http.StatusUnauthorized {
				t.Fatalf("the locking attempt: %d", rec.Code)
			}
			if rec := wrongCodeDisable(s); rec.Code != http.StatusTooManyRequests {
				t.Fatalf("the next must be locked out: %d", rec.Code)
			}
		})
	}
}

// A finished password change on a 2FA account proved the password and nothing more.
func TestAPasswordChangeKeepsFactorFailuresWhen2FAIsOn(t *testing.T) {
	s, _, _ := twofaServer(t)
	failBelowThreshold(t, s)
	rec := postChangePassword(s, `{"current":"`+rsPassword+`","new":"a-different-long-password"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if got := ipFailures(s); got != ipLockThreshold-1 {
		t.Fatalf("failures = %d, want them kept", got)
	}
}

func TestAPasswordChangeClearsFailuresWithout2FA(t *testing.T) {
	s, u, _ := twofaServer(t)
	if err := s.store.DisableTOTP(u.ID); err != nil {
		t.Fatal(err)
	}
	s.guard.recordFail(testClientIP, u.Username)
	rec := postChangePassword(s, `{"current":"`+rsPassword+`","new":"a-different-long-password"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if got := ipFailures(s); got != 0 {
		t.Fatalf("failures = %d: the whole check passed, so they clear", got)
	}
}

func TestACompleteAuthenticationClearsFailures(t *testing.T) {
	t.Run("disable", func(t *testing.T) {
		s, _, recovery := twofaServer(t)
		failBelowThreshold(t, s)
		if rec := postDisable2FA(s, `{"password":"`+rsPassword+`","code":"`+recovery[0]+`"}`); rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		if got := ipFailures(s); got != 0 {
			t.Fatalf("failures = %d", got)
		}
	})
	t.Run("login", func(t *testing.T) {
		s, u, recovery := twofaServer(t)
		failBelowThreshold(t, s)
		if rec := postLogin(s, testClientIP, u.Username, rsPassword, recovery[0]); rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		if got := ipFailures(s); got != 0 {
			t.Fatalf("failures = %d", got)
		}
	})
}

func postChangePassword(s *Server, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/account/password", strings.NewReader(body))
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	rec := httptest.NewRecorder()
	s.handleChangePassword(rec, r)
	return rec
}
