package dockercli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// File permission audit (F147).
//
// Some files are only as protected as their mode. A WireGuard private key left
// world-readable is readable by every process in every container that shares the
// directory, and by every user on the host — and nothing about a working
// deployment ever complains. It is invisible precisely because it works.
//
// Reading the MODE costs nothing and exposes nothing: a permission bit is not a
// secret, and the file's contents are never opened. That asymmetry is the whole
// reason this is safe to do at all.

// maxAuditedFiles bounds one audit, so a profile with an over-broad list cannot
// turn into unbounded sidecar work.
const maxAuditedFiles = 40

// FileMode is one audited path's permissions. Missing is true when the path does
// not exist, which is an ordinary answer — a profile lists the places a file MAY
// live across image variants, and most of them will not be there.
type FileMode struct {
	Path    string
	Mode    string // octal permission bits, e.g. "600"
	Owner   string // "uid:gid", empty when it could not be read
	Missing bool
}

// FileModes reads the permission bits of specific paths inside a target's
// volumes, via a read-only sidecar.
//
// Best-effort and bounded: an unreachable node or a sidecar without `stat`
// returns nothing, and the caller then claims nothing rather than guessing.
func FileModes(ctx context.Context, c *client.Client, targetID string, paths []string) ([]FileMode, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	if len(paths) > maxAuditedFiles {
		paths = paths[:maxAuditedFiles]
	}
	if err := ensureSidecar(ctx, c); err != nil {
		return nil, err
	}

	// One line per path: "MODE<TAB><path><TAB><octal|-><TAB><uid:gid|->". The
	// marker prefix keeps the parse independent of anything else the shell might
	// print. The owner is read alongside the mode because for a private key the
	// two answer one question together: 0600 says only its owner may read it, and
	// the owner says whether that is the account the application runs as.
	var script strings.Builder
	script.WriteString("for f in")
	for _, p := range paths {
		script.WriteString(" '")
		script.WriteString(shellEscape(p))
		script.WriteString("'")
	}
	script.WriteString(`; do m=$(stat -c '%a' "$f" 2>/dev/null); [ -n "$m" ] || m=-; ` +
		`o=$(stat -c '%u:%g' "$f" 2>/dev/null); [ -n "$o" ] || o=-; ` +
		`printf 'MODE\t%s\t%s\t%s\n' "$f" "$m" "$o"; done`)

	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: []string{"sh", "-c", script.String()}, AttachStdout: true, AttachStderr: true, Labels: sidecarLabels()},
		&container.HostConfig{VolumesFrom: []string{targetID + ":ro"}}, nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("permission audit sidecar create: %w", err)
	}
	sidecarID := created.ID
	defer removeContainer(c, sidecarID)

	att, err := c.ContainerAttach(ctx, sidecarID, container.AttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		return nil, fmt.Errorf("permission audit attach: %w", err)
	}
	if err := c.ContainerStart(ctx, sidecarID, container.StartOptions{}); err != nil {
		att.Close()
		return nil, fmt.Errorf("permission audit start: %w", err)
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

	return ParseFileModes(stdout.String()), nil
}

// ParseFileModes turns the sidecar's output into the audited list. Exported so
// the parse is testable without Docker; unknown lines are ignored.
func ParseFileModes(out string) []FileMode {
	var res []FileMode
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(strings.TrimRight(line, "\r"), "\t")
		// Three fields or four: an archive audited before the owner was read still
		// parses, and a reader that cannot see an owner claims none.
		if len(parts) < 3 || len(parts) > 4 || parts[0] != "MODE" || parts[1] == "" {
			continue
		}
		m := strings.TrimSpace(parts[2])
		if m == "" || m == "-" {
			res = append(res, FileMode{Path: parts[1], Missing: true})
			continue
		}
		owner := ""
		if len(parts) == 4 {
			// Validated, not trusted: an owner that is not numeric uid:gid would
			// end up in a chown suggestion an operator pastes into a shell.
			if o := strings.TrimSpace(parts[3]); ownerRe.MatchString(o) {
				owner = o
			}
		}
		res = append(res, FileMode{Path: parts[1], Mode: m, Owner: owner})
	}
	return res
}

// fileModeTimeout bounds an audit: it stats a handful of paths, so anything
// longer is a stuck sidecar rather than slow work.
const fileModeTimeout = 90 * time.Second

// FileModesBounded is FileModes with its own timeout, so a caller on a
// long-lived context cannot hang on it.
func FileModesBounded(ctx context.Context, c *client.Client, targetID string, paths []string) ([]FileMode, error) {
	fctx, cancel := context.WithTimeout(ctx, fileModeTimeout)
	defer cancel()
	return FileModes(fctx, c, targetID, paths)
}
