package backup

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
)

// Recognising a database nobody told us was a database (#3).
//
// Engine detection is image-substring only, and per-app volume markers live
// inside profiles. So a renamed, custom or vendored image whose bind holds
// PG_VERSION is treated as plain files and copied hot, while the server writes
// to them. R1 §Issue 3 is what that produces: a torn data directory captured as
// a success, restored weeks later into a crash-loop or — worse — into something
// that starts and then throws index corruption.
//
// An application that bundles its own PostgreSQL was this exact class of miss.
//
// This is a floor, not a replacement for detection. It does not try to dump an
// engine it cannot identify: guessing at credentials for an unknown server is
// how a backup fails loudly at the wrong moment. It quiesces the copy window and
// says, in the manifest, that this container holds a database DockBack could not
// identify.

// statefulVolumeMarkers are files whose presence at a mount root means a
// database server owns that directory.
//
// Each is created by the engine at initialisation and is never a coincidence:
// PG_VERSION is written by initdb, ibdata1 by InnoDB, mongod.lock by mongod.
// dump.rdb and appendonlydir are Redis's two persistence layouts.
var statefulVolumeMarkers = map[string]string{
	"PG_VERSION":    "postgres",
	"ibdata1":       "mysql",
	"dump.rdb":      "redis",
	"appendonlydir": "redis",
	"mongod.lock":   "mongodb",
}

// statefulMarkerPrefix tags the probe's output lines.
const statefulMarkerPrefix = "MARKER|"

// statefulMarkerScript looks for every marker at every destination root in one
// pass.
//
// Depth one only. These files sit at the top of a data directory by definition,
// and a recursive search would walk a media library to find nothing.
func statefulMarkerScript(dests []string) string {
	names := make([]string, 0, len(statefulVolumeMarkers))
	for name := range statefulVolumeMarkers {
		names = append(names, name)
	}
	sort.Strings(names) // one marker wins per destination; make it the same one twice

	var script strings.Builder
	script.WriteString("for p in")
	for _, d := range dests {
		script.WriteString(" '" + shellEscape(d) + "'")
	}
	script.WriteString("; do [ -d \"$p\" ] || continue; for m in")
	for _, name := range names {
		script.WriteString(" '" + shellEscape(name) + "'")
	}
	// The marker name is emitted before the path, so a destination containing a
	// pipe still parses from the left.
	script.WriteString(`; do if [ -e "$p/$m" ]; then printf '` + statefulMarkerPrefix + `%s|%s\n' "$m" "$p"; fi; done; done`)
	return script.String()
}

// parseStatefulMarkers reads the probe into destination → engine.
func parseStatefulMarkers(out string) map[string]string {
	found := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), statefulMarkerPrefix)
		if !ok {
			continue
		}
		marker, dest, ok := strings.Cut(rest, "|")
		if !ok || dest == "" {
			continue
		}
		engine := statefulVolumeMarkers[marker]
		if engine == "" {
			continue
		}
		if _, already := found[dest]; !already {
			found[dest] = engine
		}
	}
	return found
}

// statefulMarkers probes a container's selected mounts for database markers.
//
// Read-only, and a failure is not a verdict: a probe that could not run means
// nothing was learned, never that the directory is stateful. Stopping a
// container because a sidecar failed would be an outage caused by the backup.
func statefulMarkers(ctx context.Context, cli *client.Client, containerID string, dests []string) (map[string]string, error) {
	if len(dests) == 0 {
		return nil, nil
	}
	out, err := dockercli.CaptureSidecarRO(ctx, cli, containerID,
		[]string{"/bin/sh", "-c", statefulMarkerScript(dests)})
	if err != nil {
		return nil, err
	}
	return parseStatefulMarkers(string(out)), nil
}

// decideStatefulVolumes turns a marker probe into a verdict.
//
// severity is empty when there is nothing to report. forceStop asks the caller
// to quiesce a copy window that would otherwise run live.
func decideStatefulVolumes(found map[string]string, engineKind, pauseMode string, pauseByOperator bool) (forceStop bool, severity string) {
	if len(found) == 0 {
		return false, ""
	}
	if engineKind != "" {
		// Recognised: the dump path already owns this container and takes a
		// consistent logical copy. Nothing to add.
		return false, ""
	}
	if pauseByOperator {
		// They have said how this container is handled. Their instruction stands,
		// and the finding drops to a warning — acknowledged, not resolved.
		return false, FindingWarn
	}
	return pauseMode != PauseStop, FindingDanger
}

// describeStatefulMarkers names what was found, in a stable order.
func describeStatefulMarkers(found map[string]string) string {
	dests := make([]string, 0, len(found))
	for dest := range found {
		dests = append(dests, dest)
	}
	sort.Strings(dests)
	parts := make([]string, 0, len(dests))
	for _, dest := range dests {
		parts = append(parts, fmt.Sprintf("%s (%s)", dest, found[dest]))
	}
	return strings.Join(parts, ", ")
}

// guardStatefulVolumes records what the markers found and returns the pause mode
// the copy should actually use.
//
// Returns pauseMode unchanged whenever it has nothing to say, so a caller can
// assign its result unconditionally.
func (e *Engine) guardStatefulVolumes(ctx context.Context, cli *client.Client, man *Manifest, containerID, logID, name, engineKind, pauseMode string, pauseByOperator bool, dests []string) string {
	if engineKind != "" || len(dests) == 0 {
		return pauseMode
	}
	found, err := statefulMarkers(ctx, cli, containerID, dests)
	if err != nil {
		e.logf(logID, "INFO", "Could not check %s's mounts for database files (%v) — capturing them as ordinary files", name, err)
		return pauseMode
	}
	forceStop, severity := decideStatefulVolumes(found, engineKind, pauseMode, pauseByOperator)
	if severity == "" {
		return pauseMode
	}

	engines := describeStatefulMarkers(found)
	message := fmt.Sprintf(
		"This container's data looks like a running database, but its image is not one DockBack recognises, so there is no logical dump of it — only a file copy: %s. "+
			"A file copy of a live database can be torn, and a torn copy restores into a crash-loop or, worse, into a server that starts and then reports index corruption. "+
			"If this is a database, back it up from a container DockBack recognises, or add an application export command for it.", engines)
	e.addFinding(man, logID, findingStatefulUndetectedEngine, severity, name, message)

	if !forceStop {
		if pauseByOperator {
			e.logf(logID, "INFO", "Keeping the pause mode you set for %s (%s) even though its data looks like a database — your choice stands, and the finding above records it.", name, pauseMode)
		}
		return pauseMode
	}
	e.logf(logID, "WARN", "Stopping %s for the copy instead of %s: its mounts hold database files (%s) and copying those live is how a backup captures a torn database. Set this container's pause mode if you need different behaviour.",
		name, pauseMode, engines)
	return PauseStop
}
