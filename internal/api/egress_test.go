package api

import (
	"testing"

	"dockback/internal/config"
	"dockback/internal/egress"
)

// TestEgressCoerce covers the F39 setting validation: a valid allow-list is stored
// as a normalized CSV, empty clears the override, and a garbage entry is rejected.
func TestEgressCoerce(t *testing.T) {
	// Valid entries (host, wildcard, IP, CIDR) → trimmed, comma-joined.
	if v, ok := coerceSetting("security.egress_allow", " example.com , *.foo.net ,, 203.0.113.10 , 10.0.0.0/8 "); !ok || v != "example.com,*.foo.net,203.0.113.10,10.0.0.0/8" {
		t.Fatalf("valid list: v=%q ok=%v", v, ok)
	}
	// Empty / whitespace / commas-only → cleared (fall back to env default).
	for _, in := range []string{"", "   ", " , , "} {
		if v, ok := coerceSetting("security.egress_allow", in); !ok || v != "" {
			t.Errorf("clear %q: v=%q ok=%v, want ok/empty", in, v, ok)
		}
	}
	// A garbage entry (whitespace inside a would-be host) rejects the whole save.
	for _, in := range []string{"example.com, bad host", "not a real host"} {
		if _, ok := coerceSetting("security.egress_allow", in); ok {
			t.Errorf("garbage %q must be rejected", in)
		}
	}
}

// TestEgressLiveApply covers the live atomic swap + env fallback (F39): saving an
// override changes egress.Default() with no restart, and clearing it restores the
// env default.
func TestEgressLiveApply(t *testing.T) {
	// Reset the process-wide policy after the test so we don't leak into others.
	defer egress.Configure(nil)

	s := &Server{
		store: testStore(t),
		cfg:   &config.Config{EgressAllow: []string{"env-default.example"}},
	}

	// Boot behavior: env default installed.
	egress.Configure(s.cfg.EgressAllow)
	if err := s.egressAllowEffectiveCheck("env-default.example"); err != nil {
		t.Fatalf("env host should pass: %v", err)
	}

	// Save an override to example.com only → other hosts refused, live.
	s.applyEgress("example.com")
	if egress.Default().Check("https://example.com") != nil {
		t.Error("example.com should pass after override")
	}
	if egress.Default().Check("https://other.net") == nil {
		t.Error("other.net must be refused after override")
	}
	// The env default is no longer effective while the override is set.
	if egress.Default().Check("env-default.example") == nil {
		t.Error("env default should be refused once overridden to example.com")
	}

	// Clearing the override falls back to the env default (no restart).
	s.applyEgress("")
	if egress.Default().Check("env-default.example") != nil {
		t.Error("clearing the override should restore the env default")
	}
	if egress.Default().Check("example.com") == nil {
		t.Error("example.com should be refused once the override is cleared (only env default applies)")
	}
}

// egressAllowEffectiveCheck is a tiny test helper: does the current live policy
// permit host?
func (s *Server) egressAllowEffectiveCheck(host string) error {
	return egress.Default().Check(host)
}
