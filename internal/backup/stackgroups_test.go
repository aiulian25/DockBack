package backup

import (
	"encoding/json"
	"testing"

	"dockback/internal/store"
)

// mkGroupBackup fabricates a stored stack backup row whose manifest carries the given
// service + consistency group, for the pure group-selection tests (no store, no
// Docker). Rows are newest-first, matching ListBackups.
func mkGroupBackup(id, project, service, group string, at, created int64) *store.Backup {
	man := Manifest{Service: service, ConsistencyGroup: group, ConsistencyAt: at, Image: "app:1"}
	mb, _ := json.Marshal(man)
	return &store.Backup{
		ID: id, Stack: project, TargetName: service, Status: "success",
		CreatedAt: created, ManifestJSON: string(mb),
	}
}

func TestStackGroups(t *testing.T) {
	// db + app captured together in group g1 (complete); a later app-only group g2.
	rows := []*store.Backup{
		mkGroupBackup("b3", "blog", "app", "g2", 2000, 2000),
		mkGroupBackup("b4", "blog", "cache", "", 3000, 3000), // untagged — ignored
		mkGroupBackup("b2", "blog", "app", "g1", 1000, 1001),
		mkGroupBackup("b1", "blog", "db", "g1", 1000, 1000),
		mkGroupBackup("bx", "other", "db", "g9", 1000, 1000), // different stack — ignored
	}

	groups := stackGroupsFrom(rows, "blog", []string{"db", "app"})
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d: %+v", len(groups), groups)
	}
	// Newest first: g2 (at 2000) then g1 (at 1000).
	if groups[0].ID != "g2" || groups[1].ID != "g1" {
		t.Fatalf("groups not newest-first: %+v", groups)
	}
	// (c) Complete only when every universe service is covered.
	if !groups[1].Complete {
		t.Errorf("g1 covers db+app and must be Complete: %+v", groups[1])
	}
	if groups[0].Complete {
		t.Errorf("g2 covers only app and must NOT be Complete: %+v", groups[0])
	}

	// Legacy behavior: a stack with no consistency groups yields an empty slice.
	plain := []*store.Backup{
		{ID: "p1", Stack: "plain", TargetName: "web", Status: "success", CreatedAt: 1, ManifestJSON: `{"service":"web"}`},
	}
	if g := stackGroupsFrom(plain, "plain", []string{"web"}); len(g) != 0 {
		t.Errorf("stack with no snapshots must yield [], got %+v", g)
	}
}

func TestRestoreStackGroupSelection(t *testing.T) {
	rows := []*store.Backup{
		mkGroupBackup("b3", "blog", "app", "g2", 2000, 2000), // g2 has app but NOT db
		mkGroupBackup("b2", "blog", "app", "g1", 1000, 1001),
		mkGroupBackup("b1", "blog", "db", "g1", 1000, 1000),
	}

	// (b) group g2 lacks a db backup -> named error naming the missing service.
	if _, err := selectStackServicesFrom(rows, "blog", "g2"); err == nil || err.Error() != `no backup in group g2 for service "db"` {
		t.Fatalf("expected named missing-service error, got %v", err)
	}

	// (a) group g1 covers both services -> selection succeeds and EVERY pick is in g1.
	picks, err := selectStackServicesFrom(rows, "blog", "g1")
	if err != nil {
		t.Fatalf("g1 is complete, selection should succeed: %v", err)
	}
	if len(picks) != 2 {
		t.Fatalf("expected db+app, got %d: %v", len(picks), picks)
	}
	for svc, ss := range picks {
		if ss.man.ConsistencyGroup != "g1" {
			t.Errorf("service %q restored from group %q, want g1", svc, ss.man.ConsistencyGroup)
		}
	}

	// Default (no group) picks newest per service across groups: app from g2 (newer).
	def, err := selectStackServicesFrom(rows, "blog", "")
	if err != nil {
		t.Fatalf("default selection: %v", err)
	}
	if def["app"].man.ConsistencyGroup != "g2" {
		t.Errorf("default app pick should be newest (g2), got %q", def["app"].man.ConsistencyGroup)
	}
}

// TestRestoreStackTarget (F52) asserts the chosen target node is threaded into
// every per-service RestoreOptions, so a cross-node stack restore recreates every
// member on the target — not the source the backups came from.
func TestRestoreStackTarget(t *testing.T) {
	rows := []*store.Backup{
		mkGroupBackup("db1", "blog", "db", "", 0, 100),
		mkGroupBackup("app1", "blog", "app", "", 0, 101),
	}
	byService, err := selectStackServicesFrom(rows, "blog", "")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	order := topoOrder(byService)
	if len(order) != 2 {
		t.Fatalf("want 2 services, got %d", len(order))
	}

	const target = "hp-node"
	sopts := StackRestoreOptions{Recreate: true, Snapshot: true, RemapFromIP: "10.0.0.1", RemapToIP: "10.0.0.2",
		RemapFromPath: "/opt/docker", RemapToPath: "/opt/stacks"} // F81
	for _, s := range order {
		opt := stackServiceRestoreOptions(s, target, sopts)
		if opt.NodeID != target {
			t.Errorf("service %q: NodeID=%q, want target %q", s.service, opt.NodeID, target)
		}
		if opt.BackupID != s.backup.ID || !opt.Recreate || !opt.Snapshot {
			t.Errorf("service %q: flags not threaded: %+v", s.service, opt)
		}
		if opt.RemapFromIP != "10.0.0.1" || opt.RemapToIP != "10.0.0.2" {
			t.Errorf("service %q: remap not threaded: %+v", s.service, opt)
		}
		if opt.RemapFromPath != "/opt/docker" || opt.RemapToPath != "/opt/stacks" {
			t.Errorf("service %q: path remap not threaded (F81): %+v", s.service, opt)
		}
	}

	// Same-node (empty target defaults to source in RestoreStack) — the builder with
	// the source id produces source-targeted options, i.e. today's behavior.
	same := stackServiceRestoreOptions(order[0], "blog-source", StackRestoreOptions{})
	if same.NodeID != "blog-source" {
		t.Fatalf("same-node restore must target the source node, got %q", same.NodeID)
	}
}
