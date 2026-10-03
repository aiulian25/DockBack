package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dockback/internal/config"
	"dockback/internal/store"
)

func clusterServer(t *testing.T) *Server {
	t.Helper()
	return &Server{store: testStore(t), cfg: &config.Config{}}
}

func clusterReqFor(t *testing.T, method, target, name, body string) (*http.Request, *httptest.ResponseRecorder) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	if name != "" {
		r.SetPathValue("name", name)
	}
	return r, httptest.NewRecorder()
}

func mustNode(t *testing.T, s *Server, id, cluster string) {
	t.Helper()
	if err := s.store.UpsertNode(&store.Node{
		ID: id, Name: id, Cluster: cluster, Transport: "tcp", Address: "tcp://x:1",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCreateClusterRejectsBadNamesAndDuplicates(t *testing.T) {
	s := clusterServer(t)

	r, w := clusterReqFor(t, "POST", "/api/clusters", "", `{"name":"production","description":"","color":""}`)
	s.handleCreateCluster(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("create: want 200, got %d: %s", w.Code, w.Body.String())
	}

	// A case variant is the SAME cluster, and must be refused as a conflict
	// rather than quietly forking the fleet.
	r, w = clusterReqFor(t, "POST", "/api/clusters", "", `{"name":"Production","description":"","color":""}`)
	s.handleCreateCluster(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("case variant: want 409, got %d: %s", w.Code, w.Body.String())
	}

	// A name that would break a URL path segment / policy scope key is refused.
	r, w = clusterReqFor(t, "POST", "/api/clusters", "", `{"name":"prod/eu","description":"","color":""}`)
	s.handleCreateCluster(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf(`"prod/eu": want 400, got %d`, w.Code)
	}
}

// The contract a user must be able to trust: a populated cluster is refused,
// the refusal SAYS nothing is deleted, and nothing is.
func TestDeleteClusterWithNodesIsRefusedAndDestroysNothing(t *testing.T) {
	s := clusterServer(t)
	if _, err := s.store.EnsureCluster("homelab"); err != nil {
		t.Fatal(err)
	}
	mustNode(t, s, "n1", "homelab")

	r, w := clusterReqFor(t, "DELETE", "/api/clusters/homelab", "homelab", "")
	s.handleDeleteCluster(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", w.Code, w.Body.String())
	}
	msg := w.Body.String()
	for _, want := range []string{"still has 1 node", "never deletes nodes"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the refusal must state %q; got %s", want, msg)
		}
	}
	if _, err := s.store.GetNode("n1"); err != nil {
		t.Fatalf("the node must be untouched: %v", err)
	}
	if _, err := s.store.GetCluster("homelab"); err != nil {
		t.Fatalf("the cluster must still exist: %v", err)
	}
}

func TestDeleteClusterReassignsAndKeepsNodes(t *testing.T) {
	s := clusterServer(t)
	s.store.EnsureCluster("homelab")
	s.store.EnsureCluster("production")
	mustNode(t, s, "n1", "homelab")
	s.store.SetPolicyOverride(store.ClusterScope("homelab"), store.PolicyOverride{
		OverrideRetention: true, Generations: 7,
	})

	r, w := clusterReqFor(t, "DELETE", "/api/clusters/homelab?reassign_to=production", "homelab", "")
	s.handleDeleteCluster(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Deleted         string `json:"deleted"`
		NodesReassigned int    `json:"nodes_reassigned"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.NodesReassigned != 1 {
		t.Fatalf("nodes_reassigned = %d, want 1", resp.NodesReassigned)
	}
	n, err := s.store.GetNode("n1")
	if err != nil {
		t.Fatalf("the node must survive: %v", err)
	}
	if n.Cluster != "production" {
		t.Fatalf("node cluster = %q, want production", n.Cluster)
	}
	// The grouping's policy goes with it — those nodes fall back to global.
	if _, ok := s.store.GetPolicyOverride(store.ClusterScope("homelab")); ok {
		t.Fatal("the deleted cluster's policy override must be gone")
	}
}

// A node write must canonicalize its cluster, or a typo forks the fleet.
func TestResolveNodeClusterCanonicalizes(t *testing.T) {
	s := clusterServer(t)
	if _, err := s.store.EnsureCluster("production"); err != nil {
		t.Fatal(err)
	}
	got, err := s.resolveNodeCluster("PRODUCTION")
	if err != nil {
		t.Fatal(err)
	}
	if got != "production" {
		t.Fatalf("cluster = %q, want the canonical %q", got, "production")
	}
	// A blank cluster lands in the default rather than nowhere.
	if got, _ := s.resolveNodeCluster(""); got != store.DefaultCluster {
		t.Fatalf("blank cluster = %q, want %q", got, store.DefaultCluster)
	}
	// An unusable name is rejected here, not silently stored on the node.
	if _, err := s.resolveNodeCluster("bad/name"); err == nil {
		t.Fatal(`"bad/name" must be rejected`)
	}
}

func TestClusterPolicyRoundTripAndInheritance(t *testing.T) {
	s := clusterServer(t)
	s.store.EnsureCluster("production")
	mustNode(t, s, "n1", "production")

	body := `{"override_destinations":false,"destinations":[],"override_retention":true,` +
		`"generations":30,"keep_daily":0,"keep_weekly":0,"keep_monthly":6,"keep_yearly":0,` +
		`"autoprune":true,"override_frequency":false,"min_interval_hours":0}`
	r, w := clusterReqFor(t, "PUT", "/api/clusters/production/policy", "production", body)
	s.handleSetClusterPolicy(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("set policy: want 200, got %d: %s", w.Code, w.Body.String())
	}

	r, w = clusterReqFor(t, "GET", "/api/clusters/production/policy", "production", "")
	s.handleGetClusterPolicy(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("get policy: want 200, got %d: %s", w.Code, w.Body.String())
	}
	var got clusterPolicyResp
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Override.OverrideRetention || got.Override.Generations != 30 {
		t.Fatalf("override did not round-trip: %+v", got.Override)
	}
	if got.Resolved.Generations != 30 || got.Resolved.KeepMonthly != 6 {
		t.Fatalf("resolved must reflect the override: %+v", got.Resolved)
	}
	if got.Nodes != 1 {
		t.Fatalf("nodes = %d, want 1", got.Nodes)
	}

	// A member node inherits the cluster tier through the normal resolver.
	if p := s.effectivePolicy("n1"); p.Generations != 30 {
		t.Fatalf("member node must inherit the cluster policy, got %d generations", p.Generations)
	}
	// A node elsewhere does not.
	mustNode(t, s, "n2", "homelab")
	if p := s.effectivePolicy("n2"); p.Generations == 30 {
		t.Fatal("a node outside the cluster must not inherit its policy")
	}
}

// Renaming through the API must carry membership and policy, so a cluster's
// retention can't silently revert to global because someone fixed a typo.
func TestUpdateClusterRenameCarriesEverything(t *testing.T) {
	s := clusterServer(t)
	s.store.EnsureCluster("prod")
	mustNode(t, s, "n1", "prod")
	s.store.SetPolicyOverride(store.ClusterScope("prod"), store.PolicyOverride{
		OverrideRetention: true, Generations: 30,
	})

	r, w := clusterReqFor(t, "PATCH", "/api/clusters/prod", "prod",
		`{"name":"production","description":"customer facing","color":"#4d8fd6"}`)
	s.handleUpdateCluster(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	n, _ := s.store.GetNode("n1")
	if n.Cluster != "production" {
		t.Fatalf("membership must follow the rename, got %q", n.Cluster)
	}
	if p := s.effectivePolicy("n1"); p.Generations != 30 {
		t.Fatalf("the policy must survive the rename, got %d generations", p.Generations)
	}
	c, err := s.store.GetCluster("production")
	if err != nil {
		t.Fatal(err)
	}
	if c.Description != "customer facing" || c.Color != "#4d8fd6" {
		t.Fatalf("description/colour not saved: %+v", c)
	}
}

func TestUpdateClusterRenameOntoExistingConflicts(t *testing.T) {
	s := clusterServer(t)
	s.store.EnsureCluster("prod")
	s.store.EnsureCluster("lab")
	r, w := clusterReqFor(t, "PATCH", "/api/clusters/prod", "prod", `{"name":"lab","description":"","color":""}`)
	s.handleUpdateCluster(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", w.Code, w.Body.String())
	}
}

func TestListClustersReportsNodeCounts(t *testing.T) {
	s := clusterServer(t)
	s.store.EnsureCluster("production")
	s.store.EnsureCluster("homelab")
	mustNode(t, s, "n1", "production")
	mustNode(t, s, "n2", "production")

	r, w := clusterReqFor(t, "GET", "/api/clusters", "", "")
	s.handleListClusters(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp clusterListResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, c := range resp.Clusters {
		counts[c.Name] = c.Nodes
	}
	if counts["production"] != 2 {
		t.Fatalf("production = %d nodes, want 2 (%v)", counts["production"], counts)
	}
	if _, ok := counts["homelab"]; !ok {
		t.Fatalf("an empty cluster must still be listed (%v)", counts)
	}
}

// Assignment from the cluster screen must move the server and touch NOTHING
// else — in particular not the transport, address or sealed credential.
func TestAssignClusterMembersOnlyChangesCluster(t *testing.T) {
	s := clusterServer(t)
	s.store.EnsureCluster("production")
	if err := s.store.UpsertNode(&store.Node{
		ID: "n1", Name: "razer", Cluster: "default", Transport: "ssh",
		Address: "ssh://deploy@10.0.0.5:22", SecretEnc: []byte("sealed-blob"),
	}); err != nil {
		t.Fatal(err)
	}

	r, w := clusterReqFor(t, "POST", "/api/clusters/production/members", "production", `{"node_ids":["n1"]}`)
	s.handleAssignClusterMembers(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	n, err := s.store.GetNode("n1")
	if err != nil {
		t.Fatal(err)
	}
	if n.Cluster != "production" {
		t.Fatalf("cluster = %q, want production", n.Cluster)
	}
	if n.Transport != "ssh" || n.Address != "ssh://deploy@10.0.0.5:22" {
		t.Fatalf("the connection must be untouched, got %s %s", n.Transport, n.Address)
	}
	if string(n.SecretEnc) != "sealed-blob" {
		t.Fatal("the sealed credential must be untouched")
	}

	// Re-assigning an existing member is a no-op, not an error.
	r, w = clusterReqFor(t, "POST", "/api/clusters/production/members", "production", `{"node_ids":["n1"]}`)
	s.handleAssignClusterMembers(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("re-assign: want 200, got %d", w.Code)
	}
}

// The members endpoint has to answer both halves of the question: who is in this
// cluster, and who could be moved in.
func TestClusterMembersSplitsFleet(t *testing.T) {
	s := clusterServer(t)
	s.store.EnsureCluster("production")
	mustNode(t, s, "n1", "production")
	mustNode(t, s, "n2", "default")

	r, w := clusterReqFor(t, "GET", "/api/clusters/production/members", "production", "")
	s.handleClusterMembers(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var got struct {
		Members   []clusterMember `json:"members"`
		Available []clusterMember `json:"available"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != 1 || got.Members[0].ID != "n1" {
		t.Fatalf("members = %+v, want just n1", got.Members)
	}
	if len(got.Available) != 1 || got.Available[0].ID != "n2" {
		t.Fatalf("available = %+v, want just n2", got.Available)
	}
}

// An unknown node id must fail loudly rather than silently moving nothing.
func TestAssignClusterMembersUnknownNode(t *testing.T) {
	s := clusterServer(t)
	s.store.EnsureCluster("production")
	r, w := clusterReqFor(t, "POST", "/api/clusters/production/members", "production", `{"node_ids":["nope"]}`)
	s.handleAssignClusterMembers(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
}
