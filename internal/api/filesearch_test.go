package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// historyRow adds one successful backup to the store, created at a given moment.
func historyRow(t *testing.T, st *store.Store, id, target string, createdAt int64) *store.Backup {
	t.Helper()
	b := &store.Backup{ID: id, NodeID: "n1", TargetName: target, Status: "success", CreatedAt: createdAt}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	return b
}

const historyBeyondOnePage = fileSearchBackupsPerPage + 5

// The finding: the sole match lies past the newest thousand node backups, behind a
// busier container's history.
func TestTheOnlyMatchBeyondOnePageIsReached(t *testing.T) {
	st := testStore(t)
	for i := range historyBeyondOnePage {
		historyRow(t, st, fmt.Sprintf("busy%05d", i), "busy", int64(10_000+i))
	}
	historyRow(t, st, "oldmatch", "quiet", 5)
	indexOf := func(b *store.Backup) (backup.VolIndex, bool) {
		if b.ID == "oldmatch" {
			return fsIdx("config.xml"), true
		}
		return fsIdx("unrelated.bin"), true
	}

	var found []fileHit
	cursor := store.BackupCursor{}
	pages, scanned := 0, 0
	for {
		list, err := st.ListSuccessfulBackupsPage("n1", "", cursor, fileSearchBackupsPerPage+1)
		if err != nil {
			t.Fatal(err)
		}
		older := len(list) > fileSearchBackupsPerPage
		list = list[:min(len(list), fileSearchBackupsPerPage)]
		page, last := scanForFile(list, indexOf, "config.xml")
		found = append(found, page.Results...)
		scanned += page.Scanned
		pages++
		if !older {
			break
		}
		cursor = store.BackupCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	if len(found) != 1 || found[0].BackupID != "oldmatch" {
		t.Fatalf("found %+v", found)
	}
	if pages != 2 || scanned != historyBeyondOnePage+1 {
		t.Fatalf("%d pages, %d scanned", pages, scanned)
	}
	quiet, err := st.ListSuccessfulBackupsPage("n1", "quiet", store.BackupCursor{}, fileSearchBackupsPerPage)
	if err != nil || len(quiet) != 1 {
		t.Fatalf("a target's own history is not drowned out by another's: %d %v", len(quiet), err)
	}
}

func fileSearchResponse(t *testing.T, s *Server, query string) fileSearchPage {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleFileSearch(rec, httptest.NewRequest("GET", "/api/backups/search-file?"+query, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", query, rec.Code, rec.Body.String())
	}
	var page fileSearchPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page
}

// Through the handler: a page that leaves history unread says so and carries on.
func TestFileSearchSaysWhenOlderHistoryIsUnread(t *testing.T) {
	st := testStore(t)
	s := &Server{store: st, engine: &backup.Engine{Store: st}}
	for i := range historyBeyondOnePage {
		historyRow(t, st, fmt.Sprintf("b%05d", i), "app", int64(i+1))
	}
	first := fileSearchResponse(t, s, "node_id=n1&q=config")
	if first.Scanned != fileSearchBackupsPerPage || first.NextCursor == "" {
		t.Fatalf("first page: scanned %d, cursor %q", first.Scanned, first.NextCursor)
	}
	second := fileSearchResponse(t, s, "node_id=n1&q=config&cursor="+url.QueryEscape(first.NextCursor))
	if second.Scanned != historyBeyondOnePage-fileSearchBackupsPerPage || second.NextCursor != "" {
		t.Fatalf("second page: scanned %d, cursor %q", second.Scanned, second.NextCursor)
	}
	if first.Skipped+second.Skipped != historyBeyondOnePage {
		t.Fatalf("every indexless backup is counted once: %d + %d", first.Skipped, second.Skipped)
	}
}

// A backup taken mid-search is newer than any cursor, so it neither shifts nor
// repeats the pages behind it.
func TestPagingNeitherSkipsNorRepeatsWhileBackupsArrive(t *testing.T) {
	st := testStore(t)
	const total = 25
	for i := range total {
		historyRow(t, st, fmt.Sprintf("b%03d", i), "app", int64(100+i/3)) // shared timestamps too
	}
	seen := map[string]bool{}
	cursor := store.BackupCursor{}
	for round := 0; ; round++ {
		list, err := st.ListSuccessfulBackupsPage("n1", "app", cursor, 4)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 0 {
			break
		}
		historyRow(t, st, fmt.Sprintf("new%03d", round), "app", int64(1_000+round))
		for _, b := range list {
			if seen[b.ID] {
				t.Fatalf("%s returned twice", b.ID)
			}
			seen[b.ID] = true
		}
		last := list[len(list)-1]
		cursor = store.BackupCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	for i := range total {
		if !seen[fmt.Sprintf("b%03d", i)] {
			t.Fatalf("b%03d was never returned", i)
		}
	}
}

func TestOnlySuccessfulBackupsArePaged(t *testing.T) {
	st := testStore(t)
	historyRow(t, st, "good", "app", 2)
	failed := &store.Backup{ID: "bad", NodeID: "n1", TargetName: "app", Status: "failed", CreatedAt: 3}
	if err := st.CreateBackup(failed); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListSuccessfulBackupsPage("n1", "", store.BackupCursor{}, 10)
	if err != nil || len(list) != 1 || list[0].ID != "good" {
		t.Fatalf("%v %v", list, err)
	}
}

// A capped page has more matches than can be read, so it asks for a narrower query
// rather than offering more.
func TestACappedPageOffersNoContinuation(t *testing.T) {
	big := make([]string, maxFileSearchHits+1)
	for i := range big {
		big[i] = fmt.Sprintf("app%d.log", i)
	}
	list := []*store.Backup{{ID: "b1", CreatedAt: 2}, {ID: "b2", CreatedAt: 1}}
	page, last := scanForFile(list, func(*store.Backup) (backup.VolIndex, bool) { return fsIdx(big...), true }, "app")
	if !page.Truncated || page.Scanned != 1 || last.ID != "b1" {
		t.Fatalf("%+v", page)
	}
}

func TestACursorMustBeOneThisEndpointMade(t *testing.T) {
	st := testStore(t)
	s := &Server{store: st, engine: &backup.Engine{Store: st}}
	for _, bad := range []string{"nonsense", "12.", ".abc", "x.abc"} {
		rec := httptest.NewRecorder()
		s.handleFileSearch(rec, httptest.NewRequest("GET", "/api/backups/search-file?node_id=n1&q=x&cursor="+url.QueryEscape(bad), nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q: %d", bad, rec.Code)
		}
	}
	at, err := parseBackupCursor(formatBackupCursor(&store.Backup{ID: "abc.def", CreatedAt: 42}))
	if err != nil || at.CreatedAt != 42 || at.ID != "abc.def" {
		t.Fatalf("round trip: %+v %v", at, err)
	}
}
