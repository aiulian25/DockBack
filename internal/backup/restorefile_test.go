package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"

	"dockback/internal/storage"
	"dockback/internal/store"
)

// Single-file restore (F96) is the one path that writes into a running
// container, so the ordering of its guards is the thing worth pinning: a path
// that cannot be written must be refused BEFORE anything reaches Docker.
//
// e.Reg is deliberately nil in these tests. Any code path that got as far as
// touching the daemon would panic, so a clean ErrEntryNotFound is proof the
// refusal happened first — stronger than asserting on an error string.

func TestRestoreOneFileRefusesTraversalBeforeTouchingDocker(t *testing.T) {
	e := &Engine{Log: func(string, string, string) {}}
	b := &store.Backup{ID: "b1", Status: "success", TargetName: "app"}
	ctx := context.Background()

	for _, bad := range []string{
		"../../etc/shadow",      // classic escape
		"/../etc/shadow",        // escape past the leading slash
		"data/../../etc/passwd", // escape from inside a plausible prefix
		"..",
		"",
		"/",
	} {
		err := e.RestoreOneFile(ctx, b, "", bad, "node1", "container1", false)
		if !errors.Is(err, ErrEntryNotFound) {
			t.Fatalf("RestoreOneFile(%q) = %v, want ErrEntryNotFound with no write attempted", bad, err)
		}
	}
}

// A standalone volume backup (F23) stores volume-CONTENTS-relative paths, so
// "/" + name is a plausible-looking but WRONG container path. It must be
// identifiable rather than written.
func TestIsVolumeOnlyBackup(t *testing.T) {
	if !IsVolumeOnlyBackup(&store.Backup{TargetName: "volume:photos"}) {
		t.Fatal("a volume: backup must be recognised")
	}
	if IsVolumeOnlyBackup(&store.Backup{TargetName: "paperless"}) {
		t.Fatal("a container backup must not be")
	}
	if IsVolumeOnlyBackup(nil) {
		t.Fatal("nil must not be treated as a volume backup")
	}
}

// The write-back must reproduce the file EXACTLY as captured — the archived
// bytes and the recorded mode and ownership, which is what a `docker cp` by hand
// gets wrong. This exercises the same walk RestoreOneFile uses, without a daemon.
func TestRestoreOneFileWalkCarriesRecordedMetadata(t *testing.T) {
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

	body := bytes.Repeat([]byte("config-line\n"), 400)
	var vbuf bytes.Buffer
	vtw := tar.NewWriter(&vbuf)
	if err := vtw.WriteHeader(&tar.Header{
		Name: "data/config/app.conf", Mode: 0o600, Size: int64(len(body)),
		Uid: 1000, Gid: 1000, Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := vtw.Write(body); err != nil {
		t.Fatal(err)
	}
	// A symlink in the same archive: a write-back must never recreate one.
	if err := vtw.WriteHeader(&tar.Header{
		Name: "data/link", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink, Mode: 0o777,
	}); err != nil {
		t.Fatal(err)
	}
	if err := vtw.Close(); err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "volumes.tar"), vbuf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	man := &Manifest{
		Version: ManifestVersion, BackupID: "bf1", TargetName: "app",
		KeyFingerprint: e.KeyFP, Format: Format{Algorithm: "zstd", Archive: "tar"},
	}
	key0 := "node/app/2026-03-01_00-00-00_f.dback"
	cipherSHA, _, err := e.packEncryptStore(ctx, work, man, key0, "zstd", zstd.SpeedDefault, 0)
	if err != nil {
		t.Fatal(err)
	}
	b := &store.Backup{ID: "bf1", Status: "success", TargetName: "app"}
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

	var gotHdr *tar.Header
	var gotBody bytes.Buffer
	ok, err := e.walkToEntry(ctx, b, "", "volumes.tar", "data/config/app.conf",
		func(h *tar.Header, r io.Reader) error {
			gotHdr = h
			_, cerr := io.Copy(&gotBody, r)
			return cerr
		})
	if err != nil || !ok {
		t.Fatalf("walkToEntry: ok=%v err=%v", ok, err)
	}
	if gotHdr.Mode != 0o600 || gotHdr.Uid != 1000 || gotHdr.Gid != 1000 {
		t.Fatalf("recorded metadata lost: mode=%o uid=%d gid=%d", gotHdr.Mode, gotHdr.Uid, gotHdr.Gid)
	}
	if gotHdr.Size != int64(len(body)) || !bytes.Equal(gotBody.Bytes(), body) {
		t.Fatal("the archived bytes must round-trip exactly")
	}

	// The symlink is reachable by the walk, so the TYPE check is what stops it
	// being written back — assert the header the caller would refuse on.
	var linkHdr *tar.Header
	ok, err = e.walkToEntry(ctx, b, "", "volumes.tar", "data/link",
		func(h *tar.Header, _ io.Reader) error { linkHdr = h; return nil })
	if err != nil || !ok {
		t.Fatalf("walkToEntry(symlink): ok=%v err=%v", ok, err)
	}
	if linkHdr.Typeflag == tar.TypeReg {
		t.Fatal("a symlink must not present as a regular file — RestoreOneFile refuses on this")
	}

	// A path the archive does not contain is never found.
	if ok, err = e.walkToEntry(ctx, b, "", "volumes.tar", "etc/passwd",
		func(*tar.Header, io.Reader) error { return nil }); ok || err != nil {
		t.Fatalf("a missing path must not match: ok=%v err=%v", ok, err)
	}
}
