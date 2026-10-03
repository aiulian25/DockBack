package backup

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"dockback/internal/storage"
	"dockback/internal/store"
)

// F77 layout migration, phase ordering: copy → repoint → delete. The delete in
// phase 3 removes the OLD sidecars, so anything that makes phase 1 wrongly
// believe a sidecar was handled destroys it. A signature cannot be regenerated:
// verification then silently skips the signature check and "Scan & adopt"
// has nothing to re-import.

var errDestinationBlip = errors.New("dial tcp: connection reset by peer")

// statFailer answers Stat for one key with a transient failure — a rotated
// credential, a 502 from the proxy in front of the destination, a dropped
// connection. Every other operation passes through to the real backend.
type statFailer struct {
	storage.Backend
	failKey string
}

func (s statFailer) Stat(ctx context.Context, key string) (int64, bool, error) {
	if key == s.failKey {
		return 0, false, errDestinationBlip
	}
	return s.Backend.Stat(ctx, key)
}

func migrateFixture(t *testing.T, sidecars ...string) (*Engine, storage.Backend, *store.Backup, string) {
	t.Helper()
	dir := t.TempDir()
	local, err := storage.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	const flatKey = "wordpress-old-flat.dback"
	ctx := context.Background()
	if _, err := local.Put(ctx, flatKey, strings.NewReader("archive-bytes")); err != nil {
		t.Fatal(err)
	}
	for _, sfx := range sidecars {
		if _, err := local.Put(ctx, flatKey+sfx, strings.NewReader("sidecar-bytes")); err != nil {
			t.Fatal(err)
		}
	}
	b := &store.Backup{ID: "m1", NodeID: "n1", Stack: "blog", TargetName: "wordpress", Status: "success", CreatedAt: 1751725822}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	b.StorageKey = flatKey
	b.LocationsJSON = `[{"kind":"local","name":"local","type":"local"}]`
	if err := st.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Store: st, Storage: local, Log: func(string, string, string) {}}
	return e, local, b, flatKey
}

// TestMigrateAbortsOnStatError: a sidecar whose existence could not be checked
// must stop the migration before the catalog row is repointed, leaving every old
// object in place for a retry.
func TestMigrateAbortsOnStatError(t *testing.T) {
	const sig = ".manifest.json.sig"
	e, local, b, flatKey := migrateFixture(t, ".manifest.json", sig)
	ctx := context.Background()
	e.Storage = statFailer{Backend: local, failKey: flatKey + sig}

	const newKey = "curio/blog/wordpress/2025-07-05_14-30-22.dback"
	moved, worm, err := e.MigrateBackupLayout(ctx, b, newKey)
	if err == nil {
		t.Fatal("a sidecar that could not be checked must abort the migration, not be skipped")
	}
	if moved || worm {
		t.Errorf("nothing was moved: moved=%v worm=%v", moved, worm)
	}
	if !errors.Is(err, errDestinationBlip) {
		t.Errorf("the real cause must survive to the operator: %v", err)
	}

	// The signature is still where it was. This is the whole point: it cannot be
	// regenerated, and phase 3 would have deleted it.
	for _, sfx := range []string{"", ".manifest.json", sig} {
		if _, ok, serr := local.Stat(ctx, flatKey+sfx); !ok || serr != nil {
			t.Errorf("old object %q must be untouched, got ok=%v err=%v", flatKey+sfx, ok, serr)
		}
	}
	// The row still points at the old key, so nothing has been orphaned…
	if b.StorageKey != flatKey {
		t.Errorf("the catalog row was repointed despite the failure: %q", b.StorageKey)
	}
	row, rerr := e.Store.GetBackup("m1")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if row.StorageKey != flatKey {
		t.Errorf("the stored row was repointed despite the failure: %q", row.StorageKey)
	}
	// …and the half-written new copies were rolled back.
	if _, ok, _ := local.Stat(ctx, newKey); ok {
		t.Error("the partially copied archive must be rolled back, not left as a duplicate")
	}
}

// The other half of the contract: a sidecar that genuinely does not exist is
// still fine. A sealed-manifest destination has no .json and a readable one has
// no .enc, so a missing one must never be an error.
func TestMigrateSkipsAbsentSidecars(t *testing.T) {
	e, local, b, flatKey := migrateFixture(t, ".manifest.json") // no .sig, no .enc
	ctx := context.Background()

	const newKey = "curio/blog/wordpress/2025-07-05_14-30-22.dback"
	moved, worm, err := e.MigrateBackupLayout(ctx, b, newKey)
	if err != nil || !moved || worm {
		t.Fatalf("a backup with only some sidecars must migrate: moved=%v worm=%v err=%v", moved, worm, err)
	}
	for _, k := range []string{newKey, newKey + ".manifest.json"} {
		if _, ok, _ := local.Stat(ctx, k); !ok {
			t.Errorf("%q should have moved to the canonical key", k)
		}
	}
	if _, ok, _ := local.Stat(ctx, newKey+".manifest.json.enc"); ok {
		t.Error("a sidecar that never existed must not be invented at the new key")
	}
	if _, ok, _ := local.Stat(ctx, flatKey); ok {
		t.Error("the old archive should have been removed by phase 3")
	}
}
