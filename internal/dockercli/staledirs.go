package dockercli

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// Stale staging-directory detection (F157).
//
// Applications that write safely tend to write the same way: stage the new
// content in a temporary directory beside the target, then rename it into place,
// so a crash leaves either the old content or the new one and never a half of
// each. Radicale does this with `.Radicale.tmp-*`; the pattern is common.
//
// The consequence is that a staging directory still present long after it was
// created is the fossil of an interrupted write — and it stays there, because
// nothing in the application is looking for it. It is inert, which is why it
// survives: two of them sat in one production tree for a year, quietly doubling
// the item count of every backup of it.
//
// Only the SHAPE of the tree is read: paths, modification times and sizes. No
// file is opened.

// maxStaleDirs bounds one scan, so a pathological tree cannot turn into
// unbounded sidecar work or an unbounded manifest.
const maxStaleDirs = 50

// StaleDir is one staging directory that outlived the write it belonged to.
type StaleDir struct {
	// Path inside the container.
	Path string
	// ModTime is when it was last written.
	ModTime time.Time
	// Bytes is its size, as reported by du (a kibibyte-granular estimate — this
	// is a "how much is this costing you" figure, not an accounting record).
	Bytes int64
}

// FindStaleDirs looks under the given container paths for directories matching
// any of the globs whose modification time is older than minAge (F157).
//
// Best-effort and bounded: an unreachable node or a sidecar without the tools
// returns nothing, and the caller then reports nothing rather than guessing. The
// age filter is what makes the result meaningful — a staging directory created
// moments ago is a write in progress, not debris.
func FindStaleDirs(ctx context.Context, c *client.Client, targetID string, roots, globs []string, minAge time.Duration) ([]StaleDir, error) {
	if len(roots) == 0 || len(globs) == 0 {
		return nil, nil
	}
	if err := ensureSidecar(ctx, c); err != nil {
		return nil, err
	}
	mins := int(minAge / time.Minute)
	if mins < 1 {
		mins = 1
	}

	var script strings.Builder
	script.WriteString("for d in")
	for _, r := range roots {
		script.WriteString(" '")
		script.WriteString(shellEscape(r))
		script.WriteString("'")
	}
	script.WriteString(`; do [ -d "$d" ] || continue; find "$d" -type d \( `)
	for i, g := range globs {
		if i > 0 {
			script.WriteString(" -o ")
		}
		script.WriteString("-name '")
		script.WriteString(shellEscape(g))
		script.WriteString("'")
	}
	// -mmin is understood by both GNU find and the busybox find in the default
	// sidecar. -prune keeps the walk from descending into a match, so a staging
	// directory containing another one is reported once.
	fmt.Fprintf(&script, ` \) -mmin +%d -prune 2>/dev/null | head -n %d | while IFS= read -r p; do `, mins, maxStaleDirs)
	script.WriteString(`m=$(stat -c %Y "$p" 2>/dev/null); [ -n "$m" ] || m=0; ` +
		`k=$(du -sk "$p" 2>/dev/null | cut -f1); [ -n "$k" ] || k=0; ` +
		`printf 'STALE\t%s\t%s\t%s\n' "$p" "$m" "$k"; done; done`)

	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: []string{"sh", "-c", script.String()}, AttachStdout: true, AttachStderr: true, Labels: sidecarLabels()},
		&container.HostConfig{VolumesFrom: []string{targetID + ":ro"}}, nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("stale-directory scan sidecar create: %w", err)
	}
	sidecarID := created.ID
	defer removeContainer(c, sidecarID)

	att, err := c.ContainerAttach(ctx, sidecarID, container.AttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		return nil, fmt.Errorf("stale-directory scan attach: %w", err)
	}
	if err := c.ContainerStart(ctx, sidecarID, container.StartOptions{}); err != nil {
		att.Close()
		return nil, fmt.Errorf("stale-directory scan start: %w", err)
	}
	var stdout, stderr bytes.Buffer
	copyDone := make(chan error, 1)
	go func() { _, e := stdcopy.StdCopy(&stdout, &stderr, att.Reader); copyDone <- e }()
	select {
	case <-copyDone:
	case <-ctx.Done():
		att.Close()
		return nil, ctx.Err()
	}
	att.Close()

	return ParseStaleDirs(stdout.String()), nil
}

// ParseStaleDirs turns the sidecar's output into the list. Exported so the parse
// is testable without Docker; unrecognised lines are ignored.
func ParseStaleDirs(out string) []StaleDir {
	var res []StaleDir
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(parts) != 4 || parts[0] != "STALE" || parts[1] == "" {
			continue
		}
		d := StaleDir{Path: parts[1]}
		if sec, err := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64); err == nil && sec > 0 {
			d.ModTime = time.Unix(sec, 0).UTC()
		}
		if kb, err := strconv.ParseInt(strings.TrimSpace(parts[3]), 10, 64); err == nil && kb >= 0 {
			d.Bytes = kb * 1024
		}
		res = append(res, d)
	}
	return res
}

// staleDirTimeout bounds a scan. It walks directories and sizes a handful of
// them, so anything beyond this is a stuck sidecar rather than slow work.
const staleDirTimeout = 3 * time.Minute

// FindStaleDirsBounded is FindStaleDirs with its own timeout, so a caller on a
// long-lived context cannot hang on it.
func FindStaleDirsBounded(ctx context.Context, c *client.Client, targetID string, roots, globs []string, minAge time.Duration) ([]StaleDir, error) {
	sctx, cancel := context.WithTimeout(ctx, staleDirTimeout)
	defer cancel()
	return FindStaleDirs(sctx, c, targetID, roots, globs, minAge)
}
