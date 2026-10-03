package backup

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
)

// Export-directory cleanup after an app-native capture (F151).
//
// An app-native export writes a PLAINTEXT copy of everything the application
// holds into a directory inside the container — for a document archive, every
// document, decrypted and renamed. DockBack's archive of that directory is
// encrypted; the directory itself is not, and it stays there afterwards.
//
// So the backup quietly doubles the application's data into a second, unprotected
// copy that nobody is watching. Worse, it is a copy an operator has no reason to
// think about, because it is a side effect of a feature whose whole selling point
// is safety.
//
// Emptying it after a successful capture closes that. Three things make it safe
// enough to offer:
//
//   - It is OPT-IN, per container, default off. The exporter is incremental —
//     it skips files whose size and timestamp still match — so emptying the
//     directory makes the next run a full re-export. On a small archive that is
//     nothing; on a very large one it is real time, and only the operator knows
//     which they have.
//   - It runs ONLY after the export has been captured successfully. On every
//     failure path the directory is left exactly as it was, because a half-made
//     backup is not a reason to delete the thing it was made from.
//   - DockBack builds the command itself from the profile's own directory, with
//     the path validated first. It is deliberately NOT a free-text command in a
//     preset: "run this rm as root inside the container" is not a setting that
//     should be typed, imported from someone else's library, or reviewed once.

// cleanupExportKey is the per-container setting, mirroring how every other
// per-container backup choice is stored so scheduled and manual runs agree.
const cleanupExportKey = "backup.cleanup_export"

func cleanupExportKeyFor(nodeID, name string) string {
	return cleanupExportKey + "." + nodeID + "." + name
}

// CleanupExport reports whether this container's export directory is emptied
// after a successful app-native capture (F151). Default OFF: the exporter is
// incremental, so emptying it trades re-export time for not leaving a plaintext
// copy around, and only the operator knows which side of that they are on.
func (e *Engine) CleanupExport(nodeID, name string) bool {
	v, _ := e.Store.GetSetting(cleanupExportKeyFor(nodeID, name), "false")
	return v == "true"
}

// SetCleanupExport stores the choice, applying to this container's next backup
// whether it is scheduled or manual.
func (e *Engine) SetCleanupExport(nodeID, name string, on bool) error {
	if !on {
		return e.Store.SetSetting(cleanupExportKeyFor(nodeID, name), "")
	}
	return e.Store.SetSetting(cleanupExportKeyFor(nodeID, name), "true")
}

// minCleanupDepth is how many path segments a directory must have before its
// contents may be emptied.
//
// Two is the floor that makes "/", "/data", "/var" and every other top-level
// mount point unreachable while still allowing the real cases
// (/usr/src/paperless/export, /tmp/dockback-export). A profile pointing an
// export at a bare mount root is a misconfiguration; it must not become a
// deletion.
const minCleanupDepth = 2

// cleanableExportDir validates a directory before anything is emptied inside it,
// returning the cleaned absolute path.
//
// Everything here is a refusal, not a repair. A path that does not obviously
// name a scratch export directory is left alone and reported, because the cost
// of being wrong is somebody's data and the cost of declining is a directory
// that stays on disk.
func cleanableExportDir(dir string) (string, error) {
	d := strings.TrimSpace(dir)
	if d == "" {
		return "", fmt.Errorf("no export directory recorded")
	}
	if !strings.HasPrefix(d, "/") {
		return "", fmt.Errorf("the export directory %q is not an absolute path", dir)
	}
	// A relative segment or a glob would make the target something other than
	// what it reads as.
	if strings.ContainsAny(d, "*?[]") || strings.Contains(d, "..") {
		return "", fmt.Errorf("the export directory %q contains characters that make its target ambiguous", dir)
	}
	clean := path.Clean(d)
	if clean == "/" {
		return "", fmt.Errorf("refusing to empty the container's root directory")
	}
	segs := 0
	for _, s := range strings.Split(strings.Trim(clean, "/"), "/") {
		if s != "" {
			segs++
		}
	}
	if segs < minCleanupDepth {
		return "", fmt.Errorf("refusing to empty %q — it is a top-level directory, not an export directory", clean)
	}
	return clean, nil
}

// cleanupExportDir empties the contents of a captured export directory, leaving
// the directory itself in place (the exporter expects to find it).
//
// Best-effort by design: the backup has already succeeded and is already stored,
// so a cleanup that cannot run is a finding to report, never a reason to fail a
// good backup. It returns whether the directory was actually emptied, so the
// manifest can state it rather than imply it.
func (e *Engine) cleanupExportDir(ctx context.Context, cli *client.Client, containerID, dir, user, logID string) bool {
	clean, err := cleanableExportDir(dir)
	if err != nil {
		e.logf(logID, "WARN", "Export directory NOT emptied: %v. The plaintext export is still inside the container — remove it by hand if that matters", err)
		return false
	}
	// Built here from a validated path, quoted as a single shell literal. Dotfiles
	// are included; the directory itself is kept.
	q := "'" + shellEscape(clean) + "'"
	script := "set -e; [ -d " + q + " ] || exit 0; find " + q + " -mindepth 1 -maxdepth 1 -exec rm -rf {} +"
	if _, cerr := dockercli.ExecHook(ctx, cli, containerID, []string{"/bin/sh", "-c", script}, user, ""); cerr != nil {
		e.logf(logID, "WARN", "Could not empty the export directory %s (%v) — the backup is complete, but a plaintext copy of the exported data is still inside the container", clean, cerr)
		return false
	}
	e.logf(logID, "INFO", "Emptied %s — the plaintext export does not linger outside the encrypted archive", clean)
	return true
}
