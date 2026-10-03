package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"

	"dockback/internal/storage"
	"dockback/internal/store"
)

// "Cancel restore" promises to stop at the next safe point. Reading a member out
// of a backup means streaming and DECRYPTING the archive up to it, which on a
// multi-gigabyte backup is minutes — and it used to run on a background context,
// so a cancelled restore carried on reading and writing regardless.

// archiveWithEntries builds a real encrypted backup holding the named member.
func archiveWithEntries(t *testing.T) (*Engine, *store.Backup) {
	t.Helper()
	be, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	e := &Engine{Store: st, Storage: be, Key: key, KeyFP: KeyFingerprint(key), Log: func(string, string, string) {}}

	// A member big enough that reading it is real work, plus the one we ask for.
	work := t.TempDir()
	var vbuf bytes.Buffer
	vtw := tar.NewWriter(&vbuf)
	tarAdd(t, vtw, "big/blob.bin", bytes.Repeat([]byte("payload-"), 512*1024))
	if err := vtw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "volumes.tar"), vbuf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "config", "inspect.json"), []byte(`{"Name":"/app"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	man := &Manifest{Version: ManifestVersion, BackupID: "b1", TargetName: "app",
		KeyFingerprint: e.KeyFP, Format: Format{Algorithm: "zstd", Archive: "tar"}}
	const key0 = "node/app/2026-01-01_00-00-00_k.dback"
	cipherSHA, _, err := e.packEncryptStore(context.Background(), work, man, key0, "zstd", zstd.SpeedDefault, 0)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	b := &store.Backup{ID: "b1", Status: "success", TargetName: "app"}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	b.CipherSHA256, b.StorageKey = cipherSHA, key0
	mb, _ := json.Marshal(man)
	b.ManifestJSON = string(mb)
	b.LocationsJSON = mustJSON([]Location{{Kind: "local", Name: "local", Type: "local"}})
	if err := st.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
	return e, b
}

func TestExtractEntryHonoursCancellation(t *testing.T) {
	e, b := archiveWithEntries(t)

	// The normal path still works: a live context reads the member back.
	got, err := e.extractEntry(context.Background(), b, "", "config/inspect.json")
	if err != nil {
		t.Fatalf("a live restore must still read its config: %v", err)
	}
	if !bytes.Contains(got, []byte("/app")) {
		t.Errorf("wrong bytes came back: %q", got)
	}

	// A cancelled restore stops instead of decrypting on.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.extractEntry(ctx, b, "", "config/inspect.json"); err == nil {
		t.Fatal("a cancelled restore must stop reading the archive, not finish it")
	} else if !errors.Is(err, context.Canceled) {
		t.Errorf("the refusal must be cancellation, not something else: %v", err)
	}
}

// The whole-archive pass that reads the captured compose files had the same
// defect and the same fix.
func TestOriginalComposeReadHonoursCancellation(t *testing.T) {
	e, b := archiveWithEntries(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := e.originalComposeFromArchive(ctx, b, ""); len(got) != 0 {
		t.Errorf("a cancelled restore must not keep walking the archive, got %d file(s)", len(got))
	}
	// And it still reads normally when the run is live (this backup has none).
	if got := e.originalComposeFromArchive(context.Background(), b, ""); len(got) != 0 {
		t.Errorf("this backup carries no original compose files, got %d", len(got))
	}
}
