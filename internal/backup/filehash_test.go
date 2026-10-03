package backup

import (
	"archive/tar"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"slices"
	"strings"
	"testing"
)

// tarOf builds an archive the way the capture sidecar does: relative members.
func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range sortedTableNames(files) {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func md5of(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestVolIndexHash(t *testing.T) {
	t.Run("hashes come from the archive stream, one per regular file", func(t *testing.T) {
		archive := tarOf(t, map[string]string{
			"data/photo.jpg": "jpeg-bytes",
			"data/.immich":   "1787957665922",
			"data/notes.txt": "hello",
		})
		got := map[string]string{}
		hashTarMembers(bytes.NewReader(archive), got)

		if len(got) != 3 {
			t.Fatalf("got %d hashes: %v", len(got), got)
		}
		if got["data/photo.jpg"] != md5of("jpeg-bytes") {
			t.Errorf("photo = %q", got["data/photo.jpg"])
		}
		// R5 §9.3's marker: 13 bytes, and its content is what changes.
		if got["data/.immich"] != md5of("1787957665922") {
			t.Errorf("marker = %q", got["data/.immich"])
		}
	})

	t.Run("a truncated archive keeps what it hashed and claims nothing more", func(t *testing.T) {
		archive := tarOf(t, map[string]string{"a.txt": "one", "b.txt": "two"})
		got := map[string]string{}
		hashTarMembers(bytes.NewReader(archive[:len(archive)/2]), got)
		if len(got) == 0 {
			t.Skip("the truncation landed before the first member")
		}
		for name, sum := range got {
			if sum == "" {
				t.Errorf("%s recorded an empty hash", name)
			}
		}
	})

	t.Run("hashes are stamped onto the index entries they belong to", func(t *testing.T) {
		idx := VolIndex{Entries: []FileEntry{
			{Path: "data/photo.jpg", Size: 10},
			{Path: "data/.immich", Size: 13, Volatile: true},
			{Path: "data/excluded.log", Size: 5},
		}}
		stamped := applyIndexHashes(&idx, map[string]string{
			"data/photo.jpg": "aaa",
			"data/.immich":   "bbb",
		})
		if stamped != 2 {
			t.Fatalf("stamped %d, want 2", stamped)
		}
		// A file the walk saw and the archive did not carry keeps no hash, so a
		// comparison reports it unchecked rather than as a mismatch.
		if idx.Entries[2].MD5 != "" {
			t.Errorf("an uncarried file must have no hash: %+v", idx.Entries[2])
		}
		if !indexHasHashes(idx) {
			t.Error("this index carries hashes")
		}
		if indexHasHashes(VolIndex{Entries: []FileEntry{{Path: "a"}}}) {
			t.Error("a pre-hash index must not claim a content proof")
		}
	})
}

func TestRestoredFileVerdict(t *testing.T) {
	// R5 §9.3's shape: a large identical tree, plus markers the app rewrote.
	idx := VolIndex{Entries: []FileEntry{
		{Path: "upload/library/photo1.jpg", MD5: "aaa"},
		{Path: "upload/library/photo2.jpg", MD5: "bbb"},
		{Path: "upload/library/.immich", MD5: "old", Volatile: true},
		{Path: "upload/thumbs/.immich", MD5: "old", Volatile: true},
	}}

	t.Run("volatile markers rewritten by the app do not fail the restore", func(t *testing.T) {
		v := CompareFileHashes(idx, map[string]string{
			"upload/library/photo1.jpg": "aaa",
			"upload/library/photo2.jpg": "bbb",
			"upload/library/.immich":    "new",
			"upload/thumbs/.immich":     "new",
		})
		if !v.Clean() {
			t.Fatalf("58 GB of identical photographs must not read as a failure: %+v", v)
		}
		if v.Headline() != "2/2 durable identical; 2 volatile differ (expected)" {
			t.Errorf("headline = %q", v.Headline())
		}
	})

	t.Run("a same-size content change IS caught and named", func(t *testing.T) {
		// The defeat of path+size manifests: identical length, different bytes.
		v := CompareFileHashes(idx, map[string]string{
			"upload/library/photo1.jpg": "aaa",
			"upload/library/photo2.jpg": "CORRUPTED",
			"upload/library/.immich":    "old",
			"upload/thumbs/.immich":     "old",
		})
		if v.Clean() {
			t.Fatal("a durable content change must fail the restore — that is the point of having hashes")
		}
		if len(v.DurableDifferent) != 1 || v.DurableDifferent[0] != "upload/library/photo2.jpg" {
			t.Errorf("the specific item must be named: %v", v.DurableDifferent)
		}
	})

	t.Run("a durable file that did not arrive is reported apart from one that differs", func(t *testing.T) {
		v := CompareFileHashes(idx, map[string]string{"upload/library/photo1.jpg": "aaa"})
		if len(v.DurableMissing) != 1 || v.DurableMissing[0] != "upload/library/photo2.jpg" {
			t.Errorf("missing = %v", v.DurableMissing)
		}
		if len(v.DurableDifferent) != 0 {
			t.Errorf("absence is not difference: %v", v.DurableDifferent)
		}
	})

	t.Run("entries with no recorded hash are unchecked, never passing", func(t *testing.T) {
		v := CompareFileHashes(VolIndex{Entries: []FileEntry{
			{Path: "a", MD5: "aaa"}, {Path: "b"},
		}}, map[string]string{"a": "aaa", "b": "anything"})
		if v.DurableTotal != 1 || v.Unhashed != 1 {
			t.Errorf("got %+v", v)
		}
		if !v.Clean() {
			t.Error("an unhashed entry is not a mismatch either")
		}
	})

	t.Run("results are ordered and the report is capped", func(t *testing.T) {
		var entries []FileEntry
		restored := map[string]string{}
		for _, n := range []string{"z", "a", "m", "b", "y", "c", "x", "d"} {
			entries = append(entries, FileEntry{Path: n, MD5: "want"})
			restored[n] = "got"
		}
		v := CompareFileHashes(VolIndex{Entries: entries}, restored)
		if len(v.DurableDifferent) != 8 || v.DurableDifferent[0] != "a" {
			t.Errorf("differences must be sorted: %v", v.DurableDifferent)
		}
		if named := namedTables(v.DurableDifferent); !strings.Contains(named, "and 2 more") {
			t.Errorf("the report is capped: %s", named)
		}
	})

	t.Run("the probe survives paths with spaces and reports mount ownership", func(t *testing.T) {
		hashes, owners := parseRestoredFiles(
			"FOWNER|33:33|/var/www/html\n" +
				"FHASH|d41d8cd98f00b204e9800998ecf8427e  /var/www/html/my file.txt\n" +
				"FHASH|aaa  /var/www/html/photo.jpg\n" +
				"noise\n")
		if hashes["var/www/html/my file.txt"] != "d41d8cd98f00b204e9800998ecf8427e" {
			t.Errorf("a path with a space must survive: %v", hashes)
		}
		if hashes["var/www/html/photo.jpg"] != "aaa" {
			t.Errorf("hashes = %v", hashes)
		}
		// #15: ownership alignment wrote these and nothing ever read them back.
		if owners["/var/www/html"] != "33:33" {
			t.Errorf("owners = %v", owners)
		}
	})

	t.Run("the probe uses null separators and never the container's own binaries", func(t *testing.T) {
		script := restoredFilesScript([]string{"/var/www/html", "/data"})
		for _, want := range []string{"-print0", "xargs -0", "md5sum", "stat -c '%u:%g'", "'/var/www/html'", "'/data'"} {
			if !strings.Contains(script, want) {
				t.Errorf("script must contain %q", want)
			}
		}
	})

	t.Run("the tag is applied by the shell, never by a sed it would break", func(t *testing.T) {
		// FHASH| contains a pipe. `sed 's|^|FHASH||'` is malformed, silently
		// produces nothing, and every restored file then reads as missing.
		script := restoredFilesScript([]string{"/data"})
		if strings.Contains(script, "sed") {
			t.Error("the prefix must not go through sed — its delimiter collides with the tag")
		}
		if !strings.Contains(script, `printf '`+fileHashPrefix+`%s\n'`) {
			t.Errorf("the shell must apply the tag: %s", script)
		}
	})

	t.Run("the walk skips what its parent already covers", func(t *testing.T) {
		roots := restoredScanRoots(&Manifest{Volumes: []VolumeRef{
			{Destination: "/var/www/html", Type: "bind"},
			{Destination: "/var/www/html/data", Type: "bind", NestedIn: "/var/www/html"},
			{Destination: "/run/secrets/key", Type: "bind", Archive: "bind-files/0.bin"},
			{Destination: "/"},
		}})
		if len(roots) != 1 || !slices.Contains(roots, "/var/www/html") {
			t.Errorf("roots = %v", roots)
		}
	})
}
