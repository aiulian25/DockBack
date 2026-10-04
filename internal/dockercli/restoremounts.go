package dockercli

import (
	"context"
	"fmt"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
)

// restoreMountsFor gives a restore sidecar the target container's own mounts —
// the same sources at the same destinations — with every one of them writable.
//
// A restore has to write where the container itself only reads: a secrets file
// or a script bound :ro is part of what is being restored. `--volumes-from`
// keeps each mount's mode, and its `:rw` suffix does NOT lift a read-only bind
// (tested against Docker), which is how restores of a stack with a read-only
// secrets file failed half-way. The container is untouched and keeps its own
// read-only mounts.
//
// Mounts are expressed in the classic `-v` form where possible, because that
// form recreates a missing bind source the way --volumes-from did — and a
// missing source is exactly the disaster-recovery case. A path containing a
// colon cannot be written in that form, so it goes through the mount API
// instead. tmpfs and other mount kinds hold nothing a restore writes and are
// left out, as --volumes-from leaves them out.
func restoreMountsFor(points []types.MountPoint) (binds []string, mounts []mount.Mount) {
	for _, point := range points {
		destination := point.Destination
		if destination == "" {
			continue
		}
		var source string
		switch point.Type {
		case mount.TypeBind:
			source = point.Source
		case mount.TypeVolume:
			source = point.Name
		default:
			continue
		}
		if source == "" {
			continue
		}
		if strings.Contains(source, ":") || strings.Contains(destination, ":") {
			mounts = append(mounts, mount.Mount{Type: point.Type, Source: source, Target: destination})
			continue
		}
		binds = append(binds, source+":"+destination+":rw")
	}
	return binds, mounts
}

// restoreHostConfig returns the HostConfig for a sidecar that WRITES into the
// target's mounts. Sidecars that only read keep `VolumesFrom: [id:ro]`.
func restoreHostConfig(ctx context.Context, c *client.Client, targetID string) (*container.HostConfig, error) {
	info, err := c.ContainerInspect(ctx, targetID)
	if err != nil {
		return nil, fmt.Errorf("reading the target's mounts for the restore sidecar: %w", err)
	}
	binds, mounts := restoreMountsFor(info.Mounts)
	return &container.HostConfig{Binds: binds, Mounts: mounts}, nil
}
