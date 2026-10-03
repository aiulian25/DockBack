package backup

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dockback/internal/crypto"
	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// F86's claim is that a running DockBack cannot read a write-only backup even
// while holding the master key. These tests pin that claim at the choke point
// (archiveKey) and pin the fallbacks that keep the rest of the app honest rather
// than silently broken.

func writeOnlyEngine(t *testing.T) *Engine {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i + 1)
	}
	return &Engine{Store: st, Key: master}
}

// storeBackup persists a row WITH its manifest. CreateBackup only inserts the
// identity columns; the manifest lands via UpdateBackup, exactly as a real run
// does (create on start, update on completion).
func storeBackup(t *testing.T, e *Engine, b *store.Backup) {
	t.Helper()
	if err := e.Store.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
}

// The default is unchanged: no setting, symmetric envelope, master key opens it.
func TestWriteOnlyOffKeepsSymmetricEnvelope(t *testing.T) {
	e := writeOnlyEngine(t)
	if e.WriteOnlyEnabled() {
		t.Fatal("write-only must be OFF by default")
	}
	dek, _ := crypto.NewDataKey()
	man := &Manifest{}
	if err := e.wrapDEK(dek, man); err != nil {
		t.Fatal(err)
	}
	if man.WrappedKey == "" || man.WrappedKeyPub != "" {
		t.Fatalf("symmetric mode must set WrappedKey only, got %+v", man)
	}
	got, err := e.archiveKey(man)
	if err != nil {
		t.Fatalf("symmetric archiveKey must work with no private key: %v", err)
	}
	if string(got) != string(dek) {
		t.Fatal("symmetric archiveKey must return the original DEK")
	}
}

// THE security property: armed, the master key is not enough.
func TestWriteOnlyArchiveKeyNeedsPrivateKey(t *testing.T) {
	e := writeOnlyEngine(t)
	pub, priv, _ := crypto.NewBackupKeypair()
	if err := e.Store.SetSetting(writeOnlyPubSetting, pub); err != nil {
		t.Fatal(err)
	}
	if !e.WriteOnlyEnabled() {
		t.Fatal("write-only must report enabled once a public key is set")
	}

	dek, _ := crypto.NewDataKey()
	man := &Manifest{}
	if err := e.wrapDEK(dek, man); err != nil {
		t.Fatal(err)
	}
	// There must be NO master-key-openable copy of the DEK anywhere.
	if man.WrappedKey != "" {
		t.Fatal("write-only mode must NOT also wrap the DEK to the master key")
	}
	if man.WrappedKeyPub == "" || man.BackupPubFP == "" {
		t.Fatalf("write-only mode must record the sealed key + fingerprint, got %+v", man)
	}
	if !IsWriteOnly(man) {
		t.Fatal("IsWriteOnly must recognise the manifest")
	}

	// Holding the master key, the engine still cannot open it.
	if _, err := e.archiveKey(man); err == nil {
		t.Fatal("archiveKey MUST fail without the offline private key")
	} else if err != ErrPrivateKeyRequired {
		t.Fatalf("want ErrPrivateKeyRequired, got %v", err)
	}

	// With the offline key it opens, and returns the original DEK.
	e.restorePriv.Store(&priv)
	got, err := e.archiveKey(man)
	if err != nil {
		t.Fatalf("archiveKey with the private key: %v", err)
	}
	if string(got) != string(dek) {
		t.Fatal("the recovered DEK must equal the original")
	}

	// A key from a different keypair is rejected by fingerprint, before decrypting.
	_, otherPriv, _ := crypto.NewBackupKeypair()
	e.restorePriv.Store(&otherPriv)
	if _, err := e.archiveKey(man); err == nil {
		t.Fatal("a private key from another keypair MUST fail")
	} else if !strings.Contains(err.Error(), "different keypair") {
		t.Fatalf("the error must name the cause, got %v", err)
	}
}

// A missing key must be caught by fingerprint check BEFORE a restore stops or
// overwrites anything.
func TestCheckPrivateKeyGuards(t *testing.T) {
	pub, priv, _ := crypto.NewBackupKeypair()
	dek, _ := crypto.NewDataKey()
	wrapped, _ := crypto.WrapKeyPub(dek, pub)
	man := &Manifest{WrappedKeyPub: wrapped, BackupPubFP: crypto.BackupPubFP(pub)}

	if err := CheckPrivateKey(man, ""); err != ErrPrivateKeyRequired {
		t.Fatalf("a missing key must return ErrPrivateKeyRequired, got %v", err)
	}
	if err := CheckPrivateKey(man, priv); err != nil {
		t.Fatalf("the right key must pass: %v", err)
	}
	_, other, _ := crypto.NewBackupKeypair()
	if err := CheckPrivateKey(man, other); err == nil {
		t.Fatal("a wrong keypair must be rejected")
	}
	// A symmetric backup never needs a key, even if one is somehow supplied.
	if err := CheckPrivateKey(&Manifest{WrappedKey: "x"}, ""); err != nil {
		t.Fatalf("a symmetric backup must not require a key: %v", err)
	}
}

// A write-only parent cannot serve as an incremental diff base — its file index
// lives inside ciphertext this instance can't open. The chain must restart with
// a full baseline rather than erroring.
func TestWriteOnlyParentIsNotAnIncrementalBase(t *testing.T) {
	e := writeOnlyEngine(t)
	pub, _, _ := crypto.NewBackupKeypair()
	dek, _ := crypto.NewDataKey()
	wrapped, _ := crypto.WrapKeyPub(dek, pub)

	man := Manifest{
		VolIndex:      volumeIndexMember,
		WrappedKeyPub: wrapped,
		BackupPubFP:   crypto.BackupPubFP(pub),
		Volumes:       []VolumeRef{{Destination: "/data"}},
	}
	mj, _ := json.Marshal(&man)
	storeBackup(t, e, &store.Backup{
		ID: "b1", NodeID: "n1", TargetName: "app", Status: "success",
		ManifestJSON: string(mj),
	})

	parent, boundary := e.incrementalParent("n1", "app", []string{"/data"}, 7, "b2")
	if parent != nil || boundary {
		t.Fatal("a write-only parent must not be offered as a diff base")
	}
}

// A symmetric parent still works — the guard must not have broken incrementals
// for everyone else.
func TestSymmetricParentStillUsableAsIncrementalBase(t *testing.T) {
	e := writeOnlyEngine(t)
	dek, _ := crypto.NewDataKey()
	wrapped, _ := crypto.WrapKey(dek, e.Key)

	man := Manifest{
		VolIndex:   volumeIndexMember,
		WrappedKey: wrapped,
		Volumes:    []VolumeRef{{Destination: "/data"}},
	}
	mj, _ := json.Marshal(&man)
	storeBackup(t, e, &store.Backup{
		ID: "b1", NodeID: "n1", TargetName: "app", Status: "success",
		ManifestJSON: string(mj),
	})
	parent, _ := e.incrementalParent("n1", "app", []string{"/data"}, 7, "b2")
	if parent == nil {
		t.Fatal("a symmetric parent must still be usable as a diff base")
	}
}

// A drill must be SKIPPED, not recorded as failed — a failed drill alerts and
// downgrades a backup that is in fact perfectly good.
func TestWriteOnlyDrillIsSkippedNotFailed(t *testing.T) {
	e := writeOnlyEngine(t)
	pub, priv, _ := crypto.NewBackupKeypair()
	dek, _ := crypto.NewDataKey()
	wrapped, _ := crypto.WrapKeyPub(dek, pub)
	man := Manifest{WrappedKeyPub: wrapped, BackupPubFP: crypto.BackupPubFP(pub)}
	mj, _ := json.Marshal(&man)
	b := &store.Backup{ID: "b1", ManifestJSON: string(mj)}

	reason := e.DrillSkipReason(b)
	if reason == "" {
		t.Fatal("a write-only backup must report a drill-skip reason")
	}
	if !strings.Contains(reason, "write-only") {
		t.Fatalf("the reason must name write-only, got %q", reason)
	}
	// With the offline key present, a drill becomes possible again.
	e.restorePriv.Store(&priv)
	if r := e.DrillSkipReason(b); r != "" {
		t.Fatalf("with the private key a drill must be possible, got %q", r)
	}
	// A symmetric backup is always drillable.
	symMan, _ := json.Marshal(&Manifest{WrappedKey: "x"})
	if r := e.DrillSkipReason(&store.Backup{ID: "b2", ManifestJSON: string(symMan)}); r != "" {
		t.Fatalf("a symmetric backup must be drillable, got %q", r)
	}
}

// Master-key rotation must SKIP write-only backups (nothing of theirs is wrapped
// to the master key) without failing or corrupting them.
func TestRewrapSkipsWriteOnlyBackups(t *testing.T) {
	e := writeOnlyEngine(t)
	pub, _, _ := crypto.NewBackupKeypair()
	dek, _ := crypto.NewDataKey()
	wrapped, _ := crypto.WrapKeyPub(dek, pub)
	man := Manifest{WrappedKeyPub: wrapped, BackupPubFP: crypto.BackupPubFP(pub)}
	mj, _ := json.Marshal(&man)
	storeBackup(t, e, &store.Backup{
		ID: "b1", NodeID: "n1", TargetName: "app", Status: "success", ManifestJSON: string(mj),
	})

	newKey := make([]byte, 32)
	for i := range newKey {
		newKey[i] = 0xAB
	}
	res := e.RewrapAll(context.Background(), e.Key, newKey)
	if res.Rewrapped != 0 {
		t.Fatalf("a write-only backup must not be rewrapped, got %d", res.Rewrapped)
	}
	if res.Failed != 0 {
		t.Fatalf("skipping must not be a failure, got %d failed", res.Failed)
	}
	// And the manifest must be byte-identical afterwards.
	got, err := e.Store.GetBackup("b1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ManifestJSON != string(mj) {
		t.Fatal("a write-only manifest must be left untouched by rotation")
	}
}

// TestRollbackOpensWriteOnlySnapshot — the health gate's rollback restores the
// pre-restore safety snapshot, and while write-only mode is armed that snapshot
// is sealed to the offline keypair like everything else. The rollback is never
// handed a key (the operator cannot be prompted again mid-failure), so it used
// to be refused every single time: "Restore FAILED and rollback FAILED", an
// unhealthy container, and a manual recovery. The one safety net write-only mode
// offers only works if the rollback inherits the key the outer restore installed.
func TestRollbackOpensWriteOnlySnapshot(t *testing.T) {
	// The shared helper leaves Log nil; Restore logs, so give it a sink.
	rollbackEngine := func(t *testing.T) *Engine {
		t.Helper()
		e := writeOnlyEngine(t)
		e.Log = func(string, string, string) {}
		// A real (empty) registry: every node lookup then answers "not registered"
		// instead of dereferencing nil, so the restore fails where a restore
		// without a Docker host should fail — well past the key gate.
		e.Reg = dockercli.NewRegistry()
		return e
	}
	newSealedBackup := func(t *testing.T, e *Engine, id string) (privateKey string) {
		t.Helper()
		pub, priv, err := crypto.NewBackupKeypair()
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Store.SetSetting(writeOnlyPubSetting, pub); err != nil {
			t.Fatal(err)
		}
		dek, _ := crypto.NewDataKey()
		man := &Manifest{BackupID: id, TargetName: "app"}
		if err := e.wrapDEK(dek, man); err != nil {
			t.Fatal(err)
		}
		if !IsWriteOnly(man) {
			t.Fatal("the safety snapshot should have been sealed to the offline key")
		}
		mb, _ := json.Marshal(man)
		storeBackup(t, e, &store.Backup{ID: id, NodeID: "n1", TargetName: "app", Status: "success",
			StorageKey: "k/" + id, ManifestJSON: string(mb)})
		return priv
	}

	// The rollback the health gate actually issues: no key of its own, rollback
	// set, run inside a restore that has installed the operator's key.
	rollbackOpts := func(id string) RestoreOptions {
		return RestoreOptions{BackupID: id, NodeID: "n1", TargetID: "cid", Volumes: true, Database: true, Source: "local", rollback: true}
	}

	t.Run("it inherits the key the outer restore installed", func(t *testing.T) {
		e := rollbackEngine(t)
		priv := newSealedBackup(t, e, "snap1")
		e.restorePriv.Store(&priv) // as the outer restore does, for its whole duration

		err := e.Restore(context.Background(), rollbackOpts("snap1"))
		// It cannot COMPLETE here — there is no Docker node — but it must get past
		// the key gate, which is the whole difference between a working safety net
		// and a guaranteed manual recovery.
		if errors.Is(err, ErrPrivateKeyRequired) {
			t.Fatalf("the rollback was refused for want of a key the outer restore already holds: %v", err)
		}
		if err != nil && strings.Contains(err.Error(), "different keypair") {
			t.Fatalf("the inherited key must be the one that sealed the snapshot: %v", err)
		}
	})

	t.Run("with no key anywhere it is still refused", func(t *testing.T) {
		e := rollbackEngine(t)
		newSealedBackup(t, e, "snap2") // key discarded — nothing installed

		err := e.Restore(context.Background(), rollbackOpts("snap2"))
		if !errors.Is(err, ErrPrivateKeyRequired) {
			t.Fatalf("a rollback with no key available must still refuse, got %v", err)
		}
	})

	t.Run("an ordinary restore still needs its own key", func(t *testing.T) {
		e := rollbackEngine(t)
		priv := newSealedBackup(t, e, "snap3")
		e.restorePriv.Store(&priv) // ambient key present…

		opts := rollbackOpts("snap3")
		opts.rollback = false // …but this is a normal restore, not the engine's own rollback
		if err := e.Restore(context.Background(), opts); !errors.Is(err, ErrPrivateKeyRequired) {
			t.Fatalf("a user-initiated restore must present its own key, got %v", err)
		}
	})

	// The gate is a plain mutex held by the outer restore for its whole duration.
	// A rollback that tried to take it again would park forever — not fail, park —
	// and every deferred release above it would never run.
	t.Run("it never re-takes the gate the outer restore holds", func(t *testing.T) {
		e := rollbackEngine(t)
		priv := newSealedBackup(t, e, "snap4")
		e.restorePriv.Store(&priv)
		e.privGate.Lock() // stand in for the outer restore
		defer e.privGate.Unlock()

		opts := rollbackOpts("snap4")
		opts.PrivateKey = priv // a caller that also passes the key must not deadlock
		done := make(chan error, 1)
		go func() { done <- e.Restore(context.Background(), opts) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the rollback tried to lock the gate its own restore holds — the goroutine is parked forever")
		}
	})
}
