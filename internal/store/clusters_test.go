package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// The cluster registry's whole reason to exist is that a cluster used to be a
// typed string. These tests pin the three properties that follow from that:
// case-folding (so a fleet can't silently split), rename carrying membership AND
// policy, and — the one a user must be able to trust — delete never destroying
// data.

func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func addNode(t *testing.T, st *Store, id, cluster string) {
	t.Helper()
	if err := st.UpsertNode(&Node{ID: id, Name: id, Cluster: cluster, Transport: "tcp", Address: "tcp://x:1"}); err != nil {
		t.Fatalf("upsert node %s: %v", id, err)
	}
}

// A differently-cased spelling must JOIN the existing cluster, not fork it.
func TestEnsureClusterFoldsCase(t *testing.T) {
	st := testStore(t)
	if _, err := st.EnsureCluster("production"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	got, err := st.EnsureCluster("PRODUCTION")
	if err != nil {
		t.Fatalf("ensure again: %v", err)
	}
	if got != "production" {
		t.Fatalf("EnsureCluster must return the canonical spelling, got %q", got)
	}
	if err := st.CreateCluster("Production", "", ""); !errors.Is(err, ErrClusterExists) {
		t.Fatalf("creating a case-variant must conflict, got %v", err)
	}
	list, _ := st.ListClusters()
	var variants []string
	for _, c := range list {
		if strings.EqualFold(c.Name, "production") {
			variants = append(variants, c.Name)
		}
	}
	if len(variants) != 1 {
		t.Fatalf("case variants must be ONE cluster, got %v", variants)
	}
}

// An empty cluster resolves to the column default, so a node is never left
// pointing at a cluster that doesn't exist.
func TestEnsureClusterEmptyIsDefault(t *testing.T) {
	st := testStore(t)
	got, err := st.EnsureCluster("  ")
	if err != nil || got != DefaultCluster {
		t.Fatalf("blank must resolve to %q, got %q (%v)", DefaultCluster, got, err)
	}
}

func TestValidateClusterName(t *testing.T) {
	bad := map[string]string{
		"empty":      "",
		"slash":      "prod/eu",
		"backslash":  `prod\eu`,
		"leading sp": " prod",
		"control":    "prod\nrm -rf",
	}
	for what, name := range bad {
		if err := ValidateClusterName(name); err == nil {
			t.Errorf("%s (%q) must be rejected", what, name)
		}
	}
	for _, ok := range []string{"prod", "edge-sites", "prod eu", "Ünïcode", "生产"} {
		if err := ValidateClusterName(ok); err != nil {
			t.Errorf("%q must be accepted: %v", ok, err)
		}
	}
	long := make([]rune, maxClusterNameLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if err := ValidateClusterName(string(long)); err == nil {
		t.Error("an over-long name must be rejected")
	}
}

// Renaming has to move membership and the policy scope with the name, or a
// cluster's retention would silently revert to global on rename.
func TestRenameCarriesNodesAndPolicy(t *testing.T) {
	st := testStore(t)
	if _, err := st.EnsureCluster("prod"); err != nil {
		t.Fatal(err)
	}
	addNode(t, st, "n1", "prod")
	addNode(t, st, "n2", "prod")
	addNode(t, st, "n3", "lab")
	if err := st.SetPolicyOverride(ClusterScope("prod"), PolicyOverride{OverrideRetention: true, Generations: 30}); err != nil {
		t.Fatal(err)
	}

	if err := st.RenameCluster("prod", "production"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	c, err := st.GetCluster("production")
	if err != nil {
		t.Fatalf("renamed cluster must exist: %v", err)
	}
	if c.Nodes != 2 {
		t.Fatalf("renamed cluster must keep its 2 nodes, got %d", c.Nodes)
	}
	if got := st.NodeCluster("n1"); got != "production" {
		t.Fatalf("node membership must follow the rename, got %q", got)
	}
	if got := st.NodeCluster("n3"); got != "lab" {
		t.Fatalf("an unrelated node must not move, got %q", got)
	}
	ov, ok := st.GetPolicyOverride(ClusterScope("production"))
	if !ok || ov.Generations != 30 {
		t.Fatal("the policy override must move with the rename")
	}
	if _, ok := st.GetPolicyOverride(ClusterScope("prod")); ok {
		t.Fatal("the old scope key must not linger")
	}
}

// Re-casing is a rename of itself, not a collision with itself.
func TestRenameRecase(t *testing.T) {
	st := testStore(t)
	st.EnsureCluster("prod")
	addNode(t, st, "n1", "prod")
	if err := st.RenameCluster("prod", "PROD"); err != nil {
		t.Fatalf("re-casing must be allowed: %v", err)
	}
	if got := st.NodeCluster("n1"); got != "PROD" {
		t.Fatalf("membership must adopt the new casing, got %q", got)
	}
}

func TestRenameOntoExistingIsRefused(t *testing.T) {
	st := testStore(t)
	st.EnsureCluster("prod")
	st.EnsureCluster("lab")
	if err := st.RenameCluster("prod", "lab"); !errors.Is(err, ErrClusterExists) {
		t.Fatalf("renaming onto an existing cluster must conflict, got %v", err)
	}
}

// THE contract users need to trust: deleting a cluster is a grouping change.
func TestDeleteClusterNeverDeletesData(t *testing.T) {
	st := testStore(t)
	st.EnsureCluster("lab")
	addNode(t, st, "n1", "lab")

	// A cluster with nodes is refused rather than orphaning them.
	if _, err := st.DeleteCluster("lab", ""); !errors.Is(err, ErrClusterInUse) {
		t.Fatalf("a populated cluster must be refused, got %v", err)
	}
	if _, err := st.GetNode("n1"); err != nil {
		t.Fatalf("the refused delete must leave the node alone: %v", err)
	}

	// With a target, the nodes move and only then does the grouping go away.
	st.EnsureCluster("production")
	st.SetPolicyOverride(ClusterScope("lab"), PolicyOverride{OverrideRetention: true, Generations: 7})
	moved, err := st.DeleteCluster("lab", "production")
	if err != nil {
		t.Fatalf("delete with reassignment: %v", err)
	}
	if moved != 1 {
		t.Fatalf("moved = %d, want 1", moved)
	}
	n, err := st.GetNode("n1")
	if err != nil {
		t.Fatalf("the node must SURVIVE its cluster's deletion: %v", err)
	}
	if n.Cluster != "production" {
		t.Fatalf("the node must land in the target cluster, got %q", n.Cluster)
	}
	if _, ok := st.GetPolicyOverride(ClusterScope("lab")); ok {
		t.Fatal("the deleted cluster's policy override must be gone")
	}
	if _, err := st.GetCluster("lab"); err == nil {
		t.Fatal("the cluster must be gone")
	}
}

// A node soft-deleted inside its undo window must not be restorable into a
// cluster that no longer exists.
func TestDeleteClusterMovesSoftDeletedNodes(t *testing.T) {
	st := testStore(t)
	st.EnsureCluster("lab")
	addNode(t, st, "gone", "lab")
	if err := st.SoftDeleteNode("gone"); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if _, err := st.DeleteCluster("lab", ""); err != nil {
		t.Fatalf("a cluster whose only node is soft-deleted must delete cleanly: %v", err)
	}
	if err := st.RestoreNode("gone"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	n, err := st.GetNode("gone")
	if err != nil {
		t.Fatal(err)
	}
	if n.Cluster != DefaultCluster {
		t.Fatalf("an undone node must land in %q, got %q", DefaultCluster, n.Cluster)
	}
}

// An upgrade must land with the clusters the fleet was already using.
func TestSeedClustersAdoptsExistingNames(t *testing.T) {
	st := testStore(t)
	addNode(t, st, "n1", "prod")
	addNode(t, st, "n2", "lab")
	st.seedClusters()

	got := map[string]int{}
	list, err := st.ListClusters()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range list {
		got[c.Name] = c.Nodes
	}
	if got["prod"] != 1 || got["lab"] != 1 {
		t.Fatalf("seeding must adopt the names already in use, got %v", got)
	}
}

// A brand-new install has no nodes to seed from, but the column default points
// at "default" — so it must exist.
func TestSeedClustersGuaranteesDefault(t *testing.T) {
	st := testStore(t)
	list, err := st.ListClusters()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != DefaultCluster {
		t.Fatalf("a fresh install must have exactly the %q cluster, got %v", DefaultCluster, list)
	}
}

// An empty cluster must still be listed — it's exactly the one about to be
// populated or removed.
func TestListClustersIncludesEmpty(t *testing.T) {
	st := testStore(t)
	st.CreateCluster("edge-sites", "remote", "#a78bfa")
	list, _ := st.ListClusters()
	for _, c := range list {
		if c.Name == "edge-sites" {
			if c.Nodes != 0 || c.Description != "remote" {
				t.Fatalf("empty cluster mis-reported: %+v", c)
			}
			return
		}
	}
	t.Fatal("an empty cluster must still appear in the list")
}

// Every cluster a node references must be registered, or it would show on the
// dashboard but be missing from the picker and the settings list. The awkward
// case is removing "default" itself while a soft-deleted node still points there.
func TestDeleteClusterKeepsRegistryConsistent(t *testing.T) {
	st := testStore(t)
	addNode(t, st, "gone", DefaultCluster)
	if err := st.SoftDeleteNode("gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteCluster(DefaultCluster, ""); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.RestoreNode("gone"); err != nil {
		t.Fatal(err)
	}
	n, err := st.GetNode("gone")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetCluster(n.Cluster); err != nil {
		t.Fatalf("node references cluster %q which is not registered: %v", n.Cluster, err)
	}
}
