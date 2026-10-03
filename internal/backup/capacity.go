package backup

import (
	"context"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// Refusing a restore that would fill the destination's disk (#37).
//
// R5 §3: 58 GB restored onto a root filesystem with 74 GB free would have
// succeeded, reported success, and left ~16 GB for the seventeen other
// containers on that host. The same machine had 2.2 TB free one mount point
// away. Backups have had a free-space guard since PLAN §2.13; restores summed
// nothing at all.
//
// The check is per-FILESYSTEM, never per host: a host aggregate would have
// called that machine roomy and been exactly wrong.

const (
	// capacityFloorBytes is the least free space worth leaving behind whatever
	// the disk's size. A filesystem with under a couple of gibibytes spare is one
	// where the next log rotation is the outage.
	capacityFloorBytes = 2 << 30

	// capacityFractionPercent scales the margin with the filesystem, because 2
	// GiB left on a 4 TB array is not "fine", it is a rounding error.
	capacityFractionPercent = 10

	// capacityFloorEnv overrides the floor, for tests that need a refusal on a
	// disk that is genuinely roomy.
	capacityFloorEnv = "DOCKBACK_RESTORE_MIN_FREE_BYTES"
)

// RestoreCapacity is the arithmetic behind a placement decision, kept whole so
// the operator sees the same numbers the refusal was made from.
type RestoreCapacity struct {
	// Path is the host location this was measured for.
	Path string `json:"path"`
	// Filesystem and MountPoint identify WHICH disk answered — the distinction
	// the whole feature exists to make.
	Filesystem string `json:"filesystem,omitempty"`
	MountPoint string `json:"mount_point,omitempty"`

	PayloadBytes int64 `json:"payload_bytes"`
	FreeBytes    int64 `json:"free_bytes"`
	TotalBytes   int64 `json:"total_bytes,omitempty"`
	// AfterBytes is what would be left. The number an operator actually needs.
	AfterBytes  int64 `json:"after_bytes"`
	MarginBytes int64 `json:"margin_bytes"`

	// Estimated marks a payload derived from the stored archive rather than
	// measured at capture, so nothing presents a guess as a measurement.
	Estimated bool `json:"estimated,omitempty"`
	// Refuse is the verdict.
	Refuse bool `json:"refuse"`
}

// capacityMargin is the free space a restore must leave behind.
func capacityMargin(total int64) int64 {
	floor := int64(capacityFloorBytes)
	if v := strings.TrimSpace(os.Getenv(capacityFloorEnv)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			floor = n
		}
	}
	if fraction := total / 100 * capacityFractionPercent; fraction > floor {
		return fraction
	}
	return floor
}

// evaluateRestoreCapacity decides whether a payload may land on a filesystem.
//
// Unknown never refuses. A payload of zero means the size could not be
// established and a free space of zero means df could not answer; in both cases
// the honest verdict is that no comparison was made, and blocking a restore on
// a comparison that did not happen would be worse than the risk.
func EvaluateRestoreCapacity(payload int64, usage dockercli.FSUsage, targetPath string, estimated bool) RestoreCapacity {
	c := RestoreCapacity{
		Path:         targetPath,
		Filesystem:   usage.Filesystem,
		MountPoint:   usage.MountPoint,
		PayloadBytes: payload,
		FreeBytes:    usage.FreeBytes,
		TotalBytes:   usage.TotalBytes,
		MarginBytes:  capacityMargin(usage.TotalBytes),
		Estimated:    estimated,
	}
	c.AfterBytes = usage.FreeBytes - payload
	c.Refuse = payload > 0 && usage.FreeBytes > 0 && c.AfterBytes < c.MarginBytes
	return c
}

// RefusalMessage states the whole case in one sentence, the way the bind
// preflight does: every number that made the decision, and the way out.
//
// Named after what it is for — an operator reading this has already been told
// "no" and needs to know what to change, not to be told again that it failed.
func (c RestoreCapacity) RefusalMessage(targetName string) string {
	shortfall := c.MarginBytes - c.AfterBytes
	return fmt.Sprintf(
		"restoring %s here would fill the disk: it needs about %s, and %s (on %s) has %s free, "+
			"which would leave %s where at least %s must stay clear — %s short. "+
			"Restore it to a path on a filesystem with more room (the restore dialog's host-path remap moves the whole stack in one step), or free space here first",
		targetName, humanBytes(c.PayloadBytes), c.Path, c.mountLabel(), humanBytes(c.FreeBytes),
		humanBytes(c.AfterBytes), humanBytes(c.MarginBytes), humanBytes(shortfall))
}

// mountLabel names the filesystem the way an operator recognises it.
func (c RestoreCapacity) mountLabel() string {
	if c.MountPoint != "" {
		return c.MountPoint
	}
	if c.Filesystem != "" {
		return c.Filesystem
	}
	return "the destination filesystem"
}

// restorePayloadBytes is how much data this backup puts on disk.
//
// The capture measures this exactly and records it (SelectionBytes). Older
// archives predate the field, so the stored size is divided by this container's
// learned compression ratio — the same ratio the backup-side guard already
// learns per container (F17). A guess, and marked as one.
func (e *Engine) RestorePayloadBytes(b *store.Backup, man *Manifest) (bytes int64, estimated bool) {
	selection, stored := int64(0), int64(0)
	if man != nil {
		selection, stored = man.SelectionBytes, man.CipherSize
	}
	if stored <= 0 && b != nil {
		stored = b.SizeBytes
	}
	ratio := 0.0
	if b != nil && selection <= 0 && stored > 0 {
		ratio = e.learnedRatio(b.NodeID, b.TargetName)
	}
	return restorePayload(selection, stored, ratio)
}

// restorePayload picks the payload figure and says whether it is a guess.
//
// Divided by the ratio, not multiplied: recordRatio stores stored/uncompressed,
// so recovering the uncompressed size from a stored one inverts it. Multiplying
// would under-state a compressible backup by the square of its own ratio, which
// is the direction that lets a restore overflow a disk.
func restorePayload(selectionBytes, storedBytes int64, learnedRatio float64) (int64, bool) {
	if selectionBytes > 0 {
		return selectionBytes, false
	}
	if storedBytes <= 0 {
		return 0, true
	}
	ratio := learnedRatio
	if ratio <= 0 {
		ratio = estimatedCompressionRatio("")
	}
	return int64(float64(storedBytes) / ratio), true
}

// restoreTargetPath is the host location this restore will write to.
//
// The bind sources, after any remap, are where the data actually lands, so their
// common base is the path to measure. A deployment with no recorded binds writes
// under the configured base directory instead.
func RestoreTargetPath(man *Manifest, fromPath, toPath, hostBaseDir string) string {
	sources := make([]string, 0, 8)
	for _, m := range RecordedBindSources(man, fromPath, toPath) {
		if m.Source != "" {
			sources = append(sources, m.Source)
		}
	}
	if base := CommonPathBase(sources); base != "" {
		return base
	}
	if to := strings.TrimSpace(toPath); to != "" {
		return to
	}
	return strings.TrimSpace(hostBaseDir)
}

// commonPathBase is the deepest directory containing every path.
func CommonPathBase(paths []string) string {
	base := ""
	for _, p := range paths {
		p = path.Clean(strings.TrimSpace(p))
		if !strings.HasPrefix(p, "/") {
			continue
		}
		if base == "" {
			base = p
			continue
		}
		for base != "/" && !pathContains(base, p) && base != p {
			base = path.Dir(base)
		}
	}
	// "/" is every path's ancestor and tells an operator nothing, but it is also
	// the honest answer when the binds genuinely span the root — and the
	// filesystem holding "/" is then exactly the one at risk.
	return base
}

// guardRestoreCapacity refuses a restore that would leave the destination
// filesystem below its safety margin, BEFORE anything is created.
//
// Never blocks on a comparison it could not make: a probe that fails is one log
// line and the restore proceeds, because the alternative is refusing every
// restore onto a host whose df this build cannot read.
func (e *Engine) guardRestoreCapacity(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions, clone bool) error {
	target := RestoreTargetPath(man, opts.RemapFromPath, opts.RemapToPath, opts.HostBaseDir)
	if clone {
		// stripForClone drops every bind, so a clone writes into fresh named
		// volumes on the daemon's own disk. Measuring the source's bind paths
		// would report a filesystem the clone never touches.
		root, err := dockercli.DockerRootDir(ctx, cli)
		if err != nil {
			e.logf(b.ID, "INFO", "Could not read the daemon's data root (%v) — cloning without a capacity check", err)
			return nil
		}
		target = root
	}
	if target == "" {
		return nil
	}
	payload, estimated := e.RestorePayloadBytes(b, man)
	if payload <= 0 {
		return nil
	}
	usage, err := dockercli.FSFreeBytes(ctx, cli, target)
	if err != nil {
		e.logf(b.ID, "INFO", "Could not read free space for %s (%v) — restoring without a capacity check", target, err)
		return nil
	}

	capacity := EvaluateRestoreCapacity(payload, usage, target, estimated)
	if capacity.Refuse {
		return fmt.Errorf("%s", capacity.RefusalMessage(b.TargetName))
	}
	measured := "measured at capture"
	if estimated {
		measured = "estimated from the stored archive"
	}
	e.logf(b.ID, "INFO", "Capacity check: about %s to restore (%s) into %s on %s, which has %s free — about %s would remain",
		humanBytes(capacity.PayloadBytes), measured, target, capacity.mountLabel(),
		humanBytes(capacity.FreeBytes), humanBytes(capacity.AfterBytes))
	return nil
}
