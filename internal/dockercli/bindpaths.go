// Bind-source preflight for cross-host restores (F81 companion).
//
// A restore replays the source container's bind mounts verbatim, after the
// optional host-path remap. On the machine the container came FROM every source
// existed; on a new machine they usually do not, and the daemon's two behaviours
// are both wrong for us:
//
//   - a spec that uses the Mounts API is refused outright, naming only the FIRST
//     missing path — so an operator discovers the rest one failed restore at a
//     time;
//   - a spec that uses legacy Binds silently invents an empty DIRECTORY, even
//     where the container expects a FILE. That is how a mounted secret becomes a
//     directory and the application starts up broken rather than not at all.
//
// So DockBack looks before it leaps: stat every source on the target first, in
// one read-only pass, and let the caller report all of them together.
package dockercli

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
)

// The whole target filesystem, mounted read-only, so one short-lived sidecar can
// stat every bind source instead of one sidecar per path. Same shape and same
// reasoning as the Machine page's host probe (machine.go): read-only,
// unprivileged, short-lived, and used for nothing but stat.
const hostProbeMount = "/dockback-hostfs"

const hostProbeTimeout = 90 * time.Second

// HostPathKind is what the target host has at a probed path.
type HostPathKind string

const (
	HostPathMissing HostPathKind = "missing"
	HostPathDir     HostPathKind = "directory"
	HostPathFile    HostPathKind = "file"
)

// BindMount is one host-path bind a recreated container would need, as recorded
// in its saved `docker inspect`.
type BindMount struct {
	Source      string // absolute path on the host
	Destination string // path inside the container
}

// ContainerBindMounts returns the host-path binds a recreated container would
// make, read from its saved inspect JSON: HostConfig.Binds source segments and
// Mounts[] entries of Type=="bind".
//
// Named-volume sources (no leading '/') are not host paths and are never
// returned — Docker creates those on demand, which is correct for a volume and
// exactly what F90 already handles.
//
// Deduplicated by source and sorted, so a report built from it is stable.
func ContainerBindMounts(inspectJSON []byte) []BindMount {
	var insp types.ContainerJSON
	if err := json.Unmarshal(inspectJSON, &insp); err != nil {
		return nil
	}
	bySource := map[string]string{}
	if insp.HostConfig != nil {
		for _, bind := range insp.HostConfig.Binds {
			source, rest, found := strings.Cut(bind, ":")
			if !found || !strings.HasPrefix(source, "/") {
				continue
			}
			destination, _, _ := strings.Cut(rest, ":") // drop any ":ro"/":z" options
			if _, seen := bySource[source]; !seen {
				bySource[source] = destination
			}
		}
	}
	// HostConfig.Mounts is where compose long-syntax and `--mount` put a bind,
	// and it is the field ContainerCreate reads — so a bind declared only there
	// must be found here, or the restore prepares a path the daemon never asks
	// for and is refused over the one it does.
	if insp.HostConfig != nil {
		for _, m := range insp.HostConfig.Mounts {
			if m.Type != mount.TypeBind || !strings.HasPrefix(m.Source, "/") {
				continue
			}
			if _, seen := bySource[m.Source]; !seen {
				bySource[m.Source] = m.Target
			}
		}
	}
	for _, m := range insp.Mounts {
		if m.Type != "bind" || !strings.HasPrefix(m.Source, "/") {
			continue
		}
		if _, seen := bySource[m.Source]; !seen {
			bySource[m.Source] = m.Destination
		}
	}
	out := make([]BindMount, 0, len(bySource))
	for source, destination := range bySource {
		out = append(out, BindMount{Source: source, Destination: destination})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}

// ProbeHostPaths reports what the node's host filesystem has at each absolute
// path. Paths that cannot be probed at all are reported as missing by the
// caller's own reading of the returned map — an absent key means the probe
// produced no verdict for it, which is deliberately distinct from "missing".
//
// Read-only throughout: the sidecar mounts / with ReadOnly set and runs nothing
// but `[ -d ]` / `[ -e ]`. It cannot create the paths it is asked about, which
// is the point — the daemon's willingness to invent them is the hazard this
// exists to get ahead of.
func ProbeHostPaths(ctx context.Context, c *client.Client, paths []string) (map[string]HostPathKind, error) {
	if len(paths) == 0 {
		return map[string]HostPathKind{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, hostProbeTimeout)
	defer cancel()

	if err := ensureSidecar(ctx, c); err != nil {
		return nil, err
	}
	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: []string{"sleep", "60"}, Labels: sidecarLabels()},
		&container.HostConfig{Binds: []string{"/:" + hostProbeMount + ":ro"}},
		nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("host-path probe sidecar create: %w", err)
	}
	defer removeContainer(c, created.ID)
	if err := c.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("host-path probe sidecar start: %w", err)
	}

	out, err := ExecCapture(ctx, c, created.ID, []string{"sh", "-c", hostProbeScript(paths)})
	if err != nil {
		return nil, fmt.Errorf("host-path probe: %w", err)
	}
	return parseHostProbe(string(out)), nil
}

// Creating the bind sources a target does not have.
//
// The mount points are numbered under one prefix, one per distinct PARENT
// directory, and each parent is bound individually rather than binding / and
// walking to them. That costs a little grouping and buys the property the rest
// of the host-write code already has: a mistake in path construction can only
// reach a directory this container already binds, instead of the whole
// filesystem. The socket-proxy would permit the wider bind — that is not the
// point; the point is what a bug in here could do with it.
const (
	hostEnsureMountPrefix = "/dockback-ensure"
	hostEnsureTimeout     = 3 * time.Minute
	// Bounds one sidecar's mount list, so a container with a pathological number
	// of binds is handled in batches rather than in one create nobody sized for.
	maxHostEnsureBinds = 32
)

// HostPathSpec is one bind source EnsureHostPaths must bring into existence, and
// what it should be when it does.
//
// Kind is required and never inferred. Creating a directory where the container
// expects a file is the exact failure this whole path exists to prevent, so a
// spec that does not say which it is gets skipped rather than guessed at.
type HostPathSpec struct {
	Path  string // absolute host path
	Kind  string // MountKindDir | MountKindFile
	Owner string // "uid:gid"; applied only when it parses as one
	Mode  string // octal bits; applied only when they parse
}

// hostPathGroup is the specs that share one parent directory, so the parent is
// bound once however many of its children have to be created.
type hostPathGroup struct {
	parent string
	specs  []HostPathSpec
}

// EnsureHostPaths creates each missing bind source on the node's host
// filesystem, as the kind it is meant to be and with the ownership and mode the
// backup recorded.
//
// Best-effort by design, and the caller must treat it that way: it reports only
// whether the sidecar itself could run, never whether an individual path landed.
// One path failing must not stop the others — a restore that creates six of
// seven directories and names the seventh is far more useful than one that
// stops at the first. The caller re-probes afterwards, and that probe, not this
// function's return, is what decides whether the restore may continue.
func EnsureHostPaths(ctx context.Context, c *client.Client, specs []HostPathSpec) error {
	groups := groupHostPaths(specs)
	if len(groups) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, hostEnsureTimeout)
	defer cancel()

	if err := ensureSidecar(ctx, c); err != nil {
		return err
	}
	for start := 0; start < len(groups); start += maxHostEnsureBinds {
		end := start + maxHostEnsureBinds
		if end > len(groups) {
			end = len(groups)
		}
		if err := ensureHostPathBatch(ctx, c, groups[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// groupHostPaths validates every spec and folds those sharing a parent together.
//
// Validation happens here as well as in the caller's planning, deliberately: the
// planner decides what to TELL the operator, this decides what actually reaches
// a command. A path that fails is dropped silently because the caller's re-probe
// will report it as still missing, which is the honest outcome either way.
func groupHostPaths(specs []HostPathSpec) []hostPathGroup {
	var groups []hostPathGroup
	at := map[string]int{}
	for _, s := range specs {
		if s.Kind != MountKindDir && s.Kind != MountKindFile {
			continue
		}
		parent, _, err := validateHostPath(s.Path, "bind mount source")
		if err != nil {
			continue
		}
		i, seen := at[parent]
		if !seen {
			i = len(groups)
			at[parent] = i
			groups = append(groups, hostPathGroup{parent: parent})
		}
		groups[i].specs = append(groups[i].specs, s)
	}
	return groups
}

// hostEnsureScript builds the fixed script one batch runs. Pure, so the quoting
// and the file-vs-directory distinction are testable without a daemon.
//
// No `set -e`: each path is independent, and one that cannot be created must not
// take the rest of them down with it.
func hostEnsureScript(groups []hostPathGroup) string {
	var script strings.Builder
	for i, g := range groups {
		mount := hostEnsureMountPrefix + strconv.Itoa(i)
		for _, spec := range g.specs {
			target := mount + "/" + path.Base(path.Clean(spec.Path))
			if spec.Kind == MountKindFile {
				// Never truncate: a file that turned up between the probe and now is
				// the real one, and an empty placeholder over it would be data loss.
				// umask 077 so a placeholder for a secret is not born world-readable.
				script.WriteString("[ -e " + shQuote(target) + " ] || (umask 077; : > " + shQuote(target) + "); ")
			} else {
				script.WriteString("mkdir -p " + shQuote(target) + "; ")
			}
			if ownerRe.MatchString(spec.Owner) {
				script.WriteString("chown " + spec.Owner + " " + shQuote(target) + "; ")
			}
			if modeRe.MatchString(spec.Mode) {
				script.WriteString("chmod " + spec.Mode + " " + shQuote(target) + "; ")
			}
		}
	}
	return script.String()
}

// ensureHostPathBatch runs one sidecar with one bind per parent in the batch.
func ensureHostPathBatch(ctx context.Context, c *client.Client, groups []hostPathGroup) error {
	binds := make([]string, 0, len(groups))
	for i, g := range groups {
		// Docker creates the parent with mkdir -p when it is missing, which on a
		// fresh host it usually is — the same behaviour the stack reconstruction
		// already relies on.
		binds = append(binds, g.parent+":"+hostEnsureMountPrefix+strconv.Itoa(i))
	}
	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: []string{"sleep", "180"}, Labels: sidecarLabels()},
		&container.HostConfig{Binds: binds}, nil, nil, "")
	if err != nil {
		return fmt.Errorf("bind-source sidecar create: %w", err)
	}
	defer removeContainer(c, created.ID)
	if err := c.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("bind-source sidecar start: %w", err)
	}
	if _, err := ExecCapture(ctx, c, created.ID, []string{"sh", "-c", hostEnsureScript(groups)}); err != nil {
		return fmt.Errorf("creating bind mount sources: %w", err)
	}
	return nil
}

// hostProbeScript emits one "<kind> <path>" line per path. Split out so the
// quoting is unit-testable without a daemon.
func hostProbeScript(paths []string) string {
	var script strings.Builder
	script.WriteString("for p in")
	for _, p := range paths {
		script.WriteString(" " + shQuote(p))
	}
	script.WriteString("; do t=missing; ")
	script.WriteString(`if [ -d "` + hostProbeMount + `$p" ]; then t=directory; `)
	script.WriteString(`elif [ -e "` + hostProbeMount + `$p" ]; then t=file; fi; `)
	script.WriteString(`printf '%s %s\n' "$t" "$p"; done`)
	return script.String()
}

// parseHostProbe reads the script's output back. A line it cannot understand is
// dropped rather than guessed at: no verdict is safer than a wrong one, because
// the caller treats a verdict of "missing" as grounds to stop the restore.
func parseHostProbe(out string) map[string]HostPathKind {
	kinds := map[string]HostPathKind{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		kind, path, found := strings.Cut(line, " ")
		if !found || path == "" {
			continue
		}
		switch HostPathKind(kind) {
		case HostPathMissing, HostPathDir, HostPathFile:
			kinds[path] = HostPathKind(kind)
		}
	}
	return kinds
}
