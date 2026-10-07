package dockercli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// Sidecar helpers for incremental volume backups (F61). All run in the SAME tiny
// Alpine sidecar attached with `--volumes-from target` that the full-volume tar
// already uses (PLAN §2.12), so they work even for distroless app containers that
// lack `find`/`stat`/`tar` themselves, and inherit the same isolation boundary.

// MaxIndexBytes caps the file-index listing captured from a sidecar so a
// pathological volume (millions of tiny files) can't exhaust memory building the
// index. ~512 MiB ≈ several million files at ~60 bytes/line; well past any real
// volume, and a hard stop rather than an OOM.
const MaxIndexBytes = 512 << 20

// CaptureSidecarRO runs cmd in a read-only sidecar (`--volumes-from target:ro`)
// and returns its stdout, bounded by MaxIndexBytes. Used to build the volume file
// index for an incremental backup (F61) via BuildIndexCmd. Read-only: it can never
// mutate the target's data.
// CaptureSidecarInNetns runs a command inside the TARGET's network namespace and
// returns its output.
//
// The application may publish no port at all on a cross-host target, and asking
// the host to reach it would answer a different question anyway — what the
// operator's browser sees is what the CONTAINER answers. Sharing its namespace
// asks it directly, and leaves DockBack's own egress allow-list untouched
// because nothing leaves the host.
//
// No volumes are attached: this looks at the network and nothing else.
func CaptureSidecarInNetns(ctx context.Context, c *client.Client, targetID string, cmd []string) ([]byte, error) {
	if err := ensureSidecar(ctx, c); err != nil {
		return nil, err
	}
	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: cmd, AttachStdout: true, AttachStderr: true, Labels: sidecarLabels()},
		&container.HostConfig{NetworkMode: container.NetworkMode("container:" + targetID), AutoRemove: false},
		nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("http probe sidecar create: %w", err)
	}
	sidecarID := created.ID
	defer removeContainer(c, sidecarID)

	att, err := c.ContainerAttach(ctx, sidecarID, container.AttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		return nil, fmt.Errorf("http probe sidecar attach: %w", err)
	}
	defer att.Close()
	if err := c.ContainerStart(ctx, sidecarID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("http probe sidecar start: %w", err)
	}
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, att.Reader); err != nil {
		return nil, fmt.Errorf("http probe sidecar read: %w", err)
	}
	waitCh, errCh := c.ContainerWait(context.Background(), sidecarID, container.WaitConditionNotRunning)
	select {
	case <-waitCh:
	case e := <-errCh:
		return nil, e
	case <-time.After(90 * time.Second):
		return nil, fmt.Errorf("http probe timed out")
	}
	return stdout.Bytes(), nil
}

// CaptureSidecarAs runs a command against the target's volumes AS a given user,
// with those volumes WRITABLE, and returns its output.
//
// The user is the point. R4 §Issue 34: a writability check that ran as root
// "reported all five binds writable" and "would have passed even with completely
// wrong ownership — the exact failure the check exists to catch". Docker itself
// is what drops the privilege, the same way that report's own reproduction did
// (`docker exec -u 1000:1000`), which also means no helper binary has to exist
// inside the image — alpine ships no su-exec, measured.
func CaptureSidecarAs(ctx context.Context, c *client.Client, targetID, user string, cmd []string) ([]byte, error) {
	if err := ensureSidecar(ctx, c); err != nil {
		return nil, err
	}
	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: cmd, User: user, AttachStdout: true, AttachStderr: true, Labels: sidecarLabels()},
		&container.HostConfig{VolumesFrom: []string{targetID}, AutoRemove: false},
		nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("write-test sidecar create: %w", err)
	}
	sidecarID := created.ID
	defer removeContainer(c, sidecarID)

	att, err := c.ContainerAttach(ctx, sidecarID, container.AttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		return nil, fmt.Errorf("write-test sidecar attach: %w", err)
	}
	defer att.Close()
	if err := c.ContainerStart(ctx, sidecarID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("write-test sidecar start: %w", err)
	}
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, att.Reader); err != nil {
		return nil, fmt.Errorf("write-test sidecar read: %w", err)
	}
	waitCh, errCh := c.ContainerWait(context.Background(), sidecarID, container.WaitConditionNotRunning)
	select {
	case <-waitCh:
	case e := <-errCh:
		return nil, e
	case <-time.After(2 * time.Minute):
		return nil, fmt.Errorf("write test timed out")
	}
	return stdout.Bytes(), nil
}

func CaptureSidecarRO(ctx context.Context, c *client.Client, targetID string, cmd []string) ([]byte, error) {
	if err := ensureSidecar(ctx, c); err != nil {
		return nil, err
	}
	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: cmd, AttachStdout: true, AttachStderr: true, Labels: sidecarLabels()},
		&container.HostConfig{VolumesFrom: []string{targetID + ":ro"}, AutoRemove: false},
		nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("index sidecar create: %w", err)
	}
	sidecarID := created.ID
	defer removeContainer(c, sidecarID)

	att, err := c.ContainerAttach(ctx, sidecarID, container.AttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		return nil, fmt.Errorf("index sidecar attach: %w", err)
	}
	defer att.Close()
	if err := c.ContainerStart(ctx, sidecarID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("index sidecar start: %w", err)
	}

	var stdout, stderr bytes.Buffer
	// Bound stdout; stderr (stat errors on vanished files) is captured but not fatal.
	lw := &limitedWriter{w: &stdout, n: MaxIndexBytes}
	_, copyErr := stdcopy.StdCopy(lw, &stderr, att.Reader)
	att.Close()

	waitCh, errCh := c.ContainerWait(context.Background(), sidecarID, container.WaitConditionNotRunning)
	select {
	case st := <-waitCh:
		if st.StatusCode != 0 && copyErr == nil && stdout.Len() == 0 {
			return nil, fmt.Errorf("index sidecar exited %d: %s", st.StatusCode, strings.TrimSpace(stderr.String()))
		}
	case e := <-errCh:
		if copyErr == nil {
			copyErr = e
		}
	case <-time.After(15 * time.Minute):
		return nil, fmt.Errorf("index sidecar timed out")
	}
	if lw.over {
		return nil, fmt.Errorf("volume file index exceeds %d bytes — too many files for an incremental backup", MaxIndexBytes)
	}
	if copyErr != nil {
		return nil, copyErr
	}
	return stdout.Bytes(), nil
}

// TarVolumesFromList streams a tar of ONLY the given volume-relative paths from a
// read-only sidecar — the incremental "changed files only" payload (F61). The path
// list is fed via stdin (unbounded, no ARG_MAX limit) into a temp file the sidecar
// then hands to `tar -T`, so a large changed-set is fine. Paths are volume-relative
// (leading slash stripped) so `tar -C /` restores them to the right place, exactly
// like the full `volumes.tar`.
func TarVolumesFromList(ctx context.Context, c *client.Client, targetID string, relPaths []string) (io.ReadCloser, error) {
	return tarFromList(ctx, c, targetID, relPaths, "")
}

// TarVolumeEntries streams a tar of exactly the given volume-relative entries —
// a directory as itself, never what is inside it — from a read-only sidecar:
// the second pass of a short freeze, copying again only what changed while the
// first pass ran with the application still up. A tar without --no-recursion
// fails the stream rather than copying whole directories.
func TarVolumeEntries(ctx context.Context, c *client.Client, targetID string, relPaths []string) (io.ReadCloser, error) {
	return tarFromList(ctx, c, targetID, relPaths, " --no-recursion")
}

func tarFromList(ctx context.Context, c *client.Client, targetID string, relPaths []string, tarFlags string) (io.ReadCloser, error) {
	if len(relPaths) == 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	if err := ensureSidecar(ctx, c); err != nil {
		return nil, err
	}
	// cat the stdin list to a temp file, then tar strictly from that list. Using a
	// file (not `-T -`) sidesteps BusyBox tar stdin-list quirks.
	sh := "cat > /tmp/dback-inc.list && tar -cf - -C /" + tarFlags + " -T /tmp/dback-inc.list"
	created, err := c.ContainerCreate(ctx,
		&container.Config{
			Image: sidecarRef(), Cmd: []string{"/bin/sh", "-c", sh},
			OpenStdin: true, StdinOnce: true, AttachStdin: true, AttachStdout: true, AttachStderr: true,
			Labels: sidecarLabels(),
		},
		&container.HostConfig{VolumesFrom: []string{targetID + ":ro"}, AutoRemove: false},
		nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("delta sidecar create: %w", err)
	}
	sidecarID := created.ID
	att, err := c.ContainerAttach(ctx, sidecarID, container.AttachOptions{Stream: true, Stdin: true, Stdout: true, Stderr: true})
	if err != nil {
		_ = removeContainer(c, sidecarID)
		return nil, fmt.Errorf("delta sidecar attach: %w", err)
	}
	if err := c.ContainerStart(ctx, sidecarID, container.StartOptions{}); err != nil {
		att.Close()
		_ = removeContainer(c, sidecarID)
		return nil, fmt.Errorf("delta sidecar start: %w", err)
	}

	// Feed the NL-joined list, then EOF so `cat` completes and `tar` runs.
	go func() {
		_, _ = io.Copy(att.Conn, strings.NewReader(strings.Join(relPaths, "\n")+"\n"))
		att.CloseWrite()
	}()

	pr, pw := io.Pipe()
	// Cancel must end the stream immediately (see abortSidecarOnCancel).
	streamDone := make(chan struct{})
	abortSidecarOnCancel(ctx, c, sidecarID, pw, streamDone)
	go func() {
		defer close(streamDone)
		var stderr bytes.Buffer
		_, copyErr := stdcopy.StdCopy(pw, &stderr, att.Reader)
		att.Close()
		if ctx.Err() != nil {
			_ = removeContainer(c, sidecarID)
			pw.CloseWithError(ctx.Err())
			return
		}
		waitCh, errCh := c.ContainerWait(context.Background(), sidecarID, container.WaitConditionNotRunning)
		finalErr := copyErr
		select {
		case st := <-waitCh:
			if st.StatusCode != 0 && finalErr == nil {
				finalErr = fmt.Errorf("delta sidecar tar exited %d: %s", st.StatusCode, strings.TrimSpace(stderr.String()))
			}
		case e := <-errCh:
			if finalErr == nil {
				finalErr = e
			}
		case <-time.After(30 * time.Minute):
			if finalErr == nil {
				finalErr = fmt.Errorf("delta sidecar tar timed out")
			}
		}
		_ = removeContainer(c, sidecarID)
		pw.CloseWithError(finalErr)
	}()
	return pr, nil
}

// DeleteVolumePaths removes the given volume-relative paths from the target's
// volumes via a read-WRITE sidecar — the deletion phase of an incremental restore
// (F61), applied after the delta tar so a file removed since the parent is removed
// on restore too. Paths are NUL-delimited on stdin (`xargs -0`) so odd filenames
// are safe, and each is validated caller-side to be a benign volume-relative path.
// A no-op for an empty list.
func DeleteVolumePaths(ctx context.Context, c *client.Client, targetID string, relPaths []string) error {
	if len(relPaths) == 0 {
		return nil
	}
	if err := ensureSidecar(ctx, c); err != nil {
		return err
	}
	hostConfig, err := restoreHostConfig(ctx, c, targetID)
	if err != nil {
		return err
	}
	created, err := c.ContainerCreate(ctx,
		&container.Config{
			Image: sidecarRef(), Cmd: []string{"/bin/sh", "-c", "cd / && xargs -0 rm -f"},
			OpenStdin: true, StdinOnce: true, AttachStdin: true, AttachStdout: true, AttachStderr: true,
			Labels: sidecarLabels(),
		},
		hostConfig,
		nil, nil, "")
	if err != nil {
		return fmt.Errorf("delete sidecar create: %w", err)
	}
	sidecarID := created.ID
	defer removeContainer(c, sidecarID)
	att, err := c.ContainerAttach(ctx, sidecarID, container.AttachOptions{Stream: true, Stdin: true, Stdout: true, Stderr: true})
	if err != nil {
		return fmt.Errorf("delete sidecar attach: %w", err)
	}
	defer att.Close()
	if err := c.ContainerStart(ctx, sidecarID, container.StartOptions{}); err != nil {
		return fmt.Errorf("delete sidecar start: %w", err)
	}
	// NUL-delimited list (xargs -0).
	if _, err := io.Copy(att.Conn, strings.NewReader(strings.Join(relPaths, "\x00")+"\x00")); err != nil {
		return fmt.Errorf("streaming delete list: %w", err)
	}
	att.CloseWrite()
	waitCh, errCh := c.ContainerWait(ctx, sidecarID, container.WaitConditionNotRunning)
	select {
	case st := <-waitCh:
		if st.StatusCode != 0 {
			return fmt.Errorf("delete sidecar exited %d", st.StatusCode)
		}
	case e := <-errCh:
		return e
	case <-time.After(10 * time.Minute):
		return fmt.Errorf("delete sidecar timed out")
	}
	return nil
}

// limitedWriter caps how many bytes are written through it, flipping `over` once
// the limit is exceeded so the caller can reject an oversized index.
type limitedWriter struct {
	w    io.Writer
	n    int64
	over bool
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.over {
		return len(p), nil // swallow the rest; caller checks l.over
	}
	if int64(len(p)) > l.n {
		l.over = true
		if l.n > 0 {
			_, _ = l.w.Write(p[:l.n])
		}
		l.n = 0
		return len(p), nil
	}
	l.n -= int64(len(p))
	return l.w.Write(p)
}
