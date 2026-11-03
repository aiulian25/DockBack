package backup

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dockback/internal/storage"
	"dockback/internal/store"
)

const replicaWrappedKey = "wrapped-key-of-this-backup"

func replicaManifest(t *testing.T) []byte {
	t.Helper()
	manBytes, err := json.Marshal(Manifest{BackupID: "bk-replica", NodeID: "node1", TargetName: "nginx", CreatedAt: "2026-01-01T00:00:00Z", WrappedKey: replicaWrappedKey})
	if err != nil {
		t.Fatal(err)
	}
	return manBytes
}

// blockSidecar makes the destination refuse one object by holding a directory at its
// path, and returns how to stop refusing.
func blockSidecar(t *testing.T, dir, suffix string) func() {
	t.Helper()
	blocker := filepath.Join(dir, lockArchiveKey+suffix)
	if err := os.MkdirAll(blocker, 0o755); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.RemoveAll(blocker); err != nil {
			t.Fatal(err)
		}
	}
}

// fastMirrorRetries keeps a refusal that reads as transient from taking 20 seconds.
func fastMirrorRetries(t *testing.T) {
	t.Helper()
	original := mirrorRetryBackoff
	mirrorRetryBackoff = time.Millisecond
	t.Cleanup(func() { mirrorRetryBackoff = original })
}

func onlyDest(t *testing.T, locs []Location) Location {
	t.Helper()
	if len(locs) != 1 || locs[0].Kind != "dest" {
		t.Fatalf("want one destination copy, got %+v", locs)
	}
	return locs[0]
}

func storedBackup(t *testing.T, e *Engine, st *store.Store, manBytes []byte, locs []Location) *store.Backup {
	t.Helper()
	if err := st.UpsertNode(&store.Node{ID: "node1", Name: "node1", Transport: "local", Status: "online"}); err != nil {
		t.Fatal(err)
	}
	b := &store.Backup{ID: "bk-replica", NodeID: "node1", TargetName: "nginx", Status: "success", StorageKey: lockArchiveKey,
		ManifestJSON: string(manBytes), LocationsJSON: mustJSON(locs)}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// Each required object failing alone leaves a copy that is on record as not
// protection, and repairing it writes the envelope and never resends the archive.
func TestAnEnvelopeThatFailsLeavesARepairableCopy(t *testing.T) {
	cases := []struct {
		name, blocked string
		seal          string
		wantObjects   []string
	}{
		{"readable manifest", sidecarManifest, "false", []string{sidecarManifest, sidecarSignature}},
		{"signature", sidecarSignature, "false", []string{sidecarManifest, sidecarSignature}},
		{"sealed manifest", sidecarSealed, "true", []string{sidecarSealed}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, st, key, _ := lockTestEngine(t)
			ctx := context.Background()
			if _, err := e.Storage.Put(ctx, lockArchiveKey, strings.NewReader("ciphertext-bytes")); err != nil {
				t.Fatal(err)
			}
			manBytes := replicaManifest(t)
			dir := t.TempDir()
			sealDest(t, st, key, "d1", dir, c.seal)
			fastMirrorRetries(t)
			unblock := blockSidecar(t, dir, c.blocked)

			copyLocation := onlyDest(t, e.mirror(ctx, "bk-replica", lockArchiveKey, manBytes, nil, false, true, nil))
			if copyLocation.Status != LocationIncomplete || !strings.Contains(copyLocation.Detail, "envelope") {
				t.Fatalf("archive without its envelope must be incomplete, got %+v", copyLocation)
			}
			if noData(copyLocation.Status) || !needsPublishing(copyLocation.Status) {
				t.Fatal("an incomplete copy holds data, so restore and prune must see it, and it still needs publishing")
			}
			if d, _ := st.GetDestination("d1"); d.Status != "error" {
				t.Fatalf("the destination must read as failing, got %q", d.Status)
			}

			// A marker in place of the archive shows a repair did not send it again.
			const remote = "the bytes already on the destination"
			archive := filepath.Join(dir, lockArchiveKey)
			if err := os.WriteFile(archive, []byte(remote), 0o640); err != nil {
				t.Fatal(err)
			}
			unblock()
			storedBackup(t, e, st, manBytes, []Location{{Kind: "local", Name: "local", Type: "local"}, copyLocation})
			if err := e.MirrorExisting(ctx, "bk-replica", nil, true); err != nil {
				t.Fatal(err)
			}

			got, err := st.GetBackup("bk-replica")
			if err != nil {
				t.Fatal(err)
			}
			for _, l := range e.locations(got) {
				if l.Kind == "dest" && (l.Status != "" || l.Detail != "") {
					t.Fatalf("a repaired copy is an ordinary good copy, got %+v", l)
				}
			}
			if body, _ := os.ReadFile(archive); string(body) != remote {
				t.Fatalf("the repair sent the archive again: %q", body)
			}
			for _, suffix := range c.wantObjects {
				if _, err := os.Stat(filepath.Join(dir, lockArchiveKey+suffix)); err != nil {
					t.Errorf("%s missing after the repair: %v", suffix, err)
				}
			}
			if err := e.MirrorExisting(ctx, "bk-replica", nil, true); err == nil {
				t.Fatal("once repaired, there must be nothing left to mirror")
			}
		})
	}
}

// The test that matters: with the catalog gone, only the destination's own objects
// are left. They cannot be taken up before the repair and can after it.
func TestOnlyACompleteReplicaSurvivesLosingTheCatalog(t *testing.T) {
	e, st, key, _ := lockTestEngine(t)
	ctx := context.Background()
	if _, err := e.Storage.Put(ctx, lockArchiveKey, strings.NewReader("ciphertext-bytes")); err != nil {
		t.Fatal(err)
	}
	manBytes := replicaManifest(t)
	dir := t.TempDir()
	sealDest(t, st, key, "d1", dir, "false")
	fastMirrorRetries(t)
	unblock := blockSidecar(t, dir, sidecarManifest)
	be, err := storage.NewLocalFromConfig(map[string]string{"path": dir})
	if err != nil {
		t.Fatal(err)
	}
	cleanInstance := func() *Engine {
		clean, _, _, _ := lockTestEngine(t)
		clean.Key = key
		return clean
	}

	onlyDest(t, e.mirror(ctx, "bk-replica", lockArchiveKey, manBytes, nil, false, true, nil))
	if adopted, _, err := cleanInstance().AdoptFromBackend(ctx, be, "d1", "d1", "local"); err != nil || adopted != 0 {
		t.Fatalf("a copy without its envelope cannot be taken up: adopted %d, %v", adopted, err)
	}

	unblock()
	repaired := onlyDest(t, e.mirror(ctx, "bk-replica", lockArchiveKey, manBytes, nil, false, true, map[string]Location{"d1": {DestID: "d1"}}))
	if repaired.Status != "" {
		t.Fatalf("repair failed: %+v", repaired)
	}
	if adopted, _, err := cleanInstance().AdoptFromBackend(ctx, be, "d1", "d1", "local"); err != nil || adopted != 1 {
		t.Fatalf("a repaired copy must stand alone: adopted %d, %v", adopted, err)
	}
}

// droppingBackend reports success for sidecar writes and stores nothing.
type droppingBackend struct{ storage.Backend }

func (d droppingBackend) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	return 0, nil
}

func TestPublishEnvelopeDoesNotTakeAWriteOnTrust(t *testing.T) {
	e, _, _, _ := lockTestEngine(t)
	ctx := context.Background()
	if err := e.publishEnvelope(ctx, droppingBackend{e.Storage}, lockArchiveKey, replicaManifest(t), false); err == nil {
		t.Fatal("a write that did not land must not count")
	}
}

// A file lock waits for the whole publication, so the envelope can still be finished.
func TestAFilesystemLockWaitsForTheWholePublication(t *testing.T) {
	e, st, key, _ := lockTestEngine(t)
	ctx := context.Background()
	if _, err := e.Storage.Put(ctx, lockArchiveKey, strings.NewReader("ciphertext-bytes")); err != nil {
		t.Fatal(err)
	}
	manBytes := replicaManifest(t)
	dir := t.TempDir()
	lockDest(t, st, key, "d1", dir, 30)
	fastMirrorRetries(t)
	unblock := blockSidecar(t, dir, sidecarSignature)

	copyLocation := onlyDest(t, e.mirror(ctx, "bk-replica", lockArchiveKey, manBytes, nil, false, true, nil))
	if copyLocation.Status != LocationIncomplete || copyLocation.Immutable {
		t.Fatalf("nothing is locked yet, so nothing may claim to be: %+v", copyLocation)
	}
	assertWritable(t, filepath.Join(dir, lockArchiveKey), true)

	unblock()
	repaired := onlyDest(t, e.mirror(ctx, "bk-replica", lockArchiveKey, manBytes, nil, false, true, map[string]Location{"d1": copyLocation}))
	if repaired.Status != "" || !repaired.Immutable {
		t.Fatalf("a finished copy takes its lock: %+v", repaired)
	}
	for _, name := range []string{lockArchiveKey, lockArchiveKey + sidecarManifest, lockArchiveKey + sidecarSignature} {
		assertWritable(t, filepath.Join(dir, name), false)
	}
}

// A copy found by a scrub may already be locked. Its archive stays as it is and keeps
// the expiry it has, because the lock did not restart.
func TestRepairOfALockedArchiveLeavesItAlone(t *testing.T) {
	e, st, key, _ := lockTestEngine(t)
	ctx := context.Background()
	manBytes := replicaManifest(t)
	dir := t.TempDir()
	lockDest(t, st, key, "d1", dir, 30)
	archive := filepath.Join(dir, lockArchiveKey)
	if err := os.MkdirAll(filepath.Dir(archive), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, []byte("locked bytes"), 0o440); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Storage.Put(ctx, lockArchiveKey, strings.NewReader("different local bytes")); err != nil {
		t.Fatal(err)
	}
	lockedUntil := time.Now().Add(10 * 24 * time.Hour).Unix()
	prior := Location{DestID: "d1", Status: LocationIncomplete, Immutable: true, LockUntil: lockedUntil}

	repaired := onlyDest(t, e.mirror(ctx, "bk-replica", lockArchiveKey, manBytes, nil, false, true, map[string]Location{"d1": prior}))
	if repaired.Status != "" || !repaired.Immutable || repaired.LockUntil != lockedUntil {
		t.Fatalf("repair must keep the archive's lock: %+v", repaired)
	}
	if body, _ := os.ReadFile(archive); string(body) != "locked bytes" {
		t.Fatalf("a locked archive was overwritten: %q", body)
	}
	assertWritable(t, filepath.Join(dir, lockArchiveKey+sidecarManifest), false)
}

func TestARepairFindingNoArchiveUploadsItAgain(t *testing.T) {
	e, st, key, _ := lockTestEngine(t)
	ctx := context.Background()
	if _, err := e.Storage.Put(ctx, lockArchiveKey, strings.NewReader("ciphertext-bytes")); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	sealDest(t, st, key, "d1", dir, "false")
	repaired := onlyDest(t, e.mirror(ctx, "bk-replica", lockArchiveKey, replicaManifest(t), nil, false, true, map[string]Location{"d1": {DestID: "d1", Status: LocationIncomplete}}))
	if repaired.Status != "" {
		t.Fatalf("repair failed: %+v", repaired)
	}
	if body, _ := os.ReadFile(filepath.Join(dir, lockArchiveKey)); string(body) != "ciphertext-bytes" {
		t.Fatalf("the archive was not uploaded: %q", body)
	}
}

// A scrub catches what an older version recorded as good: this is how existing rows
// get found, and the status it leaves is the one a repair acts on.
func TestScrubFindsAnUnusableEnvelopeOnAReplica(t *testing.T) {
	stale, _ := json.Marshal(Manifest{BackupID: "bk-replica", WrappedKey: "a key from before a rotation"})
	cases := []struct {
		name           string
		manifest       func(t *testing.T) []byte
		sidecar        []byte
		wantProblem    string
		wantIncomplete bool
	}{
		{name: "missing", manifest: replicaManifest, wantProblem: "missing", wantIncomplete: true},
		{name: "another wrapped key", manifest: replicaManifest, sidecar: stale, wantProblem: "wrapped key", wantIncomplete: true},
		{name: "matching", manifest: replicaManifest, sidecar: replicaManifest(t)},
		{name: "legacy archive needs none", manifest: func(t *testing.T) []byte {
			legacy, _ := json.Marshal(Manifest{BackupID: "bk-replica"})
			return legacy
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, st, key, _ := lockTestEngine(t)
			ctx := context.Background()
			dir := t.TempDir()
			sealDest(t, st, key, "d1", dir, "false")
			be, err := storage.NewLocalFromConfig(map[string]string{"path": dir})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := be.Put(ctx, lockArchiveKey, strings.NewReader("not a real archive")); err != nil {
				t.Fatal(err)
			}
			if c.sidecar != nil {
				if err := e.writeManifestSidecarSealed(ctx, be, lockArchiveKey, c.sidecar, false); err != nil {
					t.Fatal(err)
				}
			}
			b := storedBackup(t, e, st, c.manifest(t), []Location{{Kind: "local", Name: "local", Type: "local"}, {Kind: "dest", DestID: "d1", Name: "d1", Type: "local"}})

			rep := e.Scrub(ctx, b, "d1")
			var problem string
			for _, check := range rep.Checks {
				if check.Name == replicaEnvelopeCheck && !check.OK {
					problem = check.Info
				}
			}
			if c.wantProblem == "" && problem != "" {
				t.Fatalf("no problem expected, got %q", problem)
			}
			if !strings.Contains(problem, c.wantProblem) {
				t.Fatalf("problem = %q, want it to mention %q", problem, c.wantProblem)
			}
			for _, l := range e.locations(b) {
				if l.Kind == "dest" && (l.Status == LocationIncomplete) != c.wantIncomplete {
					t.Fatalf("status = %q, want incomplete: %v", l.Status, c.wantIncomplete)
				}
			}
		})
	}
}

func TestLocalScrubDoesNotAskForAReplicaEnvelope(t *testing.T) {
	e, st, _, _ := lockTestEngine(t)
	ctx := context.Background()
	if _, err := e.Storage.Put(ctx, lockArchiveKey, strings.NewReader("not a real archive")); err != nil {
		t.Fatal(err)
	}
	b := storedBackup(t, e, st, replicaManifest(t), []Location{{Kind: "local", Name: "local", Type: "local"}})
	for _, check := range e.Scrub(ctx, b, "local").Checks {
		if check.Name == replicaEnvelopeCheck {
			t.Fatalf("this check is about replicas: %+v", check)
		}
	}
}

func assertWritable(t *testing.T, path string, want bool) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm()&0o200 != 0; got != want {
		t.Errorf("%s writable = %v, want %v", filepath.Base(path), got, want)
	}
}
