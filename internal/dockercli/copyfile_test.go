package dockercli

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

// This is the only path in DockBack that WRITES into a container, so what it can
// and cannot express is the whole security story. These tests pin both halves:
// what a valid write-back looks like, and what shapes are unrepresentable.

func TestSingleFileTarCarriesModeAndOwnership(t *testing.T) {
	body := []byte("server { listen 80; }")
	meta := FileMeta{
		Mode: 0o640, Size: int64(len(body)), UID: 1000, GID: 1000,
		ModTime: time.Unix(1_700_000_000, 0),
	}

	tr := tar.NewReader(singleFileTar("app.conf", meta, bytes.NewReader(body)))
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("reading the generated tar: %v", err)
	}
	if hdr.Name != "app.conf" {
		t.Fatalf("name = %q — CopyToContainer resolves it against the parent dir, so it must be the base name alone", hdr.Name)
	}
	if hdr.Typeflag != tar.TypeReg {
		t.Fatalf("typeflag = %v, want a regular file", hdr.Typeflag)
	}
	// A restored config the application cannot read is not a recovery.
	if hdr.Mode != 0o640 || hdr.Uid != 1000 || hdr.Gid != 1000 {
		t.Fatalf("mode/ownership not preserved: mode=%o uid=%d gid=%d", hdr.Mode, hdr.Uid, hdr.Gid)
	}
	got, _ := io.ReadAll(tr)
	if !bytes.Equal(got, body) {
		t.Fatalf("body = %q, want the exact archived bytes", got)
	}
	if _, err := tr.Next(); err != io.EOF {
		t.Fatal("the stream must contain exactly one entry")
	}
}

// Only the permission bits are honored. A file type smuggled through the mode
// field must not turn a write-back into a device node or setuid surprise beyond
// what the archive recorded.
func TestSingleFileTarNormalizesMode(t *testing.T) {
	tr := tar.NewReader(singleFileTar("f", FileMeta{Mode: int64(0o120777), Size: 0}, strings.NewReader("")))
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Mode != 0o777 || hdr.Typeflag != tar.TypeReg {
		t.Fatalf("mode=%o typeflag=%v — type bits must be dropped", hdr.Mode, hdr.Typeflag)
	}
	// A missing mode still has to land readable rather than 0.
	tr = tar.NewReader(singleFileTar("f", FileMeta{Size: 0}, strings.NewReader("")))
	if hdr, _ = tr.Next(); hdr.Mode != 0o644 {
		t.Fatalf("mode = %o, want a sane 0644 default", hdr.Mode)
	}
}

// A header declaring one length while the body carries another is how a
// truncated member corrupts the stream. It must fail, not write a damaged file.
func TestSingleFileTarRejectsShortBody(t *testing.T) {
	rc := singleFileTar("f", FileMeta{Mode: 0o644, Size: 100}, strings.NewReader("only ten!!"))
	_, err := io.ReadAll(rc)
	if err == nil {
		t.Fatal("a body shorter than the declared size must fail the stream")
	}
	if !strings.Contains(err.Error(), "short") {
		t.Fatalf("error should name the problem: %v", err)
	}
}

// The destination is validated before any write is attempted. `path.Clean` must
// be a no-op, so no `..` segment ever reaches the daemon to be resolved.
func TestSplitContainerPathRejectsUnsafePaths(t *testing.T) {
	for _, bad := range []string{
		"", "relative/path", "/", "/data/", "/data/..", "/data/../../etc/passwd",
		"/data/./app.conf", "/data//app.conf", "/data/app\x00.conf",
	} {
		if _, _, err := splitContainerPath(bad); err == nil {
			t.Fatalf("splitContainerPath(%q) must be rejected", bad)
		}
	}
	dir, base, err := splitContainerPath("/data/config/app.conf")
	if err != nil || dir != "/data/config" || base != "app.conf" {
		t.Fatalf("split = %q,%q,%v", dir, base, err)
	}
	// A file at the root still splits to a usable parent.
	if dir, base, err = splitContainerPath("/app.conf"); err != nil || dir != "/" || base != "app.conf" {
		t.Fatalf("root split = %q,%q,%v", dir, base, err)
	}
}
