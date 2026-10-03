package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
)

// handleListDatabases lists the app (non-system) databases of a Postgres or
// MySQL/MariaDB container so the UI can offer a per-database backup selection
// (F8). Read-only: it runs a listing query inside the container via the same
// exec path the PITR/extension probes use. Engines without per-database selection
// (mongodb/redis) or a non-database container return supported=false with an
// empty list, so the UI simply omits the selector.
func (s *Server) handleListDatabases(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cid := r.PathValue("cid")
	cli, err := s.reg.Get(id)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	insp, err := cli.ContainerInspect(ctx, cid)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	engine := backup.DBEngine(insp.Config.Image)
	cmd := backup.DBListCommand(engine, insp.Config.Env)
	resp := map[string]any{"engine": engine, "supported": cmd != nil, "databases": []string{}}
	if cmd == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	// The databases can only be listed from a RUNNING engine; if it's stopped the
	// UI falls back to the full-cluster dump (empty selection).
	if insp.State == nil || !insp.State.Running {
		resp["running"] = false
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp["running"] = true
	out, err := dockercli.ExecCapture(ctx, cli, cid, cmd)
	if err != nil {
		// Best-effort: a listing failure (auth, tools) leaves the UI on the
		// full-cluster default rather than erroring the page.
		writeJSON(w, http.StatusOK, resp)
		return
	}
	var dbs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l := strings.TrimSpace(line); l != "" {
			dbs = append(dbs, l)
		}
	}
	if len(dbs) > 0 {
		resp["databases"] = dbs
	}
	writeJSON(w, http.StatusOK, resp)
}
