package api

import (
	"net/http"
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

// splitDiff turns DiffIndex's (changed, deleted) between two generations into
// the UI's three lists — added (new path), changed (existed, size/mtime moved),
// deleted — with per-path size deltas. Pure; reuses the exact same diff the
// incremental engine trusts (F61).
// Every returned list is non-nil so the JSON carries [] rather than null — the
// UI maps over them directly (same convention as handleFileSearch's hits).
func splitDiff(old, cur backup.VolIndex) (added, changed []diffEntry, deleted []string) {
	added, changed, deleted = []diffEntry{}, []diffEntry{}, []string{}
	changedPaths, deletedPaths := backup.DiffIndex(old, cur)
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
	deleted = append(deleted, deletedPaths...)
	return added, changed, deleted
}

// capDiff bounds one diff list, reporting whether it was truncated.
func capDiff[T any](list []T) ([]T, bool) {
	if len(list) > maxDiffEntries {
		return list[:maxDiffEntries], true
	}
	return list, false
}

// handleFileSearch — GET /api/backups/search-file?node_id=&q=[&target=] (F70).
// Searches every indexed successful backup on the node (optionally one target),
// newest first.
func (s *Server) handleFileSearch(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	nodeID := qp.Get("node_id")
	q := strings.TrimSpace(qp.Get("q"))
	target := qp.Get("target")
	if nodeID == "" || q == "" {
		errJSON(w, http.StatusBadRequest, "node_id and q are required")
		return
	}
	list, err := s.store.ListBackups(nodeID, 1000) // newest first
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	hits := []fileHit{}
	searched, skipped, truncated := 0, 0, false
	for _, b := range list {
		if b.Status != "success" || (target != "" && b.TargetName != target) {
			continue
		}
		idx, ok := s.engine.VolIndexCached(r.Context(), b)
		if !ok {
			skipped++ // legacy/non-indexed backup — honest count, never an error
			continue
		}
		searched++
		hits, truncated = searchIndex(hits, b, idx, q)
		if truncated {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"results": hits, "searched": searched, "skipped": skipped, "truncated": truncated,
	})
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
