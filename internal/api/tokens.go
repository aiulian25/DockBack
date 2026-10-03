package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"dockback/internal/store"
)

// handleListTokens returns the API tokens' metadata (F45) — name, scopes, created,
// last-used — NEVER the token value or its hash.
func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	toks, err := s.store.ListAPITokens()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if toks == nil {
		toks = []*store.APIToken{}
	}
	writeJSON(w, http.StatusOK, toks)
}

// handleCreateToken mints a new scoped token. The plaintext value is returned
// EXACTLY ONCE (stored only as a SHA-256 hash), so the UI must show-and-forget it.
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string   `json:"name"`
		Scopes       []string `json:"scopes"`
		TTLDays      int      `json:"ttl_days"`      // F65: 0 = never expires
		AllowedCIDRs []string `json:"allowed_cidrs"` // F201: empty = any source address
		stepUpBody            // F64: password (+ TOTP code when 2FA is on)
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	// F64: minting a bearer credential mutates the security surface — require a
	// fresh step-up (password, + second factor when enrolled).
	if !s.requireFreshAuth(w, r, req.stepUpBody) {
		return
	}
	// F65: optional expiry. 0 keeps the pre-F65 behavior (never expires);
	// anything else is clamped to 1..3650 days (10 years).
	if req.TTLDays < 0 || req.TTLDays > 3650 {
		errJSON(w, http.StatusBadRequest, "ttl_days must be between 0 (never) and 3650")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 64 {
		errJSON(w, http.StatusBadRequest, "a token name (1-64 chars) is required")
		return
	}
	// Validate + dedupe the scopes against the fixed allow-list.
	seen := map[string]bool{}
	var scopes []string
	for _, sc := range req.Scopes {
		sc = strings.TrimSpace(strings.ToLower(sc))
		if sc == "" {
			continue
		}
		if !validScopes[sc] {
			errJSON(w, http.StatusBadRequest, "unknown scope: "+sc)
			return
		}
		if !seen[sc] {
			seen[sc] = true
			scopes = append(scopes, sc)
		}
	}
	if len(scopes) == 0 {
		errJSON(w, http.StatusBadRequest, "select at least one scope (read, backup, metrics)")
		return
	}
	sort.Strings(scopes)
	scopeCSV := strings.Join(scopes, ",")

	// F201: optional source pin. Normalized to canonical CIDR form so what is
	// stored is what is enforced, and what the UI later shows back is what the
	// operator can compare against their firewall.
	_, cidrs, cidrOK := parseTokenCIDRs(req.AllowedCIDRs)
	if !cidrOK {
		errJSON(w, http.StatusBadRequest, "allowed source addresses must be IPs or CIDR ranges, e.g. 10.0.0.5 or 10.0.0.0/24")
		return
	}
	if len(cidrs) > 0 && s.proxyTrustIsSpoofable() {
		// Refused rather than issued-and-useless. In trust-all-proxies mode the
		// forwarded address is attacker-controlled, so this pin would keep out
		// nobody while looking like it does — and an operator who believes a token
		// is pinned stops protecting it another way.
		errJSON(w, http.StatusBadRequest,
			"a source-address pin cannot be enforced while DOCKBACK_TRUST_PROXY is on with no DOCKBACK_TRUSTED_PROXIES set: "+
				"the forwarded client address can be set by anyone. Set DOCKBACK_TRUSTED_PROXIES to your proxy's address, or create the token without a pin.")
		return
	}
	cidrCSV := strings.Join(cidrs, ",")

	var expiresAt int64
	if req.TTLDays > 0 {
		expiresAt = time.Now().Add(time.Duration(req.TTLDays) * 24 * time.Hour).Unix()
	}

	value := tokenPrefix + randToken() // dback_<64 hex> — 256 bits of entropy
	id := randToken()[:16]
	if err := s.store.CreateAPIToken(id, name, sha256Hex(value), scopeCSV, expiresAt, cidrCSV); err != nil {
		errJSON(w, http.StatusInternalServerError, "could not create token")
		return
	}
	_ = s.store.Audit(userFrom(r), "token.create", name,
		fmt.Sprintf("scopes=%s ttl_days=%d sources=%s; step-up ok", scopeCSV, req.TTLDays, firstNonEmptyStr(cidrCSV, "any")))
	// The ONLY time the plaintext leaves the server.
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            id,
		"name":          name,
		"scopes":        scopeCSV,
		"token":         value,
		"expires_at":    expiresAt,
		"allowed_cidrs": cidrCSV,
	})
}

// handleDeleteToken revokes a token by id (F45).
func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteAPIToken(id); err != nil {
		errJSON(w, http.StatusInternalServerError, "could not delete token")
		return
	}
	_ = s.store.Audit(userFrom(r), "token.delete", id, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
