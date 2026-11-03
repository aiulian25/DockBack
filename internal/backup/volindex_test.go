package backup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

const testMarker = "DOCKBACK-test"

// inventoryOutput is what inventoryCmd prints for these records.
func inventoryOutput(records ...string) []byte {
	out := "1700000000\n"
	for _, record := range records {
		out += testMarker + record + "\n"
	}
	return []byte(out + testMarker + walkEndSuffix + "\n")
}

func TestInventoryCmdFramesEveryRecordAndPrunes(t *testing.T) {
	cmd := strings.Join(inventoryCmd([]string{"/config", "", "/data"}, []string{"/data/cache"}, testMarker), " ")
	for _, want := range []string{
		"find /config /data ( -path /data/cache ) -prune -o -exec stat -c " + testMarker + walkStatFormat + " {} +",
		"echo " + testMarker + walkEndSuffix,
		"date +%s",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("%q missing from %s", want, cmd)
		}
	}
}

func TestParseInventoryReadsAnyName(t *testing.T) {
	names := []string{"/data/plain", "/data/a|b|c", "/data/new\nline|1|2|3", "/data/-dash", "/data/ spaced ", "/data/\xff\xfe"}
	var records []string
	for i, name := range names {
		records = append(records, fmt.Sprintf("|%d|20|81a4|30|1000|1001|%s", i, name))
	}
	records = append(records, "|4096|20|41ed|30|0|0|/data")
	walk, walkedAt, err := parseInventory(inventoryOutput(records...), testMarker)
	if err != nil {
		t.Fatal(err)
	}
	if walkedAt != 1700000000 || len(walk) != len(names)+1 {
		t.Fatalf("clock %d, %d entries: %v", walkedAt, len(walk), walk)
	}
	for i, name := range names {
		entry, ok := walk[strings.TrimPrefix(name, "/")]
		if !ok || entry.size != int64(i) || entry.uid != 1000 || entry.gid != 1001 || entry.ctime != 30 {
			t.Errorf("%q read as %+v (found %v)", name, entry, ok)
		}
	}
	if !walk["data"].isDir() {
		t.Errorf("the directory was not read as one: %+v", walk["data"])
	}
}

// A listing that misses an entry reads as that entry deleted, so a partial one is
// refused whole.
func TestParseInventoryRefusesWhatIsNotWhole(t *testing.T) {
	whole := inventoryOutput("|1|20|81a4|30|0|0|/data/a")
	cases := map[string][]byte{
		"no end line":      bytes.TrimSuffix(whole, []byte(testMarker+walkEndSuffix+"\n")),
		"another's marker": inventoryOutput("|1|20|81a4|30|0|0|/data/a")[:len(whole)-5],
		"bad record":       inventoryOutput("|x|20|81a4|30|0|0|/data/a"),
		"short record":     inventoryOutput("|1|20|/data/a"),
		"no clock":         []byte(testMarker + walkEndSuffix + "\n"),
	}
	for name, out := range cases {
		if _, _, err := parseInventory(out, testMarker); !errors.Is(err, errWalkIncomplete) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestIndexFromWalkSeparatesFilesFromTheRest(t *testing.T) {
	idx := indexFromWalk(map[string]walkEntry{
		"data":      {mode: modeDir},
		"data/file": {mode: modeRegular, size: 3},
		"data/link": {mode: 0o120777},
		"data/sock": {mode: modeSocket},
	}, 50)
	if idx.Version != volIndexVersion || idx.WalkedAt != 50 {
		t.Errorf("header: %+v", idx)
	}
	if len(idx.Entries) != 1 || idx.Entries[0].Path != "data/file" {
		t.Errorf("files: %+v", idx.Entries)
	}
	if len(idx.Others) != 2 || idx.Others[0].Path != "data" || idx.Others[1].Path != "data/link" {
		t.Errorf("others, sockets left out: %+v", idx.Others)
	}
}

func TestOddPathsSurviveTheStoredIndex(t *testing.T) {
	odd := "data/\xff\xfe"
	entries := []FileEntry{{Path: odd}, {Path: "data/plain"}}
	encodeOddPaths(entries)
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	var loaded []FileEntry
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	decodeOddPaths(loaded)
	if loaded[0].Path != odd || loaded[1].Path != "data/plain" || loaded[1].PathB64 != "" {
		t.Errorf("round trip: %+v", loaded)
	}
}

// Each of these is invisible to a size and mtime comparison.
func TestDiffIndexSeesWhatSizeAndMtimeCannot(t *testing.T) {
	base := FileEntry{Path: "x", Size: 1, MtimeUnix: 10, Mode: modeRegular | 0o644, CtimeUnix: 10}
	const walkedAt = 100
	with := func(change func(*FileEntry)) FileEntry {
		entry := base
		change(&entry)
		return entry
	}
	cases := map[string]FileEntry{
		"chmod":           with(func(f *FileEntry) { f.Mode = modeRegular | 0o600 }),
		"chown":           with(func(f *FileEntry) { f.UID = 1234 }),
		"chgrp":           with(func(f *FileEntry) { f.GID = 1234 }),
		"same-size write": with(func(f *FileEntry) { f.CtimeUnix = 90 }),
	}
	for name, cur := range cases {
		changed, _ := DiffIndex(VolIndex{WalkedAt: walkedAt, Entries: []FileEntry{base}}, VolIndex{Entries: []FileEntry{cur}})
		if !reflect.DeepEqual(changed, []string{"x"}) {
			t.Errorf("%s not seen: %v", name, changed)
		}
	}
	if changed, _ := DiffIndex(VolIndex{WalkedAt: walkedAt, Entries: []FileEntry{base}}, VolIndex{Entries: []FileEntry{base}}); len(changed) != 0 {
		t.Errorf("an untouched file was copied: %v", changed)
	}
}

// An entry changed in the very second the parent's walk began could change again
// without its whole-second ctime moving.
func TestDiffIndexRecopiesWhatTheParentWalkCouldNotSettle(t *testing.T) {
	racy := FileEntry{Path: "x", CtimeUnix: 100}
	changed, _ := DiffIndex(VolIndex{WalkedAt: 100, Entries: []FileEntry{racy}}, VolIndex{Entries: []FileEntry{racy}})
	if !reflect.DeepEqual(changed, []string{"x"}) {
		t.Errorf("racy entry not copied again: %v", changed)
	}
}

func TestDiffIndexCoversDirectoriesAndLinks(t *testing.T) {
	prev := VolIndex{WalkedAt: 100, Others: []FileEntry{
		{Path: "emptied", Mode: modeDir},
		{Path: "link", Mode: 0o120777, CtimeUnix: 5},
	}}
	cur := VolIndex{Others: []FileEntry{
		{Path: "fresh", Mode: modeDir},
		{Path: "link", Mode: 0o120777, CtimeUnix: 50},
	}}
	changed, deleted := DiffIndex(prev, cur)
	if !reflect.DeepEqual(changed, []string{"fresh", "link"}) || !reflect.DeepEqual(deleted, []string{"emptied"}) {
		t.Errorf("changed %v, deleted %v", changed, deleted)
	}
}

func TestReplacedEntriesAreTypeChangesOnly(t *testing.T) {
	prev := VolIndex{
		Entries: []FileEntry{{Path: "file-to-dir", Mode: modeRegular | 0o644}, {Path: "same", Mode: modeRegular | 0o644}},
		Others:  []FileEntry{{Path: "dir-to-link", Mode: modeDir | 0o755}},
	}
	cur := VolIndex{
		Entries: []FileEntry{{Path: "same", Mode: modeRegular | 0o600}},
		Others:  []FileEntry{{Path: "dir-to-link", Mode: 0o120777}, {Path: "file-to-dir", Mode: modeDir | 0o755}},
	}
	if got := replacedEntries(prev, cur); !reflect.DeepEqual(got, []string{"dir-to-link", "file-to-dir"}) {
		t.Errorf("replaced = %v", got)
	}
}

// A chown -R at every application start is not an attack.
func TestTripwireJudgesContentAlone(t *testing.T) {
	prev := VolIndex{
		Entries: []FileEntry{{Path: "a", Size: 1, MtimeUnix: 1}, {Path: "b", Size: 1, MtimeUnix: 1}, {Path: "gone", Size: 1, MtimeUnix: 1}},
		Others:  []FileEntry{{Path: "gone-dir", Mode: modeDir}},
	}
	cur := VolIndex{Entries: []FileEntry{{Path: "a", Size: 1, MtimeUnix: 1, UID: 1000, CtimeUnix: 9}, {Path: "b", Size: 2, MtimeUnix: 1}, {Path: "new", Size: 1, MtimeUnix: 1}}}
	if got := ContentChangedFiles(prev, cur); !reflect.DeepEqual(got, []string{"b", "new"}) {
		t.Errorf("content changed = %v", got)
	}
	if got := DeletedFiles(prev, []string{"gone", "gone-dir"}); !reflect.DeepEqual(got, []string{"gone"}) {
		t.Errorf("deleted files = %v", got)
	}
}

func TestDeltaCannotCarry(t *testing.T) {
	cases := []struct {
		name             string
		changed, removed []string
		refused          bool
	}{
		{"ordinary names", []string{"a|b", " c"}, []string{"d"}, false},
		{"a changed name that is not UTF-8 travels in tar", []string{"\xff"}, nil, false},
		{"a changed name with a newline", []string{"a\nb"}, nil, true},
		{"a removed name with a newline", nil, []string{"a\nb"}, true},
		{"a removed name that is not UTF-8", nil, []string{"\xff"}, true},
	}
	for _, c := range cases {
		if got := deltaCannotCarry(c.changed, c.removed) != ""; got != c.refused {
			t.Errorf("%s: refused = %v", c.name, got)
		}
	}
}

// A trailing space is part of a name, and trimming it would remove another file.
func TestSanitizeVolPathsKeepsTheNameExactly(t *testing.T) {
	got := sanitizeVolPaths([]string{"data/name ", " ", "", "/abs", "data/../etc"})
	if !reflect.DeepEqual(got, []string{"data/name "}) {
		t.Errorf("sanitized = %q", got)
	}
}

func TestVolIndexDiffDetectsAddModifyDelete(t *testing.T) {
	prev := VolIndex{WalkedAt: 1000, Entries: []FileEntry{
		{Path: "config/a", Size: 10, MtimeUnix: 100},
		{Path: "config/b", Size: 20, MtimeUnix: 200},
		{Path: "config/gone", Size: 5, MtimeUnix: 50},
	}}
	cur := VolIndex{Entries: []FileEntry{
		{Path: "config/a", Size: 10, MtimeUnix: 100}, // unchanged
		{Path: "config/b", Size: 20, MtimeUnix: 999}, // mtime changed
		{Path: "config/c", Size: 1, MtimeUnix: 1},    // new
	}}
	changed, deleted := DiffIndex(prev, cur)
	if !reflect.DeepEqual(changed, []string{"config/b", "config/c"}) {
		t.Errorf("changed = %v, want [config/b config/c]", changed)
	}
	if !reflect.DeepEqual(deleted, []string{"config/gone"}) {
		t.Errorf("deleted = %v, want [config/gone]", deleted)
	}

	// Size-only change is also detected.
	c2, _ := DiffIndex(
		VolIndex{WalkedAt: 1000, Entries: []FileEntry{{Path: "x", Size: 1, MtimeUnix: 1}}},
		VolIndex{Entries: []FileEntry{{Path: "x", Size: 2, MtimeUnix: 1}}},
	)
	if !reflect.DeepEqual(c2, []string{"x"}) {
		t.Errorf("size-only change not detected: %v", c2)
	}
}

func TestChainResolveOrdersFullToDeltas(t *testing.T) {
	full := &Manifest{BackupID: "b0", CipherSHA256: "sha0"}
	d1 := &Manifest{BackupID: "b1", Parent: "b0", ParentCipherSHA256: "sha0", Incremental: true, CipherSHA256: "sha1", ChainDepth: 1}
	d2 := &Manifest{BackupID: "b2", Parent: "b1", ParentCipherSHA256: "sha1", Incremental: true, CipherSHA256: "sha2", ChainDepth: 2}
	byID := map[string]*Manifest{"b0": full, "b1": d1, "b2": d2}

	chain, err := resolveChain("b2", byID)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{chain[0].BackupID, chain[1].BackupID, chain[2].BackupID}
	if !reflect.DeepEqual(got, []string{"b0", "b1", "b2"}) {
		t.Fatalf("chain order = %v, want [b0 b1 b2]", got)
	}

	// A missing parent fails closed.
	if _, err := resolveChain("b2", map[string]*Manifest{"b2": d2, "b1": d1}); err == nil {
		t.Error("expected error for missing full baseline")
	}
	// A tampered pin fails closed.
	bad := &Manifest{BackupID: "bx", Parent: "b0", ParentCipherSHA256: "WRONG", Incremental: true, CipherSHA256: "shx"}
	if _, err := resolveChain("bx", map[string]*Manifest{"b0": full, "bx": bad}); err == nil {
		t.Error("expected chain-integrity error for mismatched parent pin")
	}
	// A cycle fails closed.
	cyc1 := &Manifest{BackupID: "c1", Parent: "c2", Incremental: true}
	cyc2 := &Manifest{BackupID: "c2", Parent: "c1", Incremental: true}
	if _, err := resolveChain("c1", map[string]*Manifest{"c1": cyc1, "c2": cyc2}); err == nil {
		t.Error("expected cycle error")
	}

	// F85: a SYNTHETIC full is manifest-identical to a real full (no Parent,
	// depth 0, not incremental) — a chain rooted on one resolves normally.
	syn := &Manifest{BackupID: "s0", CipherSHA256: "shas0", VolIndex: "volumes-index.json.zst"}
	sd1 := &Manifest{BackupID: "s1", Parent: "s0", ParentCipherSHA256: "shas0", Incremental: true, CipherSHA256: "shas1", ChainDepth: 1}
	sChain, err := resolveChain("s1", map[string]*Manifest{"s0": syn, "s1": sd1})
	if err != nil || len(sChain) != 2 || sChain[0].BackupID != "s0" {
		t.Fatalf("chain rooted on a synthetic full must resolve: %v (err %v)", sChain, err)
	}
}

func TestVolIndexSanitizePaths(t *testing.T) {
	in := []string{"config/a", "/etc/passwd", "config/../../../etc/shadow", "", "  ", "data/ok", "a/../b"}
	got := sanitizeVolPaths(in)
	want := []string{"config/a", "data/ok"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitizeVolPaths = %v, want %v", got, want)
	}
}

// F63: ChainDescendants returns the full subtree a delete would orphan.
func TestChainDescendants(t *testing.T) {
	// full ← d1 ← d2 ← d3 ; full ← d1b (branch) ; other chain unrelated.
	parentOf := map[string]string{
		"d1": "full", "d2": "d1", "d3": "d2", "d1b": "full",
		"x1": "xfull",
	}
	got := ChainDescendants("full", parentOf)
	want := []string{"d1", "d1b", "d2", "d3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("descendants of full = %v, want %v", got, want)
	}
	if got := ChainDescendants("d2", parentOf); !reflect.DeepEqual(got, []string{"d3"}) {
		t.Fatalf("descendants of d2 = %v, want [d3]", got)
	}
	if got := ChainDescendants("d3", parentOf); len(got) != 0 {
		t.Fatalf("a leaf has no descendants, got %v", got)
	}
	if got := ChainDescendants("unknown", parentOf); len(got) != 0 {
		t.Fatalf("unknown id has no descendants, got %v", got)
	}
}

// TestChainRetentionNeverOrphans is the property test: over many random chains and
// random keep-sets, protectedAncestors must guarantee that after protection no
// KEPT delta is left without its parent.
func TestChainRetentionNeverOrphans(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 500; iter++ {
		n := 1 + rng.Intn(20)
		// Build a random forest: each backup's parent is some earlier id or "" (a full).
		parentOf := map[string]string{}
		ids := make([]string, n)
		for i := 0; i < n; i++ {
			id := "b" + strconv.Itoa(i)
			ids[i] = id
			if i > 0 && rng.Intn(2) == 0 {
				parentOf[id] = "b" + strconv.Itoa(rng.Intn(i)) // link to an earlier backup
			} else {
				parentOf[id] = "" // a full baseline
			}
		}
		// Random keep-set (the retention policy's decision).
		var keep []string
		keepSet := map[string]bool{}
		for _, id := range ids {
			if rng.Intn(2) == 0 {
				keep = append(keep, id)
				keepSet[id] = true
			}
		}
		prot := protectedAncestors(keep, parentOf)
		// Property: for every kept backup, its whole ancestor chain is kept-or-protected.
		for _, id := range keep {
			cur := parentOf[id]
			for cur != "" {
				if !keepSet[cur] && !prot[cur] {
					t.Fatalf("iter %d: kept %s but ancestor %s is neither kept nor protected", iter, id, cur)
				}
				cur = parentOf[cur]
			}
		}
	}
}
