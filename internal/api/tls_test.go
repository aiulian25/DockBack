package api

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"
)

// TestSecureCookiesUnderDirectTLS locks in PLAN §3.12: when the app serves HTTPS
// itself (r.TLS set), cookies are marked Secure even without a trusted proxy.
func TestSecureCookiesUnderDirectTLS(t *testing.T) {
	s := newTestServer(t)

	plain := httptest.NewRequest("GET", "http://host/api/me", nil)
	if s.secureCookies(plain) {
		t.Fatal("plain HTTP must not yield Secure cookies")
	}

	overTLS := httptest.NewRequest("GET", "https://host/api/me", nil)
	overTLS.TLS = &tls.ConnectionState{}
	if !s.secureCookies(overTLS) {
		t.Fatal("direct built-in TLS must yield Secure cookies")
	}
}
