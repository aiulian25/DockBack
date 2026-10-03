package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"dockback/internal/backup"
)

// Single-file restore endpoint (F96).
//
// Destructive, but narrowly so: it overwrites ONE file at a path that already
// exists inside the backup. That shapes the gate — full CSRF and the same
// container-scoped exclusive lock a full restore takes (so this can never race a
// restore or a backup of the same stack), but no step-up re-authentication:
// step-up guards key material and account changes, and demanding it to put back
// a config file would train operators to type their password for routine work.

type restoreFileReq struct {
	Path       string `json:"path"`
	TargetID   string `json:"target_id"`
	NodeID     string `json:"node_id"`
	Source     string `json:"source"`
	KeepBackup bool   `json:"keep_backup"`
}

func (s *Server) handleRestoreFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req restoreFileReq
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	req.Path = strings.TrimSpace(req.Path)
	if req.Path == "" {
		errJSON(w, http.StatusBadRequest, "path is required")
		return
	}
	if req.TargetID == "" {
		errJSON(w, http.StatusBadRequest, "target_id is required")
		return
	}

	b, err := s.store.GetBackup(id)
	if err != nil {
		errJSON(w, http.StatusNotFound, "backup not found")
		return
	}
	// Reading an incomplete archive is harmless (the extract endpoint allows it);
	// WRITING from one is not — a failed or still-running backup may hold a
	// half-captured file, and overwriting live data with that is worse than the
	// deletion being recovered from.
	if b.Status != "success" {
		errJSON(w, http.StatusBadRequest, "only a completed backup can be written back from — this one is "+b.Status)
		return
	}
	if backup.IsVolumeOnlyBackup(b) {
		errJSON(w, http.StatusBadRequest, "this is a standalone volume backup — its files have no container path to be written back to; restore the volume instead")
		return
	}
	// The write target may be a different node than the backup came from
	// (cross-host recovery), so the node is taken from the request and validated.
	nodeID := req.NodeID
	if nodeID == "" {
		nodeID = b.NodeID
	}
	if _, nerr := s.store.GetNode(nodeID); nerr != nil {
		errJSON(w, http.StatusNotFound, "target node not found")
		return
	}

	// Same exclusive lock a full restore takes, keyed on the same stack: writing a
	// file into a container mid-backup would capture a half-written state, and
	// mid-restore would be overwritten by it.
	lockKey := stackKey(nodeID, b.Stack, b.TargetName)
	if !s.locks.acquireRestore(lockKey) {
		errJSON(w, http.StatusConflict, "a backup or restore of this stack is already in progress — try again once it finishes")
		return
	}
	defer s.releaseRestoreAndDispatch(lockKey)

	// Audited BEFORE the write, naming the path: a write that fails half way still
	// has to leave a trace of what was attempted.
	detail := "node=" + nodeID + " target=" + req.TargetID + " path=" + req.Path
	if req.KeepBackup {
		detail += " (kept a copy of the previous file)"
	}
	_ = s.store.Audit(userFrom(r), "restore.file", id, detail)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	ferr := s.engine.RestoreOneFile(ctx, b, req.Source, req.Path, nodeID, req.TargetID, req.KeepBackup)
	switch {
	case errors.Is(ferr, backup.ErrEntryNotFound):
		errJSON(w, http.StatusBadRequest, "no such file in this backup")
	case ferr != nil:
		s.logSink(id, "ERR", "Single-file restore of "+req.Path+" failed: "+ferr.Error())
		errJSON(w, http.StatusBadGateway, ferr.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"status": "restored", "path": req.Path})
	}
}
