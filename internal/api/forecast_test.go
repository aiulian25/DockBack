package api

import (
	"testing"

	"dockback/internal/store"
)

func TestForecastLinearGrowth(t *testing.T) {
	const day = int64(86400)
	const gb = uint64(1) << 30
	var samples []store.DestSample
	// 11 daily points, used growing 1 GiB/day, total 100 GiB.
	for i := int64(0); i <= 10; i++ {
		samples = append(samples, store.DestSample{Ts: i * day, Total: 100 * gb, Used: uint64(i) * gb})
	}
	now := 10 * day
	total, free := 100*gb, 90*gb // 10 GiB used, 90 free at the last point
	f := forecastFromSamples(samples, total, free, now)

	if f.Points != 11 {
		t.Fatalf("points = %d, want 11", f.Points)
	}
	// ~1 GiB/day ⇒ ~30 GiB/month.
	perMonth := float64(f.GrowthPerMonth) / float64(gb)
	if perMonth < 29 || perMonth > 31 {
		t.Errorf("growth/month = %.1f GiB, want ~30", perMonth)
	}
	// 90 GiB free at ~1 GiB/day ⇒ ~90 days.
	if f.DaysToFull < 88 || f.DaysToFull > 92 {
		t.Errorf("days to full = %d, want ~90", f.DaysToFull)
	}
	if f.FillDate <= now {
		t.Errorf("fill date %d should be in the future (now=%d)", f.FillDate, now)
	}
}

func TestForecastTooFewPoints(t *testing.T) {
	f := forecastFromSamples([]store.DestSample{{Ts: 0, Total: 100, Used: 10}}, 100, 90, 100)
	if f.DaysToFull != -1 {
		t.Errorf("single point should not project a fill date, got %d", f.DaysToFull)
	}
}

func TestForecastShrinkingNotProjected(t *testing.T) {
	const day = int64(86400)
	samples := []store.DestSample{
		{Ts: 0, Total: 100, Used: 50},
		{Ts: 5 * day, Total: 100, Used: 20}, // pruning shrank usage
	}
	f := forecastFromSamples(samples, 100, 80, 5*day)
	if f.DaysToFull != -1 {
		t.Errorf("shrinking usage should not project a fill date, got %d", f.DaysToFull)
	}
	if f.GrowthPerMonth >= 0 {
		t.Errorf("growth should be negative when shrinking, got %d", f.GrowthPerMonth)
	}
}
