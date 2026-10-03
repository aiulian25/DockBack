package backup

import (
	"path/filepath"
	"testing"

	"dockback/internal/store"
)

// F84 pure decision: every guard individually. Only an implicitly-balanced run
// over a big, proven-incompressible selection switches to fast.
func TestAutotuneCompression(t *testing.T) {
	const gib = int64(1) << 30
	cases := []struct {
		name       string
		enabled    bool
		explicit   bool
		preset     string
		learned    float64
		selection  int64
		wantPreset string
		wantSwitch bool
	}{
		{"switches: implicit balanced, ratio 0.99, 2 GiB", true, false, "balanced", 0.99, 2 * gib, "fast", true},
		{"switches: unset preset counts as balanced", true, false, "", 0.99, 2 * gib, "fast", true},
		{"exactly at thresholds still switches", true, false, "balanced", 0.97, gib, "fast", true},
		{"toggle off", false, false, "balanced", 0.99, 2 * gib, "balanced", false},
		{"explicit choice wins even at ratio 1.0", true, true, "balanced", 1.0, 2 * gib, "balanced", false},
		{"non-default preset untouched (xz)", true, false, "xz", 0.99, 2 * gib, "xz", false},
		{"non-default preset untouched (fast)", true, false, "fast", 0.99, 2 * gib, "fast", false},
		{"compressible data keeps balanced", true, false, "balanced", 0.50, 2 * gib, "balanced", false},
		{"no history (ratio 0) keeps balanced", true, false, "balanced", 0, 2 * gib, "balanced", false},
		{"just under the ratio floor", true, false, "balanced", 0.9699, 2 * gib, "balanced", false},
		{"small selection keeps balanced", true, false, "balanced", 0.99, gib - 1, "balanced", false},
	}
	for _, c := range cases {
		got, switched := AutotuneCompression(c.enabled, c.explicit, c.preset, c.learned, c.selection)
		if got != c.wantPreset || switched != c.wantSwitch {
			t.Errorf("%s: got (%q,%v), want (%q,%v)", c.name, got, switched, c.wantPreset, c.wantSwitch)
		}
	}
}

// Engine-level resolution: the learned ratio comes from the persisted setting
// and the in-app toggle gates the decision — the exact path (*Engine).Run uses.
func TestAutotuneCompressionEngine(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &Engine{Store: st, Log: func(string, string, string) {}}
	const sel = int64(2) << 30

	// Incompressible history → the implicit default switches, and the archive
	// self-describes as zstd-fast via parseCompression's label.
	_ = st.SetSetting(ratioSettingKey("n1", "media"), "0.99")
	preset, learned, switched := e.autotuneCompression(Options{NodeID: "n1", Compression: "balanced"}, "media", sel)
	if !switched || preset != "fast" || learned < 0.98 {
		t.Fatalf("expected switch to fast: preset=%q learned=%v switched=%v", preset, learned, switched)
	}
	if _, _, _, label := parseCompression(preset); label != "zstd-fast" {
		t.Fatalf("manifest label should be zstd-fast, got %q", label)
	}

	// Compressible history → untouched.
	_ = st.SetSetting(ratioSettingKey("n1", "db"), "0.50")
	if preset, _, switched := e.autotuneCompression(Options{NodeID: "n1", Compression: "balanced"}, "db", sel); switched || preset != "balanced" {
		t.Fatalf("compressible data must keep balanced: %q %v", preset, switched)
	}

	// Explicit xz at ratio 0.99 → untouched (xz-max label preserved).
	if preset, _, switched := e.autotuneCompression(Options{NodeID: "n1", Compression: "xz", CompressionExplicit: true}, "media", sel); switched || preset != "xz" {
		t.Fatalf("explicit xz must be untouched: %q %v", preset, switched)
	}

	// Toggle off → untouched even for the media container.
	_ = st.SetSetting("backup.autotune_compression", "false")
	if _, _, switched := e.autotuneCompression(Options{NodeID: "n1", Compression: "balanced"}, "media", sel); switched {
		t.Fatal("toggle off must disable autotune")
	}

	// No ratio history at all → untouched.
	_ = st.SetSetting("backup.autotune_compression", "true")
	if _, _, switched := e.autotuneCompression(Options{NodeID: "n1", Compression: "balanced"}, "unknown", sel); switched {
		t.Fatal("no learned ratio must mean no autotune")
	}
}
