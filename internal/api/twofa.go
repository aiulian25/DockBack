package api

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"dockback/internal/crypto"
	"dockback/internal/store"
)

// Two-factor auth (TOTP) — PLAN §10.2 / Security.md §4. The secret is sealed at
// rest with the master key; recovery codes are stored as one-time SHA-256 hashes.

const totpIssuer = "DockBack"

// sealSecret encrypts a TOTP secret for storage (hex-encoded sealed blob).
func (s *Server) sealSecret(secret string) (string, error) {
	blob, err := crypto.SealString(secret, s.cfg.EncryptionKey)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(blob), nil
}

// openSecret reverses sealSecret ("" stays "").
func (s *Server) openSecret(stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	blob, err := hex.DecodeString(stored)
	if err != nil {
		return "", err
	}
	return crypto.OpenString(blob, s.cfg.EncryptionKey)
}

// currentUser resolves the authenticated user from the session cookie.
func (s *Server) currentUser(r *http.Request) (*store.User, bool) {
	token := readCookie(r, sessionCookie)
	if token == "" {
		return nil, false
	}
	_, username, err := s.store.SessionUser(token)
	if err != nil {
		return nil, false
	}
	u, err := s.store.GetUserByName(username)
	if err != nil {
		return nil, false
	}
	return u, true
}

// verifyUserTOTP checks a login's second factor: a current TOTP code, or a
// one-time recovery code (which is then consumed). Returns true on success.
func (s *Server) verifyUserTOTP(u *store.User, code string) bool {
	secret, err := s.openSecret(u.TOTPSecret)
	if err != nil || secret == "" {
		return false
	}
	// TOTP with a single-use guard (SEC-4): reject a code whose 30s step was
	// already consumed, then record the step so it can't be replayed.
	if step, ok := crypto.VerifyTOTPStep(secret, code, u.TOTPLastStep); ok {
		_ = s.store.SetTOTPLastStep(u.ID, step)
		return true
	}
	var hashes []string
	if u.TOTPRecovery != "" {
		_ = json.Unmarshal([]byte(u.TOTPRecovery), &hashes)
	}
	if remaining, ok := crypto.MatchRecoveryCode(hashes, code); ok {
		b, _ := json.Marshal(remaining)
		_ = s.store.UpdateRecovery(u.ID, string(b))
		_ = s.store.Audit(u.Username, "login.2fa_recovery", "", "remaining="+itoa(len(remaining)))
		return true
	}
	return false
}

// handleTOTPStatus reports whether 2FA is on and how many recovery codes remain.
func (s *Server) handleTOTPStatus(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		errJSON(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	remaining := 0
	if u.TOTPRecovery != "" {
		var h []string
		_ = json.Unmarshal([]byte(u.TOTPRecovery), &h)
		remaining = len(h)
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": u.TOTPSecret != "", "recovery_remaining": remaining})
}

// handleTOTPBegin starts enrolment: generate + stash a pending secret, return the
// otpauth URI + secret for the authenticator app (QR rendered client-side).
func (s *Server) handleTOTPBegin(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		errJSON(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	if u.TOTPSecret != "" {
		errJSON(w, http.StatusBadRequest, "two-factor is already enabled — disable it first to re-enrol")
		return
	}
	secret := crypto.NewTOTPSecret()
	sealed, err := s.sealSecret(secret)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "could not prepare secret")
		return
	}
	if err := s.store.SetTOTPPending(u.ID, sealed); err != nil {
		errJSON(w, http.StatusInternalServerError, "could not save secret")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"secret":      secret,
		"otpauth_uri": crypto.TOTPURI(totpIssuer, u.Username, secret),
	})
}

// handleTOTPEnable confirms enrolment with a live code, then activates 2FA and
// returns the one-time recovery codes (shown once).
func (s *Server) handleTOTPEnable(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	u, ok := s.currentUser(r)
	if !ok {
		errJSON(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	pending, err := s.openSecret(u.TOTPPending)
	if err != nil || pending == "" {
		errJSON(w, http.StatusBadRequest, "start enrolment first")
		return
	}
	// Verify the enrolment code and capture its step, so activating 2FA seeds the
	// replay guard (SEC-4) and that same code can't be replayed at login.
	step, ok2 := crypto.VerifyTOTPStep(pending, req.Code, -1)
	if !ok2 {
		errJSON(w, http.StatusBadRequest, "that code didn't match — check your authenticator and try again")
		return
	}
	display, hashed := crypto.NewRecoveryCodes(10)
	recoveryJSON, _ := json.Marshal(hashed)
	if err := s.store.EnableTOTP(u.ID, u.TOTPPending, string(recoveryJSON), step); err != nil {
		errJSON(w, http.StatusInternalServerError, "could not enable two-factor")
		return
	}
	_ = s.store.Audit(u.Username, "2fa.enabled", "", "ip="+s.clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": display})
}

// handleTOTPDisable turns 2FA off after re-authenticating with BOTH factors.
//
// The second factor has to confirm its own removal. With the password alone, a
// stolen session plus a phished password stripped 2FA from the only admin
// account — the one action where the factor being removed is exactly the factor
// that would have stopped it. A lost authenticator still has a way through:
// verifyUserTOTP accepts a one-time recovery code.
func (s *Server) handleTOTPDisable(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	u, ok := s.currentUser(r)
	if !ok {
		errJSON(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	// Verify the password through the SHARED login guard, so a stolen session
	// cannot use 2FA-disable as an unmetered password oracle — the sharpest
	// escalation available, since success strips the second factor entirely.
	if !s.verifyAccountPassword(w, r, u.Username, u.PasswordHash, req.Password, "two-factor-disable attempts") {
		_ = s.store.Audit(u.Username, "2fa.disable_failed", "", "ip="+s.clientIP(r))
		return
	}
	// Second factor, checked exactly as requireFreshAuth does: a missing code is
	// signalled but not counted as a failed attempt, while a WRONG one is — so a
	// six-digit space cannot be walked without hitting the shared lockout.
	ip := s.clientIP(r)
	if strings.TrimSpace(req.Code) == "" {
		errJSON(w, http.StatusBadRequest, "two-factor code required")
		return
	}
	if !s.verifyUserTOTP(u, req.Code) {
		out := s.guard.recordFail(ip, u.Username)
		_ = s.store.Audit(u.Username, "2fa.disable_failed", "", "bad 2fa code ip="+ip)
		s.noteStepUpFailure(r, ip, u.Username, "two-factor code", out) // F198
		errJSON(w, http.StatusUnauthorized, "invalid two-factor code")
		return
	}
	if err := s.store.DisableTOTP(u.ID); err != nil {
		errJSON(w, http.StatusInternalServerError, "could not disable two-factor")
		return
	}
	_ = s.store.Audit(u.Username, "2fa.disabled", "", "ip="+s.clientIP(r))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
