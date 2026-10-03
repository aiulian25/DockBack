package api

import (
	"math"
	"testing"

	"dockback/internal/store"
)

// F18: scrub-per-cycle, "almost full" percentage, and forecast-days resolvers must
// default to today's hardcoded values, honor a configured value, and self-clamp to
// a sane range even if an out-of-band value slips into the settings store.
func TestF18TuningResolvers(t *testing.T) {
	s := &Server{store: testStore(t)}

	// Defaults reproduce today's 3 / 90% / 30-day behavior.
	if got := s.scrubPerCycle(); got != 3 {
		t.Errorf("default scrubPerCycle = %d, want 3", got)
	}
	if got := s.destFullThreshold(); !approx(got, 0.90) {
		t.Errorf("default destFullThreshold = %v, want 0.90", got)
	}
	if got := s.forecastWarnDays(); got != 30 {
		t.Errorf("default forecastWarnDays = %d, want 30", got)
	}

	// Configured values are honored.
	_ = s.store.SetSetting("scrub.per_cycle", "5")
	if got := s.scrubPerCycle(); got != 5 {
		t.Errorf("scrubPerCycle = %d, want 5", got)
	}
	_ = s.store.SetSetting("alert.dest_full_pct", "80")
	if got := s.destFullThreshold(); !approx(got, 0.80) {
		t.Errorf("destFullThreshold = %v, want 0.80", got)
	}
	_ = s.store.SetSetting("alert.forecast_days", "14")
	if got := s.forecastWarnDays(); got != 14 {
		t.Errorf("forecastWarnDays = %d, want 14", got)
	}

	// Self-clamping (defense in depth beyond coerceSetting).
	_ = s.store.SetSetting("scrub.per_cycle", "99")
	if got := s.scrubPerCycle(); got != 5 {
		t.Errorf("scrubPerCycle high should clamp to 5, got %d", got)
	}
	_ = s.store.SetSetting("scrub.per_cycle", "0")
	if got := s.scrubPerCycle(); got != 1 {
		t.Errorf("scrubPerCycle low should clamp to 1, got %d", got)
	}
	_ = s.store.SetSetting("alert.dest_full_pct", "10")
	if got := s.destFullThreshold(); !approx(got, 0.50) {
		t.Errorf("destFullThreshold low should clamp to 0.50, got %v", got)
	}
	_ = s.store.SetSetting("alert.dest_full_pct", "150")
	if got := s.destFullThreshold(); !approx(got, 0.99) {
		t.Errorf("destFullThreshold high should clamp to 0.99, got %v", got)
	}
	_ = s.store.SetSetting("alert.forecast_days", "0")
	if got := s.forecastWarnDays(); got != 1 {
		t.Errorf("forecastWarnDays low should clamp to 1, got %d", got)
	}
}

// F18: selecting the scrub batch honors a configured cap of 5.
func TestSelectScrubBatchCapFive(t *testing.T) {
	var all []*store.Backup
	for i := int64(1); i <= 8; i++ {
		all = append(all, &store.Backup{ID: string(rune('a' + i)), Status: "success", LastVerifiedAt: i})
	}
	if got := selectScrubBatch(all, 100, 5); len(got) != 5 {
		t.Fatalf("batch size = %d, want 5", len(got))
	}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
