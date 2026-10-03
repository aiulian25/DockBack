package api

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dockback/internal/config"
)

func cidr(s string) *net.IPNet { _, n, _ := net.ParseCIDR(s); return n }

func reqFrom(remote string) *http.Request {
	r := httptest.NewRequest("GET", "/api/me", nil)
	r.RemoteAddr = remote
	return r
}

func TestTrustForwardedAllowlist(t *testing.T) {
	// Trust-all when TrustProxy on and no allowlist (legacy).
	s := &Server{cfg: &config.Config{TrustProxy: true}}
	if !s.trustForwarded(reqFrom("203.0.113.9:5000")) {
		t.Error("expected trust-all with no allowlist")
	}
	// With allowlist: only peers inside it are trusted.
	s2 := &Server{cfg: &config.Config{TrustProxy: true, TrustedProxies: []*net.IPNet{cidr("172.16.0.0/12")}}}
	if !s2.trustForwarded(reqFrom("172.20.0.1:33333")) {
		t.Error("proxy peer in CIDR should be trusted")
	}
	if s2.trustForwarded(reqFrom("10.168.1.50:40000")) {
		t.Error("LAN peer outside CIDR must NOT be trusted (spoof guard)")
	}
	// TrustProxy off → never trust, even if peer is listed.
	s3 := &Server{cfg: &config.Config{TrustProxy: false, TrustedProxies: []*net.IPNet{cidr("172.16.0.0/12")}}}
	if s3.trustForwarded(reqFrom("172.20.0.1:1")) {
		t.Error("TrustProxy off must never trust forwarded headers")
	}
}

func TestClientIPSpoofResistance(t *testing.T) {
	s := &Server{cfg: &config.Config{TrustProxy: true, TrustedProxies: []*net.IPNet{cidr("172.16.0.0/12")}}}
	// From a trusted proxy: honor the forwarded client IP.
	r := reqFrom("172.20.0.1:5")
	r.Header.Set("X-Forwarded-For", "9.9.9.9")
	if got := s.clientIP(r); got != "9.9.9.9" {
		t.Errorf("trusted proxy: want forwarded client 9.9.9.9, got %s", got)
	}
	// From an untrusted LAN peer: ignore the spoofed header, use the real peer.
	r2 := reqFrom("10.168.1.50:5")
	r2.Header.Set("X-Forwarded-For", "9.9.9.9")
	if got := s.clientIP(r2); got != "10.168.1.50" {
		t.Errorf("untrusted peer: spoofed XFF must be ignored, got %s", got)
	}
}

// A proxy APPENDS the peer it saw, so everything left of its own entry is
// client-supplied. Reading the chain from the left let any caller pick its own
// IP with one header and walk away from the login lockout, the API-token source
// pins and the audit trail's IP column.
func TestClientIPWalksTheForwardedChainFromTheRight(t *testing.T) {
	s := &Server{cfg: &config.Config{TrustProxy: true, TrustedProxies: []*net.IPNet{cidr("172.16.0.0/12")}}}
	fromProxy := func(xff string) *http.Request {
		r := reqFrom("172.20.0.1:5")
		r.Header.Set("X-Forwarded-For", xff)
		return r
	}

	// The client prepended a forged entry; the real peer is the one our proxy wrote.
	if got := s.clientIP(fromProxy("9.9.9.9, 203.0.113.7")); got != "203.0.113.7" {
		t.Errorf("forged leading entry must be ignored, got %s", got)
	}
	// A two-hop chain: our own proxies are skipped, the caller is not.
	if got := s.clientIP(fromProxy("9.9.9.9, 172.20.0.2, 203.0.113.7")); got != "203.0.113.7" {
		t.Errorf("chain walk = %s, want 203.0.113.7", got)
	}
	if got := s.clientIP(fromProxy("203.0.113.7, 172.20.0.2")); got != "203.0.113.7" {
		t.Errorf("a trailing proxy hop must be skipped, got %s", got)
	}
	// Nothing but our own proxies: fall through rather than report one of them.
	if got := s.clientIP(fromProxy("172.20.0.2, 172.20.0.3")); got != "172.20.0.1" {
		t.Errorf("an all-proxy chain must fall back to the peer, got %s", got)
	}
	realIP := fromProxy("172.20.0.2")
	realIP.Header.Set("X-Real-IP", "203.0.113.9")
	if got := s.clientIP(realIP); got != "203.0.113.9" {
		t.Errorf("X-Real-IP must still be honoured when XFF yields nothing, got %s", got)
	}
	// Junk entries are stepped over; an address with a port still identifies its host.
	if got := s.clientIP(fromProxy("203.0.113.7, not-an-ip")); got != "203.0.113.7" {
		t.Errorf("unparseable entry must be skipped, got %s", got)
	}
	if got := s.clientIP(fromProxy("[2001:db8::a]:4433")); got != "2001:db8::a" {
		t.Errorf("bracketed IPv6 with a port = %s, want 2001:db8::a", got)
	}
	if got := s.clientIP(fromProxy("")); got != "172.20.0.1" {
		t.Errorf("an empty header must fall back to the peer, got %s", got)
	}

	// Legacy trust-all: still no chain to reason about, but read from the right.
	legacy := &Server{cfg: &config.Config{TrustProxy: true}}
	if got := legacy.clientIP(fromProxy("9.9.9.9, 203.0.113.7")); got != "203.0.113.7" {
		t.Errorf("trust-all must take the last entry, got %s", got)
	}
}

func headerServer(trustProxy bool) *Server {
	return &Server{cfg: &config.Config{TrustProxy: trustProxy}}
}

func runHeaders(t *testing.T, s *Server, path string, https bool) http.Header {
	t.Helper()
	h := s.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	r := httptest.NewRequest("GET", path, nil)
	if https {
		r.Header.Set("X-Forwarded-Proto", "https")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Result().Header
}

func TestSecurityHeadersAlwaysPresent(t *testing.T) {
	hdr := runHeaders(t, headerServer(false), "/index.html", false)
	for _, k := range []string{
		"X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy",
		"Cross-Origin-Opener-Policy", "Cross-Origin-Resource-Policy",
		"Permissions-Policy", "Content-Security-Policy",
	} {
		if hdr.Get(k) == "" {
			t.Errorf("missing header %s", k)
		}
	}
}

func TestCSPStyleSrcStrict(t *testing.T) {
	csp := runHeaders(t, headerServer(false), "/index.html", false).Get("Content-Security-Policy")
	// style-src itself must not carry 'unsafe-inline' — injected <style>/external
	// stylesheets are blocked. Inline style= attributes (dynamic bar dimensions)
	// are scoped to the much narrower style-src-attr.
	if !strings.Contains(csp, "style-src 'self';") {
		t.Errorf("style-src not locked to 'self': %q", csp)
	}
	if strings.Contains(csp, "style-src 'self' 'unsafe-inline'") {
		t.Errorf("style-src must not allow 'unsafe-inline': %q", csp)
	}
	if !strings.Contains(csp, "style-src-attr 'unsafe-inline'") {
		t.Errorf("dynamic bar widths need style-src-attr 'unsafe-inline': %q", csp)
	}
}

func TestCacheControlScopedToAPI(t *testing.T) {
	s := headerServer(false)
	if got := runHeaders(t, s, "/api/me", false).Get("Cache-Control"); got != "no-store" {
		t.Errorf("api path Cache-Control = %q, want no-store", got)
	}
	if got := runHeaders(t, s, "/assets/index.js", false).Get("Cache-Control"); got != "" {
		t.Errorf("static asset should not be no-store, got %q", got)
	}
}

func TestHSTSGating(t *testing.T) {
	// No trust-proxy → never HSTS, even over https.
	if got := runHeaders(t, headerServer(false), "/api/me", true).Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS must not be set without trust-proxy, got %q", got)
	}
	// Trust-proxy but plain http → no HSTS.
	if got := runHeaders(t, headerServer(true), "/api/me", false).Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS must not be set over plain http, got %q", got)
	}
	// Trust-proxy + https → HSTS present.
	if got := runHeaders(t, headerServer(true), "/api/me", true).Get("Strict-Transport-Security"); got == "" {
		t.Error("HSTS should be set behind a trusted https proxy")
	}
}

func TestHostCookiePrefixGating(t *testing.T) {
	// Plain HTTP (no trust-proxy): plain names, not Secure (so HTTP login works).
	s := headerServer(false)
	r := httptest.NewRequest("POST", "/api/login", nil)
	w := httptest.NewRecorder()
	s.setSessionCookies(w, r, "tok", "csrf", 3600)
	for _, c := range w.Result().Cookies() {
		if c.Name == "__Host-dback_session" || c.Secure {
			t.Fatalf("plain HTTP must use unprefixed, non-secure cookies, got %s secure=%v", c.Name, c.Secure)
		}
	}

	// Behind https proxy: __Host- prefix + Secure.
	s2 := headerServer(true)
	r2 := httptest.NewRequest("POST", "/api/login", nil)
	r2.Header.Set("X-Forwarded-Proto", "https")
	w2 := httptest.NewRecorder()
	s2.setSessionCookies(w2, r2, "tok", "csrf", 3600)
	var sawHostSession bool
	for _, c := range w2.Result().Cookies() {
		if c.Name == "__Host-dback_session" {
			sawHostSession = true
			if !c.Secure || c.Path != "/" {
				t.Fatalf("__Host- cookie must be Secure + Path=/, got secure=%v path=%s", c.Secure, c.Path)
			}
		}
	}
	if !sawHostSession {
		t.Fatal("expected __Host- prefixed session cookie behind https")
	}
}

func TestReadCookieAcceptsBothNames(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: "__Host-dback_session", Value: "prefixed"})
	if got := readCookie(r, sessionCookie); got != "prefixed" {
		t.Errorf("readCookie should prefer __Host- name, got %q", got)
	}
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.AddCookie(&http.Cookie{Name: "dback_session", Value: "plain"})
	if got := readCookie(r2, sessionCookie); got != "plain" {
		t.Errorf("readCookie should accept plain name, got %q", got)
	}
}
