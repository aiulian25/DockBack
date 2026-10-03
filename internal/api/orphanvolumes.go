package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// Orphaned named volumes (F23): data left behind by a removed container — a backup
// blind spot. These handlers surface such volumes and let each be backed up
// directly (and restored into a re-created named volume).

// handleListOrphanVolumes lists named volumes attached to no container on a node.
func (s *Server) handleListOrphanVolumes(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cli, err := s.reg.Get(id)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	vols, err := dockercli.OrphanVolumes(ctx, cli)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"volumes": vols})
}

// handleBackupOrphanVolume enqueues a backup of a single standalone named volume.
func (s *Server) handleBackupOrphanVolume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := r.PathValue("name")
	if !dockercli.ValidVolumeName(name) {
		errJSON(w, http.StatusBadRequest, "invalid volume name")
		return
	}
	node, err := s.store.GetNode(id)
	if err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	// Confirm the volume actually exists and is orphaned right now, so we don't back
	// up an in-use volume through this path (its container backup is the right tool).
	cli, cerr := s.reg.Get(id)
	if cerr != nil {
		errJSON(w, http.StatusBadGateway, cerr.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	orphans, oerr := dockercli.OrphanVolumes(ctx, cli)
	cancel()
	if oerr != nil {
		errJSON(w, http.StatusBadGateway, oerr.Error())
		return
	}
	found := false
	for _, v := range orphans {
		if v.Name == name {
			found = true
			break
		}
	}
	if !found {
		errJSON(w, http.StatusConflict, "that volume is attached to a container (or no longer exists) — back it up via its container instead")
		return
	}

	bid := s.enqueueBackup(node.Name, backup.Options{
		NodeID:      id,
		VolumeOnly:  name,
		Compression: "balanced",
	}, prioInteractive)
	_ = s.store.Audit(userFrom(r), "backup.volume", name, "node="+id)
	writeJSON(w, http.StatusAccepted, map[string]string{"backup_id": bid, "status": "started"})
}

// volumeRestoreOptions builds the engine options for a standalone-volume restore
// (F23/F208). Separated from the handler so the mapping is testable — it is the
// place a field silently going missing costs the operator their data, which is
// exactly what happened to Snapshot.
func volumeRestoreOptions(backupID string, req restoreReq) backup.RestoreOptions {
	return backup.RestoreOptions{
		BackupID: backupID,
		NodeID:   req.NodeID,
		Volumes:  true,
		Source:   req.Source,
		// F208: the operator's "Snapshot current state before restoring" choice.
		// It was dropped here, so a volume restore overwrote live data with no
		// rollback point no matter what the dialog said — while the container path
		// had taken one since PLAN §3.7.
		Snapshot: req.Snapshot,
	}
}

// restoreVolumeBackup launches a standalone-volume restore (F23): it recreates the
// named volume and untars its backed-up contents. No container target is involved,
// so it takes a volume-scoped exclusive lock and runs the engine's volume path.
func (s *Server) restoreVolumeBackup(w http.ResponseWriter, r *http.Request, b *store.Backup, req restoreReq) {
	// F208: "restore as a copy" does not exist for a standalone volume, and
	// silently accepting it was worse than not offering it. The request was taken,
	// the new name was DISCARDED, and the original volume was overwritten — while
	// the dialog that sent it had just promised the original would be left
	// untouched. Refused with the reason, mirroring how a single-file restore
	// refuses a volume backup (restorefile.go).
	// Any non-empty value, NOT a trimmed one — the container path decides
	// "is this a clone?" with the same untrimmed test (handlers.go), and a
	// whitespace-only name trimmed away here would fall through to exactly the
	// silent in-place overwrite this refusal exists to prevent.
	if req.AsName != "" {
		errJSON(w, http.StatusBadRequest,
			"this is a standalone volume backup — it cannot be restored as a copy under a different name; it restores into its own volume, overwriting what is there")
		return
	}
	vol := strings.TrimPrefix(b.TargetName, "volume:")
	lockKey := stackKey(req.NodeID, "", "volume:"+vol)
	if !s.locks.acquireRestore(lockKey) {
		errJSON(w, http.StatusConflict, "a restore of this volume is already in progress — try again once it finishes")
		return
	}
	// F218: the known-bad gate is checked for this path too (handleRestore, before
	// the dispatch here), so its override has to be recorded here too — otherwise
	// the one restore type whose archive is pure data loses the note.
	vdetail := "node=" + req.NodeID + " volume=" + vol + " snapshot=" + boolWord(req.Snapshot)
	if req.ConfirmUnverified && b.Verified == "failed" {
		vdetail += " (verification FAILED — override confirmed)"
	}
	_ = s.store.Audit(userFrom(r), "restore.volume", b.ID, vdetail)
	id := b.ID
	opts := volumeRestoreOptions(id, req)
	go func() {
		defer guardPanic("restore", id, func() { s.logSink(id, "ERR", "Restore failed: internal error (panic)") })
		defer s.releaseRestoreAndDispatch(lockKey)
		// F208: registered like every other restore, so it appears in
		// GET /api/restores and can be CANCELED. It was a bare goroutine —
		// invisible and unstoppable — which the safety snapshot makes materially
		// worse, since the run now begins by capturing the whole volume before it
		// reaches the part the operator was waiting for.
		ctx, finish, ok := s.beginRestoreRun(context.Background(), id, "volume "+vol, req.NodeID, 2*time.Hour)
		if !ok {
			s.logSink(id, "ERR", "Restore failed: another restore of this backup is already running")
			return
		}
		defer finish()
		// F208: reported through the shared outcome helper now that this restore can
		// be canceled — a cancel is the operator's own decision, and logging it as
		// "Restore failed" would read as a defect and raise the wrong alert. Same
		// reporting the container and stack paths already use.
		s.restoreOutcome(id, s.engine.Restore(ctx, opts), "Restore completed")
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}
