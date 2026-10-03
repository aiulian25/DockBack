package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"dockback/internal/crypto"
	"dockback/internal/storage"
	"dockback/internal/store"
)

// F204 — proving a saved offline recovery key actually opens a write-only backup.
//
// Write-only mode moves the only key that can read a backup onto a sheet of
// paper. Every other failure mode has a check; this one had none, and it fails
// silently until the disaster it exists for.

// woBackup writes a real write-only archive to a local backend and returns the
// store record plus the private key that opens it.
func woBackup(t *testing.T, e *Engine, be storage.Backend, key string) (*store.Backup, string) {
	t.Helper()
	pub, priv, err := crypto.NewBackupKeypair()
	if err != nil {
		t.Fatal(err)
	}
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		t.Fatal(err)
	}
	wrapped, err := crypto.WrapKeyPub(dek, pub)
	if err != nil {
		t.Fatal(err)
	}
	// A payload spanning more than one frame, so "first frame only" is a real
	// claim about this archive rather than an accident of it being small.
	plain := bytes.Repeat([]byte("write-only archive payload. "), 80000)
	var ct bytes.Buffer
	if _, err := crypto.Encrypt(&ct, bytes.NewReader(plain), dek); err != nil {
		t.Fatal(err)
	}
	if ct.Len() < 2*(1<<20) {
		t.Fatalf("payload should span multiple frames, archive is %d bytes", ct.Len())
	}
	if _, err := be.Put(context.Background(), key, bytes.NewReader(ct.Bytes())); err != nil {
		t.Fatal(err)
	}
	man := Manifest{WrappedKeyPub: wrapped, BackupPubFP: crypto.BackupPubFP(pub)}
	mj, _ := json.Marshal(man)
	return &store.Backup{
		ID:            "b-wo",
		StorageKey:    key,
		ManifestJSON:  string(mj),
		LocationsJSON: `[{"kind":"local","name":"local","type":"local"}]`,
	}, priv
}

func woEngine(t *testing.T) (*Engine, storage.Backend) {
	t.Helper()
	be, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{Storage: be, Log: func(string, string, string) {}}, be
}

// AC1 — the correct private key passes, and reports the keypair it proved.
func TestVerifyRecoveryKeyAcceptsTheRightKey(t *testing.T) {
	e, be := woEngine(t)
	b, priv := woBackup(t, e, be, "node1/app/wo.dback")

	fp, err := e.VerifyRecoveryKey(context.Background(), b, priv)
	if err != nil {
		t.Fatalf("the correct key must open the backup: %v", err)
	}
	if len(fp) != 16 {
		t.Errorf("fingerprint = %q, want 16 hex characters", fp)
	}
	var man Manifest
	_ = json.Unmarshal([]byte(b.ManifestJSON), &man)
	if fp != man.BackupPubFP {
		t.Errorf("fingerprint %q should match the backup's keypair %q", fp, man.BackupPubFP)
	}

	// Leading/trailing whitespace from a copy-paste must not fail a good key.
	if _, err := e.VerifyRecoveryKey(context.Background(), b, "  "+priv+"\n"); err != nil {
		t.Errorf("a pasted key with surrounding whitespace must still pass: %v", err)
	}

	// The drill must not leave the key behind in shared engine state — that field
	// is read by concurrent restores.
	if e.restorePrivFor() != "" {
		t.Error("the drill must never assign the private key to the shared restore state")
	}
}

// AC2 — a key from another keypair fails, with a reason about the KEYPAIR
// rather than about corruption, and never reports a false pass.
func TestVerifyRecoveryKeyRejectsAWrongKey(t *testing.T) {
	e, be := woEngine(t)
	b, priv := woBackup(t, e, be, "node1/app/wo.dback")

	_, other, err := crypto.NewBackupKeypair()
	if err != nil {
		t.Fatal(err)
	}
	if other == priv {
		t.Fatal("need a different keypair")
	}
	fp, verr := e.VerifyRecoveryKey(context.Background(), b, other)
	if verr == nil {
		t.Fatal("a key from a different keypair must not pass")
	}
	if !strings.Contains(verr.Error(), "keypair") {
		t.Errorf("the reason should name the keypair mismatch, got: %v", verr)
	}
	// It still says WHICH key was tried — that is what makes the answer useful
	// to somebody holding several recovery sheets.
	if len(fp) != 16 {
		t.Errorf("a failure must still report the fingerprint tried, got %q", fp)
	}

	// Garbage in the field is a refusal, not a panic and not a pass.
	for _, junk := range []string{"not-base64!!", "", "   ", "c2hvcnQ="} {
		if _, err := e.VerifyRecoveryKey(context.Background(), b, junk); err == nil {
			t.Errorf("junk key %q must not pass", junk)
		}
	}
}

// A key from the RIGHT keypair against a corrupt archive fails at the frame,
// not at the fingerprint — the two must stay distinguishable, since one means
// "find the other sheet" and the other means "this copy is damaged".
func TestVerifyRecoveryKeySeparatesWrongKeyFromCorruptArchive(t *testing.T) {
	e, be := woEngine(t)
	const key = "node1/app/wo.dback"
	b, priv := woBackup(t, e, be, key)

	// Corrupt the first frame's ciphertext in place, leaving the header intact.
	ctx := context.Background()
	rc, err := be.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(rc); err != nil {
		t.Fatal(err)
	}
	rc.Close()
	raw := buf.Bytes()
	raw[len(raw)/2] ^= 0xff
	raw[20] ^= 0xff // inside frame 0
	if _, err := be.Put(ctx, key, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}

	_, verr := e.VerifyRecoveryKey(ctx, b, priv)
	if verr == nil {
		t.Fatal("a corrupt first frame must fail")
	}
	if strings.Contains(verr.Error(), "belongs to a different keypair") {
		t.Errorf("a damaged archive must not be reported as the wrong key: %v", verr)
	}
	if !strings.Contains(verr.Error(), "auth failed") {
		t.Errorf("the reason should point at the frame, got: %v", verr)
	}
}

// AC3 — a backup that is not write-only gets a distinct answer, never a pass
// and never a "your key is wrong".
func TestVerifyRecoveryKeyOnANonWriteOnlyBackup(t *testing.T) {
	e, _ := woEngine(t)
	_, priv, err := crypto.NewBackupKeypair()
	if err != nil {
		t.Fatal(err)
	}
	b := &store.Backup{ID: "b-sym", StorageKey: "k", ManifestJSON: `{"wrapped_key":"whatever"}`}

	_, verr := e.VerifyRecoveryKey(context.Background(), b, priv)
	if !errors.Is(verr, ErrNotWriteOnly) {
		t.Fatalf("want ErrNotWriteOnly, got %v", verr)
	}

	// A backup with no manifest at all is likewise not write-only.
	if _, verr := e.VerifyRecoveryKey(context.Background(), &store.Backup{ID: "b-none"}, priv); !errors.Is(verr, ErrNotWriteOnly) {
		t.Errorf("a manifest-less backup is not write-only, got %v", verr)
	}
}

// An unreachable copy must not be reported as a bad key: that would be a false
// alarm about the one thing this feature exists to give confidence in.
func TestVerifyRecoveryKeyDistinguishesUnreachableStorage(t *testing.T) {
	e, be := woEngine(t)
	b, priv := woBackup(t, e, be, "node1/app/wo.dback")
	b.StorageKey = "node1/app/does-not-exist.dback"

	fp, verr := e.VerifyRecoveryKey(context.Background(), b, priv)
	if verr == nil {
		t.Fatal("a missing archive cannot pass")
	}
	if !strings.Contains(verr.Error(), "could not reach a copy") {
		t.Errorf("an unreachable copy must say so rather than blame the key: %v", verr)
	}
	if fp == "" {
		t.Error("the fingerprint is derived from the key alone and should still be reported")
	}
}

// The drill reads only the first frame — it must not stream the whole archive.
// Asserted by counting bytes actually pulled from the backend.
func TestVerifyRecoveryKeyReadsOnlyTheFirstFrame(t *testing.T) {
	e, be := woEngine(t)
	const key = "node1/app/wo.dback"
	b, priv := woBackup(t, e, be, key)

	counting := &countingBackend{Backend: be}
	e.Storage = counting

	if _, err := e.VerifyRecoveryKey(context.Background(), b, priv); err != nil {
		t.Fatal(err)
	}
	total, _, _ := be.Stat(context.Background(), key)
	if counting.read >= total {
		t.Fatalf("read %d of %d bytes — the drill must stop after the first frame", counting.read, total)
	}
	if counting.read > int64(crypto.MaxFirstFrameBytes) {
		t.Errorf("read %d bytes, bound is %d", counting.read, crypto.MaxFirstFrameBytes)
	}
}

// AC2's log clause, asserted rather than grepped by hand: nothing the drill
// writes to the engine log may contain the key, on any path.
func TestVerifyRecoveryKeyNeverLogsTheKey(t *testing.T) {
	be, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var logged strings.Builder
	e := &Engine{Storage: be, Log: func(id, level, msg string) {
		logged.WriteString(id + " " + level + " " + msg + "\n")
	}}
	b, priv := woBackup(t, e, be, "node1/app/wo.dback")
	_, other, _ := crypto.NewBackupKeypair()

	// Every outcome: pass, wrong keypair, junk, unreachable copy.
	_, _ = e.VerifyRecoveryKey(context.Background(), b, priv)
	_, _ = e.VerifyRecoveryKey(context.Background(), b, other)
	_, _ = e.VerifyRecoveryKey(context.Background(), b, "not-base64!!")
	missing := *b
	missing.StorageKey = "node1/app/gone.dback"
	_, _ = e.VerifyRecoveryKey(context.Background(), &missing, priv)

	for _, secret := range []string{priv, other} {
		if strings.Contains(logged.String(), secret) {
			t.Fatalf("a private key reached the engine log:\n%s", logged.String())
		}
	}
}

// countingBackend counts bytes pulled through Get.
type countingBackend struct {
	storage.Backend
	read int64
}

func (c *countingBackend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := c.Backend.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return &countingReader{rc: rc, n: &c.read}, nil
}

type countingReader struct {
	rc io.ReadCloser
	n  *int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	*r.n += int64(n)
	return n, err
}

func (r *countingReader) Close() error { return r.rc.Close() }
