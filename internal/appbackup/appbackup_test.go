package appbackup

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"dockback/internal/crypto"
	"dockback/internal/store"
)

// makeSnapshot writes a minimal but valid DockBack DB snapshot and returns its path.
func makeSnapshot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "dockback.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser("admin", "hash-xyz"); err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(dir, "snap.db")
	if err := s.SnapshotTo(snap); err != nil {
		t.Fatal(err)
	}
	s.Close()
	return snap
}

// writeV1 crafts a LEGACY (v1) archive: the tar encrypted directly with the master
// key, with no plaintext header — exactly what pre-F37 DockBack produced.
func writeV1(t *testing.T, w io.Writer, snap string, key []byte, m Manifest) {
	t.Helper()
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		if err := writeArchive(tw, snap, m); err != nil {
			_ = tw.Close()
			pw.CloseWithError(err)
			return
		}
		pw.CloseWithError(tw.Close())
	}()
	if _, err := crypto.Encrypt(w, pr, key); err != nil {
		t.Fatalf("writeV1 encrypt: %v", err)
	}
}

// TestEnvelopeV2Header verifies a NEW app-backup is v2: a plaintext header with the
// magic, version 2, and a wrapped_key, and that it round-trips via the DEK.
func TestEnvelopeV2Header(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	snap := makeSnapshot(t)

	var buf bytes.Buffer
	if err := Create(&buf, snap, key, Manifest{AppVersion: "t", KeyFingerprint: "fp1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	blob := buf.Bytes()
	if !bytes.HasPrefix(blob, []byte(magicV2)) {
		t.Fatal("v2 archive must start with the magic header")
	}
	// Parse the plaintext header.
	rest := blob[len(magicV2):]
	hlen := binary.BigEndian.Uint32(rest[:4])
	var hm Manifest
	if err := json.Unmarshal(rest[4:4+hlen], &hm); err != nil {
		t.Fatalf("header json: %v", err)
	}
	if hm.Version != 2 || hm.WrappedKey == "" || hm.Format != Format {
		t.Fatalf("bad v2 header: %+v", hm)
	}

	// Round-trips via the DEK under the correct key.
	got, err := Restore(bytes.NewReader(blob), key, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatalf("restore v2: %v", err)
	}
	if got.Version != 2 || got.WrappedKey != "" {
		t.Fatalf("returned manifest should be v2 with the wrapped key stripped: %+v", got)
	}
	// A wrong master key cannot unwrap the DEK.
	bad := make([]byte, 32)
	rand.Read(bad)
	if _, err := Restore(bytes.NewReader(blob), bad, t.TempDir(), t.TempDir()); err == nil {
		t.Fatal("restore v2 with wrong key must fail")
	}
}

// TestEnvelopeV1Compat verifies a legacy v1 archive (no header) still restores with
// the master key exactly as before — the reader dispatches on the header, so v2
// support never breaks old archives.
func TestEnvelopeV1Compat(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	snap := makeSnapshot(t)

	var buf bytes.Buffer
	writeV1(t, &buf, snap, key, Manifest{Format: Format, Version: 1, AppVersion: "t", KeyFingerprint: "oldfp"})
	blob := buf.Bytes()
	if bytes.HasPrefix(blob, []byte(magicV2)) {
		t.Fatal("v1 archive must NOT carry the v2 magic")
	}

	got, err := Restore(bytes.NewReader(blob), key, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatalf("restore v1: %v", err)
	}
	if got.Version != 1 || got.Format != Format || got.KeyFingerprint != "oldfp" {
		t.Fatalf("v1 manifest mismatch: %+v", got)
	}
	// Wrong key still fails.
	bad := make([]byte, 32)
	rand.Read(bad)
	if _, err := Restore(bytes.NewReader(blob), bad, t.TempDir(), t.TempDir()); err == nil {
		t.Fatal("restore v1 with wrong key must fail")
	}
}

func TestCreateRestoreRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)

	// Source instance with some state.
	srcDir := t.TempDir()
	src, err := store.Open(filepath.Join(srcDir, "dockback.db"))
	if err != nil {
		t.Fatal(err)
	}
	uid, err := src.CreateUser("admin", "hash-xyz")
	if err != nil {
		t.Fatal(err)
	}
	if err := src.SetSetting("key_fingerprint", "abc123"); err != nil {
		t.Fatal(err)
	}
	// A live session that MUST be scrubbed from the snapshot.
	_ = src.CreateSession("live-token", uid, 0)
	_ = src.SetSetting("csrf:live-token", "csrfval")

	// Snapshot + archive.
	tmp := t.TempDir()
	snap := filepath.Join(tmp, "snap.db")
	if err := src.SnapshotTo(snap); err != nil {
		t.Fatal(err)
	}
	src.Close()

	var buf bytes.Buffer
	m := Manifest{Format: Format, Version: Version, AppVersion: "test", KeyFingerprint: "abc123"}
	if err := Create(&buf, snap, key, m); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Wrong key must be rejected.
	badKey := make([]byte, 32)
	rand.Read(badKey)
	if _, err := Restore(bytes.NewReader(buf.Bytes()), badKey, t.TempDir(), tmp); err == nil {
		t.Fatal("restore with wrong key should fail")
	}

	// Restore into a fresh data dir.
	dstDir := t.TempDir()
	got, err := Restore(bytes.NewReader(buf.Bytes()), key, dstDir, tmp)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got.Format != Format || got.KeyFingerprint != "abc123" {
		t.Fatalf("manifest mismatch: %+v", got)
	}

	// Apply the staged restore and confirm state survived (and sessions scrubbed).
	if err := store.ApplyPendingRestore(dstDir); err != nil {
		t.Fatalf("apply: %v", err)
	}
	dst, err := store.Open(filepath.Join(dstDir, "dockback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	u, err := dst.GetUserByName("admin")
	if err != nil || u.PasswordHash != "hash-xyz" {
		t.Fatalf("admin not restored: %v / %+v", err, u)
	}
	if v, _ := dst.GetSetting("key_fingerprint", ""); v != "abc123" {
		t.Fatalf("setting not restored: %q", v)
	}
	if _, _, err := dst.SessionUser("live-token"); err == nil {
		t.Fatal("session should have been scrubbed from the backup")
	}
}

func TestLocalCatalog(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)

	srcDir := t.TempDir()
	src, err := store.Open(filepath.Join(srcDir, "dockback.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.CreateUser("admin", "hash-1"); err != nil {
		t.Fatal(err)
	}

	tmp := t.TempDir()
	snap := filepath.Join(tmp, "snap.db")
	if err := src.SnapshotTo(snap); err != nil {
		t.Fatal(err)
	}
	src.Close()

	catalog := t.TempDir()
	e, err := CreateFile(catalog, snap, key, Entry{CreatedAt: 1700000000, AppVersion: "t", KeyFingerprint: "fp", Nodes: 2, Backups: 5, Destinations: 1})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !ValidName(e.File) {
		t.Fatalf("bad generated name %q", e.File)
	}

	list, err := List(catalog)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
	if list[0].Nodes != 2 || list[0].Backups != 5 || list[0].SizeBytes == 0 {
		t.Fatalf("metadata not persisted: %+v", list[0])
	}

	// Traversal / bad names are rejected.
	if ValidName("../../etc/passwd") || ValidName("evil.dback") {
		t.Fatal("name validation too loose")
	}

	// Restore the stored file into a fresh data dir.
	dstDir := t.TempDir()
	if _, err := RestoreFile(catalog, e.File, key, dstDir, tmp); err != nil {
		t.Fatalf("restore file: %v", err)
	}
	if err := store.ApplyPendingRestore(dstDir); err != nil {
		t.Fatalf("apply: %v", err)
	}
	dst, err := store.Open(filepath.Join(dstDir, "dockback.db"))
	if err != nil {
		t.Fatal(err)
	}
	if u, err := dst.GetUserByName("admin"); err != nil || u.PasswordHash != "hash-1" {
		t.Fatalf("restored state wrong: %v", err)
	}
	dst.Close()

	// Delete removes archive + sidecar.
	if err := DeleteFile(catalog, e.File); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if list, _ := List(catalog); len(list) != 0 {
		t.Fatalf("expected empty after delete, got %d", len(list))
	}
}

// TestVerifyFile (F58): a pristine app-backup passes the integrity drill; a
// byte corrupted mid-file makes it fail. Uses the same helpers as the round-trip.
func TestVerifyFile(t *testing.T) {
	dir := t.TempDir()
	tmp := t.TempDir()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	snap := makeSnapshot(t)

	entry, err := CreateFile(dir, snap, key, Entry{CreatedAt: 1000, AppVersion: "test", KeyFingerprint: "fp"})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}

	// Pristine → passes.
	if err := VerifyFile(dir, entry.File, key, tmp); err != nil {
		t.Fatalf("pristine backup must verify, got: %v", err)
	}

	// Corrupt a byte in the encrypted body (past the plaintext header) → fails.
	path := filepath.Join(dir, entry.File)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 40 {
		t.Fatalf("archive unexpectedly small (%d bytes)", len(raw))
	}
	mid := len(raw) - 16 // deep in the ciphertext, safely past the header
	raw[mid] ^= 0xFF
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFile(dir, entry.File, key, tmp); err == nil {
		t.Fatal("corrupted backup must FAIL verification, but it passed")
	}

	// Wrong key → fails (never silently "ok").
	pristine, _ := CreateFile(dir, snap, key, Entry{CreatedAt: 2000, AppVersion: "test", KeyFingerprint: "fp"})
	badKey := make([]byte, 32)
	if err := VerifyFile(dir, pristine.File, badKey, tmp); err == nil {
		t.Fatal("wrong key must FAIL verification")
	}
}

// TestRewrapFile (F72): rotating A→B rewrites only the envelope header — the
// file verifies with B afterwards (and no longer with A), the payload
// ciphertext is byte-identical, a second rewrap is a clean no-op, and a file
// sealed under an UNRELATED key fails loudly instead of being silently skipped.
func TestRewrapFile(t *testing.T) {
	keyA := make([]byte, 32)
	rand.Read(keyA)
	keyB := make([]byte, 32)
	rand.Read(keyB)

	// A real v2 app-backup on key A.
	srcDir := t.TempDir()
	src, err := store.Open(filepath.Join(srcDir, "dockback.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.CreateUser("admin", "hash"); err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	snap := filepath.Join(tmp, "snap.db")
	if err := src.SnapshotTo(snap); err != nil {
		t.Fatal(err)
	}
	src.Close()

	dir := t.TempDir()
	ent, err := CreateFile(dir, snap, keyA, Entry{CreatedAt: 1700000000, AppVersion: "test", KeyFingerprint: "fp-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyFile(dir, ent.File, keyA, t.TempDir()); err != nil {
		t.Fatalf("pre-rewrap verify with A: %v", err)
	}
	// Payload ciphertext before (everything after the header must not change).
	before, err := os.ReadFile(filepath.Join(dir, ent.File))
	if err != nil {
		t.Fatal(err)
	}

	// Rotate A → B.
	changed, err := RewrapFile(dir, ent.File, "fp-b", keyA, keyB)
	if err != nil || !changed {
		t.Fatalf("rewrap: changed=%v err=%v", changed, err)
	}
	if err := VerifyFile(dir, ent.File, keyB, t.TempDir()); err != nil {
		t.Fatalf("verify with NEW key after rewrap: %v", err)
	}
	if err := VerifyFile(dir, ent.File, keyA, t.TempDir()); err == nil {
		t.Fatal("verify with the OLD key must fail after rewrap")
	}
	// The ciphertext tail is byte-identical — only the header was rewritten.
	after, err := os.ReadFile(filepath.Join(dir, ent.File))
	if err != nil {
		t.Fatal(err)
	}
	tail := func(b []byte) []byte {
		hlen := binary.BigEndian.Uint32(b[len(magicV2) : len(magicV2)+4])
		return b[len(magicV2)+4+int(hlen):]
	}
	if !bytes.Equal(tail(before), tail(after)) {
		t.Fatal("payload ciphertext changed during rewrap — it must be byte-copied")
	}

	// A re-run of the same rotation is a clean no-op (already on the new key).
	changed, err = RewrapFile(dir, ent.File, "fp-b", keyA, keyB)
	if err != nil || changed {
		t.Fatalf("second rewrap must be a no-op: changed=%v err=%v", changed, err)
	}

	// A file sealed under an UNRELATED key is a loud FAILURE, not a skip.
	foreign := make([]byte, 32)
	rand.Read(foreign)
	fent, err := CreateFile(dir, snap, foreign, Entry{CreatedAt: 1700000001, AppVersion: "test", KeyFingerprint: "fp-x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RewrapFile(dir, fent.File, "fp-b", keyA, keyB); err == nil {
		t.Fatal("a foreign-key file must be reported as failed, not silently skipped")
	}
}
