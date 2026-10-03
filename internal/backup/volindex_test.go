package backup

import (
	"math/rand"
	"reflect"
	"strconv"
	"testing"
)

func TestVolIndexParseAndBuildCmd(t *testing.T) {
	cmd := BuildIndexCmd([]string{"/config", "/data", ""})
	want := []string{"find", "/config", "/data", "-type", "f", "-exec", "stat", "-c", "%n|%s|%Y|%f", "{}", "+"}
	if !reflect.DeepEqual(cmd, want) {
		t.Fatalf("BuildIndexCmd = %v, want %v", cmd, want)
	}

	// Real busybox-stat-shaped output, including a path that itself contains '|'
	// and a malformed line that must be skipped.
	out := []byte("/config/app.conf|1024|1700000000|81a4\n" +
		"/config/weird|name.txt|10|1700000001|81a4\n" +
		"garbage line without pipes\n" +
		"/data/db.sqlite|4096|1700000002|81a4\n")
	idx, err := ParseIndexOutput(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Entries) != 3 {
		t.Fatalf("parsed %d entries, want 3: %+v", len(idx.Entries), idx.Entries)
	}
	// Sorted by path, leading slash stripped.
	if idx.Entries[0].Path != "config/app.conf" || idx.Entries[0].Size != 1024 || idx.Entries[0].MtimeUnix != 1700000000 {
		t.Errorf("entry0 wrong: %+v", idx.Entries[0])
	}
	if idx.Entries[0].Mode != 0x81a4 {
		t.Errorf("mode parse wrong: %x", idx.Entries[0].Mode)
	}
	// The pipe-containing path must survive (split-from-right).
	if idx.Entries[1].Path != "config/weird|name.txt" {
		t.Errorf("pipe path lost: %q", idx.Entries[1].Path)
	}
}

func TestVolIndexDiffDetectsAddModifyDelete(t *testing.T) {
	prev := VolIndex{Entries: []FileEntry{
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
		VolIndex{Entries: []FileEntry{{Path: "x", Size: 1, MtimeUnix: 1}}},
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
