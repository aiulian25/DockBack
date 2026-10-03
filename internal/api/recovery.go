package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"dockback/internal/version"
)

// Offline recovery tool (F35): "restore without DockBack".
//
// DockBack embeds dockback-recover.py — a single, dependency-free Python script
// that reimplements the DBACKv1 on-disk format exactly — so the running instance
// can hand you the tool AND its fingerprint. The recovery sheet (KeyEscrowModal)
// prints that fingerprint next to the key, turning the printed sheet into a
// complete, self-verifying recovery kit: with the key, the script, and a `.dback`
// file you can decrypt on any machine with Python, even if DockBack is gone.

// recoveryToolRepo is the public image-only repo that publishes the script as a
// per-version release asset (the release checklist attaches the SAME file, so its hash
// matches recoveryToolSHA for this build).
const recoveryToolRepo = "aiulian25/dockback"

// SetRecoveryTool registers the embedded recovery script and precomputes its
// SHA-256, so the fingerprint served to the UI is the exact script this build
// ships (no hardcoded value). Called once at startup from main.
func (s *Server) SetRecoveryTool(name string, data []byte) {
	s.recoveryToolName = name
	s.recoveryTool = data
	if len(data) > 0 {
		sum := sha256.Sum256(data)
		s.recoveryToolSHA = hex.EncodeToString(sum[:])
	}
}

// recoveryReleaseURL is the GitHub release-asset URL for the running version's
// script (a stable "latest" fallback for unstamped/dev builds).
func recoveryReleaseURL(filename string) string {
	tag := "latest/download"
	if v := version.Version; strings.HasPrefix(v, "v") {
		tag = "download/" + v
	}
	return "https://github.com/" + recoveryToolRepo + "/releases/" + tag + "/" + filename
}

// handleRecoveryTool returns metadata for the recovery kit: the script's filename,
// its SHA-256 fingerprint, the per-version release-asset URL, and a one-line
// invocation — all real values derived from the embedded script and build version.
func (s *Server) handleRecoveryTool(w http.ResponseWriter, r *http.Request) {
	name := s.recoveryToolName
	if name == "" {
		name = "dockback-recover.py"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"filename":    name,
		"sha256":      s.recoveryToolSHA,
		"size":        len(s.recoveryTool),
		"available":   len(s.recoveryTool) > 0,
		"version":     version.Version,
		"release_url": recoveryReleaseURL(name),
		"invocation":  "python3 " + name + " --key <64-hex key> --in <backup>.dback --verify --out backup.tar",
	})
}

// handleRecoveryToolDownload serves the embedded script so an admin can save it
// straight from their own instance (offline, no GitHub needed).
func (s *Server) handleRecoveryToolDownload(w http.ResponseWriter, r *http.Request) {
	if len(s.recoveryTool) == 0 {
		errJSON(w, http.StatusNotFound, "recovery tool is not embedded in this build")
		return
	}
	name := s.recoveryToolName
	if name == "" {
		name = "dockback-recover.py"
	}
	w.Header().Set("Content-Type", "text/x-python; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(s.recoveryTool)
}
