package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Persisted per-run logs (F8): the live SSE stream is in-memory only, so a
// finished backup/restore/mirror run's log is lost on reload. logSink also
// appends every line to the run_logs table keyed by backup id; these handlers
// serve it back as JSON (for the UI) or as a plain-text download.

// handleRunLog returns a run's persisted log lines as JSON.
func (s *Server) handleRunLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	lines, err := s.store.GetRunLog(id)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backup_id": id, "lines": lines})
}

// handleOpsLog returns the persisted fleet-wide activity log for one reserved
// source (schedule/queue/critical/notify), so operational history survives a
// restart (F46). Only the fixed reserved sources are readable — the "ops:" key is
// server-controlled, never derived from arbitrary input.
func (s *Server) handleOpsLog(w http.ResponseWriter, r *http.Request) {
	src := r.PathValue("source")
	if !reservedLogSources[src] {
		errJSON(w, http.StatusNotFound, "unknown activity source")
		return
	}
	lines, err := s.store.GetRunLog("ops:" + src)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"source": src, "lines": lines})
}

// handleRunLogDownload streams a run's persisted log as a text/plain attachment.
func (s *Server) handleRunLogDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	lines, err := s.store.GetRunLog(id)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "dockback-"+safeLogName(id)+".log.txt"))
	for _, l := range lines {
		ts := time.Unix(l.TS, 0).UTC().Format(time.RFC3339)
		fmt.Fprintf(w, "%s  %-4s  %s\n", ts, l.Level, l.Msg)
	}
}

// safeLogName reduces a backup id to a filesystem/header-safe token for the
// download filename (defense in depth — ids are already opaque tokens).
func safeLogName(id string) string {
	var b strings.Builder
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			b.WriteRune(c)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "run"
	}
	return b.String()
}
