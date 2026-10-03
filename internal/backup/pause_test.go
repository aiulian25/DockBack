package backup

import (
	"path/filepath"
	"testing"

	"dockback/internal/store"
)

func TestResolvePauseMode(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Store: st, Log: func(string, string, string) {}}

	const app = "nginx:1.27"

	// Default: no setting, no flags -> pause (brief freeze for a consistent snapshot).
	if got := e.resolvePauseMode(Options{NodeID: "n1"}, app, "app"); got != PausePause {
		t.Fatalf("default = %q, want pause", got)
	}

	// Legacy StopApp flag -> stop.
	if got := e.resolvePauseMode(Options{NodeID: "n1", StopApp: true}, app, "app"); got != PauseStop {
		t.Fatalf("StopApp = %q, want stop", got)
	}

	// Remembered per-container setting is honored (scheduled/bulk backups).
	_ = st.SetSetting(PauseModeKey("n1", "app"), PausePause)
	if got := e.resolvePauseMode(Options{NodeID: "n1"}, app, "app"); got != PausePause {
		t.Fatalf("remembered = %q, want pause", got)
	}

	// Explicit per-run choice overrides everything (incl. StopApp).
	if got := e.resolvePauseMode(Options{NodeID: "n1", PauseMode: PauseStop, StopApp: false}, app, "app"); got != PauseStop {
		t.Fatalf("explicit = %q, want stop", got)
	}
	if got := e.resolvePauseMode(Options{NodeID: "n1", PauseMode: PauseNone}, app, "app"); got != PauseNone {
		t.Fatalf("explicit none = %q, want none (overrides remembered pause)", got)
	}
}

// TestResolvePauseModePangolin locks in the image-aware default: an embedded-
// SQLite app (Pangolin) defaults to a full STOP for a pristine snapshot, but a
// remembered/explicit choice still wins.
func TestResolvePauseModePangolin(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Store: st, Log: func(string, string, string) {}}

	const img = "fossorial/pangolin:1.0"

	// No stored choice -> stop (embedded SQLite wants a clean shutdown).
	if got := e.resolvePauseMode(Options{NodeID: "n1"}, img, "pangolin"); got != PauseStop {
		t.Fatalf("pangolin default = %q, want stop", got)
	}
	// A non-Pangolin app on the same node still defaults to pause.
	if got := e.resolvePauseMode(Options{NodeID: "n1"}, "traefik:v3", "traefik"); got != PausePause {
		t.Fatalf("traefik default = %q, want pause", got)
	}
	// A remembered choice overrides the image default.
	_ = st.SetSetting(PauseModeKey("n1", "pangolin"), PausePause)
	if got := e.resolvePauseMode(Options{NodeID: "n1"}, img, "pangolin"); got != PausePause {
		t.Fatalf("pangolin remembered = %q, want pause (user override wins)", got)
	}
	// An explicit per-run choice also overrides it.
	if got := e.resolvePauseMode(Options{NodeID: "n1", PauseMode: PauseNone}, img, "other"); got != PauseNone {
		t.Fatalf("pangolin explicit none = %q, want none", got)
	}
}

// TestShouldPause locks in the quiesce gate — notably that a STOPPED target is
// never paused/stopped (F9), and that databases and no-volume runs are skipped.
func TestShouldPause(t *testing.T) {
	cases := []struct {
		name     string
		pm       string
		engine   string
		running  bool
		volCount int
		want     bool
	}{
		{"running app with volumes -> pause", PausePause, "", true, 1, true},
		{"running app, stop mode -> quiesce", PauseStop, "", true, 2, true},
		{"stopped app -> no pause (F9)", PausePause, "", false, 3, false},
		{"stopped app, stop mode -> no quiesce (F9)", PauseStop, "", false, 3, false},
		{"database (running) -> never paused", PausePause, "postgres", true, 1, false},
		{"pause disabled -> no pause", PauseNone, "", true, 1, false},
		{"no volumes -> nothing to quiesce", PausePause, "", true, 0, false},
	}
	for _, c := range cases {
		if got := shouldPause(c.pm, c.engine, c.running, c.volCount); got != c.want {
			t.Errorf("%s: shouldPause(%q,%q,running=%v,vols=%d) = %v, want %v",
				c.name, c.pm, c.engine, c.running, c.volCount, got, c.want)
		}
	}
}
