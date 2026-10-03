package dockercli

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// AutoConfFile is the file ALTER SYSTEM writes into a cluster's data directory.
// PostgreSQL reads it last, so what is in here overrides postgresql.conf — which
// is what makes it both the way to configure a restored cluster and the way to
// make one unstartable.
const AutoConfFile = "postgresql.auto.conf"

// sidecarEditTimeout bounds a sidecar that only edits one small text file. It is
// short on purpose: this runs while a restore is waiting on it.
const sidecarEditTimeout = 2 * time.Minute

// autoConfSettingName is the shape of a setting this will strip. Deliberately
// narrower than PostgreSQL's own grammar — it goes into a regular expression
// inside a shell command, and every setting that can strand a server this way is
// a plain lower-case identifier.
var autoConfSettingName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// safeVolumePath is an absolute path with nothing in it a shell would treat as
// syntax, so it can be embedded in a command as a quoted literal.
var safeVolumePath = regexp.MustCompile(`^/[A-Za-z0-9_./-]*$`)

// runSidecar runs one shell command against the target's volumes and waits for
// it to finish.
//
// The sidecar is how a stopped or crash-looping container's files are still
// reachable: exec needs a running container, VolumesFrom does not. That is the
// whole reason this exists rather than an ExecCapture.
func runSidecar(ctx context.Context, c *client.Client, targetID, cmd string, timeout time.Duration) error {
	if err := ensureSidecar(ctx, c); err != nil {
		return err
	}
	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: []string{"sh", "-c", cmd}},
		&container.HostConfig{VolumesFrom: []string{targetID}},
		nil, nil, "")
	if err != nil {
		return fmt.Errorf("sidecar create: %w", err)
	}
	defer removeContainer(c, created.ID)
	if err := c.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("sidecar start: %w", err)
	}
	waitCh, errCh := c.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	select {
	case <-waitCh:
		return nil
	case e := <-errCh:
		return e
	case <-time.After(timeout):
		return fmt.Errorf("sidecar timed out")
	}
}

// StripAutoConfSetting removes one setting from a PostgreSQL cluster's
// postgresql.auto.conf, so a server that ALTER SYSTEM has made unstartable can
// boot again.
//
// This is the only way back from a bad shared_preload_libraries. The documented
// retraction is ALTER SYSTEM RESET, ALTER SYSTEM is SQL, SQL needs a running
// server — and a preloaded library that is missing or mismatched is precisely
// what stops the server from running. The retraction is impossible in the one
// case it is needed, which leaves the file itself.
//
// The file is rewritten in place rather than replaced, so it keeps its inode,
// owner and mode: PostgreSQL refuses to start on a data directory holding files
// it does not own, and a rescue that made the cluster unstartable a second way
// would be worse than the problem.
func StripAutoConfSetting(ctx context.Context, c *client.Client, targetID, dataDir, name string) error {
	if !safeVolumePath.MatchString(dataDir) {
		return fmt.Errorf("refusing to edit configuration under %q: not a plain absolute path", dataDir)
	}
	if !autoConfSettingName.MatchString(name) {
		return fmt.Errorf("refusing to strip %q: not a setting name PostgreSQL could have written", name)
	}
	file := dataDir + "/" + AutoConfFile
	// grep exits 1 when it prints nothing, which for a file holding only this one
	// setting is success, not failure — hence the `|| true` inside the redirect.
	cmd := fmt.Sprintf(`f=%q; [ -f "$f" ] || exit 0; { grep -v "^[[:space:]]*%s[[:space:]]*=" "$f" || true; } > "$f.dockback" && cat "$f.dockback" > "$f"; rm -f "$f.dockback"`,
		file, name)
	return runSidecar(ctx, c, targetID, cmd, sidecarEditTimeout)
}
