package backup

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"dockback/internal/crypto"
	"dockback/internal/storage"
	"dockback/internal/store"
)

// Filesystem retention lock, end to end (F97). "Immutable" used to be an S3-only
// badge, so the NAS or SSH box most homelabs actually use had no protection at
// all — and prune would remove a copy there on schedule regardless.

// lockDest creates an enabled local destination whose sealed config carries a
// retention lock of days (0 = no lock configured).
func lockDest(t *testing.T, st *store.Store, key []byte, id, dir string, days int) {
	t.Helper()
	cfg := map[string]string{"path": dir}
	if days > 0 {
		cfg["retention_lock_days"] = strconv.Itoa(days)
	}
	js, _ := json.Marshal(cfg)
	enc, err := crypto.SealString(string(js), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDestination(&store.Destination{ID: id, Name: id, Type: "local", Enabled: true, ConfigEnc: enc}); err != nil {
		t.Fatal(err)
	}
}

func lockTestEngine(t *testing.T) (*Engine, *store.Store, []byte, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	localDir := t.TempDir()
	be, err := storage.NewLocal(localDir)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	return &Engine{Store: st, Storage: be, Key: key, Log: func(string, string, string) {}}, st, key, localDir
}

const lockArchiveKey = "node1/web/2026-01-01_00-00-00_abc.dback"

// THE acceptance case: mirroring to a local destination with a lock configured
// records a live lock on that copy AND actually applies it on disk. Recording a
// lock without applying one would make prune protect a copy nothing guards.
func TestMirrorAppliesAndRecordsRetentionLock(t *testing.T) {
	e, st, key, _ := lockTestEngine(t)
	ctx := context.Background()
	if _, err := e.Storage.Put(ctx, lockArchiveKey, strings.NewReader("ciphertext-bytes")); err != nil {
		t.Fatal(err)
	}
	manBytes, _ := json.Marshal(Manifest{BackupID: "bk-lock", NodeID: "node1", TargetName: "nginx"})

	lockedDir, plainDir := t.TempDir(), t.TempDir()
	lockDest(t, st, key, "d-locked", lockedDir, 30)
	lockDest(t, st, key, "d-plain", plainDir, 0)

	locs := e.mirror(ctx, "run1", lockArchiveKey, manBytes, nil, false, true)
	if len(locs) != 2 {
		t.Fatalf("want 2 locations, got %+v", locs)
	}
	byID := map[string]Location{}
	for _, l := range locs {
		if l.Status == "failed" {
			t.Fatalf("mirror to %s failed: %s", l.Name, l.Detail)
		}
		byID[l.DestID] = l
	}

	locked := byID["d-locked"]
	if !locked.Immutable {
		t.Fatal("a lock-configured destination must record its copy as immutable")
	}
	want := time.Now().Add(30 * 24 * time.Hour).Unix()
	if locked.LockUntil < want-120 || locked.LockUntil > want+120 {
		t.Fatalf("lock_until = %d, want ~%d (30 days out)", locked.LockUntil, want)
	}

	// A destination with no lock configured must be untouched — this feature is
	// strictly opt-in and must not change any existing destination's behaviour.
	if plain := byID["d-plain"]; plain.Immutable || plain.LockUntil != 0 {
		t.Fatalf("an unconfigured destination must not be marked immutable: %+v", plain)
	}
	f, err := os.OpenFile(filepath.Join(plainDir, lockArchiveKey), os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("an unlocked destination's copy must stay writable: %v", err)
	}
	f.Close()

	// The lock is REAL on disk, not just bookkeeping — and it covers the manifest
	// sidecar too, so the map to a protected archive cannot be rewritten.
	for _, name := range []string{lockArchiveKey, lockArchiveKey + ".manifest.json"} {
		p := filepath.Join(lockedDir, name)
		if _, serr := os.Stat(p); serr != nil {
			t.Fatalf("expected %s to exist: %v", name, serr)
		}
		if f, oerr := os.OpenFile(p, os.O_WRONLY, 0); oerr == nil {
			f.Close()
			t.Fatalf("%s must not be writable after the lock", name)
		}
	}
}

// A live lock survives prune; an expired one does not. A copy that can never be
// removed is a disk that eventually fills, so the expiry has to actually work.
func TestDeleteArtifactsHonoursLockUntilThenReleases(t *testing.T) {
	e, st, key, _ := lockTestEngine(t)
	ctx := context.Background()
	destDir := t.TempDir()
	lockDest(t, st, key, "d-locked", destDir, 30)

	be, err := storage.NewLocalFromConfig(map[string]string{"path": destDir, "retention_lock_days": "30"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.Put(ctx, lockArchiveKey, strings.NewReader("ciphertext")); err != nil {
		t.Fatal(err)
	}
	if err := be.LockUntil(ctx, lockArchiveKey, time.Now().Add(30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	b := &store.Backup{ID: "bk1", NodeID: "node1", TargetName: "nginx", Status: "success", StorageKey: lockArchiveKey}
	live := []Location{{Kind: "dest", DestID: "d-locked", Name: "d-locked", Type: "local",
		Immutable: true, LockUntil: time.Now().Add(30 * 24 * time.Hour).Unix()}}
	b.LocationsJSON = mustJSON(live)

	// Prune while the lock is live: the copy stays, and hasLiveImmutable retains
	// the whole backup — the existing WORM behaviour, now reachable for a NAS.
	e.DeleteArtifacts(ctx, b)
	if _, ok, _ := be.Stat(ctx, lockArchiveKey); !ok {
		t.Fatal("a copy under a live retention lock must survive prune")
	}
	if !e.hasLiveImmutable(b) {
		t.Fatal("a live filesystem lock must retain the backup, exactly as S3 WORM does")
	}

	// Once the recorded lock has expired, the same prune removes it.
	expired := []Location{{Kind: "dest", DestID: "d-locked", Name: "d-locked", Type: "local",
		Immutable: true, LockUntil: time.Now().Add(-time.Hour).Unix()}}
	b.LocationsJSON = mustJSON(expired)
	if e.hasLiveImmutable(b) {
		t.Fatal("an expired lock must not retain the backup")
	}
	e.DeleteArtifacts(ctx, b)
	if _, ok, _ := be.Stat(ctx, lockArchiveKey); ok {
		t.Fatal("an expired lock must not block prune — the disk would fill forever")
	}
}

// applyRetentionLock fails only on the ARCHIVE. A sidecar that a destination's
// sealing preference never wrote is expected to be absent, and must not turn a
// good, genuinely-locked upload into an unprotected one.
func TestApplyRetentionLockToleratesMissingSidecars(t *testing.T) {
	e, _, _, _ := lockTestEngine(t)
	ctx := context.Background()
	dir := t.TempDir()
	be, err := storage.NewLocalFromConfig(map[string]string{"path": dir, "retention_lock_days": "5"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.Put(ctx, lockArchiveKey, strings.NewReader("only the archive")); err != nil {
		t.Fatal(err)
	}
	if err := e.applyRetentionLock(ctx, be, lockArchiveKey, time.Now().Add(5*24*time.Hour)); err != nil {
		t.Fatalf("missing sidecars must not fail the lock: %v", err)
	}
	if f, oerr := os.OpenFile(filepath.Join(dir, lockArchiveKey), os.O_WRONLY, 0); oerr == nil {
		f.Close()
		t.Fatal("the archive itself must be locked")
	}
	// A missing archive IS an error — that is the object the lock exists for.
	if err := e.applyRetentionLock(ctx, be, "node1/web/absent.dback", time.Now()); err == nil {
		t.Fatal("a missing archive must fail the lock")
	}
}
