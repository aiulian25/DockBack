package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// F13: a stopped container that still holds a NAMED data volume and has no backup
// belongs in stopped_at_risk — but not one with only tmpfs/anonymous mounts, and a
// stopped-at-risk container must never inflate the running-unprotected number.
func TestCoverageStoppedAtRisk(t *testing.T) {
	st := testStore(t)
	if err := st.UpsertNode(&store.Node{ID: "n1", Name: "Node1"}); err != nil {
		t.Fatal(err)
	}
	// A stopped container that already has a successful backup must NOT be at-risk.
	if err := st.CreateBackup(&store.Backup{ID: "b1", NodeID: "n1", TargetName: "backed-up", Status: "success", CreatedAt: 100}); err != nil {
		t.Fatal(err)
	}
	anon := strings.Repeat("a", 64) // Docker-assigned anonymous-volume id

	s := &Server{store: st, stats: map[string]*nodeStat{}}
	s.setStat("n1", &nodeStat{Reachable: true, Containers: []*dockercli.Container{
		// Stopped, named volume, no backup → at risk.
		{ID: "c1", Name: "occasional-app", State: "exited", Mounts: []dockercli.Mount{{Type: "volume", Name: "occasional_data", Destination: "/data"}}},
		// Stopped, only tmpfs → not at risk.
		{ID: "c2", Name: "throwaway", State: "exited", Mounts: []dockercli.Mount{{Type: "tmpfs", Destination: "/tmp"}}},
		// Stopped, only an anonymous volume → not at risk.
		{ID: "c3", Name: "anon-only", State: "exited", Mounts: []dockercli.Mount{{Type: "volume", Name: anon, Destination: "/v"}}},
		// Stopped, named volume, but already backed up → not at risk.
		{ID: "c4", Name: "backed-up", State: "exited", Mounts: []dockercli.Mount{{Type: "volume", Name: "kept_data", Destination: "/data"}}},
		// Running, named volume, no backup → running-unprotected (NOT stopped_at_risk).
		{ID: "c5", Name: "running-app", State: "running", Mounts: []dockercli.Mount{{Type: "volume", Name: "run_data", Destination: "/data"}}},
	}})

	rec := httptest.NewRecorder()
	s.handleCoverage(rec, httptest.NewRequest("GET", "/api/coverage", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp coverageResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	if len(resp.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(resp.Nodes))
	}
	cn := resp.Nodes[0]

	atRisk := map[string]bool{}
	for _, c := range cn.StoppedAtRisk {
		atRisk[c.Name] = true
	}
	if !atRisk["occasional-app"] {
		t.Errorf("stopped container with a named volume + no backup should be at risk; got %v", names(cn.StoppedAtRisk))
	}
	for _, bad := range []string{"throwaway", "anon-only", "backed-up", "running-app"} {
		if atRisk[bad] {
			t.Errorf("%q must NOT be in stopped_at_risk", bad)
		}
	}
	if resp.StoppedAtRiskTotal != 1 {
		t.Errorf("stopped_at_risk_total = %d, want 1", resp.StoppedAtRiskTotal)
	}

	// The running-unprotected surface must be unchanged: only the running app,
	// and stopped-at-risk never inflates it.
	if cn.Running != 1 {
		t.Errorf("running = %d, want 1 (stopped containers excluded)", cn.Running)
	}
	if len(cn.Unprotected) != 1 || cn.Unprotected[0].Name != "running-app" {
		t.Errorf("unprotected = %v, want [running-app]", names(cn.Unprotected))
	}
	if resp.UnprotectedTotal != 1 {
		t.Errorf("unprotected_total = %d, want 1", resp.UnprotectedTotal)
	}
}

func names(cs []coverageContainer) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

// F13: the anonymous-volume-id detector must accept a 64-hex id and reject a
// human-chosen name (including short hex names).
func TestIsAnonymousVolumeName(t *testing.T) {
	if !isAnonymousVolumeName(strings.Repeat("0", 64)) {
		t.Error("64 hex chars should read as anonymous")
	}
	for _, named := range []string{"", "pgdata", "immich_data", "abc123", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("g", 64)} {
		if isAnonymousVolumeName(named) {
			t.Errorf("%q should read as a named (non-anonymous) volume", named)
		}
	}
}
