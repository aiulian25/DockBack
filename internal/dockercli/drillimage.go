package dockercli

import (
	"context"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

// Putting back what a throwaway borrowed (#36).
//
// A verification drill and a test clone both pull an image they may be the only
// reason the host has. Nothing removed it: there was no ImageRemove anywhere in
// this tool, so a drill of a backup whose image is not on this machine left it
// there — for an Immich or a Nextcloud, gigabytes per drill.
//
// ISSUES.md #36 is about the way the obvious fix fails. Removing an image BY TAG
// untags it and leaves the layers, and the untagged image still carries its
// RepoDigest — so it is not `dangling`, `docker image prune` never reclaims it,
// and "a tool that checks dangling reports a clean teardown and leaves a 1 GB
// orphan". So: remove by ID, and never verify with the dangling filter.
//
// Two rules keep this from deleting somebody's image:
//
//   - Only what THIS drill put here. Presence is checked BEFORE the pull, so an
//     image that was already on the host is never a candidate, whatever else
//     happens.
//   - Only what nothing else is using. Checked, and then left to Docker as well:
//     the removal is unforced, so the daemon refuses if anything still refers to
//     the image and the refusal is treated as "not garbage" rather than as an
//     error worth reporting.

// imageOpTimeout bounds the teardown's own calls. Cleanup runs after the work is
// done and must never be what makes a drill look slow or hung.
const imageOpTimeout = 2 * time.Minute

// DrillImage is what a throwaway borrowed, and whether it is ours to give back.
//
// The zero value means "nothing to release", which is the common case: most
// drills run on a host that already has the image, and every one of those must
// leave it exactly where it found it.
type DrillImage struct {
	// ID is the image identifier as the daemon resolved it — never a tag.
	// Removing by tag is #36 itself.
	ID string
	// Ref is what was asked for, for the log line only.
	Ref string
	// PulledHere records that this call is what put the image on the host.
	PulledHere bool
}

// EnsureDrillImage makes a reference available and remembers whether it had to
// fetch it.
//
// The presence check happens first and its answer is the whole safety property:
// an image already on the host is recorded as not-ours and can never be removed
// later, no matter what the reference count says at teardown.
func EnsureDrillImage(ctx context.Context, c *client.Client, ref string) (DrillImage, error) {
	if strings.TrimSpace(ref) == "" {
		return DrillImage{}, nil
	}
	if insp, _, err := c.ImageInspectWithRaw(ctx, ref); err == nil {
		// Already here before this drill. Borrowed, not acquired.
		return DrillImage{ID: insp.ID, Ref: ref}, nil
	}
	if err := pullImage(ctx, c, ref); err != nil {
		return DrillImage{}, err
	}
	insp, _, err := c.ImageInspectWithRaw(ctx, ref)
	if err != nil {
		// Pulled, but the id could not be read. Without an id there is nothing
		// safe to remove — by-tag removal is the defect this exists to avoid.
		return DrillImage{Ref: ref}, nil
	}
	return DrillImage{ID: insp.ID, Ref: ref, PulledHere: true}, nil
}

// ImageReclaimable decides whether a throwaway's image may be removed.
//
// Pure, and both conditions are necessary. otherRefs counts containers still
// built on the image APART from the throwaway itself: an image in use is not
// garbage, and one that predates the drill was never the drill's to reclaim.
func ImageReclaimable(pulledHere bool, otherRefs int) bool {
	return pulledHere && otherRefs == 0
}

// ImageReferenceCount counts the containers built on an image, in any state.
//
// Stopped containers count: an image a stopped container was created from cannot
// be removed without force, and forcing it would break that container's next
// start. `all` is set for exactly that reason.
func ImageReferenceCount(ctx context.Context, c *client.Client, imageID string, excludeContainerID string) (int, error) {
	list, err := c.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("ancestor", imageID)),
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, ctr := range list {
		if ctr.ID == excludeContainerID {
			continue
		}
		n++
	}
	return n, nil
}

// ReleaseDrillImage removes an image a throwaway pulled, when nothing else is
// using it.
//
// Silent about every reason not to. An image that was already here, one another
// container still references, a daemon that refuses the removal — none of those
// is a problem, and a teardown that reported them would be noise on the end of
// every drill. Returns whether the image was actually removed, so a caller with
// somewhere to say it can.
func ReleaseDrillImage(ctx context.Context, c *client.Client, img DrillImage, excludeContainerID string) bool {
	if img.ID == "" || !img.PulledHere {
		return false
	}
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), imageOpTimeout)
	defer cancel()

	refs, err := ImageReferenceCount(opCtx, c, img.ID, excludeContainerID)
	if err != nil || !ImageReclaimable(img.PulledHere, refs) {
		return false
	}
	// Unforced on purpose: the daemon is the final authority on whether anything
	// still refers to this image, and its refusal is an answer, not a failure.
	// By ID, never by tag — untagging leaves the layers and their repo digest
	// behind, which is #36.
	removed, err := c.ImageRemove(opCtx, img.ID, image.RemoveOptions{Force: false, PruneChildren: true})
	return err == nil && len(removed) > 0
}
