package backup

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"testing"
)

// A database restored from its dump gets its init scripts and configuration
// back, and never a byte of a raw data directory, even from an old backup that
// still carries one.
func TestCopyTarWithoutTheDataDirectory(t *testing.T) {
	var src bytes.Buffer
	tw := tar.NewWriter(&src)
	for name, body := range map[string]string{
		"var/lib/postgresql/data/PG_VERSION":  "16",
		"var/lib/postgresql/data/base/1/1259": "raw page",
		"docker-entrypoint-initdb.d/01.sql":   "create extension pgcrypto;",
		"etc/postgresql/conf.d/tuning.conf":   "shared_buffers = 1GB",
		"var/lib/postgresql/data-archive/x":   "a sibling, not inside the data dir",
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: "var/lib/postgresql/data", Typeflag: tar.TypeDir, Mode: 0o700}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	var dst bytes.Buffer
	kept, err := copyTarWithout(&src, &dst, "var/lib/postgresql/data")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	tr := tar.NewReader(&dst)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got[hdr.Name] = true
	}
	want := []string{"docker-entrypoint-initdb.d/01.sql", "etc/postgresql/conf.d/tuning.conf", "var/lib/postgresql/data-archive/x"}
	if kept != len(want) || len(got) != len(want) {
		t.Fatalf("kept %d %v, want exactly %v", kept, got, want)
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("%s must be restored", name)
		}
	}
}

func TestHasMountsBesides(t *testing.T) {
	dataOnly := &Manifest{Volumes: []VolumeRef{{Destination: "/var/lib/postgresql/data"}}}
	if hasMountsBesides(dataOnly, "/var/lib/postgresql/data") {
		t.Error("a database with only its data directory has nothing else to restore")
	}
	withInit := &Manifest{Volumes: []VolumeRef{{Destination: "/var/lib/postgresql/data"}, {Destination: "/docker-entrypoint-initdb.d"}}}
	if !hasMountsBesides(withInit, "/var/lib/postgresql/data") {
		t.Error("an init-scripts mount must be restored")
	}
}
