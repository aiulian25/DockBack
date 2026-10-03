package backup

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

	"dockback/internal/appbackup"
	"dockback/internal/crypto"
	"dockback/internal/store"
)

// makeSnapshot writes a minimal valid DockBack DB snapshot for app-backup tests.
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

// ciphertextOf returns the encrypted body of an app-backup file: for a v2 archive,
// the bytes after the plaintext header; for anything else, the whole file.
func ciphertextOf(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const magic = "DBCFGv2\n"
	if !bytes.HasPrefix(b, []byte(magic)) {
		return b
	}
	rest := b[len(magic):]
	hlen := binary.BigEndian.Uint32(rest[:4])
	return rest[4+hlen:]
}

// craftV1AppBackup writes a legacy (pre-envelope) app-backup file — the tar
// encrypted directly with the master key, no header.
func craftV1AppBackup(t *testing.T, dir, name, snap string, key []byte) {
	t.Helper()
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		m := appbackup.Manifest{Format: appbackup.Format, Version: 1, CreatedAt: 1, AppVersion: "t", KeyFingerprint: "oldfp"}
		mb, _ := json.Marshal(m)
		_ = tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(mb))})
		_, _ = tw.Write(mb)
		f, _ := os.Open(snap)
		fi, _ := f.Stat()
		_ = tw.WriteHeader(&tar.Header{Name: "dockback.db", Mode: 0o600, Size: fi.Size()})
		_, _ = io.Copy(tw, f)
		f.Close()
		pw.CloseWithError(tw.Close())
	}()
	var buf bytes.Buffer
	if _, err := crypto.Encrypt(&buf, pr, key); err != nil {
		t.Fatalf("craftV1 encrypt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRewrapAppBackups covers F37: a v2 app-backup is re-wrapped old->new (decrypts
// under new, not old; ciphertext body unchanged); a v1 archive is skipped and left
// byte-for-byte intact and still restores with the old key.
func TestRewrapAppBackups(t *testing.T) {
	oldKey := make([]byte, 32)
	newKey := make([]byte, 32)
	rand.Read(oldKey)
	rand.Read(newKey)

	dir := t.TempDir()
	snap := makeSnapshot(t)

	// A v2 app-backup, wrapped by the OLD key.
	v2, err := appbackup.CreateFile(dir, snap, oldKey, appbackup.Entry{CreatedAt: 1700000001, AppVersion: "t", KeyFingerprint: KeyFingerprint(oldKey)})
	if err != nil {
		t.Fatalf("create v2: %v", err)
	}
	v2Path := filepath.Join(dir, v2.File)
	v2CipherBefore := ciphertextOf(t, v2Path)

	// A v1 (legacy) app-backup with a valid name.
	v1Name := "dockback-config-20200101-000000.dback"
	craftV1AppBackup(t, dir, v1Name, snap, oldKey)
	v1Path := filepath.Join(dir, v1Name)
	v1Before, _ := os.ReadFile(v1Path)

	// Rotate.
	e := &Engine{Log: func(string, string, string) {}}
	res := e.RewrapAppBackups(t.Context(), dir, oldKey, newKey)
	if res.Rewrapped != 1 || res.Skipped != 1 || res.Failed != 0 {
		t.Fatalf("rewrap result = %+v, want rewrapped=1 skipped=1 failed=0", res)
	}

	// v2: the ciphertext body is byte-for-byte unchanged (only the header re-wrapped).
	if !bytes.Equal(v2CipherBefore, ciphertextOf(t, v2Path)) {
		t.Error("v2 ciphertext body changed during rotation — must be untouched")
	}
	// v2: restores under the NEW key.
	if _, err := appbackup.RestoreFile(dir, v2.File, newKey, t.TempDir(), t.TempDir()); err != nil {
		t.Errorf("v2 should restore under the new key: %v", err)
	}
	// v2: does NOT restore under the OLD key any more.
	if _, err := appbackup.RestoreFile(dir, v2.File, oldKey, t.TempDir(), t.TempDir()); err == nil {
		t.Error("v2 must NOT restore under the old key after rotation")
	}

	// v1: left completely untouched (byte-identical) and still restores with the old key.
	v1After, _ := os.ReadFile(v1Path)
	if !bytes.Equal(v1Before, v1After) {
		t.Error("v1 archive was modified — a legacy archive must be left byte-for-byte intact")
	}
	if _, err := appbackup.RestoreFile(dir, v1Name, oldKey, t.TempDir(), t.TempDir()); err != nil {
		t.Errorf("v1 should still restore with the old key: %v", err)
	}
}
