package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
)

// Config-drift indicator (F73) — the on-demand field breakdown. The container
// page shows only a fingerprint-compare boolean; when the user expands it, this
// endpoint extracts config/inspect.json from the newest successful backup's
// archive and diffs it against the live inspect field by field.

// handleContainerDrift — GET /api/nodes/{id}/containers/{cid}/drift.
func (s *Server) handleContainerDrift(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("id")
	cid := r.PathValue("cid")
	cli, err := s.reg.Get(nodeID)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	d, err := dockercli.InspectContainer(ctx, cli, cid)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}

	// Newest successful backup of this container (by name on this node).
	all, _ := s.store.ListBackupsForTarget(nodeID, d.Name, 1000) // newest first
	for _, b := range all {
		if b.Status != "success" || b.ManifestJSON == "" {
			continue
		}
		var man backup.Manifest
		if json.Unmarshal([]byte(b.ManifestJSON), &man) != nil || man.ConfigFP == "" {
			// Legacy backup without a fingerprint — never claim drift.
			break
		}
		var stored bytes.Buffer
		if err := s.engine.ExtractOne(ctx, b, "", "config/inspect.json", &stored); err != nil {
			errJSON(w, http.StatusInternalServerError, "could not read the backup's stored config: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, backup.ConfigDrift(stored.Bytes(), d.Raw))
		return
	}
	// No comparable backup — no drift claim.
	writeJSON(w, http.StatusOK, backup.DriftReport{})
}
