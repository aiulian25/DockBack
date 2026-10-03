package api

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dockback/internal/backup"
	"dockback/internal/storage"
	"dockback/internal/store"
)

// F77 layout migration: a flat-key archive (+ sidecar) moves to the canonical
// per-stack key, the row is repointed, a re-run finds nothing, an unreachable
// location leaves everything untouched, and WORM copies are skipped.

func migrateTestServer(t *testing.T) (*Server, *store.Store, storage.Backend, string) {
	t.Helper()
	dir := t.TempDir()
	be, err := storage.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := testStore(t)
	e := &backup.Engine{Store: st, Storage: be, Log: func(string, string, string) {}}
	return &Server{store: st, engine: e}, st, be, dir
}

func plantFlat(t *testing.T, st *store.Store, be storage.Backend, id, flatKey, locations string) *store.Backup {
	t.Helper()
	ctx := context.Background()
	if _, err := be.Put(ctx, flatKey, strings.NewReader("archive-bytes")); err != nil {
		t.Fatal(err)
	}
	if _, err := be.Put(ctx, flatKey+".manifest.json", strings.NewReader(`{"version":1}`)); err != nil {
		t.Fatal(err)
	}
	man := backup.Manifest{NodeName: "curio", Stack: "blog", TargetName: "wordpress"}
	mb, _ := json.Marshal(man)
	b := &store.Backup{ID: id, NodeID: "n1", Stack: "blog", TargetName: "wordpress", Status: "success", CreatedAt: 1751725822}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	b.StorageKey = flatKey
	b.ManifestJSON = string(mb)
	b.LocationsJSON = locations
	if err := st.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMigrateLayoutMovesFlatArchive(t *testing.T) {
	s, st, be, _ := migrateTestServer(t)
	ctx := context.Background()
	b := plantFlat(t, st, be, "m1", "wordpress-old-flat.dback", `[{"kind":"local","name":"local","type":"local"}]`)

	all, _ := st.ListBackups("", 100)
	cands := migrateLayoutCandidates(all)
	if len(cands) != 1 || cands[0].b.ID != "m1" {
		t.Fatalf("candidates = %+v, want the one flat row", cands)
	}
	newKey := cands[0].newKey
	if filepath.Dir(newKey) != filepath.Join("curio", "blog", "wordpress") {
		t.Fatalf("canonical key %q not in per-stack layout", newKey)
	}

	moved, worm, err := s.engine.MigrateBackupLayout(ctx, b, newKey)
	if err != nil || !moved || worm {
		t.Fatalf("move: moved=%v worm=%v err=%v", moved, worm, err)
	}

	// Archive + sidecar at the new key; old objects gone; row repointed.
	for _, k := range []string{newKey, newKey + ".manifest.json"} {
		if _, ok, _ := be.Stat(ctx, k); !ok {
			t.Fatalf("expected %q at the canonical key", k)
		}
	}
	for _, k := range []string{"wordpress-old-flat.dback", "wordpress-old-flat.dback.manifest.json"} {
		if _, ok, _ := be.Stat(ctx, k); ok {
			t.Fatalf("old object %q must be deleted after the move", k)
		}
	}
	row, err := st.GetBackup("m1")
	if err != nil || row.StorageKey != newKey {
		t.Fatalf("row not repointed: key=%q err=%v", row.StorageKey, err)
	}

	// Re-run: nothing left to do.
	all, _ = st.ListBackups("", 100)
	if left := migrateLayoutCandidates(all); len(left) != 0 {
		t.Fatalf("re-run must find 0 candidates, got %d", len(left))
	}
}

func TestMigrateLayoutFailClosedAndWORM(t *testing.T) {
	s, st, be, _ := migrateTestServer(t)
	ctx := context.Background()

	// An unreachable destination location: move nothing, keep the old objects.
	b := plantFlat(t, st, be, "m2", "app-old-flat.dback",
		`[{"kind":"local","name":"local","type":"local"},{"kind":"dest","dest_id":"gone","name":"offsite","type":"s3"}]`)
	var man backup.Manifest
	_ = json.Unmarshal([]byte(b.ManifestJSON), &man)
	moved, worm, err := s.engine.MigrateBackupLayout(ctx, b, backup.CanonicalKey(b, &man))
	if err == nil || moved || worm {
		t.Fatalf("unreachable location must fail the move: moved=%v worm=%v err=%v", moved, worm, err)
	}
	if _, ok, _ := be.Stat(ctx, "app-old-flat.dback"); !ok {
		t.Fatal("old object must remain after a failed move")
	}
	if row, _ := st.GetBackup("m2"); row.StorageKey != "app-old-flat.dback" {
		t.Fatalf("row must keep the old key after a failed move, got %q", row.StorageKey)
	}

	// A live WORM copy skips the whole backup, untouched.
	lock := time.Now().Add(24 * time.Hour).Unix()
	bw := plantFlat(t, st, be, "m3", "db-old-flat.dback",
		`[{"kind":"local","name":"local","type":"local"},{"kind":"dest","dest_id":"d1","name":"worm","type":"s3","immutable":true,"lock_until":`+jsonInt(lock)+`}]`)
	var man3 backup.Manifest
	_ = json.Unmarshal([]byte(bw.ManifestJSON), &man3)
	moved, worm, err = s.engine.MigrateBackupLayout(ctx, bw, backup.CanonicalKey(bw, &man3))
	if err != nil || moved || !worm {
		t.Fatalf("WORM copy must skip: moved=%v worm=%v err=%v", moved, worm, err)
	}
	if _, ok, _ := be.Stat(ctx, "db-old-flat.dback"); !ok {
		t.Fatal("WORM-skipped backup must be left untouched")
	}
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }
