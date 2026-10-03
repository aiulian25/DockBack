package backup

import (
	"path/filepath"
	"testing"

	"dockback/internal/store"
)

func TestEstimateCompressed(t *testing.T) {
	const uncompressed = 10_000

	// learned <= 0 → fall back to the preset constant (today's behaviour).
	fast := estimateCompressed(uncompressed, "fast", 0)
	balanced := estimateCompressed(uncompressed, "balanced", 0)
	mx := estimateCompressed(uncompressed, "max", 0)
	dflt := estimateCompressed(uncompressed, "", 0) // unset == balanced

	// Higher compression presets estimate smaller archives.
	if !(mx < balanced && balanced < fast) {
		t.Fatalf("expected max < balanced < fast, got max=%d balanced=%d fast=%d", mx, balanced, fast)
	}
	if dflt != balanced {
		t.Fatalf("unset preset should match balanced: %d vs %d", dflt, balanced)
	}
	// The estimate is below the uncompressed size (it's compressed) but non-zero.
	if balanced <= 0 || balanced >= uncompressed {
		t.Fatalf("balanced estimate out of range: %d", balanced)
	}
}

// F17: a learned ratio drives the estimate over the preset constant, and is
// clamped to a sane band so one odd run can't wildly mis-budget.
func TestEstimateCompressedLearnedRatio(t *testing.T) {
	const uncompressed = 10_000

	// A learned ratio of 0.25 yields ~0.25x — not the 0.70x balanced preset.
	if got := estimateCompressed(uncompressed, "balanced", 0.25); got != 2500 {
		t.Errorf("learned 0.25 estimate = %d, want 2500 (0.25x)", got)
	}
	// The learned ratio overrides the preset regardless of preset.
	if got := estimateCompressed(uncompressed, "max", 0.25); got != 2500 {
		t.Errorf("learned ratio should override the preset: got %d, want 2500", got)
	}
	// Clamped low: a fluke tiny ratio can't drop below the floor.
	if got := estimateCompressed(uncompressed, "balanced", 0.01); got != int64(minLearnedRatio*uncompressed) {
		t.Errorf("learned 0.01 should clamp to the floor %.2f: got %d", minLearnedRatio, got)
	}
	// Clamped high: incompressible data can't exceed 1.0x the selection.
	if got := estimateCompressed(uncompressed, "balanced", 1.8); got != int64(maxLearnedRatio*uncompressed) {
		t.Errorf("learned 1.8 should clamp to the ceiling %.2f: got %d", maxLearnedRatio, got)
	}
}

// F17: recordRatio persists an EWMA-smoothed observation, and learnedRatio reads
// it back; a first observation seeds it directly, later ones smooth toward the new
// value. No history returns 0 (so the caller uses the preset).
func TestLearnedRatioSmoothing(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := &Engine{Store: st, Log: func(string, string, string) {}}

	if r := e.learnedRatio("n1", "app"); r != 0 {
		t.Errorf("no history should return 0, got %v", r)
	}

	// First observation: 2500/10000 = 0.25, seeded directly.
	e.recordRatio("n1", "app", 10_000, 2_500)
	if r := e.learnedRatio("n1", "app"); r < 0.24 || r > 0.26 {
		t.Errorf("first observation should seed ~0.25, got %v", r)
	}

	// Second observation 0.75 smooths halfway toward it: 0.5*0.75 + 0.5*0.25 = 0.5.
	e.recordRatio("n1", "app", 10_000, 7_500)
	if r := e.learnedRatio("n1", "app"); r < 0.49 || r > 0.51 {
		t.Errorf("smoothed ratio should be ~0.50, got %v", r)
	}

	// A different container is independent.
	if r := e.learnedRatio("n1", "other"); r != 0 {
		t.Errorf("unrelated container must have no learned ratio, got %v", r)
	}
	// Guard against divide-by-zero / nonsense inputs.
	e.recordRatio("n1", "z", 0, 100)
	if r := e.learnedRatio("n1", "z"); r != 0 {
		t.Errorf("zero uncompressed must not record a ratio, got %v", r)
	}
}
