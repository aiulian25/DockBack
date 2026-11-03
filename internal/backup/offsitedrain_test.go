package backup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dockback/internal/crypto"
	"dockback/internal/store"
)

const (
	drainParentKey = "node1/web/2026-01-01_00-00-00_parent.dback"
	drainDeltaKey  = "node1/web/2026-01-02_00-00-00_delta.dback"
)

// A window that cannot include now, in server-local time as the engine reads it.
func closedWindow() (start, end string) {
	now := time.Now()
	return now.Add(2 * time.Hour).Format("15:04"), now.Add(3 * time.Hour).Format("15:04")
}

func windowDest(t *testing.T, st *store.Store, key []byte, id, dir, start, end string) {
	t.Helper()
	cfg, _ := json.Marshal(map[string]string{"path": dir, "upload_window_start": start, "upload_window_end": end})
	enc, err := crypto.SealString(string(cfg), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDestination(&store.Destination{ID: id, Name: id, Type: "local", Enabled: true, ConfigEnc: enc}); err != nil {
		t.Fatal(err)
	}
}

type drainGeneration struct {
	id, parent, archiveKey string
	createdAt              int64
	locations              []Location
}

func addGeneration(t *testing.T, e *Engine, st *store.Store, g drainGeneration) {
	t.Helper()
	if err := st.UpsertNode(&store.Node{ID: "node1", Name: "node1", Transport: "local", Status: "online"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Storage.Put(context.Background(), g.archiveKey, strings.NewReader("ciphertext of "+g.id)); err != nil {
		t.Fatal(err)
	}
	manBytes, _ := json.Marshal(Manifest{BackupID: g.id, NodeID: "node1", TargetName: "nginx", Parent: g.parent, Incremental: g.parent != "", WrappedKey: replicaWrappedKey})
	b := &store.Backup{ID: g.id, NodeID: "node1", TargetName: "nginx", Status: "success", StorageKey: g.archiveKey, ManifestJSON: string(manBytes),
		LocationsJSON: mustJSON(g.locations), CreatedAt: g.createdAt}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
}

func deferredOn(destID string) []Location {
	return []Location{{Kind: "local", Name: "local", Type: "local"}, {Kind: "dest", DestID: destID, Name: destID, Type: "local", Status: "deferred"}}
}

func copyOn(t *testing.T, e *Engine, st *store.Store, backupID, destID string) Location {
	t.Helper()
	b, err := st.GetBackup(backupID)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range e.locations(b) {
		if l.Kind == "dest" && l.DestID == destID {
			return l
		}
	}
	t.Fatalf("%s has no entry for %s: %s", backupID, destID, b.LocationsJSON)
	return Location{}
}

func onDisk(dir, key string) bool {
	_, err := os.Stat(filepath.Join(dir, key))
	return err == nil
}

// The finding: nothing but a new backup or a person ever sent a deferred copy. Here
// the window opens with neither, and the copy goes exactly once, even across a
// restart, which starts a new Engine over the same catalog.
func TestADeferredCopyGoesWhenItsWindowOpensAndOnlyOnce(t *testing.T) {
	e, st, key, _ := lockTestEngine(t)
	ctx := context.Background()
	dir := t.TempDir()
	start, end := closedWindow()
	windowDest(t, st, key, "d1", dir, start, end)
	addGeneration(t, e, st, drainGeneration{id: "bk1", archiveKey: lockArchiveKey, locations: deferredOn("d1")})

	if ran := e.DrainOffsite(ctx, time.Now()); ran != 0 || onDisk(dir, lockArchiveKey) {
		t.Fatalf("a closed window must hold the copy back: ran %d", ran)
	}
	if l := copyOn(t, e, st, "bk1", "d1"); l.Status != "deferred" || l.Attempts != 0 {
		t.Fatalf("waiting for a window is not a failed attempt: %+v", l)
	}

	if err := st.DeleteDestination("d1"); err != nil {
		t.Fatal(err)
	}
	windowDest(t, st, key, "d1", dir, "", "")
	if ran := e.DrainOffsite(ctx, time.Now()); ran != 1 {
		t.Fatalf("the open window must send it with no new backup: ran %d", ran)
	}
	if l := copyOn(t, e, st, "bk1", "d1"); l.Status != "" || l.Detail != "" {
		t.Fatalf("it should now be an ordinary good copy: %+v", l)
	}
	const remote = "the bytes already on the destination"
	if err := os.WriteFile(filepath.Join(dir, lockArchiveKey), []byte(remote), 0o640); err != nil {
		t.Fatal(err)
	}

	restarted := &Engine{Store: st, Storage: e.Storage, Key: key, Log: func(string, string, string) {}}
	if ran := restarted.DrainOffsite(ctx, time.Now()); ran != 0 {
		t.Fatalf("a finished copy must not go again after a restart: ran %d", ran)
	}
	if body, _ := os.ReadFile(filepath.Join(dir, lockArchiveKey)); string(body) != remote {
		t.Fatalf("the archive was sent twice: %q", body)
	}
}

// An interrupted upload records nothing, so the copy is still pending for the next
// process: that is what restart recovery consists of.
func TestACanceledDrainLeavesTheCopyPending(t *testing.T) {
	e, st, key, _ := lockTestEngine(t)
	dir := t.TempDir()
	sealDest(t, st, key, "d1", dir, "false")
	addGeneration(t, e, st, drainGeneration{id: "bk1", archiveKey: lockArchiveKey, locations: deferredOn("d1")})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ran := e.DrainOffsite(ctx, time.Now()); ran != 0 {
		t.Fatalf("a stopping process claims nothing: ran %d", ran)
	}
	if l := copyOn(t, e, st, "bk1", "d1"); l.Status != "deferred" {
		t.Fatalf("still pending: %+v", l)
	}
	if ran := e.DrainOffsite(context.Background(), time.Now()); ran != 1 {
		t.Fatalf("the next process sends it: ran %d", ran)
	}
}

func TestAFailingCopyBacksOffAndRecovers(t *testing.T) {
	e, st, key, _ := lockTestEngine(t)
	ctx := context.Background()
	fastMirrorRetries(t)
	dir := t.TempDir()
	sealDest(t, st, key, "d1", dir, "false")
	addGeneration(t, e, st, drainGeneration{id: "bk-replica", archiveKey: lockArchiveKey, locations: deferredOn("d1")})
	unblock := blockSidecar(t, dir, sidecarManifest)

	now := time.Now()
	e.DrainOffsite(ctx, now)
	first := copyOn(t, e, st, "bk-replica", "d1")
	if first.Status != LocationIncomplete || first.Attempts != 1 || first.NextAttemptAt <= now.Unix() {
		t.Fatalf("a failure is counted and waits: %+v", first)
	}
	if ran := e.DrainOffsite(ctx, now); ran != 0 {
		t.Fatalf("not due yet: ran %d", ran)
	}
	later := time.Unix(first.NextAttemptAt, 0)
	if ran := e.DrainOffsite(ctx, later); ran != 1 {
		t.Fatalf("due now: ran %d", ran)
	}
	if second := copyOn(t, e, st, "bk-replica", "d1"); second.Attempts != 2 {
		t.Fatalf("the count carries on: %+v", second)
	}
	unblock()
	if ran := e.DrainOffsite(ctx, later.Add(offsiteRetryMax)); ran != 1 {
		t.Fatalf("due again: ran %d", ran)
	}
	if l := copyOn(t, e, st, "bk-replica", "d1"); l.Status != "" || l.Attempts != 0 || l.NextAttemptAt != 0 {
		t.Fatalf("a good copy carries no retry state: %+v", l)
	}
}

func TestOffsiteRetryDelayIsBoundedAndAlertsOnce(t *testing.T) {
	want := map[int]time.Duration{1: 15 * time.Minute, 2: 30 * time.Minute, 4: 2 * time.Hour, 7: 16 * time.Hour, 8: 24 * time.Hour, 50: 24 * time.Hour}
	for attempts, delay := range want {
		if got := offsiteRetryDelay(attempts); got != delay {
			t.Errorf("attempt %d waits %s, want %s", attempts, got, delay)
		}
	}
	alerts := 0
	prior := []Location{}
	for range offsiteAlertAfterAttempts + 3 {
		fresh := []Location{{Kind: "dest", DestID: "d1", Status: "failed"}}
		alerts += len(stampOffsiteAttempts(fresh, prior, time.Now()))
		prior = fresh
	}
	if alerts != 1 {
		t.Fatalf("a copy that keeps failing alerts once, got %d", alerts)
	}
}

// A dead source means a visible answer, not retrying for ever.
func TestAMissingSourceStopsRetryingAndSaysWhy(t *testing.T) {
	e, st, key, _ := lockTestEngine(t)
	ctx := context.Background()
	dir := t.TempDir()
	sealDest(t, st, key, "d1", dir, "false")
	addGeneration(t, e, st, drainGeneration{id: "bk1", archiveKey: lockArchiveKey, locations: deferredOn("d1")})
	if err := e.Storage.Delete(ctx, lockArchiveKey); err != nil {
		t.Fatal(err)
	}
	e.DrainOffsite(ctx, time.Now())
	l := copyOn(t, e, st, "bk1", "d1")
	if !l.GaveUp || !strings.Contains(l.Detail, "local archive") {
		t.Fatalf("the copy must say why it stopped: %+v", l)
	}
	if ran := e.DrainOffsite(ctx, time.Now().Add(offsiteRetryMax)); ran != 0 {
		t.Fatalf("it must not keep trying: ran %d", ran)
	}
	if _, err := e.Storage.Put(ctx, lockArchiveKey, strings.NewReader("ciphertext")); err != nil {
		t.Fatal(err)
	}
	if err := e.MirrorExisting(ctx, "bk1", []string{"d1"}, true); err != nil {
		t.Fatal(err)
	}
	if l := copyOn(t, e, st, "bk1", "d1"); l.Status != "" || l.GaveUp {
		t.Fatalf("a deliberate send can still recover it: %+v", l)
	}
}

func TestADeltaDoesNotReadAsGoodBeforeItsParent(t *testing.T) {
	cases := []struct {
		name            string
		parentLocations []Location
	}{
		{"parent deferred", deferredOn("d1")},
		{"parent never sent there", []Location{{Kind: "local", Name: "local", Type: "local"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, st, key, _ := lockTestEngine(t)
			ctx := context.Background()
			dir := t.TempDir()
			sealDest(t, st, key, "d1", dir, "false")
			addGeneration(t, e, st, drainGeneration{id: "parent", archiveKey: drainParentKey, createdAt: 1, locations: c.parentLocations})
			addGeneration(t, e, st, drainGeneration{id: "delta", parent: "parent", archiveKey: drainDeltaKey, createdAt: 2, locations: deferredOn("d1")})

			e.DrainOffsite(ctx, time.Now())
			for _, id := range []string{"parent", "delta"} {
				if l := copyOn(t, e, st, id, "d1"); l.Status != "" {
					t.Errorf("%s should be good: %+v", id, l)
				}
			}
			if !onDisk(dir, drainParentKey) || !onDisk(dir, drainDeltaKey) {
				t.Fatal("both generations must be on the destination")
			}
		})
	}
}

func TestADeltaWaitsWhileItsParentCannotGetThere(t *testing.T) {
	e, st, key, _ := lockTestEngine(t)
	ctx := context.Background()
	fastMirrorRetries(t)
	dir := t.TempDir()
	sealDest(t, st, key, "d1", dir, "false")
	if err := os.MkdirAll(filepath.Join(dir, drainParentKey+sidecarManifest), 0o755); err != nil {
		t.Fatal(err)
	}
	addGeneration(t, e, st, drainGeneration{id: "parent", archiveKey: drainParentKey, createdAt: 1, locations: deferredOn("d1")})
	addGeneration(t, e, st, drainGeneration{id: "delta", parent: "parent", archiveKey: drainDeltaKey, createdAt: 2, locations: deferredOn("d1")})

	e.DrainOffsite(ctx, time.Now())
	if l := copyOn(t, e, st, "parent", "d1"); l.Status != LocationIncomplete {
		t.Fatalf("parent: %+v", l)
	}
	if l := copyOn(t, e, st, "delta", "d1"); l.Status != "deferred" || l.Attempts != 0 {
		t.Fatalf("a delta waiting for its parent has not failed: %+v", l)
	}
	if onDisk(dir, drainDeltaKey) {
		t.Fatal("the delta must not be sent before its parent is complete")
	}

	// The same rule holds at capture and for a deliberate send, since it lives in mirror.
	manBytes, _ := json.Marshal(Manifest{BackupID: "delta", Parent: "parent", Incremental: true})
	l := onlyDest(t, e.mirror(ctx, "delta", drainDeltaKey, manBytes, nil, false, true, nil))
	if l.Status != "deferred" || !strings.Contains(l.Detail, "parent") {
		t.Fatalf("mirror itself must refuse to call this good: %+v", l)
	}
}

// The drain, a manual send and a backfill go through one claim.
func TestAMirrorAlreadyRunningIsNotAFailure(t *testing.T) {
	e, st, key, _ := lockTestEngine(t)
	ctx := context.Background()
	dir := t.TempDir()
	sealDest(t, st, key, "d1", dir, "false")
	addGeneration(t, e, st, drainGeneration{id: "bk1", archiveKey: lockArchiveKey, locations: deferredOn("d1")})
	e.mirrorClaims.Store("bk1", struct{}{})

	if err := e.MirrorExisting(ctx, "bk1", []string{"d1"}, true); !errors.Is(err, ErrMirrorBusy) {
		t.Fatalf("got %v", err)
	}
	e.DrainOffsite(ctx, time.Now())
	if l := copyOn(t, e, st, "bk1", "d1"); l.Status != "deferred" || l.Attempts != 0 || onDisk(dir, lockArchiveKey) {
		t.Fatalf("the running mirror owns this copy: %+v", l)
	}
	e.mirrorClaims.Delete("bk1")
	e.DrainOffsite(ctx, time.Now())
	if !onDisk(dir, lockArchiveKey) {
		t.Fatal("once released, it goes")
	}
}

// A backfill honours the window, as its help says. Only a deliberate send overrides it.
func TestOnlyADeliberateSendOverridesTheWindow(t *testing.T) {
	e, st, key, _ := lockTestEngine(t)
	ctx := context.Background()
	dir := t.TempDir()
	start, end := closedWindow()
	windowDest(t, st, key, "d1", dir, start, end)
	addGeneration(t, e, st, drainGeneration{id: "bk1", archiveKey: lockArchiveKey, locations: []Location{{Kind: "local", Name: "local", Type: "local"}}})

	if err := e.MirrorExisting(ctx, "bk1", []string{"d1"}, false); err != nil {
		t.Fatal(err)
	}
	if l := copyOn(t, e, st, "bk1", "d1"); l.Status != "deferred" || onDisk(dir, lockArchiveKey) {
		t.Fatalf("the backfill path must leave it deferred: %+v", l)
	}
	if err := e.MirrorExisting(ctx, "bk1", []string{"d1"}, true); err != nil {
		t.Fatal(err)
	}
	if !onDisk(dir, lockArchiveKey) {
		t.Fatal("a deliberate send goes regardless of the window")
	}
}

func TestTheDrainLeavesAloneWhatItShould(t *testing.T) {
	cases := map[string]func(*testing.T, *store.Store, *store.Backup){
		"a source that failed verification": func(t *testing.T, st *store.Store, b *store.Backup) {
			b.Verified = "failed"
			if err := st.UpdateBackup(b); err != nil {
				t.Fatal(err)
			}
		},
		"a disabled destination": func(t *testing.T, st *store.Store, _ *store.Backup) {
			d, err := st.GetDestination("d1")
			if err != nil {
				t.Fatal(err)
			}
			if err := st.DeleteDestination("d1"); err != nil {
				t.Fatal(err)
			}
			d.Enabled = false
			if err := st.CreateDestination(d); err != nil {
				t.Fatal(err)
			}
		},
		"a deleted destination": func(t *testing.T, st *store.Store, _ *store.Backup) {
			if err := st.DeleteDestination("d1"); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			e, st, key, _ := lockTestEngine(t)
			dir := t.TempDir()
			sealDest(t, st, key, "d1", dir, "false")
			addGeneration(t, e, st, drainGeneration{id: "bk1", archiveKey: lockArchiveKey, locations: deferredOn("d1")})
			b, err := st.GetBackup("bk1")
			if err != nil {
				t.Fatal(err)
			}
			arrange(t, st, b)
			if ran := e.DrainOffsite(context.Background(), time.Now()); ran != 0 {
				t.Fatalf("ran %d", ran)
			}
		})
	}
}
