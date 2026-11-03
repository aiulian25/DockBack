package dockercli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

const (
	// cloneRedactionTimeout bounds editing a few small files.
	cloneRedactionTimeout = 5 * time.Minute
	// cloneRedactionMaxOutput bounds what the helper can return: a status line.
	cloneRedactionMaxOutput = 64 << 10
)

// cloneVolumesOnly is the part of a clone's mounts a redaction may touch: its named
// volumes, which DockBack made for it. A bind path is somebody else's directory on the
// host, possibly the original's, so a redaction must never be able to reach it. Pure.
func cloneVolumesOnly(points []types.MountPoint) []mount.Mount {
	var mounts []mount.Mount
	for _, point := range points {
		if point.Type != mount.TypeVolume || point.Name == "" || point.Destination == "" {
			continue
		}
		mounts = append(mounts, mount.Mount{Type: mount.TypeVolume, Source: point.Name, Target: point.Destination})
	}
	return mounts
}

// cloneRedactionHostConfig holds the helper to what editing those files takes: no
// network, a read-only root, and of root's powers only the one that lets it write a
// file owned by the application's uid. Pure.
func cloneRedactionHostConfig(points []types.MountPoint) *container.HostConfig {
	return &container.HostConfig{
		Mounts:         cloneVolumesOnly(points),
		NetworkMode:    cloneNetworkMode,
		ReadonlyRootfs: true,
		CapDrop:        []string{"ALL"},
		CapAdd:         []string{"DAC_OVERRIDE"},
		SecurityOpt:    []string{"no-new-privileges"},
	}
}

// RunCloneRedaction runs script against a clone's own volumes, in a throwaway helper,
// and returns what it printed. The clone may be stopped, and for a required redaction
// it has to be: docker exec needs a running container, and an application that has
// already started has already had the chance to use what is being removed. A script
// that exits non-zero is an error, returned with its output.
func RunCloneRedaction(ctx context.Context, c *client.Client, cloneID, script string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, cloneRedactionTimeout)
	defer cancel()
	if err := ensureSidecar(ctx, c); err != nil {
		return nil, err
	}
	info, err := c.ContainerInspect(ctx, cloneID)
	if err != nil {
		return nil, fmt.Errorf("reading the copy's mounts: %w", err)
	}
	created, err := c.ContainerCreate(ctx,
		&container.Config{
			Image: sidecarRef(), Cmd: []string{"/bin/sh", "-c", script},
			AttachStdout: true, AttachStderr: true, Labels: sidecarLabels(),
		},
		cloneRedactionHostConfig(info.Mounts), nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("redaction helper create: %w", err)
	}
	defer removeContainer(c, created.ID)
	attached, err := c.ContainerAttach(ctx, created.ID, container.AttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		return nil, fmt.Errorf("redaction helper attach: %w", err)
	}
	defer attached.Close()
	if err := c.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("redaction helper start: %w", err)
	}
	var output bytes.Buffer
	bounded := &limitedWriter{w: &output, n: cloneRedactionMaxOutput}
	_, copyErr := stdcopy.StdCopy(bounded, bounded, attached.Reader)
	status, waitErr := c.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	select {
	case done := <-status:
		if done.StatusCode != 0 {
			return output.Bytes(), fmt.Errorf("redaction helper exited %d", done.StatusCode)
		}
	case err := <-waitErr:
		return output.Bytes(), fmt.Errorf("redaction helper wait: %w", err)
	case <-ctx.Done():
		return output.Bytes(), errors.Join(errors.New("redaction helper did not finish"), ctx.Err())
	}
	if copyErr != nil {
		return output.Bytes(), fmt.Errorf("reading the redaction helper's output: %w", copyErr)
	}
	return output.Bytes(), nil
}
