package backup

import (
	"path/filepath"
	"testing"
	"time"

	"dockback/internal/store"
)

// F30: the post-restore health-gate timeout resolves per-container override >
// global setting > shipped default, clamps out-of-range overrides, and clears.
func TestRestoreHealthTimeoutResolution(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := &Engine{Store: st, Log: func(string, string, string) {}}

	// Default (nothing set) → 300s / 5 minutes.
	if got := e.RestoreHealthTimeoutGlobal(); got != defaultRestoreHealthSeconds {
		t.Errorf("global default = %d, want %d", got, defaultRestoreHealthSeconds)
	}
	if got := e.restoreHealthTimeout("n1", "app"); got != 5*time.Minute {
		t.Errorf("effective default = %v, want 5m", got)
	}

	// Global setting takes effect (the acceptance: 900 → 15m).
	_ = st.SetSetting(restoreHealthKey, "900")
	if got := e.restoreHealthTimeout("n1", "app"); got != 15*time.Minute {
		t.Errorf("with global=900, effective = %v, want 15m", got)
	}

	// Per-container override wins over the global value.
	if err := e.SetRestoreHealthTimeoutOverride("n1", "app", 120); err != nil {
		t.Fatal(err)
	}
	if n, ok := e.RestoreHealthTimeoutOverride("n1", "app"); !ok || n != 120 {
		t.Errorf("override = (%d,%v), want (120,true)", n, ok)
	}
	if got := e.restoreHealthTimeout("n1", "app"); got != 120*time.Second {
		t.Errorf("override should win: effective = %v, want 120s", got)
	}
	// An unrelated container still sees the global value.
	if got := e.restoreHealthTimeout("n1", "other"); got != 15*time.Minute {
		t.Errorf("unrelated container should use global: %v, want 15m", got)
	}

	// Out-of-range override is clamped; a non-positive value clears it.
	_ = e.SetRestoreHealthTimeoutOverride("n1", "app", 999999)
	if n, _ := e.RestoreHealthTimeoutOverride("n1", "app"); n != maxRestoreHealthSeconds {
		t.Errorf("override should clamp to %d, got %d", maxRestoreHealthSeconds, n)
	}
	_ = e.SetRestoreHealthTimeoutOverride("n1", "app", 5) // below min
	if n, _ := e.RestoreHealthTimeoutOverride("n1", "app"); n != minRestoreHealthSeconds {
		t.Errorf("override should clamp up to %d, got %d", minRestoreHealthSeconds, n)
	}
	_ = e.SetRestoreHealthTimeoutOverride("n1", "app", 0)
	if _, ok := e.RestoreHealthTimeoutOverride("n1", "app"); ok {
		t.Error("override should be cleared by a non-positive value")
	}
	if got := e.restoreHealthTimeout("n1", "app"); got != 15*time.Minute {
		t.Errorf("after clearing override, falls back to global: %v, want 15m", got)
	}
}
