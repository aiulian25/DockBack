package crypto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP/2FA primitives (RFC 6238, HMAC-SHA1, 30s, 6 digits) implemented in-house
// to avoid a new dependency; covered by RFC test vectors in totp_test.go.
// Authenticator apps (Google/Microsoft Authenticator, Aegis, 1Password, …) all
// default to these parameters.
const (
	totpDigits = 6
	totpPeriod = 30 // seconds
	totpSkew   = 1  // accept ±1 step for clock drift
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a random 160-bit base32 secret.
func NewTOTPSecret() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return b32.EncodeToString(b)
}

// TOTPURI builds the otpauth:// provisioning URI scanned during enrolment.
func TOTPURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(totpDigits))
	q.Set("period", fmt.Sprint(totpPeriod))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// hotp computes the RFC 4226 HOTP value for a counter (the TOTP building block).
func hotp(key []byte, counter uint64) string {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, counter)
	h := hmac.New(sha1.New, key)
	h.Write(buf)
	sum := h.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := (uint32(sum[off]&0x7f) << 24) |
		(uint32(sum[off+1]) << 16) |
		(uint32(sum[off+2]) << 8) |
		uint32(sum[off+3])
	return fmt.Sprintf("%0*d", totpDigits, code%pow10(totpDigits))
}

func pow10(n int) uint32 {
	p := uint32(1)
	for i := 0; i < n; i++ {
		p *= 10
	}
	return p
}

// VerifyTOTPStep reports whether code matches secret within the allowed skew and,
// on success, returns the matched 30-second step. It enforces single-use: a code
// whose step is <= lastStep (already consumed) is rejected, closing the ~90s
// replay window (SEC-4). Callers persist the returned step as the new lastStep.
// Pass lastStep = -1 to disable the replay guard (steps are always positive).
func VerifyTOTPStep(secret, code string, lastStep int64) (step int64, ok bool) {
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return 0, false
	}
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil || len(key) == 0 {
		return 0, false
	}
	now := time.Now().Unix() / totpPeriod
	matched := int64(-1)
	for d := -totpSkew; d <= totpSkew; d++ {
		// Constant-time compare every step (don't short-circuit) to avoid leaking
		// which step matched via timing.
		st := now + int64(d)
		if subtle.ConstantTimeCompare([]byte(hotp(key, uint64(st))), []byte(code)) == 1 {
			matched = st
		}
	}
	// Reject a replay: the code's step must be newer than the last one consumed.
	if matched < 0 || matched <= lastStep {
		return 0, false
	}
	return matched, true
}

// VerifyTOTP reports whether code matches secret within the allowed skew, WITHOUT
// the single-use guard (used by enrolment and RFC-vector tests). Prefer
// VerifyTOTPStep on the login path so a consumed code can't be replayed.
func VerifyTOTP(secret, code string) bool {
	_, ok := VerifyTOTPStep(secret, code, -1)
	return ok
}

// --- Recovery codes (one-time, high-entropy → fast SHA-256 hash is sufficient) ---

// NewRecoveryCodes returns n display codes ("abcd-efgh") and their stored hashes.
func NewRecoveryCodes(n int) (display, hashed []string) {
	for i := 0; i < n; i++ {
		b := make([]byte, 5) // 8 base32 chars
		_, _ = rand.Read(b)
		c := strings.ToLower(b32.EncodeToString(b))
		code := c[:4] + "-" + c[4:]
		display = append(display, code)
		hashed = append(hashed, HashRecoveryCode(code))
	}
	return
}

// HashRecoveryCode normalizes (lowercase, strip spaces/dashes) then SHA-256s.
func HashRecoveryCode(code string) string {
	norm := strings.ToLower(code)
	norm = strings.ReplaceAll(norm, " ", "")
	norm = strings.ReplaceAll(norm, "-", "")
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])
}

// MatchRecoveryCode returns the remaining hashes after consuming a matching code
// (one-time use), or (nil,false) if none match.
func MatchRecoveryCode(hashes []string, code string) (remaining []string, ok bool) {
	want := HashRecoveryCode(code)
	for i, h := range hashes {
		if subtle.ConstantTimeCompare([]byte(h), []byte(want)) == 1 {
			out := append([]string{}, hashes[:i]...)
			return append(out, hashes[i+1:]...), true
		}
	}
	return nil, false
}
