package backup

import (
	"context"
	"path/filepath"
	"testing"

	"dockback/internal/store"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
)

// TestSelectMountsExcludesDumpedDBDataDir locks in PLAN §4.1: when a consistent
// DB dump was taken, the engine's raw data directory is NOT also captured (a
// live-file copy is frequently corrupt) — unless the user explicitly opts in.
func TestSelectMountsExcludesDumpedDBDataDir(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Store: st, Log: func(string, string, string) {}}

	insp := types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{Name: "/mysql-1"},
		Config:            &container.Config{Image: "mysql:8"},
		Mounts: []types.MountPoint{
			{Type: "volume", Name: "dbdata", Destination: "/var/lib/mysql", RW: true},
			{Type: "volume", Name: "dbconf", Destination: "/etc/mysql", RW: true},
		},
	}
	// A remembered selection that includes BOTH (so defaultSelection's size sidecar
	// isn't invoked in the test).
	e.saveMountSelection("node1", "mysql-1", []string{"/var/lib/mysql", "/etc/mysql"}, nil)

	ctx := context.Background()

	// Default run with a dump taken: the data dir is dropped, config kept.
	dests, _, _ := e.selectMounts(ctx, nil, insp, Options{NodeID: "node1"}, "id", "/var/lib/mysql")
	if contains(dests, "/var/lib/mysql") {
		t.Fatal("raw DB data dir must NOT be captured when a dump was taken (§4.1)")
	}
	if !contains(dests, "/etc/mysql") {
		t.Fatal("non-data DB volumes should still be captured")
	}

	// Explicit opt-in keeps the raw data dir (power-user override).
	dests2, _, _ := e.selectMounts(ctx, nil, insp,
		Options{NodeID: "node1", IncludeMounts: []string{"/var/lib/mysql", "/etc/mysql"}}, "id", "/var/lib/mysql")
	if !contains(dests2, "/var/lib/mysql") {
		t.Fatal("an explicit selection of the data dir should be honored")
	}

	// No dump (non-DB / fallback): nothing is excluded.
	dests3, _, _ := e.selectMounts(ctx, nil, insp, Options{NodeID: "node1"}, "id", "")
	if !contains(dests3, "/var/lib/mysql") {
		t.Fatal("without a dump, the data dir must be captured (no data loss)")
	}
}
