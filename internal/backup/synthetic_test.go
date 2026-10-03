package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"dockback/internal/storage"
	"dockback/internal/store"
)

func synthEngine(t *testing.T) *Engine {
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
	return &Engine{Store: st, Storage: be, Key: key, KeyFP: KeyFingerprint(key), WorkDir: t.TempDir(), Log: func(string, string, string) {}}
}

// mkChainGen packs one archived chain generation: payload member (full or
// delta) + the stored index, with real encryption/manifest/catalog rows so
// streamArchive reads it back exactly like production.
func mkChainGen(t *testing.T, e *Engine, id, parentID, parentPin string, depth int, files map[string][]byte, dirs []string, idx VolIndex, deleted []string) *store.Backup {
	t.Helper()
	work := t.TempDir()
	incremental := parentID != ""
	member := "volumes.tar"
	if incremental {
		member = volumeDeltaMember
	}
	var vbuf bytes.Buffer
	vtw := tar.NewWriter(&vbuf)
	for _, d := range dirs {
		if err := vtw.WriteHeader(&tar.Header{Name: d + "/", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
			t.Fatal(err)
		}
	}
	// Deterministic member order.
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for _, n := range names {
		tarAdd(t, vtw, n, files[n])
	}
	if err := vtw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, member), vbuf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeVolIndex(work, idx); err != nil {
		t.Fatal(err)
	}
	man := &Manifest{
		Version: ManifestVersion, BackupID: id, TargetName: "app",
		KeyFingerprint: e.KeyFP, Format: Format{Algorithm: "zstd", Archive: "tar"},
		Incremental: incremental, Parent: parentID, ParentCipherSHA256: parentPin,
		ChainDepth: depth, Deleted: deleted, VolIndex: volumeIndexMember,
		Volumes: []VolumeRef{{Destination: "/data", Type: "volume", Name: "data"}},
	}
	skey := "node/app/" + id + ".dback"
	cipherSHA, _, err := e.packEncryptStore(context.Background(), work, man, skey, "zstd", zstd.SpeedDefault, 0)
	if err != nil {
		t.Fatalf("pack %s: %v", id, err)
	}
	// Mirror storeAndVerify: the catalog manifest-of-record carries the archive's
	// cipher sha — the value child deltas pin against.
	man.CipherSHA256 = cipherSHA
	synthTestSeq++
	b := &store.Backup{ID: id, NodeID: "n1", Status: "success", TargetName: "app", CreatedAt: 1_000_000 + synthTestSeq}
	if err := e.Store.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	b.CipherSHA256 = cipherSHA
	b.StorageKey = skey
	mb, _ := json.Marshal(man)
	b.ManifestJSON = string(mb)
	b.LocationsJSON = mustJSON([]Location{{Kind: "local", Name: "local", Type: "local"}})
	if err := e.Store.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// synthTestSeq gives every fabricated row a distinct, increasing CreatedAt so
// newest-first catalog ordering is deterministic in tests.
var synthTestSeq int64

func idxOf(entries ...FileEntry) VolIndex { return VolIndex{Entries: entries} }
func fe(p string, size, mtime int64) FileEntry {
	return FileEntry{Path: p, Size: size, MtimeUnix: mtime, Mode: 0o644}
}

// localDelta writes a plain local tar (the freshly-spooled delta) and returns its path.
func localDelta(t *testing.T, files map[string][]byte) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for n, d := range files {
		tarAdd(t, tw, n, d)
	}
	_ = tw.Close()
	p := filepath.Join(t.TempDir(), "fresh.tar")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// readTar returns member name→content for regular files and the set of
// non-regular member names.
func readTar(t *testing.T, path string) (map[string]string, map[string]bool) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	files, other := map[string]string{}, map[string]bool{}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			files[cleanEntryName(h.Name)] = string(b)
		} else {
			other[cleanEntryName(h.Name)] = true
		}
	}
	return files, other
}

// F85 core: baseline{a,b} + delta{b',c, Deleted:[a]} + fresh{d} merges to
// exactly {b',c,d}, newest bytes win, baseline dir structure kept, deleted
// files gone — and the merge is byte-deterministic.
func TestSyntheticMerge(t *testing.T) {
	e := synthEngine(t)
	ctx := context.Background()

	baseIdx := idxOf(fe("a.txt", 2, 100), fe("sub/b.txt", 2, 100))
	b1 := mkChainGen(t, e, "gen1full0001", "", "", 0,
		map[string][]byte{"a.txt": []byte("A1"), "sub/b.txt": []byte("B1")}, []string{"sub"}, baseIdx, nil)

	d2Idx := idxOf(fe("c.txt", 2, 200), fe("sub/b.txt", 2, 200)) // b modified, a deleted
	b2 := mkChainGen(t, e, "gen2delta001", b1.ID, b1.CipherSHA256, 1,
		map[string][]byte{"sub/b.txt": []byte("B2"), "c.txt": []byte("C1")}, nil, d2Idx, []string{"a.txt"})

	freshIdx := idxOf(fe("c.txt", 2, 200), fe("d.txt", 2, 300), fe("sub/b.txt", 2, 200))
	freshChanged := []string{"d.txt"}
	fresh := localDelta(t, map[string][]byte{"d.txt": []byte("D1")})

	var b2man Manifest
	_ = json.Unmarshal([]byte(b2.ManifestJSON), &b2man)
	chain, err := e.resolveRestoreChain(b2, &b2man)
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	if len(chain) != 2 || chain[0].ID != b1.ID || chain[1].ID != b2.ID {
		t.Fatalf("chain order wrong: %+v", chain)
	}

	dst := filepath.Join(t.TempDir(), "volumes.tar")
	sha, files, err := e.synthesizeFullVolumes(ctx, chain, fresh, freshIdx, freshChanged, dst)
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if files != 3 {
		t.Fatalf("merged file count = %d, want 3", files)
	}
	got, other := readTar(t, dst)
	want := map[string]string{"sub/b.txt": "B2", "c.txt": "C1", "d.txt": "D1"}
	if len(got) != len(want) {
		t.Fatalf("merged files = %v, want %v", got, want)
	}
	for p, v := range want {
		if got[p] != v {
			t.Fatalf("merged %s = %q, want %q (newest-wins violated)", p, got[p], v)
		}
	}
	if _, exists := got["a.txt"]; exists {
		t.Fatal("deleted file resurrected from the baseline")
	}
	if !other["sub"] {
		t.Fatal("baseline directory structure must be carried")
	}

	// Determinism: a second merge is byte-identical.
	dst2 := filepath.Join(t.TempDir(), "volumes2.tar")
	sha2, _, err := e.synthesizeFullVolumes(ctx, chain, fresh, freshIdx, freshChanged, dst2)
	if err != nil || sha2 != sha {
		t.Fatalf("merge must be deterministic: %s vs %s (err %v)", sha, sha2, err)
	}
}

// A file deleted mid-chain and re-added later comes back with the NEW bytes.
func TestSyntheticMergeResurrection(t *testing.T) {
	e := synthEngine(t)
	b1 := mkChainGen(t, e, "res1full0001", "", "", 0,
		map[string][]byte{"a.txt": []byte("OLD")}, nil, idxOf(fe("a.txt", 3, 100)), nil)
	b2 := mkChainGen(t, e, "res2delta001", b1.ID, b1.CipherSHA256, 1,
		map[string][]byte{}, nil, idxOf(), []string{"a.txt"}) // a deleted

	freshIdx := idxOf(fe("a.txt", 3, 300)) // re-added
	fresh := localDelta(t, map[string][]byte{"a.txt": []byte("NEW")})

	var b2man Manifest
	_ = json.Unmarshal([]byte(b2.ManifestJSON), &b2man)
	chain, err := e.resolveRestoreChain(b2, &b2man)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "v.tar")
	_, files, err := e.synthesizeFullVolumes(context.Background(), chain, fresh, freshIdx, []string{"a.txt"}, dst)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := readTar(t, dst)
	if files != 1 || got["a.txt"] != "NEW" {
		t.Fatalf("resurrected file must carry the new bytes: %v (files=%d)", got, files)
	}
}

// Fail-closed inputs: a tampered chain pin refuses to resolve, and a chain
// link whose manifest is unreadable refuses to merge — both trigger the
// engine's fallback to a source-read full.
func TestSyntheticMergeFailsClosed(t *testing.T) {
	e := synthEngine(t)
	b1 := mkChainGen(t, e, "fc1full00001", "", "", 0,
		map[string][]byte{"a.txt": []byte("A")}, nil, idxOf(fe("a.txt", 1, 100)), nil)
	b2 := mkChainGen(t, e, "fc2delta0001", b1.ID, b1.CipherSHA256, 1,
		map[string][]byte{"b.txt": []byte("B")}, nil, idxOf(fe("a.txt", 1, 100), fe("b.txt", 1, 200)), nil)

	// Tamper the pin: resolveRestoreChain must refuse.
	var man Manifest
	_ = json.Unmarshal([]byte(b2.ManifestJSON), &man)
	man.ParentCipherSHA256 = strings.Repeat("0", 64)
	if _, err := e.resolveRestoreChain(b2, &man); err == nil {
		t.Fatal("tampered chain pin must refuse to resolve")
	}

	// A chain entry without a manifest must refuse to merge.
	bogus := []*store.Backup{{ID: "nomanifest01", NodeID: "n1", Status: "success", TargetName: "app"}}
	if _, _, err := e.synthesizeFullVolumes(context.Background(), bogus, localDelta(t, nil), idxOf(), nil, filepath.Join(t.TempDir(), "x.tar")); err == nil {
		t.Fatal("chain link without a manifest must fail the merge")
	}
}

// F85 step 6: after a synthetic full (a normal full: no parent, depth 0, index
// present) the next delta parents on it exactly like after a real full, and
// the boundary is detected via atCap.
func TestIncrementalParentAfterSyntheticFull(t *testing.T) {
	e := synthEngine(t)
	syn := mkChainGen(t, e, "synfull00001", "", "", 0,
		map[string][]byte{"a.txt": []byte("A")}, nil, idxOf(fe("a.txt", 1, 100)), nil)

	p, atCap := e.incrementalParent("n1", "app", []string{"/data"}, 7, "self")
	if p == nil || p.ID != syn.ID || atCap {
		t.Fatalf("post-synthetic delta must parent on the synthetic full: p=%v atCap=%v", p, atCap)
	}

	// At the cap the tip is still returned, flagged.
	deep := mkChainGen(t, e, "deepdelta001", syn.ID, syn.CipherSHA256, 6,
		map[string][]byte{"z.txt": []byte("Z")}, nil, idxOf(fe("a.txt", 1, 100), fe("z.txt", 1, 700)), nil)
	p, atCap = e.incrementalParent("n1", "app", []string{"/data"}, 7, "self")
	if p == nil || p.ID != deep.ID || !atCap {
		t.Fatalf("depth cap must return the tip with atCap=true: p=%v atCap=%v", p, atCap)
	}
	// A changed mount selection is a structural reset — no synthetic merge.
	if p, atCap := e.incrementalParent("n1", "app", []string{"/other"}, 7, "self"); p != nil || atCap {
		t.Fatalf("changed mount set must force a plain full: p=%v atCap=%v", p, atCap)
	}
}
