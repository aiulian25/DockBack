package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"dockback/internal/backup"
	"dockback/internal/storage"
	"dockback/internal/store"
)

// F63: manual deletes must never orphan an incremental chain.

func chainTestServer(t *testing.T) *Server {
	t.Helper()
	st := testStore(t)
	be, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Server{store: st, engine: &backup.Engine{Store: st, Storage: be, Log: func(string, string, string) {}}}
}

// mkChainRow persists a backup row whose manifest records a chain parent.
func mkChainRow(t *testing.T, st *store.Store, id, parent string, depth int, created int64) {
	t.Helper()
	man, _ := json.Marshal(backup.Manifest{BackupID: id, TargetName: "app", Parent: parent, Incremental: parent != "", ChainDepth: depth})
	b := &store.Backup{ID: id, NodeID: "n1", TargetName: "app", Status: "success", CreatedAt: created, ManifestJSON: string(man), StorageKey: "k/" + id}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteChainGuardAndCascade(t *testing.T) {
	s := chainTestServer(t)
	// full ← d1 ← d2, plus an unrelated chainless row.
	mkChainRow(t, s.store, "full00000001", "", 0, 1000)
	mkChainRow(t, s.store, "delta0000001", "full00000001", 1, 2000)
	mkChainRow(t, s.store, "delta0000002", "delta0000001", 2, 3000)
	mkChainRow(t, s.store, "solo00000001", "", 0, 4000)
	ctx := context.Background()

	// Deleting the baseline with live deltas → typed dependents error, row kept.
	err := s.deleteBackupByID(ctx, "admin", "full00000001")
	var dep *errHasDependents
	if !errors.As(err, &dep) {
		t.Fatalf("baseline delete must refuse with dependents, got %v", err)
	}
	if len(dep.ids) != 2 {
		t.Fatalf("dependents = %v, want the 2 deltas", dep.ids)
	}
	if _, gerr := s.store.GetBackup("full00000001"); gerr != nil {
		t.Fatal("refused delete must leave the row intact")
	}
	// Mid-chain delete refuses too (separate var — dep still holds the
	// baseline's full dependent set for the cascade below).
	var midDep *errHasDependents
	if err := s.deleteBackupByID(ctx, "admin", "delta0000001"); !errors.As(err, &midDep) {
		t.Fatalf("mid-chain delete must refuse, got %v", err)
	}

	// A chainless backup deletes exactly like today.
	if err := s.deleteBackupByID(ctx, "admin", "solo00000001"); err != nil {
		t.Fatalf("chainless delete must succeed: %v", err)
	}

	// Cascade removes the whole subtree newest-first (leaves before ancestors).
	deleted, cerr := s.deleteChainCascade("admin", "full00000001", dep.ids)
	if cerr != nil {
		t.Fatalf("cascade: %v (deleted=%v)", cerr, deleted)
	}
	order := map[string]int{}
	for i, id := range deleted {
		order[id] = i
	}
	if !(order["delta0000002"] < order["delta0000001"] && order["delta0000001"] < order["full00000001"]) {
		t.Fatalf("cascade order must be leaves-first, got %v", deleted)
	}
	for _, id := range []string{"full00000001", "delta0000001", "delta0000002"} {
		if _, gerr := s.store.GetBackup(id); gerr == nil {
			t.Fatalf("%s should be gone after cascade", id)
		}
	}
}

func TestDeleteChainBulk(t *testing.T) {
	s := chainTestServer(t)
	mkChainRow(t, s.store, "bfull0000001", "", 0, 1000)
	mkChainRow(t, s.store, "bdelta000001", "bfull0000001", 1, 2000)
	ctx := context.Background()

	// Bulk {baseline, delta} in ANY order succeeds via the pass loop: the delta
	// deletes first, unblocking the baseline on the next pass.
	remaining := []string{"bfull0000001", "bdelta000001"} // worst-case order
	deletedCount := 0
	for pass := 0; pass < 3 && len(remaining) > 0; pass++ {
		var next []string
		for _, id := range remaining {
			err := s.deleteBackupByID(ctx, "admin", id)
			if err == nil {
				deletedCount++
				continue
			}
			var dep *errHasDependents
			if errors.As(err, &dep) {
				next = append(next, id)
				continue
			}
			t.Fatalf("unexpected error for %s: %v", id, err)
		}
		remaining = next
	}
	if deletedCount != 2 || len(remaining) != 0 {
		t.Fatalf("bulk pass-loop should delete both, got deleted=%d remaining=%v", deletedCount, remaining)
	}

	// A baseline whose delta is NOT in the request stays refused.
	mkChainRow(t, s.store, "cfull0000001", "", 0, 5000)
	mkChainRow(t, s.store, "cdelta000001", "cfull0000001", 1, 6000)
	if err := s.deleteBackupByID(ctx, "admin", "cfull0000001"); err == nil {
		t.Fatal("baseline with an outside-request delta must stay refused")
	}
	if _, gerr := s.store.GetBackup("cdelta000001"); gerr != nil {
		t.Fatal("the dependent delta must survive")
	}
}
