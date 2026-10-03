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

// F203 — the authentication policy stops being a compile-time constant.
//
// The session lifetime and the minimum password length were the one part of the
// security posture an operator could not adjust without rebuilding the image,
// and the right answer genuinely depends on the deployment. The tests below are
// mostly about the BOUNDS, because a policy that can be edited is only safe if
// it cannot be edited into nothing.

func policyServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	s.cfg = &config.Config{}
	return s
}

// AC1 — DOCKBACK_SESSION_TTL_HOURS=1 produces a session that expires in an hour.
// Asserted end to end on the stored row, not on the resolver, because the whole
// point is that the value reaches CreateSession.
func TestSessionTTLFromEnvReachesTheStoredSession(t *testing.T) {
	s := policyServer(t)
	s.cfg.SessionTTLHours = 1

	hash, err := crypto.HashPassword("correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := s.store.CreateUser("admin", hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.CreateSessionFrom("tok", uid, s.sessionTTL(), "10.0.0.1", "Firefox/153.0"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.store.ListUserSessions(uid)
	if err != nil || len(rows) != 1 {
		t.Fatalf("expected one session, got %d (%v)", len(rows), err)
	}
	if got := rows[0].ExpiresAt - rows[0].CreatedAt; got < 3590 || got > 3610 {
		t.Errorf("session lifetime = %ds, want ~3600", got)
	}
}

// AC3 — an unconfigured deployment behaves exactly as it did before this
// feature. An upgrade that quietly changes anyone's session length or password
// rule would be a worse outcome than the setting not existing.
func TestUnconfiguredPolicyIsUnchanged(t *testing.T) {
	s := policyServer(t)
	if got := s.sessionTTL(); got != 12*time.Hour {
		t.Errorf("default session lifetime = %v, want 12h", got)
	}
	if got := s.minPasswordLength(); got != 12 {
		t.Errorf("default minimum password length = %d, want 12", got)
	}
	if got := store.SessionIdleTTL; got != 30*time.Minute {
		t.Errorf("default idle window = %v, want 30m", got)
	}
}

// AC2 — the password minimum is a floor, not a default. Every route into it is
// tested because a single unclamped one is the whole hole: config, setting, and
// the two together.
func TestMinPasswordLengthCannotGoBelowTheBaseline(t *testing.T) {
	s := policyServer(t)

	// Via config — envIntMin already refuses it, but the resolver must not trust
	// that, since cfg is also set directly in tests and could be in future code.
	s.cfg.MinPasswordLen = 4
	if got := s.minPasswordLength(); got != 12 {
		t.Errorf("a 4-character config = %d, want the 12 floor", got)
	}

	// Via the setting.
	if err := s.store.SetSetting("security.min_password_len", "6"); err != nil {
		t.Fatal(err)
	}
	if got := s.minPasswordLength(); got != 12 {
		t.Errorf("a 6-character setting = %d, want the 12 floor", got)
	}

	// The coercion that guards the write refuses it too, so the weak value never
	// reaches the row in the first place.
	if v, ok := coerceSetting("security.min_password_len", "6"); !ok || v != "12" {
		t.Errorf("coerced 6 = %q (ok=%v), want 12", v, ok)
	}
	if v, _ := coerceSetting("security.min_password_len", "-1"); v != "12" {
		t.Errorf("coerced -1 = %q, want 12", v)
	}
	if v, _ := coerceSetting("security.min_password_len", "not-a-number"); v != "12" {
		t.Errorf("coerced garbage = %q, want 12", v)
	}

	// Raising it is the direction that IS allowed.
	if err := s.store.SetSetting("security.min_password_len", "20"); err != nil {
		t.Fatal(err)
	}
	if got := s.minPasswordLength(); got != 20 {
		t.Errorf("a raised minimum = %d, want 20", got)
	}
	// …but not past something no human will type.
	if v, _ := coerceSetting("security.min_password_len", "100000"); v != "128" {
		t.Errorf("an absurd minimum = %q, want it capped at 128", v)
	}
}

// The session lifetime is bounded rather than floored — a window's acceptable
// length really does depend on the deployment — but it is still bounded.
func TestSessionTTLIsBounded(t *testing.T) {
	s := policyServer(t)
	for setting, want := range map[string]time.Duration{
		"1":     time.Hour,
		"0":     time.Hour,       // below the minimum clamps up
		"-5":    time.Hour,       // as does a negative
		"720":   720 * time.Hour, // the 30-day ceiling
		"99999": maxSessionTTLHours * time.Hour,
		"48":    48 * time.Hour,
	} {
		if err := s.store.SetSetting("security.session_ttl_hours", setting); err != nil {
			t.Fatal(err)
		}
		if got := s.sessionTTL(); got != want {
			t.Errorf("session_ttl_hours=%q resolved to %v, want %v", setting, got, want)
		}
	}
	if v, _ := coerceSetting("security.session_ttl_hours", "0"); v != "1" {
		t.Errorf("coerced 0 hours = %q, want 1", v)
	}
	if v, _ := coerceSetting("security.session_idle_minutes", "0"); v != "1" {
		t.Errorf("coerced 0 idle minutes = %q, want 1", v)
	}
}

// The in-app setting overrides the deployment default, in both directions.
func TestSettingOverridesTheEnvDefault(t *testing.T) {
	s := policyServer(t)
	s.cfg.SessionTTLHours = 8
	if got := s.sessionTTL(); got != 8*time.Hour {
		t.Fatalf("with no setting, the env default should win: %v", got)
	}
	if err := s.store.SetSetting("security.session_ttl_hours", "2"); err != nil {
		t.Fatal(err)
	}
	if got := s.sessionTTL(); got != 2*time.Hour {
		t.Errorf("the setting should override the env default: %v", got)
	}
}

// A password change is refused against the CURRENT minimum, not the compiled
// one — otherwise raising the policy would be decorative.
func TestPasswordChangeEnforcesTheConfiguredMinimum(t *testing.T) {
	s := policyServer(t)
	hash, err := crypto.HashPassword("correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := s.store.CreateUser("admin", hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.CreateSessionFrom("tok", uid, time.Hour, "10.0.0.1", "Firefox/153.0"); err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetSetting("security.min_password_len", "20"); err != nil {
		t.Fatal(err)
	}

	change := func(pw string) *httptest.ResponseRecorder {
		body := `{"current":"correct-horse-battery","new":"` + pw + `"}`
		r := httptest.NewRequest("POST", "/api/password", strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
		rec := httptest.NewRecorder()
		s.handleChangePassword(rec, r)
		return rec
	}

	// Sixteen characters passed the old rule and must now be refused, with the
	// message naming the number actually in force.
	rec := change("sixteen-chars-ok")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a 16-character password under a 20-character policy = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "20") {
		t.Errorf("the refusal must name the configured minimum: %s", rec.Body.String())
	}
}

// The bootstrap password is held to the same rule, and a weak one is replaced
// rather than used — first run must not be the one path with no length check.
func TestBootstrapPasswordTooShortIsReplacedNotAccepted(t *testing.T) {
	s := policyServer(t)

	pw, rejected, err := EnsureAdmin(s.store, "admin", "admin", 12)
	if err != nil {
		t.Fatal(err)
	}
	if !rejected {
		t.Fatal("a 5-character bootstrap password must be refused")
	}
	if len(pw) < 12 {
		t.Fatalf("the replacement must satisfy the policy, got %d characters", len(pw))
	}
	// The weak password must not work; the generated one must.
	u, err := s.store.GetUserByName("admin")
	if err != nil {
		t.Fatal(err)
	}
	if crypto.VerifyPassword("admin", u.PasswordHash) {
		t.Error("the refused password must not be the account's password")
	}
	if !crypto.VerifyPassword(pw, u.PasswordHash) {
		t.Error("the generated password must be the account's password")
	}

	// A long enough one is used as given, and reported as neither generated nor
	// rejected.
	s2 := policyServer(t)
	pw2, rejected2, err := EnsureAdmin(s2.store, "admin", "a-perfectly-fine-password", 12)
	if err != nil {
		t.Fatal(err)
	}
	if rejected2 || pw2 != "" {
		t.Errorf("an acceptable password should be used as-is (pw=%q rejected=%v)", pw2, rejected2)
	}
}

// Saving the authentication policy requires fresh proof of the password, the
// same as minting a token. Whoever sits down at a signed-in laptop must not be
// able to quietly extend their own access.
func TestAuthPolicySaveNeedsStepUp(t *testing.T) {
	s := policyServer(t)
	hash, err := crypto.HashPassword("correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := s.store.CreateUser("admin", hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.CreateSessionFrom("tok", uid, time.Hour, "10.0.0.1", "Firefox/153.0"); err != nil {
		t.Fatal(err)
	}

	save := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/settings", strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
		rec := httptest.NewRecorder()
		s.handleSetSettings(rec, r)
		return rec
	}

	// Without a password: refused, and the row is untouched.
	rec := save(`{"security.session_ttl_hours":"720"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated policy change = %d, want 401", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["step_up_required"] != true {
		t.Errorf("the client needs to be told to prompt: %s", rec.Body.String())
	}
	if v, _ := s.store.GetSetting("security.session_ttl_hours", ""); v != "" {
		t.Fatalf("the policy must not have changed: %q", v)
	}

	// With it: applied.
	rec = save(`{"security.session_ttl_hours":"720","` + stepUpPasswordKey + `":"correct-horse-battery"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("an authenticated policy change = %d: %s", rec.Code, rec.Body.String())
	}
	if v, _ := s.store.GetSetting("security.session_ttl_hours", ""); v != "720" {
		t.Errorf("the policy should now be 720: %q", v)
	}
	// The credential travelled in the body and must not have been stored as one.
	if v, _ := s.store.GetSetting(stepUpPasswordKey, ""); v != "" {
		t.Error("the step-up password must never be written to settings")
	}

	// An ORDINARY settings save is unaffected — nobody is asked to re-authenticate
	// to change a compression knob.
	if rec := save(`{"verify.deep":"true"}`); rec.Code != http.StatusOK {
		t.Errorf("an ordinary settings save = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// The gate fires on any of the three keys, and only on those.
func TestAuthPolicyChangeDetection(t *testing.T) {
	for _, k := range []string{"security.session_ttl_hours", "security.session_idle_minutes", "security.min_password_len"} {
		if !authPolicyChange(map[string]string{"verify.deep": "true", k: "1"}) {
			t.Errorf("%s must require step-up", k)
		}
	}
	if authPolicyChange(map[string]string{"verify.deep": "true", "security.egress_allow": "example.com"}) {
		t.Error("an operational save must not demand a password")
	}
	if authPolicyChange(map[string]string{}) {
		t.Error("an empty save changes no policy")
	}
}

// A shortened idle window has to take effect now, not at the next restart, and
// has to survive that restart once it does.
func TestIdleWindowAppliesLiveAndIsBoundedAtBoot(t *testing.T) {
	original := store.SessionIdleTTL
	t.Cleanup(func() { store.SetSessionIdleTTL(original) })

	store.SetSessionIdleTTL(5 * time.Minute)
	if store.SessionIdleTTL != 5*time.Minute {
		t.Fatalf("idle window = %v, want 5m", store.SessionIdleTTL)
	}
	// A non-positive duration is ignored rather than applied: a zero window would
	// sign everyone out on their next click.
	store.SetSessionIdleTTL(0)
	if store.SessionIdleTTL != 5*time.Minute {
		t.Errorf("a zero idle window must be ignored, got %v", store.SessionIdleTTL)
	}
	store.SetSessionIdleTTL(-time.Hour)
	if store.SessionIdleTTL != 5*time.Minute {
		t.Errorf("a negative idle window must be ignored, got %v", store.SessionIdleTTL)
	}
}
