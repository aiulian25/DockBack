package dockercli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// Reading how much room a host path actually has (#37).
//
// R5 §3: a 58 GB library restored onto a host whose root filesystem had 74 GB
// free would have "worked" and left ~16 GB for the seventeen other containers
// sharing that disk. The same host had 2.2 TB free on /mnt/data. So the question
// is never "how much room does this host have" — it is "how much room does the
// filesystem THIS path lives on have", and only df can answer that.

// FSUsage describes the filesystem a host path resolves to.
type FSUsage struct {
	// Filesystem is the device or source as df reports it.
	Filesystem string
	// MountPoint is the filesystem's mount point — the thing that makes two
	// paths on one host different answers.
	MountPoint string
	// FreeBytes is space available to an unprivileged writer, which is what a
	// restore actually gets: it excludes the root-reserved blocks that make
	// "free" and "available" different numbers.
	FreeBytes int64
	// TotalBytes is the filesystem's size, used to scale the safety margin.
	TotalBytes int64
}

// dfCommand asks for POSIX output in 1 KiB units.
//
// Not -B1. Byte units are a GNU extension that BusyBox happens to accept in the
// default sidecar image but is not required to, and an operator may point
// DOCKBACK_SIDECAR_IMAGE at anything. -Pk is POSIX and universal, and a kibibyte
// of granularity does not change any decision made about gigabytes.
const dfCommand = "df -Pk"

// FSFreeBytes reports the filesystem backing a host path.
//
// The path need not exist yet — a restore asks about a directory it has not
// created. The probe walks up to the nearest existing ancestor, which is on the
// same filesystem as the path would be, unless something else gets mounted in
// between; that is the right answer for the question being asked.
func FSFreeBytes(ctx context.Context, c *client.Client, hostPath string) (FSUsage, error) {
	clean := strings.TrimSpace(hostPath)
	if !strings.HasPrefix(clean, "/") {
		return FSUsage{}, fmt.Errorf("free-space probe needs an absolute path, got %q", hostPath)
	}
	if strings.ContainsAny(clean, "\n\r\x00") {
		return FSUsage{}, fmt.Errorf("free-space probe refused path %q", hostPath)
	}

	ctx, cancel := context.WithTimeout(ctx, hostProbeTimeout)
	defer cancel()
	if err := ensureSidecar(ctx, c); err != nil {
		return FSUsage{}, err
	}
	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: []string{"sleep", "60"}, Labels: sidecarLabels()},
		&container.HostConfig{Binds: []string{"/:" + hostProbeMount + ":ro"}},
		nil, nil, "")
	if err != nil {
		return FSUsage{}, fmt.Errorf("free-space probe sidecar create: %w", err)
	}
	defer removeContainer(c, created.ID)
	if err := c.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return FSUsage{}, fmt.Errorf("free-space probe sidecar start: %w", err)
	}

	out, err := ExecCapture(ctx, c, created.ID, []string{"sh", "-c", dfScript(clean)})
	if err != nil {
		return FSUsage{}, fmt.Errorf("free-space probe: %w", err)
	}
	return parseDFOutput(string(out), hostProbeMount)
}

// dfScript walks to the nearest existing ancestor and asks df about it.
func dfScript(hostPath string) string {
	return "p=" + shQuote(hostProbeMount+hostPath) + "; " +
		`while [ ! -e "$p" ] && [ "$p" != "/" ]; do p=$(dirname "$p"); done; ` +
		dfCommand + ` "$p"`
}

// parseDFOutput reads one df -Pk report.
//
// Fields are counted from the END. A device name can contain spaces (an NFS or
// CIFS export routinely does), and only the five trailing columns are fixed, so
// splitting from the left would mis-assign every number on exactly the hosts
// R5 §3 is about.
//
// mountPrefix, when the report came from a probe that bound the host at a
// prefix, is stripped from the mount point so the caller sees the host's own
// path rather than the sidecar's view of it.
func parseDFOutput(out, mountPrefix string) (FSUsage, error) {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 6 || strings.EqualFold(fields[0], "Filesystem") {
			continue
		}
		n := len(fields)
		avail, aerr := parseDFBlocks(fields[n-3])
		total, terr := parseDFBlocks(fields[n-5])
		if aerr != nil {
			// df prints "-" for a filesystem that cannot report a size (some
			// network and pseudo filesystems). Unknown is not zero, and it is not
			// "plenty" either — say so rather than let a caller compare against a
			// number that means nothing.
			return FSUsage{}, fmt.Errorf("filesystem reports no available-space figure (%q)", fields[n-3])
		}
		if terr != nil {
			total = 0
		}
		mount := fields[n-1]
		if mountPrefix != "" && strings.HasPrefix(mount, mountPrefix) {
			if trimmed := strings.TrimPrefix(mount, mountPrefix); trimmed != "" {
				mount = trimmed
			} else {
				mount = "/"
			}
		}
		return FSUsage{
			Filesystem: strings.Join(fields[:n-5], " "),
			MountPoint: mount,
			FreeBytes:  avail,
			TotalBytes: total,
		}, nil
	}
	return FSUsage{}, fmt.Errorf("could not read a filesystem report from df output")
}

// parseDFBlocks converts one df -Pk column to bytes.
func parseDFBlocks(field string) (int64, error) {
	blocks, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
	if err != nil || blocks < 0 {
		return 0, fmt.Errorf("not a block count: %q", field)
	}
	return blocks * 1024, nil
}

// DockerRootDir is where the daemon keeps its volumes and images.
//
// A clone gets fresh named volumes rather than the source's bind paths, so this
// is the filesystem its data actually lands on — asking about the recorded bind
// sources would measure a disk the clone never touches.
func DockerRootDir(ctx context.Context, c *client.Client) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	info, err := c.Info(ctx)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(info.DockerRootDir) == "" {
		return "", fmt.Errorf("the daemon reports no data root")
	}
	return info.DockerRootDir, nil
}
