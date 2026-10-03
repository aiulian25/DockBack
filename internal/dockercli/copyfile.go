package dockercli

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// Writing a single recovered file back into a running container (F96).
//
// Everything else in DockBack reads FROM a container. This is the only write
// path that puts one file back, so the guarantees are deliberately narrow: one
// regular file, at one absolute path, with the size the archive recorded — and
// nothing else. A tar stream can express symlinks, device nodes, directory
// entries and `../` escapes; none of those are constructible here, because the
// header is built from validated parts rather than copied from the archive.

// FileMeta is what a recovered file must carry back with it. Restoring a config
// file that the application then cannot read (wrong mode) or cannot open (wrong
// owner) is not a recovery, so ownership and mode travel with the bytes.
type FileMeta struct {
	Mode    int64 // permission bits; masked to 0o7777, type bits are ignored
	Size    int64 // exact byte count — the reader must supply precisely this many
	UID     int
	GID     int
	ModTime time.Time
}

// CopyFileToContainer streams r into container id as a single regular file at
// destPath, preserving mode and ownership.
//
// Streaming end to end: the tar is generated into a pipe as the daemon consumes
// it, so a multi-gigabyte file never lands in memory or on disk.
//
// The size is a CONTRACT, not a hint. A tar header declaring one length while
// the body carries another is how a truncated or over-long member corrupts the
// stream, so a mismatch fails loudly instead of writing a damaged file.
func CopyFileToContainer(ctx context.Context, c *client.Client, id, destPath string, meta FileMeta, r io.Reader) error {
	dir, base, err := splitContainerPath(destPath)
	if err != nil {
		return err
	}
	if meta.Size < 0 {
		return fmt.Errorf("invalid file size %d", meta.Size)
	}

	pr := singleFileTar(base, meta, r)
	defer pr.Close()

	if err := c.CopyToContainer(ctx, id, dir, pr, container.CopyToContainerOptions{}); err != nil {
		return fmt.Errorf("writing %s into the container: %w", destPath, err)
	}
	return nil
}

// singleFileTar generates a one-entry tar stream on the fly.
//
// Split out from the copy so the exact shape of what gets written — one regular
// file, the recorded mode and ownership, precisely Size bytes — is testable
// without a Docker daemon.
func singleFileTar(base string, meta FileMeta, r io.Reader) io.ReadCloser {
	mode := meta.Mode & 0o7777
	if mode == 0 {
		mode = 0o644 // an archive with no recorded mode still has to land readable
	}
	mtime := meta.ModTime
	if mtime.IsZero() {
		mtime = time.Now()
	}

	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		hdr := &tar.Header{
			Typeflag: tar.TypeReg, // never a symlink, device or hardlink
			Name:     base,
			Mode:     mode,
			Size:     meta.Size,
			Uid:      meta.UID,
			Gid:      meta.GID,
			ModTime:  mtime,
			Format:   tar.FormatPAX, // UTF-8 names and >8GB sizes without games
		}
		if err := tw.WriteHeader(hdr); err != nil {
			pw.CloseWithError(err)
			return
		}
		n, err := io.Copy(tw, io.LimitReader(r, meta.Size))
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if n != meta.Size {
			pw.CloseWithError(fmt.Errorf("archived file is short: %d of %d bytes", n, meta.Size))
			return
		}
		if err := tw.Close(); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.Close()
	}()
	return pr
}

// CopyFileAsideInContainer copies srcPath to dstPath INSIDE the container's
// mounts, so a write-back can be undone.
//
// It runs in the volume sidecar rather than the target container: the target may
// be distroless (no `cp`, no shell) or stopped, and a recovery that only works
// on containers that happen to ship busybox is not a recovery. The sidecar sees
// exactly the target's volumes and binds — the same view the backup was taken
// through.
//
// A MISSING source is not an error. "I deleted one file" is the headline case
// for this feature, and there is nothing to preserve when the file is already
// gone.
func CopyFileAsideInContainer(ctx context.Context, c *client.Client, targetID, srcPath, dstPath string) error {
	if _, _, err := splitContainerPath(srcPath); err != nil {
		return err
	}
	if _, _, err := splitContainerPath(dstPath); err != nil {
		return err
	}
	if err := ensureSidecar(ctx, c); err != nil {
		return err
	}
	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: []string{"sleep", "120"}, Labels: sidecarLabels()},
		&container.HostConfig{VolumesFrom: []string{targetID}},
		nil, nil, "")
	if err != nil {
		return fmt.Errorf("sidecar create: %w", err)
	}
	defer removeContainer(c, created.ID)
	if err := c.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("sidecar start: %w", err)
	}
	// Paths travel as ARGUMENTS, never interpolated into the script, so no path
	// can be read as shell syntax however it is spelled.
	out, err := ExecCapture(ctx, c, created.ID, []string{
		"sh", "-c", `if [ -e "$1" ]; then cp -a -- "$1" "$2"; fi`, "sh", srcPath, dstPath,
	})
	if err != nil {
		return fmt.Errorf("keeping a copy of the current file: %w", err)
	}
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return fmt.Errorf("keeping a copy of the current file: %s", msg)
	}
	return nil
}

// splitContainerPath validates an in-container destination and splits it into
// the parent directory and the file name.
//
// Fail-closed on anything that is not a plain absolute path to a named file:
// `path.Clean` must be a no-op (so no `..` segment survives to be resolved by
// the daemon), and the last element must be a real name.
func splitContainerPath(p string) (dir, base string, err error) {
	if p == "" || !strings.HasPrefix(p, "/") {
		return "", "", fmt.Errorf("path must be absolute: %q", p)
	}
	if strings.ContainsRune(p, 0) {
		return "", "", fmt.Errorf("path contains a null byte")
	}
	if path.Clean(p) != p {
		return "", "", fmt.Errorf("path must be canonical: %q", p)
	}
	dir, base = path.Split(p)
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" {
		dir = "/"
	}
	if base == "" || base == "." || base == ".." {
		return "", "", fmt.Errorf("path does not name a file: %q", p)
	}
	return dir, base, nil
}
