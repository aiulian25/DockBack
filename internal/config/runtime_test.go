package config

import "testing"

// The parsing here decides whether the GC gets a ceiling at all, so the
// "unlimited" spellings matter: mistaking one for a real limit would pin the
// heap to an absurd value, and missing a real one leaves the process able to be
// OOM-killed mid-backup.

func TestCeilDivRoundsUpToWholeCores(t *testing.T) {
	cases := []struct {
		quota, period int64
		want          int
	}{
		{100000, 100000, 1}, // 1.0 CPU
		{150000, 100000, 2}, // 1.5 CPUs — must round UP, or half a core is wasted
		{200000, 100000, 2}, // 2.0 CPUs
		{50000, 100000, 1},  // 0.5 CPU — never less than one
		{10, 100000, 1},     // absurdly small quota still yields a usable value
		{400000, 100000, 4},
	}
	for _, c := range cases {
		if got := ceilDiv(c.quota, c.period); got != c.want {
			t.Errorf("ceilDiv(%d, %d) = %d, want %d", c.quota, c.period, got, c.want)
		}
	}
}

// TuneRuntime runs on every boot, including on bare metal and on hosts with no
// cgroup files at all. It must never panic and must always say something.
func TestTuneRuntimeIsSafeAnywhere(t *testing.T) {
	if got := TuneRuntime(); got == "" {
		t.Fatal("TuneRuntime must always return a line for the boot log")
	}
	// Idempotent: a second call must not misbehave.
	if got := TuneRuntime(); got == "" {
		t.Fatal("TuneRuntime must be safe to call twice")
	}
}

// The headroom must leave real room for the non-heap footprint (thread stacks,
// zstd window buffers) — a value at or above 1.0 would defeat the point.
func TestMemLimitHeadroomLeavesRoom(t *testing.T) {
	if memLimitHeadroom >= 1.0 || memLimitHeadroom < 0.5 {
		t.Fatalf("headroom %v is outside a sane range", memLimitHeadroom)
	}
	// A 1 GiB container must target well under 1 GiB.
	var gib int64 = 1 << 30
	if target := int64(float64(gib) * memLimitHeadroom); target >= gib {
		t.Fatalf("heap target %d must be below the %d container limit", target, gib)
	}
}
