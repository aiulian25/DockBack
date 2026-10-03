package dockercli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// volumeNameRe is Docker's own volume-name grammar. Enforced before a name is ever
// embedded in a sidecar bind spec, so a crafted name can't inject a mount option or
// a second bind (SEC — mirrors validContainerName).
var volumeNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,254}$`)

// ValidVolumeName reports whether name is a syntactically valid Docker volume name.
func ValidVolumeName(name string) bool { return volumeNameRe.MatchString(name) }

// VolumeExists reports whether a named volume is present on the daemon.
//
// The three-way error handling is the whole point, and it is deliberately NOT
// the bare `err == nil` idiom used elsewhere in this package (EnsureVolume,
// volumeopts.go). This is asked immediately before something DESTRUCTIVE — "is
// there anything here worth saving before I overwrite it?" — and for that
// question, conflating "no such volume" with "the daemon did not answer" is
// exactly backwards: an unreachable daemon or a permission failure would read as
// "nothing to lose" and skip the rollback point.
//
// So: not-found is a clean false, and every OTHER error is returned for the
// caller to refuse on. A missing volume genuinely has nothing to snapshot; an
// unanswered question does not mean the same thing.
func VolumeExists(ctx context.Context, c *client.Client, name string) (bool, error) {
	if !ValidVolumeName(name) {
		return false, fmt.Errorf("invalid volume name")
	}
	if _, err := c.VolumeInspect(ctx, name); err != nil {
		if client.IsErrNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// OrphanVolume is a named volume that still holds data but is attached to NO
// container — a backup blind spot: deleting a container silently leaves its data
// unprotected and invisible (F23). Bytes is a best-effort on-disk size (0 = unknown).
type OrphanVolume struct {
	Name   string `json:"name"`
	Driver string `json:"driver"`
	Bytes  int64  `json:"bytes"`
}

// orphanVolumeNames is the pure set-difference at the heart of F23: every named
// volume that no container references via its mounts. Kept side-effect-free so the
// selection logic is unit-testable without a live daemon.
func orphanVolumeNames(containers []*Container, volumeNames []string) []string {
	inUse := map[string]bool{}
	for _, c := range containers {
		if c == nil {
			continue
		}
		for _, m := range c.Mounts {
			if m.Type == "volume" && m.Name != "" {
				inUse[m.Name] = true
			}
		}
	}
	out := make([]string, 0)
	for _, name := range volumeNames {
		if name != "" && !inUse[name] {
			out = append(out, name)
		}
	}
	sort.Slice(out, func(i, j int) bool { return lessName(out[i], out[j]) })
	return out
}

// OrphanVolumes lists named volumes attached to no container (F23), each with a
// best-effort on-disk size. Anonymous volumes (64-hex docker-generated names) are
// still listed — they hold data too — but the caller/UI may choose to de-emphasize
// them.
func OrphanVolumes(ctx context.Context, c *client.Client) ([]OrphanVolume, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	conts, err := ListContainers(ctx, c)
	if err != nil {
		return nil, err
	}
	vl, err := c.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		return nil, err
	}
	driver := map[string]string{}
	names := make([]string, 0, len(vl.Volumes))
	for _, v := range vl.Volumes {
		if v == nil || v.Name == "" {
			continue
		}
		names = append(names, v.Name)
		driver[v.Name] = v.Driver
	}
	orphans := orphanVolumeNames(conts, names)
	if len(orphans) == 0 {
		return []OrphanVolume{}, nil
	}
	sizes := volumeSizes(ctx, c, orphans) // best-effort; 0 on failure

	out := make([]OrphanVolume, 0, len(orphans))
	for _, name := range orphans {
		out = append(out, OrphanVolume{Name: name, Driver: driver[name], Bytes: sizes[name]})
	}
	return out, nil
}

// volumeSizes measures each named volume's on-disk size with a single read-only
// sidecar (du -sb), best-effort — a volume that can't be measured comes back as 0.
// Capped so a host with a huge number of orphan volumes can't create an unwieldy
// number of mounts.
func volumeSizes(ctx context.Context, c *client.Client, names []string) map[string]int64 {
	out := map[string]int64{}
	if len(names) == 0 {
		return out
	}
	const maxMeasure = 100
	if len(names) > maxMeasure {
		names = names[:maxMeasure]
	}
	if err := ensureSidecar(ctx, c); err != nil {
		return out
	}
	// Mount each volume read-only at /v/<i> and print "<i>|<bytes>".
	binds := make([]string, 0, len(names))
	for i, name := range names {
		binds = append(binds, name+":/v/"+strconv.Itoa(i)+":ro")
	}
	script := `i=0; for d in /v/*; do echo "$(basename "$d")|$(du -sb "$d" 2>/dev/null | cut -f1)"; done`
	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: []string{"sh", "-c", script}, AttachStdout: true, AttachStderr: true, Labels: sidecarLabels()},
		&container.HostConfig{Binds: binds}, nil, nil, "")
	if err != nil {
		return out
	}
	sidecarID := created.ID
	defer removeContainer(c, sidecarID)

	att, err := c.ContainerAttach(ctx, sidecarID, container.AttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		return out
	}
	if err := c.ContainerStart(ctx, sidecarID, container.StartOptions{}); err != nil {
		att.Close()
		return out
	}
	var stdout, stderr bytes.Buffer
	done := make(chan error, 1)
	go func() { _, e := stdcopy.StdCopy(&stdout, &stderr, att.Reader); done <- e }()
	select {
	case <-done:
	case <-ctx.Done():
		att.Close()
		return out
	}
	att.Close()
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		i := strings.IndexByte(line, '|')
		if i < 0 {
			continue
		}
		idx, perr := strconv.Atoi(strings.TrimSpace(line[:i]))
		if perr != nil || idx < 0 || idx >= len(names) {
			continue
		}
		var n int64
		fmt.Sscan(strings.TrimSpace(line[i+1:]), &n)
		out[names[idx]] = n
	}
	return out
}

// TarNamedVolume streams a tar of a named volume's CONTENTS (relative to the
// volume root) from a read-only sidecar (F23) — the standalone-volume analogue of
// TarVolumesFrom. The caller reads and closes the returned ReadCloser; the sidecar
// is auto-removed.
func TarNamedVolume(ctx context.Context, c *client.Client, volName string) (io.ReadCloser, error) {
	if !ValidVolumeName(volName) {
		return nil, fmt.Errorf("invalid volume name")
	}
	if err := ensureSidecar(ctx, c); err != nil {
		return nil, err
	}
	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: []string{"tar", "-cf", "-", "-C", "/voldata", "."}, AttachStdout: true, AttachStderr: true, Labels: sidecarLabels()},
		&container.HostConfig{Binds: []string{volName + ":/voldata:ro"}, AutoRemove: false}, nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("volume tar sidecar create: %w", err)
	}
	sidecarID := created.ID

	att, err := c.ContainerAttach(ctx, sidecarID, container.AttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		_ = removeContainer(c, sidecarID)
		return nil, fmt.Errorf("volume tar attach: %w", err)
	}
	if err := c.ContainerStart(ctx, sidecarID, container.StartOptions{}); err != nil {
		att.Close()
		_ = removeContainer(c, sidecarID)
		return nil, fmt.Errorf("volume tar start: %w", err)
	}
	pr, pw := io.Pipe()
	// Cancel must end the stream immediately, not after the whole volume is tarred.
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
				finalErr = fmt.Errorf("volume tar sidecar exited %d: %s", st.StatusCode, strings.TrimSpace(stderr.String()))
			}
		case e := <-errCh:
			if finalErr == nil {
				finalErr = e
			}
		case <-time.After(30 * time.Minute):
			if finalErr == nil {
				finalErr = fmt.Errorf("volume tar timed out")
			}
		}
		_ = removeContainer(c, sidecarID)
		pw.CloseWithError(finalErr)
	}()
	return pr, nil
}

// UntarToNamedVolume (re)creates a named volume and extracts a contents-relative
// tar into it via a read-write sidecar (F23) — the inverse of TarNamedVolume, used
// to restore a standalone volume backup. VolumeCreate is idempotent, so an existing
// volume is reused (its contents merged/overwritten by the tar).
func UntarToNamedVolume(ctx context.Context, c *client.Client, volName string, tarStream io.Reader) error {
	if !ValidVolumeName(volName) {
		return fmt.Errorf("invalid volume name")
	}
	if err := ensureSidecar(ctx, c); err != nil {
		return err
	}
	if _, err := c.VolumeCreate(ctx, volume.CreateOptions{Name: volName}); err != nil {
		return fmt.Errorf("create volume %q: %w", volName, err)
	}
	created, err := c.ContainerCreate(ctx,
		&container.Config{
			Image: sidecarRef(), Cmd: []string{"tar", "-xf", "-", "-C", "/voldata"},
			OpenStdin: true, StdinOnce: true, AttachStdin: true, Labels: sidecarLabels(),
		},
		&container.HostConfig{Binds: []string{volName + ":/voldata"}}, nil, nil, "")
	if err != nil {
		return fmt.Errorf("volume restore sidecar create: %w", err)
	}
	sidecarID := created.ID
	defer removeContainer(c, sidecarID)

	att, err := c.ContainerAttach(ctx, sidecarID, container.AttachOptions{Stream: true, Stdin: true, Stdout: true, Stderr: true})
	if err != nil {
		return fmt.Errorf("volume restore attach: %w", err)
	}
	defer att.Close()
	if err := c.ContainerStart(ctx, sidecarID, container.StartOptions{}); err != nil {
		return fmt.Errorf("volume restore start: %w", err)
	}
	if _, err := io.Copy(att.Conn, tarStream); err != nil {
		return fmt.Errorf("streaming tar to volume sidecar: %w", err)
	}
	att.CloseWrite()

	waitCh, errCh := c.ContainerWait(ctx, sidecarID, container.WaitConditionNotRunning)
	select {
	case st := <-waitCh:
		if st.StatusCode != 0 {
			return fmt.Errorf("volume restore tar exited %d", st.StatusCode)
		}
	case e := <-errCh:
		return e
	case <-time.After(30 * time.Minute):
		return fmt.Errorf("volume restore tar timed out")
	}
	return nil
}
