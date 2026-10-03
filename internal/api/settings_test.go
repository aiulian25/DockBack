package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCoerceSettingAllowList covers the settings allow-list + coercion (F15):
// known keys are clamped/validated, unknown keys are rejected.
func TestCoerceSettingAllowList(t *testing.T) {
	// Unknown / hostile keys are rejected outright.
	for _, k := range []string{"totally.bogus", "retention.generations", "schedule", "notify.config", ""} {
		if _, ok := coerceSetting(k, "1"); ok {
			t.Errorf("key %q must not be settable via /api/settings", k)
		}
	}

	// Numeric clamps.
	cases := []struct {
		key, in, want string
	}{
		{"drill.per_cycle", "99", "5"},
		{"drill.per_cycle", "0", "1"},
		{"scrub.interval_days", "-4", "0"},
		{"db.ready_timeout_seconds", "5", "10"},       // below min
		{"db.ready_timeout_seconds", "99999", "3600"}, // above max
		{"db.ready_timeout_seconds", "abc", "300"},    // garbage → default
		{"schedule.jitter_seconds", "-1", "0"},
		{"upload.max_mbps", "-10", "0"},
		{"upload.max_mbps", "50", "50"},
		{"backup.bind_skip_gib", "0", "1"},                  // F12: below min → 1 GiB
		{"backup.bind_skip_gib", "3", "3"},                  // in range
		{"backup.bind_skip_gib", "99999", "1024"},           // above max
		{"backup.bind_skip_gib", "x", "5"},                  // garbage → default
		{"restore.health_timeout_seconds", "10", "30"},      // F30: below min → 30
		{"restore.health_timeout_seconds", "900", "900"},    // in range
		{"restore.health_timeout_seconds", "99999", "3600"}, // above max
		{"restore.health_timeout_seconds", "x", "300"},      // garbage → default
		{"drill.boot_wait_seconds", "1", "10"},              // F30: below min → 10
		{"drill.boot_wait_seconds", "120", "120"},           // in range
		{"drill.boot_wait_seconds", "9999", "600"},          // above max
		{"drill.boot_wait_seconds", "x", "45"},              // garbage → default
		{"critical.rpo_min_seconds", "10", "60"},            // F31: below min → 60
		{"critical.rpo_min_seconds", "120", "120"},          // in range (2-min RPO floor)
		{"critical.rpo_min_seconds", "99999", "3600"},       // above max
		{"critical.tick_seconds", "5", "30"},                // below min → 30
		{"critical.fail_limit", "99", "10"},                 // above max → 10
		{"critical.fail_limit", "x", "3"},                   // garbage → default
	}
	for _, c := range cases {
		got, ok := coerceSetting(c.key, c.in)
		if !ok || got != c.want {
			t.Errorf("coerceSetting(%q,%q) = %q,%v; want %q,true", c.key, c.in, got, ok, c.want)
		}
	}

	// Booleans coerce anything non-"true" to "false".
	if v, _ := coerceSetting("manifest.encrypt", "true"); v != "true" {
		t.Errorf("manifest.encrypt true = %q", v)
	}
	if v, _ := coerceSetting("verify.deep", "yes"); v != "false" {
		t.Errorf("verify.deep non-true should be false, got %q", v)
	}

	// Enum falls back to the default on an unknown value.
	if v, _ := coerceSetting("drill.scope", "weird"); v != "newest" {
		t.Errorf("drill.scope invalid = %q, want newest", v)
	}

	// F25: sidecar image — a plausible ref (incl. a private registry + tag) passes
	// verbatim (trimmed); empty/whitespace/garbage is rejected.
	sidecarOK := []struct{ in, want string }{
		{"alpine:3.20", "alpine:3.20"},
		{"registry.example.com/mirror/alpine:3.20", "registry.example.com/mirror/alpine:3.20"},
		{"registry.internal:5000/alpine@sha256:abc123", "registry.internal:5000/alpine@sha256:abc123"},
		{"  alpine:3.20  ", "alpine:3.20"}, // trimmed
	}
	for _, c := range sidecarOK {
		if got, ok := coerceSetting("backup.sidecar_image", c.in); !ok || got != c.want {
			t.Errorf("coerceSetting(sidecar,%q) = %q,%v; want %q,true", c.in, got, ok, c.want)
		}
	}
	for _, bad := range []string{"", "   ", "alpine 3.20", "alpine:3.20; rm -rf /", "alpine\t3.20", "reg/al$pine"} {
		if _, ok := coerceSetting("backup.sidecar_image", bad); ok {
			t.Errorf("coerceSetting(sidecar,%q) should be rejected", bad)
		}
	}
}

// TestHandleSetSettingsRejectsUnknownKey confirms the handler 400s on an unknown
// key and never writes it, but stores a clamped known key (F15 / SCAN_NOTES #51).
func TestHandleSetSettingsRejectsUnknownKey(t *testing.T) {
	s := &Server{store: testStore(t)}

	// Unknown key → 400, nothing written.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/settings", strings.NewReader(`{"totally.bogus":"pwn"}`))
	s.handleSetSettings(rec, req)
	if rec.Code != 400 {
		t.Fatalf("unknown key: status = %d, want 400", rec.Code)
	}
	if v, _ := s.store.GetSetting("totally.bogus", ""); v != "" {
		t.Fatalf("unknown key must not be stored, got %q", v)
	}

	// Known numeric key → 200, stored clamped.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/settings", strings.NewReader(`{"drill.per_cycle":"42"}`))
	s.handleSetSettings(rec, req)
	if rec.Code != 200 {
		t.Fatalf("known key: status = %d, want 200", rec.Code)
	}
	if v, _ := s.store.GetSetting("drill.per_cycle", ""); v != "5" {
		t.Fatalf("drill.per_cycle stored = %q, want clamped 5", v)
	}
}
