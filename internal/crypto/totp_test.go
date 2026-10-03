package crypto

import (
	"testing"
	"time"
)

// RFC 6238 test vectors (SHA-1, secret = ASCII "12345678901234567890"), reduced
// to the 6 least-significant digits of the published 8-digit values.
func TestHOTP_RFC6238Vectors(t *testing.T) {
	key := []byte("12345678901234567890")
	cases := []struct {
		counter uint64
		want    string // last 6 digits of the RFC 8-digit value
	}{
		{1, "287082"},        // T=59          -> 94287082
		{37037036, "081804"}, // T=1111111109  -> 07081804
		{41152263, "005924"}, // T=1234567890  -> 89005924
	}
	for _, c := range cases {
		if got := hotp(key, c.counter); got != c.want {
			t.Errorf("hotp(%d) = %s, want %s", c.counter, got, c.want)
		}
	}
}

func TestVerifyTOTP_RoundTrip(t *testing.T) {
	secret := NewTOTPSecret()
	// A freshly generated code for "now" must verify.
	key, _ := b32.DecodeString(secret)
	now := uint64(time.Now().Unix() / 30)
	code := hotp(key, now)
	if !VerifyTOTP(secret, code) {
		t.Fatal("current code should verify")
	}
	if VerifyTOTP(secret, "000000") && code != "000000" {
		t.Fatal("a wrong code should not verify")
	}
	if VerifyTOTP(secret, "12345") { // wrong length
		t.Fatal("malformed code must be rejected")
	}
}

// SEC-4: a code is single-use — once its step is consumed it must not verify
// again, but a later step still does.
func TestVerifyTOTPStep_ReplayGuard(t *testing.T) {
	secret := NewTOTPSecret()
	key, _ := b32.DecodeString(secret)
	now := time.Now().Unix() / totpPeriod
	code := hotp(key, uint64(now))

	// First use with no prior step consumed: accepted, returns the current step.
	step, ok := VerifyTOTPStep(secret, code, 0)
	if !ok || step != now {
		t.Fatalf("first use should verify at step %d, got step=%d ok=%v", now, step, ok)
	}
	// Replaying the same code once its step is consumed must be rejected.
	if _, ok := VerifyTOTPStep(secret, code, step); ok {
		t.Fatal("consumed TOTP code must not verify again (replay)")
	}
	// A code from a newer step (within skew) is still accepted after that.
	next := hotp(key, uint64(now+1))
	if s, ok := VerifyTOTPStep(secret, next, step); !ok || s != now+1 {
		t.Fatalf("next step's code should verify, got step=%d ok=%v", s, ok)
	}
}

func TestRecoveryCodes(t *testing.T) {
	display, hashed := NewRecoveryCodes(10)
	if len(display) != 10 || len(hashed) != 10 {
		t.Fatalf("want 10 codes, got %d/%d", len(display), len(hashed))
	}
	// A valid code matches and is consumed; dashes/spaces/case are ignored.
	noisy := " " + display[3] + " "
	remaining, ok := MatchRecoveryCode(hashed, noisy)
	if !ok {
		t.Fatal("recovery code should match")
	}
	if len(remaining) != 9 {
		t.Fatalf("code should be consumed, remaining=%d", len(remaining))
	}
	// The consumed code no longer matches.
	if _, ok := MatchRecoveryCode(remaining, display[3]); ok {
		t.Fatal("consumed code must not match again")
	}
	if _, ok := MatchRecoveryCode(hashed, "nope-nope"); ok {
		t.Fatal("unknown code must not match")
	}
}
