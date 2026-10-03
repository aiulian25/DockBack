package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"dockback/internal/backup"
	"dockback/internal/store"
)

// Layout migration (F77): a one-shot, resumable background job that moves
// legacy flat-key archives into the canonical per-stack folder layout on every
// location. One backup at a time; safe to re-run (already-canonical rows are
// not candidates); WORM-locked backups are skipped with a note.

// migrateLayoutState is the job's live progress, returned by the GET endpoint.
type migrateLayoutState struct {
	Running     bool  `json:"running"`
	Total       int   `json:"total"`
	Moved       int   `json:"moved"`
	SkippedWORM int   `json:"skipped_worm"`
	Failed      int   `json:"failed"`
	Remaining   int   `json:"remaining"`
	StartedAt   int64 `json:"started_at,omitempty"`
}

// migrateCandidate pairs a row with its parsed manifest + target key.
type migrateCandidate struct {
	b      *store.Backup
	newKey string
}

// migrateLayoutCandidates returns the successful backups whose storage key is
// not canonical yet, with their target keys. Pure over the loaded list.
func migrateLayoutCandidates(all []*store.Backup) []migrateCandidate {
	var out []migrateCandidate
	for _, b := range all {
		if b.Status != "success" || b.StorageKey == "" {
			continue
		}
		var man backup.Manifest
		if b.ManifestJSON != "" {
			_ = json.Unmarshal([]byte(b.ManifestJSON), &man)
		}
		if backup.IsCanonicalKey(b, &man) {
			continue
		}
		out = append(out, migrateCandidate{b: b, newKey: backup.CanonicalKey(b, &man)})
	}
	return out
}

func (s *Server) migrateLayoutGet() migrateLayoutState {
	s.migrateMu.Lock()
	defer s.migrateMu.Unlock()
	return s.migrateLayout
}

// handleMigrateLayoutStatus — GET /api/maintenance/migrate-layout. When idle it
// also reports how many rows WOULD move (remaining), so the button can say
// "N backups still use the old layout" before anything runs.
func (s *Server) handleMigrateLayoutStatus(w http.ResponseWriter, r *http.Request) {
	st := s.migrateLayoutGet()
	if !st.Running {
		if all, err := s.store.ListBackups("", 100000); err == nil {
			st.Remaining = len(migrateLayoutCandidates(all))
		}
	}
	writeJSON(w, http.StatusOK, st)
}

// handleMigrateLayout — POST /api/maintenance/migrate-layout (auth+csrf,
// audited). Starts the background sweep; 409 while one is running.
func (s *Server) handleMigrateLayout(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.ListBackups("", 100000)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	cands := migrateLayoutCandidates(all)

	s.migrateMu.Lock()
	if s.migrateLayout.Running {
		s.migrateMu.Unlock()
		errJSON(w, http.StatusConflict, "a layout migration is already running")
		return
	}
	s.migrateLayout = migrateLayoutState{Running: true, Total: len(cands), Remaining: len(cands), StartedAt: time.Now().Unix()}
	s.migrateMu.Unlock()

	_ = s.store.Audit(userFrom(r), "maintenance.migrate_layout", "", fmt.Sprintf("candidates=%d", len(cands)))

	go func() {
		defer func() {
			s.migrateMu.Lock()
			s.migrateLayout.Running = false
			s.migrateMu.Unlock()
		}()
		defer guardPanic("migrate-layout", "", func() { s.logSink("migrate", "ERR", "Layout migration aborted: internal error (panic)") })

		s.logSink("migrate", "INFO", fmt.Sprintf("Layout migration started — %d backup(s) on the old flat layout", len(cands)))
		for _, c := range cands {
			// Fresh full row: the loaded list may be stale (retention/delete since).
			b, gerr := s.store.GetBackup(c.b.ID)
			if gerr != nil || b.Status != "success" || b.StorageKey != c.b.StorageKey {
				s.migrateBump(func(st *migrateLayoutState) { st.Remaining-- })
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
			moved, worm, merr := s.engine.MigrateBackupLayout(ctx, b, c.newKey)
			cancel()
			switch {
			case merr != nil:
				s.migrateBump(func(st *migrateLayoutState) { st.Failed++; st.Remaining-- })
				s.logSink("migrate", "ERR", fmt.Sprintf("Layout migration: %s (%s) failed: %v — old objects kept", b.TargetName, b.ID, merr))
			case worm:
				s.migrateBump(func(st *migrateLayoutState) { st.SkippedWORM++; st.Remaining-- })
				s.logSink("migrate", "INFO", fmt.Sprintf("Layout migration: %s (%s) skipped — an immutable (WORM) copy cannot be moved; it ages out naturally", b.TargetName, b.ID))
			case moved:
				s.migrateBump(func(st *migrateLayoutState) { st.Moved++; st.Remaining-- })
				s.logSink("migrate", "INFO", fmt.Sprintf("Layout migration: %s → %s", b.TargetName, c.newKey))
			default:
				s.migrateBump(func(st *migrateLayoutState) { st.Remaining-- }) // became canonical meanwhile
			}
		}
		st := s.migrateLayoutGet()
		s.logSink("migrate", "INFO", fmt.Sprintf("Layout migration finished — %d moved, %d skipped (immutable), %d failed of %d.", st.Moved, st.SkippedWORM, st.Failed, st.Total))
		_ = s.store.Audit("migrate", "maintenance.migrate_layout.done", "", fmt.Sprintf("moved=%d skipped_worm=%d failed=%d total=%d", st.Moved, st.SkippedWORM, st.Failed, st.Total))
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{"status": "started", "candidates": len(cands)})
}

func (s *Server) migrateBump(fn func(*migrateLayoutState)) {
	s.migrateMu.Lock()
	fn(&s.migrateLayout)
	s.migrateMu.Unlock()
}
