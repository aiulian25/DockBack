package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dockback/internal/crypto"
	"dockback/internal/egress"
	"dockback/internal/notify"
	"dockback/internal/store"
)

const (
	sessionCookie = "dback_session"
	csrfCookie    = "dback_csrf"
	csrfHeader    = "X-CSRF-Token"
	sessionExtend = 30 * time.Minute // "extend session" bump near expiry

	// F203: these are now the DEFAULTS, not the policy. Both remain the values a
	// deployment that configures nothing gets, so an upgrade changes nothing.
	defaultSessionTTL = 12 * time.Hour // absolute session lifetime (auto-logout)
	minPasswordLen    = 12             // the FLOOR, never merely a default (PLAN §10.1 / Security.md §4)
)

// Configurable authentication policy (F203).
//
// The session lifetime and the minimum password length were compile-time
// constants, which made them the one part of the security posture an operator
// could not adjust without rebuilding the image. That is the wrong default for
// something whose right answer genuinely depends on the deployment: an instance
// reached only across a VPN can reasonably hold a session for a day, one on a
// shared workstation wants two hours, and a team with a password manager can
// require twenty characters where twelve was the compromise.
//
// Both resolve the same way — the env value is the deployment default, an
// in-app setting may override it, and the result is clamped. The clamps are the
// point: a settings row is written by whoever holds an admin session, so the
// bounds are what stops a weakened policy from being an unbounded one.
//
// ON WHY minPasswordLen IS A FLOOR AND sessionTTL IS NOT
//
// A short password is permanently weaker — it is guessable offline against a
// stolen hash, and nothing about the deployment changes that arithmetic. So 12
// is the shortest this app accepts, in every configuration; the setting can only
// raise it. A session lifetime is different: it is a window, not a strength, and
// how long a window is acceptable really does depend on where the door is. That
// one is bounded (1 hour to 30 days) rather than floored.

const (
	minSessionTTLHours    = 1
	maxSessionTTLHours    = 24 * 30
	minSessionIdleMinutes = 1
	maxSessionIdleMinutes = 24 * 60
	maxPasswordLen        = 128 // not a hashing limit — argon2 has none — but a policy nobody can satisfy is its own outage
)

// stepUpPasswordKey / stepUpCodeKey are the reserved body keys carrying step-up
// credentials into a settings save (F203). Named with a prefix no setting uses,
// and stripped from the request before the allow-list ever sees them, so they
// can never be confused for a setting or written to the database.
const (
	stepUpPasswordKey = "__step_up_password"
	stepUpCodeKey     = "__step_up_code"
)

// authPolicyKeys are the settings whose change alters who can authenticate and
// for how long (F203). Saving any of them requires fresh proof of the password.
var authPolicyKeys = map[string]bool{
	"security.session_ttl_hours":    true,
	"security.session_idle_minutes": true,
	"security.min_password_len":     true,
}

// authPolicyChange reports whether a settings save touches the authentication
// policy — the test for whether the step-up gate applies to this request.
func authPolicyChange(req map[string]string) bool {
	for k := range req {
		if authPolicyKeys[k] {
			return true
		}
	}
	return false
}

// sessionTTL is the absolute session lifetime this deployment enforces (F203).
func (s *Server) sessionTTL() time.Duration {
	def := defaultSessionTTL
	if s != nil && s.cfg != nil && s.cfg.SessionTTLHours > 0 {
		def = time.Duration(s.cfg.SessionTTLHours) * time.Hour
	}
	h := s.settingInt("security.session_ttl_hours", int(def.Hours()))
	if h < minSessionTTLHours {
		h = minSessionTTLHours
	}
	if h > maxSessionTTLHours {
		h = maxSessionTTLHours
	}
	return time.Duration(h) * time.Hour
}

// sessionIdleMinutes is the sliding inactivity window this deployment enforces
// (F203). The store holds the live value — it is what idleDeadline actually
// consults — so this reports from there rather than recomputing it, and the
// number the UI shows cannot drift from the number being enforced.
func (s *Server) sessionIdleMinutes() int {
	m := int(store.SessionIdleTTL.Minutes())
	if m < minSessionIdleMinutes {
		m = minSessionIdleMinutes
	}
	return m
}

// minPasswordLength is the shortest password this deployment accepts (F203).
// Never below minPasswordLen, whatever the config or the setting says.
func (s *Server) minPasswordLength() int {
	def := minPasswordLen
	if s != nil && s.cfg != nil && s.cfg.MinPasswordLen > def {
		def = s.cfg.MinPasswordLen
	}
	n := s.settingInt("security.min_password_len", def)
	if n < minPasswordLen {
		n = minPasswordLen
	}
	if n > maxPasswordLen {
		n = maxPasswordLen
	}
	return n
}

// secureCookies reports whether cookies should carry the Secure attribute:
// served over built-in TLS directly, or behind a trusted https proxy.
func (s *Server) secureCookies(r *http.Request) bool {
	if r.TLS != nil {
		return true // direct built-in TLS (PLAN §3.12)
	}
	return s.trustForwarded(r) && r.Header.Get("X-Forwarded-Proto") == "https"
}

// hostCookieName applies the __Host- prefix ONLY when the cookie is Secure. The
// prefix is rejected by browsers over plain HTTP, so using it on a non-TLS LAN
// deployment would break login — hence the gate (PLAN §10.1).
func hostCookieName(base string, secure bool) string {
	if secure {
		return "__Host-" + base
	}
	return base
}

// readCookie returns a cookie's value, accepting either the __Host- prefixed
// name (TLS) or the plain name (HTTP) — so an in-place HTTP→HTTPS upgrade never
// forces a logout.
func readCookie(r *http.Request, base string) string {
	if c, err := r.Cookie("__Host-" + base); err == nil {
		return c.Value
	}
	if c, err := r.Cookie(base); err == nil {
		return c.Value
	}
	return ""
}

// setSessionCookies issues the session + CSRF cookies (Secure + __Host- prefix
// behind a trusted https proxy). Used by login and password rotation so cookie
// flags stay consistent in one place.
func (s *Server) setSessionCookies(w http.ResponseWriter, r *http.Request, token, csrf string, maxAge int) {
	secure := s.secureCookies(r)
	http.SetCookie(w, &http.Cookie{
		Name: hostCookieName(sessionCookie, secure), Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: secure, MaxAge: maxAge,
	})
	http.SetCookie(w, &http.Cookie{
		Name: hostCookieName(csrfCookie, secure), Value: csrf, Path: "/", HttpOnly: false,
		SameSite: http.SameSiteStrictMode, Secure: secure, MaxAge: maxAge,
	})
}

// clearSessionCookies expires both the plain and __Host- variants of the auth
// cookies (the __Host- delete must carry Secure to be accepted).
func clearSessionCookies(w http.ResponseWriter) {
	for _, base := range []string{sessionCookie, csrfCookie} {
		http.SetCookie(w, &http.Cookie{Name: base, Value: "", Path: "/", MaxAge: -1})
		http.SetCookie(w, &http.Cookie{Name: "__Host-" + base, Value: "", Path: "/", MaxAge: -1, Secure: true, SameSite: http.SameSiteStrictMode})
	}
}

type ctxKey string

const userKey ctxKey = "user"

// tokenAuthKey marks a request as authenticated by an API token (F45) rather than
// a session cookie, so csrf() can skip the double-submit check (no cookies, no CSRF
// surface) while every other middleware/handler treats it identically.
const tokenAuthKey ctxKey = "tokenauth"

// tokenPrefix namespaces the token value so it's recognizable in logs/leaks and
// can't be confused with a session token.
const tokenPrefix = "dback_"

// bearerToken extracts the value from an "Authorization: Bearer <value>" header,
// or "" when absent/malformed.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

// sha256Hex is the at-rest representation of a token value (never the plaintext).
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// scopeSet parses a comma-separated scope string into a set.
func scopeSet(csv string) map[string]bool {
	out := map[string]bool{}
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out[s] = true
		}
	}
	return out
}

// validScopes are the only scopes a token may hold.
var validScopes = map[string]bool{"read": true, "backup": true, "metrics": true}

// tokenGETExfiltrates reports whether a GET path streams decrypted backup data or
// the whole (encrypted) control-plane. These are denied to EVERY token scope — a
// status/dashboard token must never be able to exfiltrate backup contents, even
// though the scope is nominally "read". (Hardening beyond the base read allow-list.)
func tokenGETExfiltrates(path string) bool {
	if path == "/api/app-backup/download" {
		return true // the entire encrypted control-plane (all secrets)
	}
	// A decrypted archive download or single-file extract of any backup.
	if strings.HasPrefix(path, "/api/backups/") &&
		(strings.HasSuffix(path, "/download") || strings.HasSuffix(path, "/extract")) {
		return true
	}
	return false
}

// backupPostAllowed is the explicit POST allow-list the "backup" scope grants —
// only backup-creation / verification / mirroring actions, never any configuration
// or secret mutation.
func backupPostAllowed(path string) bool {
	if path == "/api/backups" {
		return true
	}
	if strings.HasPrefix(path, "/api/backups/") &&
		(strings.HasSuffix(path, "/verify") || strings.HasSuffix(path, "/drill") || strings.HasSuffix(path, "/mirror")) {
		return true
	}
	// Node / stack / orphan-volume backup runs (all are backup-creation actions).
	if strings.HasPrefix(path, "/api/nodes/") && strings.HasSuffix(path, "/backup") {
		return true
	}
	return false
}

// tokenAllows is the pure scope→(method,path) authorization decision (F45):
//   - read (also implied by backup): any GET, except the decrypted/secret exports;
//   - backup: additionally the explicit backup POST allow-list;
//   - metrics: nothing under /api (it authorizes only the separate /metrics handler);
//   - everything else (settings/destinations/nodes/security mutations, DELETEs,
//     login/2FA/key-rotate): denied by construction.
func tokenAllows(scopesCSV, method, path string) bool {
	// Token management is session-only: a token can neither enumerate nor mint/revoke
	// tokens (no self-propagation, no privilege discovery), regardless of scope.
	if strings.HasPrefix(path, "/api/security/tokens") {
		return false
	}
	scopes := scopeSet(scopesCSV)
	if (scopes["read"] || scopes["backup"]) && method == http.MethodGet {
		return !tokenGETExfiltrates(path)
	}
	if scopes["backup"] && method == http.MethodPost {
		return backupPostAllowed(path)
	}
	return false
}

// parseTokenCIDRs turns an operator's source-pin list into networks (F201).
//
// A bare address is widened to a single-host network (/32 or /128) exactly as
// DOCKBACK_TRUSTED_PROXIES already does, so "10.0.0.5" means what an operator
// plainly intends by it rather than being rejected as malformed.
//
// Returns ok=false on the first entry it cannot parse. Silently dropping a bad
// entry would narrow or widen the pin in ways the operator never sees — and a
// pin that does not mean what it says is the failure this whole feature exists
// to avoid.
func parseTokenCIDRs(list []string) (out []*net.IPNet, normalized []string, ok bool) {
	for _, raw := range list {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		n, ok := egress.ParseCIDROrIP(raw)
		if !ok {
			return nil, nil, false // fail closed — a pin that cannot be read is not a pin
		}
		out = append(out, n)
		normalized = append(normalized, n.String())
	}
	return out, normalized, true
}

// tokenSourceAllowed reports whether a request from ip may present a token
// pinned to cidrCSV (F201).
//
// An EMPTY pin allows everything: that is what every token minted before this
// feature was promised, and silently narrowing them on upgrade would break
// working automation at the moment of a version bump.
//
// A non-empty pin that cannot be parsed denies everything. The alternative —
// treating an unreadable pin as "no pin" — turns a corrupted row into an open
// door, and this is a deny-list boundary where the safe direction is refusal.
func tokenSourceAllowed(cidrCSV, ip string) bool {
	cidrCSV = strings.TrimSpace(cidrCSV)
	if cidrCSV == "" {
		return true
	}
	nets, _, ok := parseTokenCIDRs(strings.Split(cidrCSV, ","))
	if !ok || len(nets) == 0 {
		return false
	}
	addr := net.ParseIP(strings.TrimSpace(ip))
	if addr == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(addr) {
			return true
		}
	}
	return false
}

// proxyTrustIsSpoofable reports whether X-Forwarded-For can be forged by any
// client that can reach this server (F201).
//
// clientIP believes X-Forwarded-For whenever TrustProxy is on. With no
// TrustedProxies allow-list that trust extends to EVERY caller, so anyone can
// claim any source address — and a token pinned to a CIDR would be trivially
// satisfiable by the attacker it was meant to exclude. The app already warns
// about this mode at startup; token creation refuses outright, because a pin
// that can be spoofed is worse than no pin at all: it stops the operator
// guarding the token by other means.
func (s *Server) proxyTrustIsSpoofable() bool {
	return s.cfg != nil && s.cfg.TrustProxy && len(s.cfg.TrustedProxies) == 0
}

// randToken returns a URL-safe random token. A crypto/rand failure (impossible on
// Linux, but never emit a predictable session/CSRF token in an exotic env) is
// fatal and unrecoverable, so we panic rather than return a weak token (SEC-9).
func randToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// handleLogin authenticates the single admin and issues session + CSRF cookies
// (PLAN §3.1). Protected by per-IP/per-account lockout + an argon2 concurrency
// cap (PLAN §10.1).
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Code     string `json:"code"` // TOTP or recovery code (when 2FA is on)
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	ip := s.clientIP(r)
	user := strings.TrimSpace(req.Username)

	// 1) Lockout check (persisted, survives restart).
	if d := s.guard.blocked(ip, user); d > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
		_ = s.store.Audit(user, "login.locked", "", "ip="+ip)
		errJSON(w, http.StatusTooManyRequests, "too many failed attempts — try again in "+humanShort(d))
		return
	}

	// 2) Verify, bounding concurrent argon2 (memory-DoS guard). Always run argon2
	//    — against the real hash, or a fixed decoy when the account doesn't exist
	//    — so response time never reveals whether a username is valid
	//    (constant-time login, no enumeration oracle; PLAN §10.1).
	u, uerr := s.store.GetUserByName(user)
	hash := s.decoyHash
	if uerr == nil {
		hash = u.PasswordHash
	}
	if !s.guard.acquireArgon(r.Context()) {
		w.Header().Set("Retry-After", "2")
		errJSON(w, http.StatusTooManyRequests, "server busy — retry shortly")
		return
	}
	match := crypto.VerifyPassword(req.Password, hash)
	s.guard.releaseArgon()
	authOK := uerr == nil && match
	if !authOK {
		out := s.guard.recordFail(ip, user)
		_ = s.store.Audit(user, "login.failed", "", "ip="+ip)
		s.noteAuthFailure(ip, user, "sign-in attempts", out) // F198
		errJSON(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	// 3) Second factor (TOTP), if the account has 2FA enabled (PLAN §10.2).
	if u.TOTPSecret != "" {
		if strings.TrimSpace(req.Code) == "" {
			// Correct password but a code is needed — signal the UI, don't count
			// it as a failed attempt.
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "two-factor code required", "totp_required": true})
			return
		}
		if !s.verifyUserTOTP(u, req.Code) {
			out := s.guard.recordFail(ip, user) // bound 6-digit brute force via lockout
			_ = s.store.Audit(user, "login.2fa_failed", "", "ip="+ip)
			// A correct password with a wrong CODE is a sharper signal than a wrong
			// password: whoever is trying already has the password (F198).
			s.noteAuthFailure(ip, user, "two-factor codes (the password was correct)", out)
			errJSON(w, http.StatusUnauthorized, "invalid two-factor code")
			return
		}
	}
	s.guard.reset(ip, user)

	token := randToken()
	// F202: record where this sign-in came from, so the owner can later tell
	// their own sessions apart from one they do not recognise.
	if err := s.store.CreateSessionFrom(token, u.ID, s.sessionTTL(), s.clientIP(r), safeLabel(r.UserAgent(), maxUserAgentLen)); err != nil {
		errJSON(w, http.StatusInternalServerError, "session error")
		return
	}
	csrf := randToken()
	// Stash csrf alongside the session so we can validate the double-submit.
	_ = s.store.SetSetting(csrfKey(store.SessionKey(token)), csrf)
	s.setSessionCookies(w, r, token, csrf, int(s.sessionTTL().Seconds()))

	_ = s.store.Audit(u.Username, "login.ok", "", "")
	writeJSON(w, http.StatusOK, map[string]string{"username": u.Username, "csrf": csrf})
}

// handleChangePassword re-authenticates with the current password, sets a new
// one, then revokes every session (incl. this one) + its CSRF entry and issues a
// fresh session for this browser — signing out all other devices and rotating
// the current token (PLAN §10.1 / Security.md §4).
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	curToken := readCookie(r, sessionCookie)
	if curToken == "" {
		errJSON(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	uid, username, err := s.store.SessionUser(curToken)
	if err != nil {
		errJSON(w, http.StatusUnauthorized, "session expired")
		return
	}
	u, err := s.store.GetUserByName(username)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "account error")
		return
	}

	// Verify the current password through the SHARED login guard, so a stolen
	// session cannot use this endpoint as an unmetered password oracle.
	if !s.verifyAccountPassword(w, r, username, u.PasswordHash, req.Current, "password-change attempts") {
		_ = s.store.Audit(username, "password.change.failed", "", "ip="+s.clientIP(r))
		return
	}

	// Policy.
	if minLen := s.minPasswordLength(); len(req.New) < minLen {
		errJSON(w, http.StatusBadRequest, "new password must be at least "+strconv.Itoa(minLen)+" characters")
		return
	}
	if req.New == req.Current {
		errJSON(w, http.StatusBadRequest, "new password must differ from the current one")
		return
	}

	hash, err := crypto.HashPassword(req.New)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "could not hash password")
		return
	}
	if err := s.store.UpdatePassword(uid, hash); err != nil {
		errJSON(w, http.StatusInternalServerError, "could not update password")
		return
	}

	// Revoke ALL sessions (incl. current) + their CSRF entries, then mint a fresh
	// session for this browser. Other devices are signed out; this token rotates.
	revoked := 0
	curKey := store.SessionKey(curToken)
	if keys, terr := s.store.ListUserSessionKeys(uid); terr == nil {
		for _, k := range keys {
			s.endSessionByKey(k)
			if k != curKey {
				revoked++
			}
		}
	}
	newToken := randToken()
	if err := s.store.CreateSession(newToken, uid, s.sessionTTL()); err != nil {
		errJSON(w, http.StatusInternalServerError, "session error")
		return
	}
	newCsrf := randToken()
	_ = s.store.SetSetting(csrfKey(store.SessionKey(newToken)), newCsrf)
	s.setSessionCookies(w, r, newToken, newCsrf, int(s.sessionTTL().Seconds()))

	_ = s.store.Audit(username, "password.changed", "", "ip="+s.clientIP(r)+" revoked_other_sessions="+strconv.Itoa(revoked))
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "csrf": newCsrf, "revoked_other_sessions": revoked})
}

// handleLogout clears the session (server-side + both cookie variants).
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token := readCookie(r, sessionCookie); token != "" {
		s.endSession(token) // F64: the csrf entry and any step-up grant die with it
	}
	clearSessionCookies(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---- Step-up re-authentication ("sudo mode", F64) ----
//
// Key-material operations (reveal, keyfile, rotate) and API-token minting are
// the highest-value targets a stolen session cookie could reach. They therefore
// demand the account password — and the TOTP code when 2FA is on — INSIDE the
// request, on top of session+CSRF. A successful step-up is cached per session
// for a short window so a multi-step flow (reveal → download sheet → keyfile)
// prompts once, not three times.

// stepUpBody carries the re-auth credentials. Handlers embed it in (or copy it
// from) their request structs and pass it to requireFreshAuth.
type stepUpBody struct {
	Password string `json:"password"`
	Code     string `json:"code"` // TOTP or recovery code (when 2FA is on)
}

// Authentication-surface alerting (F198).
//
// authAlertCooldown matches the lockout window: a determined attacker retrying
// for an hour produces one alert per window per address, not one per attempt.
const authAlertCooldown = 15 * time.Minute

// defaultAuthBurstThreshold is the failure count that raises the early warning.
// It must stay strictly BELOW ipLockPolicy.max or it would fire at the same
// instant as the lockout and say the same thing twice; authBurstThreshold
// enforces that regardless of what the setting holds.
const defaultAuthBurstThreshold = 3

// authBurstThreshold is the operator-tunable early-warning threshold, clamped to
// a range where it is still an EARLY warning: at least 2 (one typo is not a
// burst) and at most one below the lockout threshold.
func (s *Server) authBurstThreshold() int {
	n := s.settingInt("alert.auth_burst_threshold", defaultAuthBurstThreshold)
	if n < 2 {
		n = 2
	}
	if n > ipLockPolicy.max-1 {
		n = ipLockPolicy.max - 1
	}
	return n
}

// safeLabel bounds and de-fangs a value an ATTACKER chose before it is placed in
// a notification body (F198).
//
// The attempted username is attacker-controlled text on its way to email, a
// webhook and the alert inbox. Newlines are stripped because a body that reaches
// SMTP has no business carrying them, and the length is bounded so a megabyte of
// junk cannot be pushed through a notification channel. Empty renders as a
// stated absence rather than a blank the reader has to interpret.
func safeLabel(v string, max int) string {
	v = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 {
			return ' '
		}
		return r
	}, strings.TrimSpace(v))
	v = strings.Join(strings.Fields(v), " ")
	if v == "" {
		return "(none given)"
	}
	if len(v) > max {
		return v[:max] + "…"
	}
	return v
}

// noteAuthFailure raises the operator-facing signal for a failed authentication
// (F198): the lockout when this attempt caused one, otherwise the early-warning
// burst once the address has failed enough times to look deliberate.
//
// Deliberately driven by the lock TRANSITION rather than by "is this address
// locked": the blocked-attempt branch in handleLogin runs on every retry while a
// lock holds, so alerting from there would send one critical notification per
// guess — turning a working alarm into one the operator mutes.
//
// Throttled per address on top of that, so the two paths into here (sign-in and
// step-up) cannot compound, and so a burst followed by a lockout from the same
// address inside one window is one alert rather than two.
func (s *Server) noteAuthFailure(ip, user, surface string, out failOutcome) {
	who := safeLabel(user, 64)
	if out.IPLocked {
		s.notifyThrottled(notify.KindAuthLockout, ip,
			"Sign-in locked out",
			fmt.Sprintf("Repeated failed %s from %s locked further attempts for %s. Last account tried: %s. "+
				"If this was not you, treat it as an attack in progress: the address is blocked for now, but nothing stops it trying again after the lock expires.",
				surface, ip, humanShort(out.LockFor), who),
			authAlertCooldown)
		return
	}
	if out.IPFails >= s.authBurstThreshold() {
		s.notifyThrottled(notify.KindAuthFailedBurst, ip,
			"Repeated failed sign-ins",
			fmt.Sprintf("%d failed %s from %s within the last %s. Last account tried: %s. "+
				"The address is not locked yet — this is the warning before that happens.",
				out.IPFails, surface, ip, humanShort(ipLockPolicy.window), who),
			authAlertCooldown)
	}
}

// noteStepUpFailure alerts on a failed re-authentication for a destructive
// action (F198), and still runs the shared lockout/burst path — a step-up
// failure counts against the same guard a sign-in does.
//
// This one is worth separating from an ordinary bad password because of WHO is
// failing it: the request already carries a valid session. Either the operator
// mistyped in front of a confirmation dialog, or somebody holding a live session
// is trying to reach the master key, a key rotation or an overwrite restore —
// and the second reading is why the attempted path is named in the alert.
func (s *Server) noteStepUpFailure(r *http.Request, ip, user, factor string, out failOutcome) {
	s.notifyThrottled(notify.KindStepUpFailed, ip,
		"Re-authentication failed for a protected action",
		fmt.Sprintf("A signed-in session (%s) failed the %s check from %s while attempting %s. "+
			"If this was not you, that session is already authenticated — revoke it from Settings → Security and change the password.",
			safeLabel(user, 64), factor, ip, safeLabel(r.URL.Path, 120)),
		authAlertCooldown)
	s.noteAuthFailure(ip, user, "re-authentication attempts", out)
}

// stepUpWindow is how long a successful step-up stays fresh for its session.
const stepUpWindow = 5 * time.Minute

// csrfKey and stepUpKey name the settings rows paired with one session
// (deleted together on logout, password change and revocation).
//
// Both take the STORED session key, never the cookie value — the sessions table
// is keyed that way now, and a settings row keyed by the raw token would put the
// bearer credential back in the database the hashing removed it from.
// store.SessionKey turns a cookie value into that key; a value read back OUT of
// the store already is one.
func csrfKey(sessionKey string) string   { return "csrf:" + sessionKey }
func stepUpKey(sessionKey string) string { return "stepup:" + sessionKey }

// requireFreshAuth gates a security-critical handler behind fresh proof of the
// account password (+ second factor). It returns true when the caller holds a
// grant younger than stepUpWindow or the supplied credentials verify — in which
// case a new grant is written. On false it has already written the response:
// 401 with step_up_required:true (and totp_required when a code is needed) so
// the UI can prompt and retry, or 429 under the shared login lockout. Failed
// attempts count toward the SAME lockout as login, so the password cannot be
// brute-forced through this door either.
func (s *Server) requireFreshAuth(w http.ResponseWriter, r *http.Request, body stepUpBody) bool {
	token := readCookie(r, sessionCookie)
	if token == "" {
		errJSON(w, http.StatusUnauthorized, "not authenticated")
		return false
	}
	u, ok := s.currentUser(r)
	if !ok {
		errJSON(w, http.StatusUnauthorized, "session expired")
		return false
	}
	totpOn := u.TOTPSecret != ""

	// A recent grant covers the whole flow — no re-prompt.
	if v, _ := s.store.GetSetting(stepUpKey(store.SessionKey(token)), ""); v != "" {
		if at, err := strconv.ParseInt(v, 10, 64); err == nil && time.Now().Unix()-at < int64(stepUpWindow.Seconds()) {
			return true
		}
	}

	stepUpErr := func(status int, msg string) {
		writeJSON(w, status, map[string]any{"error": msg, "step_up_required": true, "totp_required": totpOn})
	}

	// Same persisted lockout as login (429 before any argon2 work).
	ip := s.clientIP(r)
	if d := s.guard.blocked(ip, u.Username); d > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
		_ = s.store.Audit(u.Username, "security.stepup.locked", "", "ip="+ip)
		errJSON(w, http.StatusTooManyRequests, "too many failed attempts — try again in "+humanShort(d))
		return false
	}

	if strings.TrimSpace(body.Password) == "" {
		stepUpErr(http.StatusUnauthorized, "re-authentication required")
		return false
	}

	// Verify the password (argon2, bounded by the shared concurrency cap).
	if !s.guard.acquireArgon(r.Context()) {
		w.Header().Set("Retry-After", "2")
		errJSON(w, http.StatusTooManyRequests, "server busy — retry shortly")
		return false
	}
	match := crypto.VerifyPassword(body.Password, u.PasswordHash)
	s.guard.releaseArgon()
	if !match {
		out := s.guard.recordFail(ip, u.Username)
		_ = s.store.Audit(u.Username, "security.stepup.failed", "", "bad password ip="+ip)
		s.noteStepUpFailure(r, ip, u.Username, "password", out) // F198
		stepUpErr(http.StatusUnauthorized, "password is incorrect")
		return false
	}

	// Second factor, when enrolled (correct password + missing code is signalled
	// but NOT counted as a failed attempt — mirrors login).
	if totpOn {
		if strings.TrimSpace(body.Code) == "" {
			stepUpErr(http.StatusUnauthorized, "two-factor code required")
			return false
		}
		if !s.verifyUserTOTP(u, body.Code) {
			out := s.guard.recordFail(ip, u.Username) // bound 6-digit brute force via lockout
			_ = s.store.Audit(u.Username, "security.stepup.failed", "", "bad 2fa code ip="+ip)
			s.noteStepUpFailure(r, ip, u.Username, "two-factor code", out) // F198
			stepUpErr(http.StatusUnauthorized, "invalid two-factor code")
			return false
		}
	}

	_ = s.store.SetSetting(stepUpKey(store.SessionKey(token)), strconv.FormatInt(time.Now().Unix(), 10))
	return true
}

// verifyAccountPassword checks the account password on a path that is NOT
// /api/login, while keeping that path inside the SAME persisted lockout.
//
// Every door that accepts the account password has to count against one
// counter, or the lockout is only a lockout on the door it was written for.
// Returns true when the password matched. On false it has ALREADY written the
// response: 429 while locked out or while argon2 is saturated, 401 otherwise.
func (s *Server) verifyAccountPassword(w http.ResponseWriter, r *http.Request, username, hash, password, surface string) bool {
	ip := s.clientIP(r)
	if d := s.guard.blocked(ip, username); d > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
		_ = s.store.Audit(username, "auth.locked", "", "surface="+surface+" ip="+ip)
		errJSON(w, http.StatusTooManyRequests, "too many failed attempts — try again in "+humanShort(d))
		return false
	}
	if !s.guard.acquireArgon(r.Context()) {
		w.Header().Set("Retry-After", "2")
		errJSON(w, http.StatusTooManyRequests, "server busy — retry shortly")
		return false
	}
	ok := crypto.VerifyPassword(password, hash)
	s.guard.releaseArgon()
	if !ok {
		out := s.guard.recordFail(ip, username)
		s.noteAuthFailure(ip, username, surface, out)
		errJSON(w, http.StatusUnauthorized, "password is incorrect")
		return false
	}
	s.guard.reset(ip, username)
	return true
}

// handleMe returns the current user (auth probe for the frontend), plus the
// absolute and sliding-idle deadlines so the UI can warn before either fires.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	var exp, idle int64
	var extended bool
	if token := readCookie(r, sessionCookie); token != "" {
		exp, idle, extended, _ = s.store.SessionInfo(token)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"username": userFrom(r), "expires_at": exp, "idle_expires_at": idle, "extended": extended,
	})
}

// handleActivity records genuine user interaction, sliding the idle window
// (PLAN §10.2). The frontend calls this — throttled — on real DOM activity, so
// background polling never keeps an abandoned session alive. Returns the
// refreshed deadlines. A dead session can't be revived (TouchSession → 401).
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	token := readCookie(r, sessionCookie)
	if token == "" {
		errJSON(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	// F202: also refresh the recorded address, so a laptop that moved between
	// networks shows where it is NOW — a stale address in a device list is worse
	// than none, because it is the thing the owner checks first.
	if err := s.store.TouchSessionFrom(token, s.clientIP(r)); err != nil {
		errJSON(w, http.StatusUnauthorized, "session expired")
		return
	}
	exp, idle, extended, err := s.store.SessionInfo(token)
	if err != nil {
		errJSON(w, http.StatusUnauthorized, "session expired")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"expires_at": exp, "idle_expires_at": idle, "extended": extended})
}

// handleRevokeOtherSessions signs the user out of every *other* session
// (devices/browsers), keeping the caller's current session — the "sign out
// everywhere else" control (PLAN §10.2). Mirrors the password-change revocation
// but without changing credentials. Running backups are server-side, unaffected.
func (s *Server) handleRevokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	curToken := readCookie(r, sessionCookie)
	if curToken == "" {
		errJSON(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	uid, username, err := s.store.SessionUser(curToken)
	if err != nil {
		errJSON(w, http.StatusUnauthorized, "session expired")
		return
	}
	revoked := 0
	curKey := store.SessionKey(curToken)
	if keys, terr := s.store.ListUserSessionKeys(uid); terr == nil {
		for _, k := range keys {
			if k == curKey {
				continue
			}
			// F202: one place clears everything keyed to a session, so a revoked
			// session cannot leave a live step-up grant behind it.
			s.endSessionByKey(k)
			revoked++
		}
	}
	_ = s.store.Audit(username, "session.revoke_others", "", "ip="+s.clientIP(r)+" revoked="+strconv.Itoa(revoked))
	writeJSON(w, http.StatusOK, map[string]any{"revoked": revoked})
}

// handleExtendSession pushes the session expiry to now+30m (never shortening)
// and refreshes the cookies' MaxAge to match, so the user can keep working past
// the 12h auto-logout window. Only ONE extension is allowed per session
// (PLAN §10.1). Running backups are server-side and unaffected either way.
func (s *Server) handleExtendSession(w http.ResponseWriter, r *http.Request) {
	token := readCookie(r, sessionCookie)
	if token == "" {
		errJSON(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	exp, _, extended, err := s.store.SessionInfo(token)
	if err != nil {
		errJSON(w, http.StatusUnauthorized, "session expired")
		return
	}
	if extended {
		// One extension per session — after that, the user must sign in again.
		errJSON(w, http.StatusConflict, "this session was already extended once")
		return
	}
	now := time.Now().Unix()
	newExp := now + int64(sessionExtend.Seconds())
	if newExp < exp { // never shorten an already-longer session
		newExp = exp
	}
	if err := s.store.ExtendSession(token, newExp); err != nil {
		errJSON(w, http.StatusInternalServerError, "could not extend session")
		return
	}
	// Clicking "extend" is user activity — slide the idle window too so the idle
	// timeout doesn't immediately fire right after extending the absolute one.
	_ = s.store.TouchSession(token)
	// Refresh the cookie lifetime so the browser doesn't drop it before the new
	// expiry (keep the existing CSRF value).
	if csrf := readCookie(r, csrfCookie); csrf != "" {
		s.setSessionCookies(w, r, token, csrf, int(newExp-now))
	}
	_, idle, _, _ := s.store.SessionInfo(token)
	_ = s.store.Audit(userFrom(r), "session.extended", "", "")
	writeJSON(w, http.StatusOK, map[string]any{"expires_at": newExp, "idle_expires_at": idle, "extended": true})
}

// auth is middleware requiring a valid session cookie.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// F45: an API token (Authorization: Bearer dback_…) authenticates
		// non-interactive automation. It is checked BEFORE the cookie path and, on a
		// bad token, fails closed (never falls back to a cookie). A valid token is
		// still restricted to its scope's methods+paths, and attributes actions to
		// "token:<name>" in the audit trail.
		if bt := bearerToken(r); bt != "" {
			t, err := s.store.GetAPITokenByHash(sha256Hex(bt))
			if err != nil {
				errJSON(w, http.StatusUnauthorized, "invalid API token")
				return
			}
			// F65: an expired token fails closed exactly like an unknown one —
			// it never falls back to the cookie path. The row stays listed (and
			// revocable) in Settings; only authentication is refused.
			if t.ExpiresAt > 0 && time.Now().Unix() > t.ExpiresAt {
				errJSON(w, http.StatusUnauthorized, "API token expired")
				return
			}
			// F201: the source pin, checked BEFORE the scope. A token presented
			// from an address it was never meant to be used from is not a
			// permissions question — it is a leaked credential being exercised, and
			// the audit row is the point.
			if t.AllowedCIDRs != "" {
				if ip := s.clientIP(r); !tokenSourceAllowed(t.AllowedCIDRs, ip) {
					_ = s.store.Audit("token:"+t.Name, "token.denied_ip", t.Name,
						"presented from "+ip+", which is outside this token's allowed source addresses")
					errJSON(w, http.StatusForbidden, "this token may not be used from this address")
					return
				}
			}
			if !tokenAllows(t.Scopes, r.Method, r.URL.Path) {
				errJSON(w, http.StatusForbidden, "this token's scope does not permit this request")
				return
			}
			_ = s.store.TouchAPIToken(t.ID)
			ctx := context.WithValue(r.Context(), userKey, "token:"+t.Name)
			ctx = context.WithValue(ctx, tokenAuthKey, true)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		token := readCookie(r, sessionCookie)
		if token == "" {
			errJSON(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		_, username, err := s.store.SessionUser(token)
		if err != nil {
			errJSON(w, http.StatusUnauthorized, "session expired")
			return
		}
		ctx := context.WithValue(r.Context(), userKey, username)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// csrf is middleware enforcing the double-submit CSRF token on mutations
// (PLAN §3.2). The header must match the session-bound token.
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// F45: a token-authenticated request carries no cookies, so it has no CSRF
		// surface — skip the double-submit check. It was already scope-checked in
		// auth(); CSRF protects cookie-bearing browser requests only.
		if v, ok := r.Context().Value(tokenAuthKey).(bool); ok && v {
			next.ServeHTTP(w, r)
			return
		}
		token := readCookie(r, sessionCookie)
		if token == "" {
			errJSON(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		want, _ := s.store.GetSetting(csrfKey(store.SessionKey(token)), "")
		got := r.Header.Get(csrfHeader)
		if want == "" || got == "" || subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
			errJSON(w, http.StatusForbidden, "csrf check failed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func userFrom(r *http.Request) string {
	if v, ok := r.Context().Value(userKey).(string); ok {
		return v
	}
	return ""
}

// EnsureAdmin bootstraps the single admin account on first run, returning the
// generated password if one was created (PLAN §8.2).
//
// F203: the bootstrap password is now held to the same minimum as every later
// password change. It previously was not, so the one credential created without
// a human at the keyboard was the one credential with no length rule — an
// operator who put "admin" in DOCKBACK_ADMIN_PASSWORD got exactly that.
//
// A password below the minimum is REFUSED AND REPLACED, not rejected with an
// error: refusing would stop the app from starting on first run, turning a weak
// setting into an outage, and there is a strictly better answer available. The
// generated password takes its place and is printed exactly like the no-password
// case, so the deployment comes up with a strong credential and the operator is
// told why theirs was not used. rejected reports that, for the caller's message.
func EnsureAdmin(st *store.Store, username, password string, minLen int) (generatedPW string, rejected bool, err error) {
	n, err := st.UserCount()
	if err != nil {
		return "", false, err
	}
	if n > 0 {
		return "", false, nil
	}
	if minLen < minPasswordLen {
		minLen = minPasswordLen
	}
	if password != "" && len(password) < minLen {
		password, rejected = "", true
	}
	generated := ""
	if password == "" {
		password = randToken()[:16]
		generated = password
	}
	hash, err := crypto.HashPassword(password)
	if err != nil {
		return "", rejected, err
	}
	if _, err := st.CreateUser(username, hash); err != nil {
		return "", rejected, err
	}
	if err := st.Audit("system", "admin.created", username, ""); err != nil {
		return "", rejected, err
	}
	return generated, rejected, nil
}

// SyncAdminUsername renames the single admin account to match an explicitly-set
// DOCKBACK_ADMIN_USER so a changed env value is reflected in login and the UI —
// WITHOUT losing data. The rename is keyed by the stable user id, so the argon2id
// password hash, 2FA secret/recovery, active sessions (by user_id), and all app
// data (backups/nodes/destinations/settings — none of which are user-scoped) are
// preserved untouched. It is a strict no-op unless the env var is explicitly set,
// exactly one account exists, and the name actually differs — so removing the var
// never reverts the name, and any non-single-admin setup is left alone. Returns
// the previous name when a rename happened (for logging).
func SyncAdminUsername(st *store.Store, username string) (string, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return "", nil
	}
	u, err := st.GetSoleUser()
	if err != nil || u == nil { // ErrNotFound (0 or >1 users) => nothing to sync
		return "", nil
	}
	if u.Username == username {
		return "", nil
	}
	old := u.Username
	if err := st.RenameUser(u.ID, username); err != nil {
		return "", err
	}
	_ = st.Audit("system", "admin.renamed", username, "from="+old)
	return old, nil
}
