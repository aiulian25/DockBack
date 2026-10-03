package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"dockback/internal/store"
)

func TestTokenAllows(t *testing.T) {
	cases := []struct {
		name   string
		scopes string
		method string
		path   string
		want   bool
	}{
		// read: any GET except decrypted/secret exports and token management.
		{"read GET backups", "read", "GET", "/api/backups", true},
		{"read GET one backup", "read", "GET", "/api/backups/b1", true},
		{"read cannot POST backup", "read", "POST", "/api/backups", false},
		{"read cannot download decrypted", "read", "GET", "/api/backups/b1/download", false},
		{"read cannot extract file", "read", "GET", "/api/backups/b1/extract", false},
		{"read cannot pull app-backup", "read", "GET", "/api/app-backup/download", false},
		{"read cannot list tokens", "read", "GET", "/api/security/tokens", false},
		{"read cannot DELETE destination", "read", "DELETE", "/api/destinations/d1", false},

		// backup: read's GETs PLUS the backup POST allow-list.
		{"backup GET still works", "backup", "GET", "/api/backups", true},
		{"backup POST backups", "backup", "POST", "/api/backups", true},
		{"backup POST verify", "backup", "POST", "/api/backups/b1/verify", true},
		{"backup POST drill", "backup", "POST", "/api/backups/b1/drill", true},
		{"backup POST mirror", "backup", "POST", "/api/backups/b1/mirror", true},
		{"backup POST stack backup", "backup", "POST", "/api/nodes/n1/stacks/blog/backup", true},
		{"backup cannot DELETE destination", "backup", "DELETE", "/api/destinations/d1", false},
		{"backup cannot restore", "backup", "POST", "/api/backups/b1/restore", false},
		{"backup cannot mutate settings", "backup", "POST", "/api/settings", false},
		{"backup cannot rotate key", "backup", "POST", "/api/security/key-rotate", false},
		{"backup cannot download decrypted", "backup", "GET", "/api/backups/b1/download", false},

		// metrics: nothing under /api (it only authorizes the /metrics handler).
		{"metrics cannot GET api", "metrics", "GET", "/api/backups", false},
		{"metrics cannot POST", "metrics", "POST", "/api/backups", false},

		// combined scopes union their grants.
		{"read+backup GET", "read,backup", "GET", "/api/nodes/n1/stacks", true},
		{"read+backup POST backup", "read,backup", "POST", "/api/backups", true},
	}
	for _, c := range cases {
		if got := tokenAllows(c.scopes, c.method, c.path); got != c.want {
			t.Errorf("%s: tokenAllows(%q,%s,%s)=%v want %v", c.name, c.scopes, c.method, c.path, got, c.want)
		}
	}
}

func TestBearerToken(t *testing.T) {
	mk := func(h string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		if h != "" {
			r.Header.Set("Authorization", h)
		}
		return r
	}
	if got := bearerToken(mk("Bearer dback_abc")); got != "dback_abc" {
		t.Errorf("bearer parse = %q", got)
	}
	if got := bearerToken(mk("bearer dback_abc")); got != "dback_abc" {
		t.Errorf("case-insensitive scheme failed: %q", got)
	}
	if got := bearerToken(mk("Basic xyz")); got != "" {
		t.Errorf("non-bearer must be empty, got %q", got)
	}
	if got := bearerToken(mk("")); got != "" {
		t.Errorf("missing header must be empty, got %q", got)
	}
}

// TestCSRFSkipsTokenAuth asserts the csrf() middleware passes a token-authenticated
// request through (no double-submit) but still enforces CSRF for a cookie request.
func TestCSRFSkipsTokenAuth(t *testing.T) {
	s := &Server{}
	reached := false
	h := s.csrf(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true; w.WriteHeader(200) }))

	// Token-authenticated request (context flag set as auth() would): passes through.
	reached = false
	r := httptest.NewRequest("POST", "/api/backups", nil)
	r = r.WithContext(context.WithValue(r.Context(), tokenAuthKey, true))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if !reached || rec.Code != 200 {
		t.Fatalf("token-auth request must skip CSRF: reached=%v code=%d", reached, rec.Code)
	}

	// Cookie request with no CSRF header: rejected (no session token → 401 before
	// the header check, but crucially NOT passed through as a token would be).
	reached = false
	r2 := httptest.NewRequest("POST", "/api/backups", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, r2)
	if reached || rec2.Code == 200 {
		t.Fatalf("cookie request without CSRF must be rejected: reached=%v code=%d", reached, rec2.Code)
	}
}

// F65: token expiry. An expired token fails closed everywhere (API auth and the
// /metrics gate) exactly like an unknown token, while expires_at=0 preserves the
// pre-F65 "valid until revoked" behavior. Expired rows stay listed and revocable.
func TestTokenExpiry(t *testing.T) {
	st := testStore(t)
	s := &Server{store: st}
	now := time.Now().Unix()

	mk := func(id, scopes string, expiresAt int64) string {
		value := tokenPrefix + id + "secretsecretsecret"
		if err := st.CreateAPIToken(id, id, sha256Hex(value), scopes, expiresAt, ""); err != nil {
			t.Fatal(err)
		}
		return value
	}
	fresh := mk("fresh", "read", now+3600)
	stale := mk("stale", "read", now-10)
	forever := mk("forever", "read", 0)

	h := s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	get := func(token string) int {
		r := httptest.NewRequest("GET", "/api/nodes", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	if got := get(fresh); got != 200 {
		t.Fatalf("unexpired token: %d, want 200", got)
	}
	if got := get(forever); got != 200 {
		t.Fatalf("expires_at=0 must never expire: %d, want 200", got)
	}
	if got := get(stale); got != 401 {
		t.Fatalf("expired token: %d, want 401", got)
	}

	// An expired token must NOT fall back to any other auth path — same as invalid.
	// And it stays listed (revocable), it just no longer authenticates.
	toks, err := st.ListAPITokens()
	if err != nil || len(toks) != 3 {
		t.Fatalf("expired tokens must stay listed: n=%d err=%v", len(toks), err)
	}
}

func TestMetricsGateExpiry(t *testing.T) {
	st := testStore(t)
	s := &Server{store: st}
	now := time.Now().Unix()

	// The ONLY metrics-scoped token is expired: the endpoint must stay locked
	// (expiry never silently re-opens /metrics) AND the expired token must not
	// open the gate.
	staleVal := tokenPrefix + "stalemetricssecret0000"
	if err := st.CreateAPIToken("m-stale", "m-stale", sha256Hex(staleVal), "metrics", now-10, ""); err != nil {
		t.Fatal(err)
	}
	scrape := func(token string) int {
		r := httptest.NewRequest("GET", "/metrics", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		s.handleMetrics(rec, r)
		return rec.Code
	}
	if got := scrape(""); got != 401 {
		t.Fatalf("locked endpoint without token: %d, want 401", got)
	}
	if got := scrape(staleVal); got != 401 {
		t.Fatalf("expired metrics token must not open the gate: %d, want 401", got)
	}

	// A live metrics token still works.
	freshVal := tokenPrefix + "freshmetricssecret0000"
	if err := st.CreateAPIToken("m-fresh", "m-fresh", sha256Hex(freshVal), "metrics", now+3600, ""); err != nil {
		t.Fatal(err)
	}
	if got := scrape(freshVal); got != 200 {
		t.Fatalf("valid metrics token: %d, want 200", got)
	}
}

// Minting with ttl_days stores expires_at ≈ now + N days (and 0 = never).
// Runs under a fresh step-up grant (F64) so only the F65 TTL logic is exercised.
func TestCreateTokenTTL(t *testing.T) {
	s := stepUpServer(t)
	_ = s.store.SetSetting(stepUpKey(store.SessionKey("sess1")), strconv.FormatInt(time.Now().Unix(), 10))

	mint := func(body string) (map[string]any, int) {
		r := httptest.NewRequest("POST", "/api/security/tokens", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "sess1"})
		rec := httptest.NewRecorder()
		s.handleCreateToken(rec, r)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out, rec.Code
	}

	out, code := mint(`{"name":"ci","scopes":["read"],"ttl_days":30}`)
	if code != 200 {
		t.Fatalf("mint ttl_days=30: %d body=%v", code, out)
	}
	want := time.Now().Add(30 * 24 * time.Hour).Unix()
	got := int64(out["expires_at"].(float64))
	if got < want-5 || got > want+5 {
		t.Fatalf("expires_at=%d, want ≈%d (±5s)", got, want)
	}

	out, code = mint(`{"name":"forever","scopes":["read"]}`)
	if code != 200 || int64(out["expires_at"].(float64)) != 0 {
		t.Fatalf("omitted ttl_days must mean never: code=%d out=%v", code, out)
	}

	if _, code = mint(`{"name":"bad","scopes":["read"],"ttl_days":-1}`); code != 400 {
		t.Fatalf("negative ttl_days: %d, want 400", code)
	}
	if _, code = mint(`{"name":"bad2","scopes":["read"],"ttl_days":4000}`); code != 400 {
		t.Fatalf("ttl_days over 3650: %d, want 400", code)
	}
}
