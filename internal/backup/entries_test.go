package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/klauspost/compress/zstd"

	"dockback/internal/storage"
	"dockback/internal/store"
)

func tarAdd(t *testing.T, tw *tar.Writer, name string, data []byte) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
}

// TestListEntriesAndExtractOne (F21): a backup whose volumes.tar holds two known
// files lists both via ListEntries, and ExtractOne round-trips one file's exact
// bytes; a path not in the archive returns ErrEntryNotFound.
func TestListEntriesAndExtractOne(t *testing.T) {
	be, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	e := &Engine{Store: st, Storage: be, Key: key, KeyFP: KeyFingerprint(key), Log: func(string, string, string) {}}
	ctx := context.Background()

	// Build the inner volumes.tar with two known files.
	fileA := []byte("server { listen 80; }")
	fileB := bytes.Repeat([]byte("photo-bytes-"), 500)
	var vbuf bytes.Buffer
	vtw := tar.NewWriter(&vbuf)
	tarAdd(t, vtw, "config/app.conf", fileA)
	tarAdd(t, vtw, "media/photo.jpg", fileB)
	if err := vtw.Close(); err != nil {
		t.Fatal(err)
	}

	// Assemble the work dir (manifest.json is added by packEncryptStore) + volumes.tar.
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "volumes.tar"), vbuf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	man := &Manifest{
		Version: ManifestVersion, BackupID: "b1", TargetName: "app",
		KeyFingerprint: e.KeyFP, Format: Format{Algorithm: "zstd", Archive: "tar"},
	}
	key0 := "node/app/2026-01-01_00-00-00_k.dback"
	cipherSHA, _, err := e.packEncryptStore(ctx, work, man, key0, "zstd", zstd.SpeedDefault, 0)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}

	// Persist the catalog row so streamArchive/openVerified can read it back local.
	b := &store.Backup{ID: "b1", Status: "success", TargetName: "app"}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	b.CipherSHA256 = cipherSHA
	b.StorageKey = key0
	mb, _ := json.Marshal(man) // man now carries the wrapped DEK set by packEncryptStore
	b.ManifestJSON = string(mb)
	b.LocationsJSON = mustJSON([]Location{{Kind: "local", Name: "local", Type: "local"}})
	if err := st.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}

	// ListEntries returns both files with correct sizes.
	entries, err := e.ListEntries(ctx, b, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]int64{}
	for _, en := range entries {
		got[en.Name] = en.Size
	}
	if got["config/app.conf"] != int64(len(fileA)) || got["media/photo.jpg"] != int64(len(fileB)) {
		t.Fatalf("entries = %+v, want the two files with their sizes", entries)
	}

	// ExtractOne round-trips the exact bytes (sha256 match).
	var out bytes.Buffer
	if err := e.ExtractOne(ctx, b, "", "media/photo.jpg", &out); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if sha256.Sum256(out.Bytes()) != sha256.Sum256(fileB) {
		t.Fatal("extracted bytes don't match the original file")
	}
	// Leading "./" / "/" in the requested path is tolerated (same file).
	out.Reset()
	if err := e.ExtractOne(ctx, b, "", "/config/app.conf", &out); err != nil || out.String() != string(fileA) {
		t.Fatalf("extract with leading slash: err=%v got=%q", err, out.String())
	}

	// A path not in the archive is a clean ErrEntryNotFound (no partial output).
	var none bytes.Buffer
	if err := e.ExtractOne(ctx, b, "", "etc/passwd", &none); !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("missing path err = %v, want ErrEntryNotFound", err)
	}
	if none.Len() != 0 {
		t.Fatal("a missing path must produce no output")
	}
	// A traversal-looking name matches nothing (never escapes) → ErrEntryNotFound.
	if err := e.ExtractOne(ctx, b, "", "../../etc/shadow", io.Discard); !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("traversal path err = %v, want ErrEntryNotFound", err)
	}
}

// TestEntriesFromUniversalIndex (F70): a NON-incremental backup that stores the
// complete file index serves its listing from the index — every entry, no 20k
// truncation, no full-archive walk — and VolIndexCached memoizes the parsed
// index so later reads skip the archive entirely.
func TestEntriesFromUniversalIndex(t *testing.T) {
	be, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	e := &Engine{Store: st, Storage: be, Key: key, KeyFP: KeyFingerprint(key), Log: func(string, string, string) {}}
	ctx := context.Background()

	// A small volumes.tar + a LARGE index (> maxBrowseEntries) inside the archive.
	var vbuf bytes.Buffer
	vtw := tar.NewWriter(&vbuf)
	tarAdd(t, vtw, "data/file-0", []byte("x"))
	if err := vtw.Close(); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "volumes.tar"), vbuf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	n := maxBrowseEntries + 1000
	idx := VolIndex{Entries: make([]FileEntry, 0, n)}
	for i := 0; i < n; i++ {
		idx.Entries = append(idx.Entries, FileEntry{Path: "data/file-" + strconv.Itoa(i), Size: int64(i), MtimeUnix: 1000})
	}
	if err := writeVolIndex(work, idx); err != nil {
		t.Fatal(err)
	}

	man := &Manifest{
		Version: ManifestVersion, BackupID: "b-idx", TargetName: "app",
		KeyFingerprint: e.KeyFP, Format: Format{Algorithm: "zstd", Archive: "tar"},
		VolIndex: volumeIndexMember, // F70: universal index, NOT incremental
	}
	key0 := "node/app/2026-02-01_00-00-00_i.dback"
	cipherSHA, _, err := e.packEncryptStore(ctx, work, man, key0, "zstd", zstd.SpeedDefault, 0)
	if err != nil {
		t.Fatal(err)
	}
	b := &store.Backup{ID: "b-idx", Status: "success", TargetName: "app"}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	b.CipherSHA256 = cipherSHA
	b.StorageKey = key0
	mb, _ := json.Marshal(man)
	b.ManifestJSON = string(mb)
	b.LocationsJSON = mustJSON([]Location{{Kind: "local", Name: "local", Type: "local"}})
	if err := st.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}

	// The full listing comes back — beyond the streaming cap, untruncated.
	entries, err := e.ListEntries(ctx, b, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != n {
		t.Fatalf("indexed listing = %d entries, want %d (no truncation)", len(entries), n)
	}

	// VolIndexCached memoizes: after the first read, even deleting the archive
	// doesn't break a second lookup.
	if idx1, ok := e.VolIndexCached(ctx, b); !ok || len(idx1.Entries) != n {
		t.Fatalf("VolIndexCached first read: ok=%v n=%d", ok, len(idx1.Entries))
	}
	if err := be.Delete(ctx, key0); err != nil {
		t.Fatal(err)
	}
	if idx2, ok := e.VolIndexCached(ctx, b); !ok || len(idx2.Entries) != n {
		t.Fatal("VolIndexCached must serve from the cache on repeat reads")
	}

	// A backup with NO index reports ok=false (search skips it; browse streams).
	legacy := &store.Backup{ID: "b-legacy", Status: "success", ManifestJSON: `{"version":1}`}
	if _, ok := e.VolIndexCached(ctx, legacy); ok {
		t.Fatal("a legacy backup without an index must report ok=false")
	}
}
