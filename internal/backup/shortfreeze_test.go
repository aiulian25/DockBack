package backup

import (
	"archive/tar"
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// archiveEntry is one member of a test archive.
type archiveEntry struct {
	name, body, link string
	kind             byte
}

func writeArchive(t *testing.T, path string, entries []archiveEntry) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, entry := range entries {
		// GNU, the format busybox writes: a long name takes extra header blocks.
		hdr := &tar.Header{Name: entry.name, Typeflag: entry.kind, Linkname: entry.link, Mode: 0o644, Size: int64(len(entry.body)), Format: tar.FormatGNU}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readArchive(t *testing.T, path string) (names []string, bodies map[string]string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	bodies = map[string]string{}
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return names, bodies
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		names = append(names, hdr.Name)
		bodies[hdr.Name] = string(body)
	}
}

func md5Of(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// The bulk is copied while the application runs and only what changed is
// copied again while it is held. Merged, the archive must hold exactly what a
// copy taken entirely while held would: the new version of a file rewritten
// during the copy, a file created during it, nothing of a folder deleted during
// it — and every untouched entry byte for byte, including one whose long name
// takes extra header blocks and one that sits after the gap the merge closes.
func TestShortFreezeMergeHoldsWhatAHeldCopyWould(t *testing.T) {
	work := t.TempDir()
	long := "data/" + strings.Repeat("x", 120)
	writeArchive(t, filepath.Join(work, volumesMember), []archiveEntry{
		{name: "data/", kind: tar.TypeDir},
		{name: "data/changed.txt", kind: tar.TypeReg, body: "old"},
		{name: "data/keep.txt", kind: tar.TypeReg, body: "keep"},
		{name: "data/gone/", kind: tar.TypeDir},
		{name: "data/gone/inner.txt", kind: tar.TypeReg, body: "inner"},
		{name: "data/link", kind: tar.TypeSymlink, link: "keep.txt"},
		{name: "data/hard", kind: tar.TypeLink, link: "data/keep.txt"},
		{name: long, kind: tar.TypeReg, body: "long"},
		{name: "data/after.txt", kind: tar.TypeReg, body: "after"},
	})
	writeArchive(t, filepath.Join(work, frozenCopyMember), []archiveEntry{
		{name: "data/", kind: tar.TypeDir},
		{name: "data/changed.txt", kind: tar.TypeReg, body: "rewritten while the copy ran"},
		{name: "data/new.txt", kind: tar.TypeReg, body: "new"},
	})
	old, recent := int64(100), int64(5000)
	live := &liveCopy{since: 1000, walk: map[string]walkEntry{
		"data":             {mode: modeDir, ctime: recent},
		"data/changed.txt": {mode: modeRegular, ctime: recent},
		"data/keep.txt":    {mode: modeRegular, ctime: old},
		"data/link":        {mode: 0o120777, ctime: old},
		"data/hard":        {mode: modeRegular, ctime: old},
		long:               {mode: modeRegular, ctime: old},
		"data/after.txt":   {mode: modeRegular, ctime: old},
		"data/new.txt":     {mode: modeRegular, ctime: recent},
	}}
	live.recopy = changedSince(live.walk, live.since)

	sha, hashes, err := live.settle(work)
	if err != nil {
		t.Fatal(err)
	}

	names, bodies := readArchive(t, filepath.Join(work, volumesMember))
	want := []string{"data/", "data/keep.txt", "data/link", "data/hard", long, "data/after.txt", "data/", "data/changed.txt", "data/new.txt"}
	if !slices.Equal(names, want) {
		t.Fatalf("merged archive holds\n %q\nwant\n %q", names, want)
	}
	if bodies["data/changed.txt"] != "rewritten while the copy ran" || bodies[long] != "long" || bodies["data/after.txt"] != "after" {
		t.Errorf("contents did not survive the merge: %q", bodies)
	}
	merged, _ := os.ReadFile(filepath.Join(work, volumesMember))
	if sum := sha256.Sum256(merged); hex.EncodeToString(sum[:]) != sha {
		t.Error("the recorded SHA-256 is not the merged archive's")
	}
	if !bytes.Equal(merged[len(merged)-tarEndBlocks*tarBlockSize:], make([]byte, tarEndBlocks*tarBlockSize)) {
		t.Error("the merged archive must end with its zero blocks and nothing of the live copy after them")
	}
	for name, body := range map[string]string{"data/keep.txt": "keep", "data/changed.txt": "rewritten while the copy ran", "data/new.txt": "new", long: "long"} {
		if hashes[name] != md5Of(body) {
			t.Errorf("%s: MD5 %q, want the merged content's", name, hashes[name])
		}
	}
	if _, err := os.Stat(filepath.Join(work, frozenCopyMember)); !os.IsNotExist(err) {
		t.Error("the second pass's archive must not be left in the backup's work folder")
	}
}

// When nothing changed during the live copy, that copy already is the held
// copy: it is kept untouched, with the hashes taken as it was written.
func TestShortFreezeKeepsAnUntouchedLiveCopy(t *testing.T) {
	work := t.TempDir()
	writeArchive(t, filepath.Join(work, volumesMember), []archiveEntry{{name: "data/a", kind: tar.TypeReg, body: "a"}})
	before, _ := os.ReadFile(filepath.Join(work, volumesMember))
	live := &liveCopy{sha: "live-sha", hashes: map[string]string{"data/a": md5Of("a")}}
	sha, hashes, err := live.settle(work)
	if err != nil || sha != "live-sha" || hashes["data/a"] != md5Of("a") {
		t.Fatalf("an untouched live copy keeps its own record: %q %v %v", sha, hashes, err)
	}
	if after, _ := os.ReadFile(filepath.Join(work, volumesMember)); !bytes.Equal(before, after) {
		t.Error("an untouched live copy must not be rewritten")
	}
}

// A hard link whose target was dropped would restore as an error, so the
// merge refuses rather than writing an archive that cannot be extracted.
func TestShortFreezeRefusesAHardLinkToADroppedEntry(t *testing.T) {
	work := t.TempDir()
	writeArchive(t, filepath.Join(work, volumesMember), []archiveEntry{
		{name: "data/a", kind: tar.TypeReg, body: "a"},
		{name: "data/b", kind: tar.TypeLink, link: "data/a"},
	})
	writeArchive(t, filepath.Join(work, frozenCopyMember), []archiveEntry{{name: "data/", kind: tar.TypeDir}})
	live := &liveCopy{walk: map[string]walkEntry{"data": {mode: modeDir}, "data/b": {mode: modeRegular}}, recopy: []string{"data"}}
	if _, _, err := live.settle(work); err == nil || !strings.Contains(err.Error(), "hard link") {
		t.Fatalf("a link to a dropped entry must fail the merge, got %v", err)
	}
}

// The walk taken while held decides what is deleted, so a listing that is not
// whole must never be trusted, and a line it cannot read is refused rather
// than guessed at. Pure.
func TestFrozenWalkIsTrustedOnlyWhole(t *testing.T) {
	out := "/data|4096|10|41ed|10\n/data/a|b.txt|3|20|81a4|9000\n/data/bro\n" + "ken|1|20|81a4|9000\n/data/sock|0|9000|c1ed|9000\n" + walkCompleteMarker + "\n"
	walk, complete := parseFrozenWalk([]byte(out))
	if !complete || len(walk) != 3 {
		t.Fatalf("a whole listing reads every line it can: %v %v", complete, walk)
	}
	if walk["data/a|b.txt"].size != 3 || !walk["data"].isDir() {
		t.Errorf("a name holding the separator still parses: %v", walk)
	}
	if got := changedSince(walk, 1000); !slices.Equal(got, []string{"data/a|b.txt"}) {
		t.Errorf("only what changed since the live copy began, sockets left out: %q", got)
	}
	if _, complete := parseFrozenWalk([]byte("/data|4096|10|41ed|10\n")); complete {
		t.Error("a listing without its end marker reported an error, and must not be trusted")
	}
	cmd := strings.Join(frozenWalkCmd([]string{"/data"}, []string{"/data/cache"}), " ")
	if !strings.Contains(cmd, "find /data ( -path /data/cache ) -prune -o -exec stat -c "+walkStatFormat) {
		t.Errorf("excluded paths are pruned from the walk: %s", cmd)
	}
}
