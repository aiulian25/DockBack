// Reading one small configuration file off a node's host filesystem.
//
// The read-only twin of WriteHostFile, and what lets the genuine compose file
// and the stack's .env (F57/F231) be captured from EVERY node rather than only
// from the ones reached over SSH.
//
// The SSH-only restriction those features shipped with was described as a
// boundary — "the socket-proxy transports deliberately can't read host files" —
// but that has not been true for some time. The Machine page binds / read-only
// to read /proc and /sys, the bind-source probe does the same to stat mount
// sources, and the stack reconstruction binds a host directory read-WRITE to put
// a compose file in it. All three work on every transport, because the
// socket-proxy permits container create and a bind is just part of that request.
//
// What the restriction actually cost was the .env: a cross-host restore of a
// socket-proxy node reconstructed a compose file with every value inlined and
// had no .env to remap, so `docker compose up` afterwards put the old machine's
// address straight back. This closes that, under the same path grammar, the same
// size cap, and the same "lands in the encrypted archive, never the manifest"
// rule the SSH path already followed.
package dockercli

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

const (
	// The whole host filesystem, READ-ONLY. Deliberately not the file's parent:
	// Docker creates a missing bind source before mounting it, even for a :ro
	// bind, so binding the parent would leave an empty directory behind on the
	// host every time a recorded compose path had gone stale. A read has no
	// business creating anything, and the same read-only root bind is what the
	// Machine probe and the bind-source probe already use.
	hostReadMount   = "/dockback-hostread"
	hostReadTimeout = 2 * time.Minute
)

// ReadHostFile returns the contents of one small file on a node's host
// filesystem, read through a short-lived, unprivileged, read-only sidecar.
//
// The path goes through ValidateFetchPath — the SAME grammar the SSH reader
// enforces — so it is absolute, free of ".." and of every shell metacharacter,
// and length-bounded. maxBytes is clamped to MaxComposeFetchBytes and applied
// INSIDE the container by head, so a file that turns out to be enormous is never
// carried into this process to be measured and discarded.
//
// A missing file comes back as an error carrying the shell's own "No such file
// or directory", which callers match on to stay quiet about the ordinary case of
// a stack that simply has no .env.
func ReadHostFile(ctx context.Context, c *client.Client, hostPath string, maxBytes int64) ([]byte, error) {
	if err := ValidateFetchPath(hostPath); err != nil {
		return nil, err
	}
	if maxBytes <= 0 || maxBytes > MaxComposeFetchBytes {
		maxBytes = MaxComposeFetchBytes
	}
	ctx, cancel := context.WithTimeout(ctx, hostReadTimeout)
	defer cancel()

	if err := ensureSidecar(ctx, c); err != nil {
		return nil, err
	}
	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: []string{"sleep", "120"}, Labels: sidecarLabels()},
		&container.HostConfig{Binds: []string{"/:" + hostReadMount + ":ro"}},
		nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("host-read sidecar create: %w", err)
	}
	defer removeContainer(c, created.ID)
	if err := c.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("host-read sidecar start: %w", err)
	}

	out, err := ExecCapture(ctx, c, created.ID, []string{"sh", "-c", hostReadScript(hostPath, maxBytes)})
	if err != nil {
		return nil, err
	}
	// One byte over the cap is how the read distinguishes "exactly at the limit"
	// from "truncated", so a file at the boundary is returned and one past it is
	// refused rather than silently cut short.
	if int64(len(out)) > maxBytes {
		return nil, fmt.Errorf("%s is larger than the %d byte limit for a captured configuration file", hostPath, maxBytes)
	}
	return out, nil
}

// hostReadScript bounds the read at the source. Split out so the quoting and the
// cap are testable without a daemon.
func hostReadScript(hostPath string, maxBytes int64) string {
	return "head -c " + strconv.FormatInt(maxBytes+1, 10) + " " + shQuote(hostReadMount+hostPath)
}
