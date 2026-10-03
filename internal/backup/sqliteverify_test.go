package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"dockback/internal/crypto"
	"dockback/internal/storage"
	"dockback/internal/store"
)

// buildSQLiteArchive writes a real DBACKv1 archive containing a manifest and one
// `sqlite/1.dbk` member with the given bytes, and returns the Engine, the
// catalog row and the manifest ready for Verify. Building the genuine article
// (tar -> zstd -> AES-GCM) rather than stubbing the reader is the point: the
// F109 checks run inside the same single decrypt-and-walk pass as everything
// else, and a stub would not prove that.
func buildSQLiteArchive(t *testing.T, dbk []byte) (*Engine, *store.Backup, *Manifest) {
	t.Helper()
	dir := t.TempDir()
	be, err := storage.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	key := bytes.Repeat([]byte{0x2b}, 32)
	e := &Engine{Store: st, Storage: be, Key: key, Log: func(string, string, string) {}}

	sum := sha256.Sum256(dbk)
	man := &Manifest{
		Version: ManifestVersion, BackupID: "b1", Image: "ghcr.io/advplyr/audiobookshelf:latest",
		Format: Format{Encryption: "AES-256-GCM (DBACKv1 chunked)", Archive: "tar"},
		SQLiteDumps: []SQLiteRef{{
			Source: "/config/absdatabase.sqlite", Archive: "sqlite/1.dbk",
			Bytes: int64(len(dbk)), SHA256: hex.EncodeToString(sum[:]), Tables: 41, Rows: 1000,
		}},
	}

	// tar (manifest.json + the snapshot) -> zstd -> encrypt.
	var plain bytes.Buffer
	zw, err := newCompressWriter(&plain, "", zstd.SpeedDefault, 0)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	write := func(name string, body []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.json", []byte(`{"version":1}`))
	write("sqlite/1.dbk", dbk)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	var cipher bytes.Buffer
	if _, err := crypto.Encrypt(&cipher, bytes.NewReader(plain.Bytes()), key); err != nil {
		t.Fatal(err)
	}
	const storageKey = "node1/abs/b1.dback"
	ctx := context.Background()
	if _, err := be.Put(ctx, storageKey, bytes.NewReader(cipher.Bytes())); err != nil {
		t.Fatal(err)
	}
	csum, err := crypto.CipherSHA256(bytes.NewReader(cipher.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	b := &store.Backup{
		ID: "b1", StorageKey: storageKey, CipherSHA256: csum,
		LocationsJSON: `[{"kind":"local","name":"local","type":"local"}]`,
	}
	return e, b, man
}

// sqliteBytes is a plausible snapshot: the real header magic plus filler, so the
// header check is exercised against something that actually looks like a
// database rather than an arbitrary blob.
func sqliteBytes(n int) []byte {
	body := append([]byte(sqliteFileMagic+"\x00"), bytes.Repeat([]byte("page-data"), n)...)
	return body
}

func findCheck(rep *VerificationReport, name string) (Check, bool) {
	for _, c := range rep.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

// TestVerifySQLiteSnapshotOK closes the gap this feature was written for: before
// it, Verify walked straight past man.SQLiteDumps, so for an application whose
// entire state is one SQLite file the most important object in the archive was
// the least verified.
func TestVerifySQLiteSnapshotOK(t *testing.T) {
	e, b, man := buildSQLiteArchive(t, sqliteBytes(50))
	rep := e.Verify(context.Background(), b, man, "")
	if !rep.OK {
		t.Fatalf("an intact snapshot must verify: %s", rep.Summary())
	}
	c, ok := findCheck(rep, "sqlite:absdatabase.sqlite")
	if !ok || !c.OK {
		t.Fatalf("expected a passing sqlite check, got %+v (all: %+v)", c, rep.Checks)
	}
	// The contract recorded at capture is reported back, so the operator can see
	// what the archive claims to hold without opening it.
	if !strings.Contains(c.Info, "41 table") || !strings.Contains(c.Info, "1000 row") {
		t.Errorf("expected the recorded table/row contract in the check detail, got %q", c.Info)
	}
	if ic, ok := findCheck(rep, "sqlite-integrity:absdatabase.sqlite"); !ok || !ic.OK {
		t.Fatalf("expected a passing checksum check, got %+v", ic)
	}
}

// TestVerifySQLiteSnapshotChecksumMismatch: a snapshot that rotted in storage
// must be caught by a routine scrub, not at the moment someone needs it back.
func TestVerifySQLiteSnapshotChecksumMismatch(t *testing.T) {
	e, b, man := buildSQLiteArchive(t, sqliteBytes(50))
	man.SQLiteDumps[0].SHA256 = strings.Repeat("a", 64) // as if capture recorded something else

	rep := e.Verify(context.Background(), b, man, "")
	if rep.OK {
		t.Fatal("a snapshot that no longer matches its recorded checksum must FAIL verification")
	}
	c, ok := findCheck(rep, "sqlite-integrity:absdatabase.sqlite")
	if !ok || c.OK {
		t.Fatalf("expected a failing checksum check, got %+v", c)
	}
	if !strings.Contains(c.Info, "do NOT rely on this backup") {
		t.Errorf("the failure must be unambiguous about not trusting the backup, got %q", c.Info)
	}
}

// TestVerifySQLiteSnapshotNotADatabase: the member is checked by its own header
// magic, the same discipline capture uses — a name is not evidence.
func TestVerifySQLiteSnapshotNotADatabase(t *testing.T) {
	e, b, man := buildSQLiteArchive(t, []byte("this is not a sqlite database at all, not even close"))
	rep := e.Verify(context.Background(), b, man, "")
	if rep.OK {
		t.Fatal("a .dbk that is not a SQLite file must FAIL verification")
	}
	c, ok := findCheck(rep, "sqlite:absdatabase.sqlite")
	if !ok || c.OK || !strings.Contains(c.Info, "header magic") {
		t.Fatalf("expected a header-magic failure, got %+v", c)
	}
}

// TestVerifySQLiteSnapshotMissing: recorded but absent from the archive.
func TestVerifySQLiteSnapshotMissing(t *testing.T) {
	e, b, man := buildSQLiteArchive(t, sqliteBytes(10))
	man.SQLiteDumps = append(man.SQLiteDumps, SQLiteRef{
		Source: "/config/ghost.sqlite", Archive: "sqlite/2.dbk", Tables: 3,
	})
	rep := e.Verify(context.Background(), b, man, "")
	if rep.OK {
		t.Fatal("a recorded snapshot missing from the archive must FAIL verification")
	}
	if c, ok := findCheck(rep, "sqlite:ghost.sqlite"); !ok || c.OK {
		t.Fatalf("expected a failing check for the missing snapshot, got %+v", c)
	}
}

// TestVerifySQLiteLegacyBackup: a pre-F109 backup records no checksum, so only
// presence and shape are claimed — an absent contract must not become a failure.
func TestVerifySQLiteLegacyBackup(t *testing.T) {
	e, b, man := buildSQLiteArchive(t, sqliteBytes(10))
	man.SQLiteDumps[0].SHA256 = ""
	man.SQLiteDumps[0].Tables = 0
	man.SQLiteDumps[0].Rows = 0

	rep := e.Verify(context.Background(), b, man, "")
	if !rep.OK {
		t.Fatalf("a legacy backup with no recorded contract must still verify: %s", rep.Summary())
	}
	if c, ok := findCheck(rep, "sqlite:absdatabase.sqlite"); !ok || !c.OK {
		t.Fatalf("expected the presence check to still run, got %+v", c)
	}
	if _, ok := findCheck(rep, "sqlite-integrity:absdatabase.sqlite"); ok {
		t.Error("no checksum was recorded, so no checksum check should be claimed")
	}
}
