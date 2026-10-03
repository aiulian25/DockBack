package backup

import (
	"path/filepath"
	"testing"

	"dockback/internal/store"
)

// F104 inserts a CLUSTER tier into the retention chain. The property that must
// hold is precedence: global < cluster < node < container, with each tier only
// applying when it actually overrides — so an install that never touches
// clusters resolves to exactly what it resolved to before.

func clusterStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	_ = st.SetSetting("retention.generations", "3")
	_ = st.SetSetting("retention.autoprune", "true")
	if err := st.UpsertNode(&store.Node{
		ID: "n1", Name: "n1", Cluster: "production", Transport: "tcp", Address: "tcp://x:1",
	}); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestClusterRetentionTier(t *testing.T) {
	st := clusterStore(t)

	// A cluster with no override changes nothing — the pre-F104 behaviour.
	if cfg, ap := EffectiveRetention(st, "n1"); cfg.Generations != 3 || !ap {
		t.Fatalf("no cluster override must inherit global, got gens=%d autoprune=%v", cfg.Generations, ap)
	}

	// The cluster override applies to its members.
	_ = st.SetPolicyOverride(store.ClusterScope("production"), store.PolicyOverride{
		OverrideRetention: true, Generations: 30, KeepMonthly: 6, Autoprune: false,
	})
	cfg, ap := EffectiveRetention(st, "n1")
	if cfg.Generations != 30 || cfg.Monthly != 6 || ap {
		t.Fatalf("cluster tier: gens=%d monthly=%d autoprune=%v, want 30/6/false", cfg.Generations, cfg.Monthly, ap)
	}

	// A node in no known cluster is untouched by it.
	if cfg, ap := EffectiveRetention(st, "unknown"); cfg.Generations != 3 || !ap {
		t.Fatalf("an unknown node must inherit global, got gens=%d autoprune=%v", cfg.Generations, ap)
	}
}

func TestNodeOverrideBeatsCluster(t *testing.T) {
	st := clusterStore(t)
	_ = st.SetPolicyOverride(store.ClusterScope("production"), store.PolicyOverride{
		OverrideRetention: true, Generations: 30,
	})
	_ = st.SetPolicyOverride(store.NodeScope("n1"), store.PolicyOverride{
		OverrideRetention: true, Generations: 5,
	})
	if cfg, _ := EffectiveRetention(st, "n1"); cfg.Generations != 5 {
		t.Fatalf("the node override must win over its cluster, got %d", cfg.Generations)
	}
}

func TestContainerOverrideBeatsClusterAndNode(t *testing.T) {
	st := clusterStore(t)
	_ = st.SetPolicyOverride(store.ClusterScope("production"), store.PolicyOverride{
		OverrideRetention: true, Generations: 30,
	})
	_ = st.SetPolicyOverride(store.ContainerScope("n1", "immich"), store.PolicyOverride{
		OverrideRetention: true, Generations: 90,
	})
	if cfg, _ := EffectiveRetentionFor(st, "n1", "immich"); cfg.Generations != 90 {
		t.Fatalf("the container override must be most specific, got %d", cfg.Generations)
	}
	// A sibling container on the same node still gets the cluster's value.
	if cfg, _ := EffectiveRetentionFor(st, "n1", "other"); cfg.Generations != 30 {
		t.Fatalf("a sibling must fall back to the cluster tier, got %d", cfg.Generations)
	}
}

// A cluster override that only sets destinations must not silently zero out
// retention — the tiers are independent, exactly as they are for a node.
func TestClusterDestinationsOnlyLeavesRetentionAlone(t *testing.T) {
	st := clusterStore(t)
	_ = st.SetPolicyOverride(store.ClusterScope("production"), store.PolicyOverride{
		OverrideDestinations: true, Destinations: []string{"s3"},
	})
	if cfg, ap := EffectiveRetention(st, "n1"); cfg.Generations != 3 || !ap {
		t.Fatalf("a destinations-only cluster override must leave retention global, got gens=%d autoprune=%v", cfg.Generations, ap)
	}
}

// The global auto-snapshot budget (F48) must survive every tier, or a cluster
// GFS override would silently disable auto-snapshot retention fleet-wide.
func TestClusterOverrideCarriesAutoKeep(t *testing.T) {
	st := clusterStore(t)
	_ = st.SetSetting("retention.autosnap_keep", "12")
	if want, _ := EffectiveRetention(st, "n2"); want.AutoKeep != 12 {
		t.Fatalf("test setup: the global auto-keep must read back as 12, got %d", want.AutoKeep)
	}
	_ = st.SetPolicyOverride(store.ClusterScope("production"), store.PolicyOverride{
		OverrideRetention: true, Generations: 30,
	})
	got, _ := EffectiveRetention(st, "n1")
	if got.AutoKeep != 12 {
		t.Fatalf("auto-snapshot budget must carry across the cluster tier: got %d, want 12", got.AutoKeep)
	}
}
