package backup

import (
	"path/filepath"
	"testing"

	"dockback/internal/store"
)

func TestEffectiveRetentionInheritsAndOverrides(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Global policy: keep 3, autoprune on.
	_ = st.SetSetting("retention.generations", "3")
	_ = st.SetSetting("retention.autoprune", "true")

	// No override -> inherits global.
	cfg, ap := EffectiveRetention(st, "n1")
	if cfg.Generations != 3 || !ap {
		t.Fatalf("inherit: gens=%d autoprune=%v, want 3/true", cfg.Generations, ap)
	}

	// Node override -> uses its own values + autoprune.
	_ = st.SetPolicyOverride(store.NodeScope("n1"), store.PolicyOverride{
		OverrideRetention: true, Generations: 10, KeepWeekly: 4, Autoprune: false,
	})
	cfg, ap = EffectiveRetention(st, "n1")
	if cfg.Generations != 10 || cfg.Weekly != 4 || ap {
		t.Fatalf("override: gens=%d weekly=%d autoprune=%v, want 10/4/false", cfg.Generations, cfg.Weekly, ap)
	}

	// A different node still inherits the global.
	cfg, ap = EffectiveRetention(st, "n2")
	if cfg.Generations != 3 || !ap {
		t.Fatalf("other node inherit: gens=%d autoprune=%v, want 3/true", cfg.Generations, ap)
	}
}
