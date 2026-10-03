package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"dockback/internal/backup"
	"dockback/internal/store"
)

// F70 universal file index: search + diff cores and the legacy-skip path.

func fsIdx(paths ...string) backup.VolIndex {
	v := backup.VolIndex{}
	for i, p := range paths {
		v.Entries = append(v.Entries, backup.FileEntry{Path: p, Size: int64(100 + i), MtimeUnix: int64(1000 + i)})
	}
	return v
}

func TestFileSearchIndexMatch(t *testing.T) {
	b1 := &store.Backup{ID: "g1", TargetName: "app", CreatedAt: 100}
	b2 := &store.Backup{ID: "g2", TargetName: "app", CreatedAt: 200}
	idx := fsIdx("config/Config.XML", "media/photo.jpg", "config/other.yml")

	// Case-insensitive substring, one hit per matching path per generation.
	hits, trunc := searchIndex(nil, b2, idx, "config.xml")
	hits, _ = searchIndex(hits, b1, idx, "config.xml")
	if trunc || len(hits) != 2 {
		t.Fatalf("hits=%d trunc=%v, want 2 hits across two generations", len(hits), trunc)
	}
	if hits[0].BackupID != "g2" || hits[0].Path != "config/Config.XML" || hits[1].BackupID != "g1" {
		t.Fatalf("hit rows wrong: %+v", hits)
	}
	if hits[0].Size != 100 || hits[0].Mtime != 1000 {
		t.Fatalf("size/mtime not carried: %+v", hits[0])
	}

	// The cap stops the scan and reports truncation.
	var big []string
	for i := 0; i < maxFileSearchHits+50; i++ {
		big = append(big, "logs/app.log")
	}
	if hits, trunc = searchIndex(nil, b1, fsIdx(big...), "app.log"); !trunc || len(hits) != maxFileSearchHits {
		t.Fatalf("cap: hits=%d trunc=%v", len(hits), trunc)
	}
}

func TestBackupDiffSplit(t *testing.T) {
	old := backup.VolIndex{Entries: []backup.FileEntry{
		{Path: "keep.txt", Size: 10, MtimeUnix: 1},
		{Path: "changed.db", Size: 50, MtimeUnix: 1},
		{Path: "gone.tmp", Size: 5, MtimeUnix: 1},
	}}
	cur := backup.VolIndex{Entries: []backup.FileEntry{
		{Path: "keep.txt", Size: 10, MtimeUnix: 1},
		{Path: "changed.db", Size: 80, MtimeUnix: 2},
		{Path: "new.jpg", Size: 999, MtimeUnix: 3},
	}}
	added, changed, deleted := splitDiff(old, cur)
	if len(added) != 1 || added[0].Path != "new.jpg" || added[0].Size != 999 {
		t.Fatalf("added wrong: %+v", added)
	}
	if len(changed) != 1 || changed[0].Path != "changed.db" || changed[0].Size != 80 || changed[0].PrevSize != 50 {
		t.Fatalf("changed wrong: %+v", changed)
	}
	if len(deleted) != 1 || deleted[0] != "gone.tmp" {
		t.Fatalf("deleted wrong: %+v", deleted)
	}

	// Identical indexes diff to nothing.
	if a, c, d := splitDiff(cur, cur); len(a)+len(c)+len(d) != 0 {
		t.Fatalf("identical indexes must diff empty: %v %v %v", a, c, d)
	}
}

// TestFileSearchSkipsLegacy: a successful backup WITHOUT a stored index is
// counted as skipped (never an error) and contributes no rows.
func TestFileSearchSkipsLegacy(t *testing.T) {
	st := testStore(t)
	s := &Server{store: st, engine: &backup.Engine{Store: st}}
	b := &store.Backup{ID: "old1", NodeID: "n1", TargetName: "app", Status: "success"}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	b.ManifestJSON = `{"version":1,"target_name":"app"}` // no vol_index → legacy
	if err := st.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/api/backups/search-file?node_id=n1&q=config", nil)
	rec := httptest.NewRecorder()
	s.handleFileSearch(rec, r)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var out struct {
		Results  []fileHit `json:"results"`
		Searched int       `json:"searched"`
		Skipped  int       `json:"skipped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 0 || out.Searched != 0 || out.Skipped != 1 {
		t.Fatalf("legacy skip wrong: %+v", out)
	}

	// Missing params are a clean 400.
	rec = httptest.NewRecorder()
	s.handleFileSearch(rec, httptest.NewRequest("GET", "/api/backups/search-file?q=x", nil))
	if rec.Code != 400 {
		t.Fatalf("missing node_id: code=%d, want 400", rec.Code)
	}
}

// The diff response must carry [] (never null) for every list — the UI maps
// over them directly, and a nil slice marshals to null and crashes the drawer.
func TestBackupDiffListsNeverNull(t *testing.T) {
	same := fsIdx("a.txt", "b.txt")
	added, changed, deleted := splitDiff(same, same)
	if added == nil || changed == nil || deleted == nil {
		t.Fatalf("identical indexes must yield empty (non-nil) lists: %v %v %v", added, changed, deleted)
	}
	js, err := json.Marshal(map[string]any{"added": added, "changed": changed, "deleted": deleted})
	if err != nil {
		t.Fatal(err)
	}
	if s := string(js); strings.Contains(s, "null") {
		t.Fatalf("diff JSON must not contain null lists: %s", s)
	}
	// A changed-only diff (no adds, no deletes — the common config-container case)
	// must still emit [] for the empty categories.
	cur := backup.VolIndex{Entries: []backup.FileEntry{{Path: "a.txt", Size: 9, MtimeUnix: 2}, {Path: "b.txt", Size: 101, MtimeUnix: 1}}}
	added, changed, deleted = splitDiff(same, cur)
	if len(changed) == 0 || added == nil || deleted == nil {
		t.Fatalf("changed-only diff: added=%v changed=%v deleted=%v", added, changed, deleted)
	}
}
