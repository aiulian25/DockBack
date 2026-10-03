package api

import (
	"testing"

	"dockback/internal/store"
)

// TestUptimePct (F40) checks the transition-duration uptime math over a window.
// rows are newest-first, as NodeHealth returns them.
func TestUptimePct(t *testing.T) {
	// No history at all → 100 (nothing was ever observed down).
	if got := uptimePct(nil, 0, 1000); got != 100 {
		t.Errorf("no rows: %.1f, want 100", got)
	}

	// Unreachable 0..500 then reachable 500..1000 → 50%.
	half := []store.NodeHealthRow{
		{Ts: 500, Reachable: true},
		{Ts: 0, Reachable: false},
	}
	if got := uptimePct(half, 0, 1000); got != 50.0 {
		t.Errorf("half down: %.1f, want 50.0", got)
	}

	// A single baseline "reachable" row before the window → up the whole window.
	up := []store.NodeHealthRow{{Ts: 100, Reachable: true}}
	if got := uptimePct(up, 0, 1000); got != 100.0 {
		t.Errorf("all up: %.1f, want 100.0", got)
	}

	// State entering the window is unreachable and never recovers → 0%.
	down := []store.NodeHealthRow{{Ts: -50, Reachable: false}}
	if got := uptimePct(down, 0, 1000); got != 0.0 {
		t.Errorf("all down: %.1f, want 0.0", got)
	}

	// Three quarters up: down 0..250, up 250..1000.
	q := []store.NodeHealthRow{
		{Ts: 250, Reachable: true},
		{Ts: 0, Reachable: false},
	}
	if got := uptimePct(q, 0, 1000); got != 75.0 {
		t.Errorf("three-quarters up: %.1f, want 75.0", got)
	}

	// Degenerate window.
	if got := uptimePct(up, 1000, 1000); got != 100 {
		t.Errorf("zero window: %.1f, want 100", got)
	}
}
