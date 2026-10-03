package egress

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestDisabledAllowsEverything(t *testing.T) {
	p := Parse(nil)
	if p.Enabled() {
		t.Fatal("empty policy should be disabled")
	}
	for _, h := range []string{"example.com", "1.2.3.4", "anything.internal"} {
		if !p.AllowHost(h) {
			t.Errorf("disabled policy should allow %q", h)
		}
		if err := p.Check(h); err != nil {
			t.Errorf("disabled Check(%q) = %v, want nil", h, err)
		}
	}
}

func TestExactAndSuffixAndCIDR(t *testing.T) {
	p := Parse([]string{
		"s3.amazonaws.com",
		"*.example.com",
		"203.0.113.0/24",
		"2001:db8::/32",
		"https://nas.lan:5006/dav", // URL form, host only
	})
	if !p.Enabled() {
		t.Fatal("policy with entries should be enabled")
	}
	allow := []string{
		"s3.amazonaws.com",  // exact
		"S3.AMAZONAWS.COM",  // case-insensitive
		"example.com",       // apex of *.example.com
		"files.example.com", // subdomain
		"a.b.example.com",   // deep subdomain
		"203.0.113.50",      // in CIDR
		"2001:db8::1",       // in IPv6 CIDR
		"nas.lan",           // from URL entry
	}
	for _, h := range allow {
		if !p.AllowHost(h) {
			t.Errorf("expected %q allowed", h)
		}
	}
	deny := []string{
		"amazonaws.com",        // not the exact host, not a suffix rule
		"evil.com",             // unrelated
		"example.com.evil.com", // suffix-trick must not match
		"203.0.114.1",          // outside CIDR
		"10.0.0.1",             // unrelated IP
	}
	for _, h := range deny {
		if p.AllowHost(h) {
			t.Errorf("expected %q denied", h)
		}
	}
}

func TestCheckExtractsHost(t *testing.T) {
	p := Parse([]string{"good.example.com"})
	for _, target := range []string{
		"good.example.com",
		"good.example.com:443",
		"https://good.example.com/path?x=1",
		"http://good.example.com:8080/",
	} {
		if err := p.Check(target); err != nil {
			t.Errorf("Check(%q) = %v, want nil", target, err)
		}
	}
	for _, target := range []string{
		"bad.example.org",
		"https://bad.example.org/path",
		"bad.example.org:21",
	} {
		if err := p.Check(target); err == nil {
			t.Errorf("Check(%q) = nil, want denied error", target)
		}
	}
}

func TestGuardDialRefusesNonAllowed(t *testing.T) {
	p := Parse([]string{"allowed.host"})
	var dialed string
	base := func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = address
		return nil, errors.New("base reached") // sentinel: we got past the guard
	}
	guard := p.GuardDial(base)

	// Denied: guard refuses before base runs.
	dialed = ""
	if _, err := guard(context.Background(), "tcp", "denied.host:443"); err == nil {
		t.Error("guard should refuse denied.host")
	}
	if dialed != "" {
		t.Errorf("base dialer must not run for denied host, got %q", dialed)
	}

	// Allowed: guard passes through to base.
	if _, err := guard(context.Background(), "tcp", "allowed.host:443"); err == nil || err.Error() != "base reached" {
		t.Errorf("guard should pass allowed.host to base, err=%v", err)
	}
	if dialed != "allowed.host:443" {
		t.Errorf("base should have been dialed, got %q", dialed)
	}
}

// GuardDialDefault must resolve the process-wide policy AT DIAL TIME, so a
// long-lived client built once at boot follows a live allow-list change made in
// Settings (F39). This is the behaviour the boot-time Policy.GuardDial capture
// could not provide.
func TestGuardDialDefaultFollowsLiveSwap(t *testing.T) {
	t.Cleanup(func() { Configure(nil) })

	var dialed string
	base := func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = address
		return nil, errors.New("base reached") // sentinel: got past the guard
	}
	// Built ONCE, then never rebuilt — like the notification dispatcher.
	guard := GuardDialDefault(base)

	// 1) Under the initial policy, a.host is allowed and b.host is denied.
	Configure([]string{"a.host"})
	dialed = ""
	if _, err := guard(context.Background(), "tcp", "b.host:443"); err == nil {
		t.Error("b.host should be denied under the first policy")
	}
	if dialed != "" {
		t.Errorf("base must not run for a denied host, got %q", dialed)
	}
	if _, err := guard(context.Background(), "tcp", "a.host:443"); err == nil || err.Error() != "base reached" {
		t.Errorf("a.host should pass to base under the first policy, err=%v", err)
	}

	// 2) Swap the live policy to allow b.host and drop a.host. The SAME wrapped
	//    dialer must now follow the new list without being rebuilt.
	Configure([]string{"b.host"})
	dialed = ""
	if _, err := guard(context.Background(), "tcp", "b.host:443"); err == nil || err.Error() != "base reached" {
		t.Errorf("b.host should be allowed after the live swap, err=%v", err)
	}
	if dialed != "b.host:443" {
		t.Errorf("base should have been dialed for b.host, got %q", dialed)
	}
	dialed = ""
	if _, err := guard(context.Background(), "tcp", "a.host:443"); err == nil {
		t.Error("a.host should be denied after the live swap")
	}
	if dialed != "" {
		t.Errorf("base must not run for the now-denied a.host, got %q", dialed)
	}

	// 3) Clearing the policy (disabled) lets everything through again.
	Configure(nil)
	if _, err := guard(context.Background(), "tcp", "anything.host:443"); err == nil || err.Error() != "base reached" {
		t.Errorf("a disabled policy should allow anything, err=%v", err)
	}
}

// ParseCIDROrIP is the single home for the bare-IP-widening rule shared by the
// trusted-proxy list and the API-token source pin (Step 14 / finding #12).
func TestParseCIDROrIP(t *testing.T) {
	ok := []struct{ in, want string }{
		{"10.0.0.5", "10.0.0.5/32"},        // bare IPv4 → /32
		{" 10.0.0.5 ", "10.0.0.5/32"},      // trimmed
		{"10.168.1.0/24", "10.168.1.0/24"}, // CIDR passthrough
		{"::1", "::1/128"},                 // bare IPv6 → /128
		{"2001:db8::/32", "2001:db8::/32"}, // IPv6 CIDR passthrough
	}
	for _, c := range ok {
		n, good := ParseCIDROrIP(c.in)
		if !good {
			t.Errorf("ParseCIDROrIP(%q) = not-ok, want %s", c.in, c.want)
			continue
		}
		if n.String() != c.want {
			t.Errorf("ParseCIDROrIP(%q) = %s, want %s", c.in, n.String(), c.want)
		}
	}
	bad := []string{"", "   ", "notanip", "10.0.0.5/99", "10.0.0.0/abc", "300.1.1.1", "10.0.0.5:443"}
	for _, in := range bad {
		if n, good := ParseCIDROrIP(in); good {
			t.Errorf("ParseCIDROrIP(%q) = ok (%v), want not-ok", in, n)
		}
	}
}

func TestConfigureAndDefault(t *testing.T) {
	t.Cleanup(func() { Configure(nil) })
	Configure([]string{"only.this.host"})
	if !Default().Enabled() {
		t.Fatal("Default should reflect Configure")
	}
	if Default().AllowHost("other.host") {
		t.Error("Default should deny other.host")
	}
	if !Default().AllowHost("only.this.host") {
		t.Error("Default should allow configured host")
	}
}
