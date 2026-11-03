package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"dockback/internal/backup"
	"dockback/internal/store"
)

// Universal file index consumers (F70): cross-backup file search and
// generation diff. Both are INDEX-ONLY operations — one small decrypted member
// per backup (memoized in the engine's bounded cache), never a full-archive
// walk. Legacy backups without a stored index are skipped by search and
// rejected by diff with a clear message; browsing them still works via the
// streaming fallback in ListEntries.

const (
	maxFileSearchHits = 500  // newest-first result cap
	maxDiffEntries    = 2000 // per-list (added/changed/deleted) cap
	// fileSearchBackupsPerPage bounds how many backups one search request reads.
	fileSearchBackupsPerPage = 1000
	backupCursorSeparator    = "."
)

// fileHit is one search result: a path as it existed in one backup generation.
type fileHit struct {
	BackupID  string `json:"backup_id"`
	CreatedAt int64  `json:"created_at"`
	Target    string `json:"target"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Mtime     int64  `json:"mtime"`
}

// searchIndex appends q's case-insensitive substring matches from one backup's
// index to hits, stopping at the cap. Pure. Returns the new slice and whether
// the cap was hit.
func searchIndex(hits []fileHit, b *store.Backup, idx backup.VolIndex, q string) ([]fileHit, bool) {
	lower := strings.ToLower(q)
	for _, fe := range idx.Entries {
		if !strings.Contains(strings.ToLower(fe.Path), lower) {
			continue
		}
		hits = append(hits, fileHit{BackupID: b.ID, CreatedAt: b.CreatedAt, Target: b.TargetName, Path: fe.Path, Size: fe.Size, Mtime: fe.MtimeUnix})
		if len(hits) >= maxFileSearchHits {
			return hits, true
		}
	}
	return hits, false
}

// diffEntry is one added/changed path in a generation diff, with size context.
type diffEntry struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	PrevSize int64  `json:"prev_size,omitempty"` // changed entries only
}

// splitDiff turns the difference in files between two generations into the UI's
// three lists — added (new path), changed (existed, size/mtime moved), deleted —
// with per-path size deltas. Pure. It judges content, not what a delta copies: a
// chown -R would list every file, and two index versions would differ throughout.
// Every returned list is non-nil so the JSON carries [] rather than null — the
// UI maps over them directly (same convention as handleFileSearch's hits).
func splitDiff(old, cur backup.VolIndex) (added, changed []diffEntry, deleted []string) {
	added, changed, deleted = []diffEntry{}, []diffEntry{}, []string{}
	changedPaths := backup.ContentChangedFiles(old, cur)
	_, gonePaths := backup.DiffIndex(old, cur)
	oldBy := make(map[string]int64, len(old.Entries))
	for _, fe := range old.Entries {
		oldBy[fe.Path] = fe.Size
	}
	curBy := make(map[string]int64, len(cur.Entries))
	for _, fe := range cur.Entries {
		curBy[fe.Path] = fe.Size
	}
	for _, p := range changedPaths {
		if prev, existed := oldBy[p]; existed {
			changed = append(changed, diffEntry{Path: p, Size: curBy[p], PrevSize: prev})
		} else {
			added = append(added, diffEntry{Path: p, Size: curBy[p]})
		}
	}
	deleted = append(deleted, backup.DeletedFiles(old, gonePaths)...)
	return added, changed, deleted
}

// capDiff bounds one diff list, reporting whether it was truncated.
func capDiff[T any](list []T) ([]T, bool) {
	if len(list) > maxDiffEntries {
		return list[:maxDiffEntries], true
	}
	return list, false
}

// fileSearchPage is one call's worth of a search: what it found, how much history it
// read, and where to carry on. NextCursor is empty when nothing older is left unread,
// or when the hit cap stopped the page, which Truncated says. So a client can never
// take part of the history for all of it.
type fileSearchPage struct {
	Results []fileHit `json:"results"`
	// Searched and Skipped count the backups this page read with and without an index.
	Searched int `json:"searched"`
	Skipped  int `json:"skipped"`
	// Truncated says the hit cap stopped this page.
	Truncated bool `json:"truncated"`
	// Scanned is how many backups this page examined.
	Scanned int `json:"scanned"`
	// NextCursor carries on into older history, when there is some.
	NextCursor string `json:"next_cursor,omitempty"`
}

// scanForFile searches a page of backups, newest first, stopping at the hit cap.
// indexOf returns a backup's index, or false for one that has none. It returns the
// page and the last backup it examined, from which a continuation starts. Pure.
func scanForFile(list []*store.Backup, indexOf func(*store.Backup) (backup.VolIndex, bool), q string) (fileSearchPage, *store.Backup) {
	page := fileSearchPage{Results: []fileHit{}}
	var last *store.Backup
	for _, b := range list {
		last = b
		page.Scanned++
		idx, ok := indexOf(b)
		if !ok {
			page.Skipped++ // legacy/non-indexed backup — honest count, never an error
			continue
		}
		page.Searched++
		page.Results, page.Truncated = searchIndex(page.Results, b, idx, q)
		if page.Truncated {
			break
		}
	}
	return page, last
}

// formatBackupCursor and parseBackupCursor carry a store.BackupCursor through a URL.
func formatBackupCursor(b *store.Backup) string {
	return strconv.FormatInt(b.CreatedAt, 10) + backupCursorSeparator + b.ID
}

func parseBackupCursor(raw string) (store.BackupCursor, error) {
	if raw == "" {
		return store.BackupCursor{}, nil
	}
	createdAt, id, ok := strings.Cut(raw, backupCursorSeparator)
	at, err := strconv.ParseInt(createdAt, 10, 64)
	if !ok || err != nil || id == "" {
		return store.BackupCursor{}, errors.New("malformed cursor")
	}
	return store.BackupCursor{CreatedAt: at, ID: id}, nil
}

// handleFileSearch — GET /api/backups/search-file?node_id=&q=[&target=][&cursor=]
// (F70). Searches the node's successful backups (optionally one target's), newest
// first, a page at a time. A response that leaves older history unread says so with
// next_cursor, which the next request passes back to carry on.
func (s *Server) handleFileSearch(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	nodeID := qp.Get("node_id")
	q := strings.TrimSpace(qp.Get("q"))
	target := qp.Get("target")
	if nodeID == "" || q == "" {
		errJSON(w, http.StatusBadRequest, "node_id and q are required")
		return
	}
	after, err := parseBackupCursor(qp.Get("cursor"))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "cursor must be one this endpoint returned")
		return
	}
	// One past the page, to learn whether older history remains without a count query.
	list, err := s.store.ListSuccessfulBackupsPage(nodeID, target, after, fileSearchBackupsPerPage+1)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	olderRemains := len(list) > fileSearchBackupsPerPage
	list = list[:min(len(list), fileSearchBackupsPerPage)]
	page, last := scanForFile(list, func(b *store.Backup) (backup.VolIndex, bool) {
		return s.engine.VolIndexCached(r.Context(), b)
	}, q)
	// A capped page has more matches than anyone can read, so it offers a narrower
	// query rather than more of the same.
	if olderRemains && !page.Truncated {
		page.NextCursor = formatBackupCursor(last)
	}
	writeJSON(w, http.StatusOK, page)
}

// handleBackupDiff — GET /api/backups/diff?a=&b= (F70). Both backups must be
// successful, indexed, and share (node, target); the diff always reads
// older → newer regardless of argument order.
func (s *Server) handleBackupDiff(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	ba, err := s.store.GetBackup(qp.Get("a"))
	if err != nil {
		errJSON(w, http.StatusNotFound, "backup a not found")
		return
	}
	bb, err := s.store.GetBackup(qp.Get("b"))
	if err != nil {
		errJSON(w, http.StatusNotFound, "backup b not found")
		return
	}
	if ba.NodeID != bb.NodeID || ba.TargetName != bb.TargetName {
		errJSON(w, http.StatusBadRequest, "both backups must belong to the same container")
		return
	}
	if ba.Status != "success" || bb.Status != "success" {
		errJSON(w, http.StatusBadRequest, "both backups must be successful")
		return
	}
	if ba.CreatedAt > bb.CreatedAt {
		ba, bb = bb, ba // normalize to older → newer
	}
	oldIdx, ok := s.engine.VolIndexCached(r.Context(), ba)
	if !ok {
		errJSON(w, http.StatusBadRequest, "the older backup has no file index (made before indexing existed) — cannot diff")
		return
	}
	newIdx, ok := s.engine.VolIndexCached(r.Context(), bb)
	if !ok {
		errJSON(w, http.StatusBadRequest, "the newer backup has no file index (made before indexing existed) — cannot diff")
		return
	}
	added, changed, deleted := splitDiff(oldIdx, newIdx)
	addedC, t1 := capDiff(added)
	changedC, t2 := capDiff(changed)
	deletedC, t3 := capDiff(deleted)
	writeJSON(w, http.StatusOK, map[string]any{
		"a":     map[string]any{"id": ba.ID, "created_at": ba.CreatedAt},
		"b":     map[string]any{"id": bb.ID, "created_at": bb.CreatedAt},
		"added": addedC, "changed": changedC, "deleted": deletedC,
		"counts":    map[string]int{"added": len(added), "changed": len(changed), "deleted": len(deleted)},
		"truncated": t1 || t2 || t3,
	})
}
