package api

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dockback/internal/config"
)

// F201 — a token pinned to the addresses it is allowed to be used from.
//
// Scope answers "what may this token do"; it never answered "from where". A
// leaked metrics or backup token worked from anywhere on the internet that could
// reach the server.

// The matcher, which is the whole rule. Empty means unrestricted, because that
// is what every token minted before this feature was promised — narrowing them
// on upgrade would break working automation at a version bump.
func TestTokenSourceAllowed(t *testing.T) {
	cases := []struct {
		name  string
		cidrs string
		ip    string
		want  bool
	}{
		{"no pin allows anything", "", "203.0.113.9", true},
		{"no pin allows even a junk address", "   ", "not-an-ip", true},

		{"inside the range", "10.0.0.0/24", "10.0.0.5", true},
		{"outside the range", "10.0.0.0/24", "10.168.1.5", false},
		{"edge of the range", "10.0.0.0/24", "10.0.0.255", true},
		{"just past the range", "10.0.0.0/24", "10.0.1.0", false},

		{"single host, normalized from a bare IP", "10.0.0.5/32", "10.0.0.5", true},
		{"single host rejects its neighbour", "10.0.0.5/32", "10.0.0.6", false},

		{"any of several ranges", "10.0.0.0/24,10.168.1.0/24", "10.168.1.7", true},
		{"none of several ranges", "10.0.0.0/24,10.168.1.0/24", "172.16.0.1", false},

		{"IPv6 inside", "2001:db8::/32", "2001:db8::1", true},
		{"IPv6 outside", "2001:db8::/32", "2001:dead::1", false},

		// A pin that cannot be read denies everything. Treating an unreadable
		// pin as "no pin" would turn a corrupted row into an open door.
		{"unparseable pin denies", "not-a-cidr", "10.0.0.5", false},
		{"one bad entry denies the whole list", "10.0.0.0/24,garbage", "10.0.0.5", false},
		// An unknown caller address cannot satisfy a pin.
		{"empty caller address denies", "10.0.0.0/24", "", false},
		{"junk caller address denies", "10.0.0.0/24", "kittens", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tokenSourceAllowed(c.cidrs, c.ip); got != c.want {
				t.Errorf("tokenSourceAllowed(%q, %q) = %v, want %v", c.cidrs, c.ip, got, c.want)
			}
		})
	}
}

// A bare address means the single host, exactly as DOCKBACK_TRUSTED_PROXIES
// already treats one — and what is stored is canonical, so the operator can
// compare it against their firewall later.
func TestParseTokenCIDRsNormalizes(t *testing.T) {
	_, norm, ok := parseTokenCIDRs([]string{"10.0.0.5", " 10.168.1.0/24 ", "2001:db8::1", ""})
	if !ok {
		t.Fatal("these are all valid")
	}
	want := []string{"10.0.0.5/32", "10.168.1.0/24", "2001:db8::1/128"}
	if strings.Join(norm, ",") != strings.Join(want, ",") {
		t.Errorf("normalized = %v, want %v", norm, want)
	}
	// A bad entry fails the whole list rather than being dropped: silently
	// discarding one would change what the pin means without the operator seeing.
	if _, _, ok := parseTokenCIDRs([]string{"10.0.0.0/24", "999.999.999.999"}); ok {
		t.Error("an unparseable entry must fail the list")
	}
	if _, _, ok := parseTokenCIDRs([]string{"10.0.0.0/24", "example.com"}); ok {
		t.Error("a hostname is not an address pin — DNS is not a boundary")
	}
}

// AC1 + AC2, through the real middleware.
func TestTokenPinnedToCIDREnforcedByMiddleware(t *testing.T) {
	s := newTestServer(t)
	s.cfg = &config.Config{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	call := func(value, fromIP string) int {
		r := httptest.NewRequest("GET", "/api/backups", nil)
		r.Header.Set("Authorization", "Bearer "+value)
		r.RemoteAddr = fromIP + ":40000"
		rec := httptest.NewRecorder()
		s.auth(next).ServeHTTP(rec, r)
		return rec.Code
	}

	// Pinned to one network.
	pinned := tokenPrefix + randToken()
	if err := s.store.CreateAPIToken("t-pin", "pinned", sha256Hex(pinned), "read", 0, "10.0.0.0/24"); err != nil {
		t.Fatal(err)
	}
	if got := call(pinned, "10.0.0.5"); got != http.StatusOK {
		t.Errorf("from inside the pin = %d, want 200", got)
	}
	if got := call(pinned, "10.168.1.5"); got != http.StatusForbidden {
		t.Errorf("from outside the pin = %d, want 403", got)
	}

	// AC2 — an unpinned token is unaffected, from anywhere.
	open := tokenPrefix + randToken()
	if err := s.store.CreateAPIToken("t-open", "open", sha256Hex(open), "read", 0, ""); err != nil {
		t.Fatal(err)
	}
	for _, ip := range []string{"10.0.0.5", "10.168.1.5", "203.0.113.1"} {
		if got := call(open, ip); got != http.StatusOK {
			t.Errorf("an unpinned token from %s = %d, want 200", ip, got)
		}
	}

	// A refused address is audited — a leaked credential being exercised is the
	// event worth having a record of.
	if !auditHas(t, s, "token.denied_ip") {
		t.Error("a refused source address must be audited")
	}
}

// The pin is checked BEFORE the scope, so a token used from the wrong place is
// refused as a wrong-place error rather than reported as a permissions problem.
func TestSourcePinIsCheckedBeforeScope(t *testing.T) {
	s := newTestServer(t)
	s.cfg = &config.Config{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	// A read-scoped token attempting a path its scope forbids, from a disallowed
	// address: the ADDRESS is the reason reported.
	v := tokenPrefix + randToken()
	if err := s.store.CreateAPIToken("t2", "t2", sha256Hex(v), "read", 0, "10.0.0.0/24"); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/settings", nil)
	r.Header.Set("Authorization", "Bearer "+v)
	r.RemoteAddr = "10.168.1.5:40000"
	rec := httptest.NewRecorder()
	s.auth(next).ServeHTTP(rec, r)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "from this address") {
		t.Errorf("the refusal should name the address, not the scope: %s", rec.Body.String())
	}
}

// The /metrics endpoint resolves tokens on its OWN path, not through the
// middleware — and a metrics token is the one most likely to be pinned, so
// gating only the middleware would have left the motivating case open.
func TestMetricsHonoursTheSourcePin(t *testing.T) {
	s := newTestServer(t)
	s.cfg = &config.Config{}

	v := tokenPrefix + randToken()
	if err := s.store.CreateAPIToken("m1", "prometheus", sha256Hex(v), "metrics", 0, "10.0.0.0/24"); err != nil {
		t.Fatal(err)
	}
	call := func(fromIP string) int {
		r := httptest.NewRequest("GET", "/metrics", nil)
		r.Header.Set("Authorization", "Bearer "+v)
		r.RemoteAddr = fromIP + ":40000"
		rec := httptest.NewRecorder()
		s.handleMetrics(rec, r)
		return rec.Code
	}
	if got := call("10.0.0.5"); got != http.StatusOK {
		t.Errorf("scrape from inside the pin = %d, want 200", got)
	}
	if got := call("203.0.113.9"); got != http.StatusUnauthorized {
		t.Errorf("scrape from outside the pin = %d, want 401 (the endpoint stays locked)", got)
	}
}

// A pin cannot be enforced when anyone can forge the forwarded address, so
// creating one is refused rather than issued-and-useless.
func TestPinRefusedWhenProxyTrustIsSpoofable(t *testing.T) {
	s := newTestServer(t)

	// Trust-all-proxies: X-Forwarded-For is attacker-controlled.
	s.cfg = &config.Config{TrustProxy: true}
	if !s.proxyTrustIsSpoofable() {
		t.Fatal("trust-all-proxies must be recognised as spoofable")
	}

	// With a proxy allow-list, the forwarded address is trustworthy again.
	_, n, _ := net.ParseCIDR("172.20.0.0/16")
	s.cfg = &config.Config{TrustProxy: true, TrustedProxies: []*net.IPNet{n}}
	if s.proxyTrustIsSpoofable() {
		t.Error("a configured proxy allow-list is not spoofable")
	}

	// No proxy trust at all: the peer address is the source of truth.
	s.cfg = &config.Config{}
	if s.proxyTrustIsSpoofable() {
		t.Error("without proxy trust there is nothing to spoof")
	}
}
