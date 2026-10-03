package api

import "testing"

// TestParseDockbackLabelsGood covers a full, well-formed label set (F19).
func TestParseDockbackLabelsGood(t *testing.T) {
	p, ok := parseDockbackLabels(map[string]string{
		"com.docker.compose.project": "media", // ignored (not dockback.*)
		"dockback.enable":            "true",
		"dockback.schedule":          "nightly",
		"dockback.pause-mode":        "stop",
		"dockback.mounts.exclude":    "/media, /downloads ,",
		"dockback.retention":         "daily:3,weekly:4,monthly:6,yearly:1,generations:10,autoprune:true",
	})
	if !ok {
		t.Fatal("expected ok=true when dockback.* labels are present")
	}
	if p.Enable == nil || !*p.Enable {
		t.Errorf("enable = %v, want true", p.Enable)
	}
	if p.Schedule != "nightly" {
		t.Errorf("schedule = %q, want nightly", p.Schedule)
	}
	if p.PauseMode != "stop" {
		t.Errorf("pause_mode = %q, want stop", p.PauseMode)
	}
	if len(p.ExcludeMounts) != 2 || p.ExcludeMounts[0] != "/media" || p.ExcludeMounts[1] != "/downloads" {
		t.Errorf("exclude_mounts = %v, want [/media /downloads] (trimmed, no empties)", p.ExcludeMounts)
	}
	if p.Retention == nil {
		t.Fatal("retention should be parsed")
	}
	r := p.Retention
	if !r.OverrideRetention {
		t.Error("OverrideRetention should be true")
	}
	if r.KeepDaily != 3 || r.KeepWeekly != 4 || r.KeepMonthly != 6 || r.KeepYearly != 1 || r.Generations != 10 || !r.Autoprune {
		t.Errorf("retention = %+v, want daily3 weekly4 monthly6 yearly1 gen10 autoprune", r)
	}
}

// TestParseDockbackLabelsGarbage: garbage values for a key are ignored, not fatal.
func TestParseDockbackLabelsGarbage(t *testing.T) {
	p, ok := parseDockbackLabels(map[string]string{
		"dockback.enable":     "maybe",               // not a bool → ignored
		"dockback.pause-mode": "freeze",              // invalid → ignored
		"dockback.retention":  "daily:-3,weekly:x,z", // negatives/garbage → ignored
	})
	if !ok {
		t.Fatal("a dockback.* key is present, so ok must be true even with bad values")
	}
	if p.Enable != nil {
		t.Errorf("enable should be unset for a non-bool value, got %v", *p.Enable)
	}
	if p.PauseMode != "" {
		t.Errorf("pause_mode should be unset for an invalid value, got %q", p.PauseMode)
	}
	if p.Retention != nil {
		t.Errorf("retention should be nil when nothing valid parsed, got %+v", p.Retention)
	}
}

// TestParseDockbackLabelsNone: no dockback.* key → ok=false.
func TestParseDockbackLabelsNone(t *testing.T) {
	if _, ok := parseDockbackLabels(map[string]string{"com.docker.compose.service": "web"}); ok {
		t.Error("expected ok=false when no dockback.* label is present")
	}
	if _, ok := parseDockbackLabels(nil); ok {
		t.Error("expected ok=false for nil labels")
	}
}

// TestParseDockbackLabelsRetentionOnly: a lone retention label still parses, and
// the acceptance-criteria shape (daily:3) maps to KeepDaily=3.
func TestParseDockbackLabelsRetentionOnly(t *testing.T) {
	p, ok := parseDockbackLabels(map[string]string{
		"dockback.enable":    "true",
		"dockback.retention": "daily:3",
	})
	if !ok || p.Retention == nil || p.Retention.KeepDaily != 3 || !p.Retention.OverrideRetention {
		t.Fatalf("daily:3 should give KeepDaily=3 with OverrideRetention; got ok=%v ret=%+v", ok, p.Retention)
	}
	// Only daily set — the rest stay zero (inherit global).
	if p.Retention.KeepWeekly != 0 || p.Retention.KeepMonthly != 0 {
		t.Errorf("unset tiers should be 0, got %+v", p.Retention)
	}
}
