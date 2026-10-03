package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"dockback/internal/notify"
	"dockback/internal/store"
)

// F198 — the auth surface reaches the operator's channels.
//
// Every other tamper signal already did: a changed SSH host key, a changed
// sidecar digest. A lockout, a failed step-up and a master-key reveal were
// written to the audit table and nowhere else, so they were invisible unless
// somebody went and read it.

// Severity is the routing decision, so it is the thing worth pinning: these
// kinds have to land in the bands an operator already filters on.
func TestAuthEventSeverity(t *testing.T) {
	// A lockout and a key reveal sit with the tamper signals. If either was not
	// you, everything else in the app is already at risk.
	for _, k := range []string{notify.KindAuthLockout, notify.KindKeyRevealed} {
		if got := notify.SeverityOf(k); got != notify.SevCritical {
			t.Errorf("%s should be critical, got %v", k, got)
		}
	}
	// A burst is the warning BEFORE a lockout, and a step-up failure is a session
	// that already exists failing to escalate. Paging on a mistyped password is
	// how an operator learns to ignore the channel that later carries the real one.
	for _, k := range []string{notify.KindAuthFailedBurst, notify.KindStepUpFailed} {
		if got := notify.SeverityOf(k); got != notify.SevWarning {
			t.Errorf("%s should be warning, got %v", k, got)
		}
	}
	// Severity drives the email subject tag and the Gotify priority band, so a
	// critical auth event must be indistinguishable in routing from a host-key
	// change — that is the whole point of reusing the existing pipeline.
	if notify.SeverityOf(notify.KindAuthLockout) != notify.SeverityOf(notify.KindHostKeyChanged) {
		t.Error("a lockout must route exactly like the other tamper signals")
	}
}

// The burst threshold must stay strictly BELOW the lockout threshold, or it
// fires at the same instant as the lockout and says the same thing twice.
func TestAuthBurstThresholdStaysAnEarlyWarning(t *testing.T) {
	s := newTestServer(t)

	if got := s.authBurstThreshold(); got != defaultAuthBurstThreshold {
		t.Errorf("default threshold = %d, want %d", got, defaultAuthBurstThreshold)
	}
	if defaultAuthBurstThreshold >= ipLockPolicy.max {
		t.Fatalf("the default (%d) must be below the lockout threshold (%d) or the burst is redundant",
			defaultAuthBurstThreshold, ipLockPolicy.max)
	}

	// An operator setting it at or above the lock threshold gets clamped back to
	// an early warning rather than a duplicate of the lockout alert.
	_ = s.store.SetSetting("alert.auth_burst_threshold", "99")
	if got := s.authBurstThreshold(); got != ipLockPolicy.max-1 {
		t.Errorf("clamped high = %d, want %d", got, ipLockPolicy.max-1)
	}
	// And one typo is not a burst.
	_ = s.store.SetSetting("alert.auth_burst_threshold", "1")
	if got := s.authBurstThreshold(); got != 2 {
		t.Errorf("clamped low = %d, want 2", got)
	}
}

// The lock TRANSITION is what alerts — not "this address is locked".
//
// handleLogin's blocked-attempt branch runs on every retry while a lock holds.
// Alerting from there would send one critical notification per guess, so the
// signal is taken from recordFail's return instead: exactly one alert per
// lockout, and silence for the retries that follow.
func TestRecordFailReportsTheLockTransitionOnce(t *testing.T) {
	s := newTestServer(t)
	const ip = "203.0.113.9"

	locks, bursts := 0, 0
	for i := 1; i <= ipLockPolicy.max; i++ {
		out := s.guard.recordFail(ip, "admin")
		if out.IPLocked {
			locks++
			if out.LockFor <= 0 {
				t.Error("a reported lock must carry how long it holds")
			}
		}
		if !out.IPLocked && out.IPFails >= defaultAuthBurstThreshold {
			bursts++
		}
	}
	if locks != 1 {
		t.Fatalf("exactly one attempt should report the transition, got %d", locks)
	}
	if bursts == 0 {
		t.Error("the run-up to a lockout should cross the burst threshold at least once")
	}

	// Every further attempt while the address is already locked reports NO new
	// transition — this is the property that keeps a grinding attacker from
	// generating one critical alert per guess.
	for i := 0; i < 5; i++ {
		if out := s.guard.recordFail(ip, "admin"); out.IPLocked {
			t.Fatal("a retry during an existing lock must not re-report the transition")
		}
	}
	if d := s.guard.blocked(ip, "admin"); d <= 0 {
		t.Error("the address should still be locked after those retries")
	}
}

// A failure against an address that never reaches the threshold stays quiet.
func TestSingleFailureRaisesNothing(t *testing.T) {
	s := newTestServer(t)
	out := s.guard.recordFail("198.51.100.4", "admin")
	if out.IPLocked {
		t.Error("one failure must not lock")
	}
	if out.IPFails >= s.authBurstThreshold() {
		t.Error("one failure must not reach the burst threshold")
	}
}

// The attempted username is attacker-controlled text on its way to email, a
// webhook and the alert inbox, so it is bounded and stripped of anything that
// could restructure the message it lands in.
func TestSafeLabel(t *testing.T) {
	if got := safeLabel("admin", 64); got != "admin" {
		t.Errorf("an ordinary name must pass through: %q", got)
	}
	if got := safeLabel("  ", 64); got != "(none given)" {
		t.Errorf("empty must read as a stated absence, got %q", got)
	}
	// Newlines are the ones that matter: a body reaching SMTP has no business
	// carrying them.
	got := safeLabel("evil\r\nSubject: injected\nX-Header: y", 64)
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("newlines must not survive: %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Errorf("runs of whitespace should collapse: %q", got)
	}
	// A megabyte of junk cannot be pushed through a notification channel.
	long := safeLabel(strings.Repeat("A", 5000), 64)
	if len([]rune(long)) > 65 {
		t.Errorf("length must be bounded, got %d runes", len([]rune(long)))
	}
	if !strings.HasSuffix(long, "…") {
		t.Errorf("truncation should be visible, got %q", long)
	}
}

// Two paths reach the shared helper (sign-in and step-up) and both run through
// the same per-address cooldown, so they cannot compound into two alerts for one
// incident.
func TestAuthAlertCooldownMatchesTheLockWindow(t *testing.T) {
	if authAlertCooldown != ipLockPolicy.window {
		t.Errorf("cooldown %v should match the lock window %v so one window yields one alert",
			authAlertCooldown, ipLockPolicy.window)
	}
	if authAlertCooldown < time.Minute {
		t.Error("a sub-minute cooldown would let a grinding attacker spam the channel")
	}
}

// The spec's acceptance criterion, driven through the real handler: a run of bad
// sign-ins that ends in a lockout must leave EXACTLY ONE auth.lockout row, and
// the retries that follow must add none.
func TestLoginLockoutRaisesExactlyOneAlert(t *testing.T) {
	s := newTestServer(t)
	const ip = "192.0.2.77"

	countKind := func(kind string) int {
		rows, err := s.store.ListAlerts(false, 500, 0)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, a := range rows {
			if a.Kind == kind {
				n++
			}
		}
		return n
	}

	// Guess until the address locks, then keep knocking.
	for i := 0; i < ipLockPolicy.max+6; i++ {
		postLogin(s, ip, "admin", "wrong-password", "")
	}

	if got := countKind(notify.KindAuthLockout); got != 1 {
		t.Errorf("auth.lockout rows = %d, want exactly 1 — retries during a lock must stay silent", got)
	}
	// The early warning fired on the way there, and only once per window.
	if got := countKind(notify.KindAuthFailedBurst); got > 1 {
		t.Errorf("auth.failed_burst rows = %d, want at most 1 per window", got)
	}
	// The alert must be actionable on sight: who, from where, and for how long.
	rows, _ := s.store.ListAlerts(false, 500, 0)
	var found *struct{ Title, Message string }
	for _, a := range rows {
		if a.Kind == notify.KindAuthLockout {
			found = &struct{ Title, Message string }{a.Title, a.Message}
		}
	}
	if found == nil {
		t.Fatal("no auth.lockout alert row was stored")
	}
	if !strings.Contains(found.Message, ip) {
		t.Errorf("the alert must name the source address: %q", found.Message)
	}
	if !strings.Contains(found.Message, "admin") {
		t.Errorf("the alert must name the account tried: %q", found.Message)
	}
	// And the audit trail is unchanged — the notification is additive, never a
	// replacement for the record.
	if !auditHas(t, s, "login.locked") {
		t.Error("the existing login.locked audit row must still be written")
	}
}

// AC3 — revealing the master key raises exactly one key.revealed alert and
// leaves the existing audit row intact.
//
// This is the most sensitive action in the app: the one secret that decrypts
// every backup ever written is put on a screen. It is a legitimate action (it is
// how the recovery sheet gets made) and it is also precisely what somebody who
// reached a session would do, so it is reported every time rather than
// throttled — "it happened twice today" is itself the thing worth knowing.
func TestKeyRevealRaisesAlertAndKeepsAudit(t *testing.T) {
	s := stepUpServer(t)

	rec, _ := stepUpPost(s, s.handleKeyReveal, `{"password":"`+stepUpTestPass+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reveal should succeed with the right password, got %d", rec.Code)
	}

	rows, err := s.store.ListAlerts(false, 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	var revealed []*store.Alert
	for _, a := range rows {
		if a.Kind == notify.KindKeyRevealed {
			revealed = append(revealed, a)
		}
	}
	if len(revealed) != 1 {
		t.Fatalf("key.revealed alert rows = %d, want exactly 1", len(revealed))
	}
	if revealed[0].Severity != notify.SevCritical.String() {
		t.Errorf("a key reveal must be stored as critical, got %q", revealed[0].Severity)
	}
	// The message has to be actionable without opening the audit table: who, and
	// which key.
	if !strings.Contains(revealed[0].Message, "test-fp") {
		t.Errorf("the alert should name the key fingerprint: %q", revealed[0].Message)
	}
	// The key itself must NEVER reach a notification channel.
	if strings.Contains(revealed[0].Message, "0000000000") {
		t.Error("key material must never appear in an alert body")
	}
	// The notification is additive — the audit row is still written.
	if !auditHas(t, s, "key.revealed") {
		t.Error("the existing key.revealed audit row must still be written")
	}
}
