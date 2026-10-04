package dockercli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/docker/docker/client"
)

// ErrRecordedImageUnavailable means the image a backup ran can be neither found
// on this host nor pulled, and its tag now names a different image.
var ErrRecordedImageUnavailable = errors.New("the image this backup ran is no longer available")

// RecordedImageError says what was recorded and what is on offer instead.
type RecordedImageError struct {
	Tag        string // the tag the container was created from
	RecordedID string // the image the backup ran
	TagID      string // the image the tag names now
	PullErr    error  // why the recorded digest could not be pulled, nil when none was recorded
}

func (e *RecordedImageError) Error() string {
	why := "no digest was recorded"
	if e.PullErr != nil {
		why = "its digest could not be pulled: " + e.PullErr.Error()
	}
	return fmt.Sprintf("the image this backup ran (%s) is not on this host and %s; %s now names a different image (%s). "+
		"Restoring would run a newer version on older data, which some applications migrate forward and cannot undo. "+
		"Load the backup's image.tar, or restore again allowing a different image",
		shortImageID(e.RecordedID), why, e.Tag, shortImageID(e.TagID))
}

func (e *RecordedImageError) Unwrap() error { return ErrRecordedImageUnavailable }

// provideRecordedImage makes the backed-up image available, and that image
// only:
//
//  1. the recorded image already on this host (a same-host restore, or an
//     image.tar the caller has just loaded), named by its tag or digest when
//     either resolves to it, so the container keeps a readable image name;
//  2. else the recorded digest, pulled;
//  3. else the tag, when it names the same image, or when the caller allows a
//     different one.
//
// The order used to check local presence for every candidate first, so a tag
// already updated on the host won over pulling the recorded digest: Uptime Kuma
// 2 came back in place of a backed-up version 1 and migrated its data forward.
// pulled reports whether this call fetched the image it returns.
func provideRecordedImage(ctx context.Context, c *client.Client, recordedID, digest, tag string, allowDifferent bool) (ref string, pulled bool, warning string, err error) {
	if recordedID != "" && imageIDOf(ctx, c, recordedID) != "" {
		for _, name := range []string{tag, digest} {
			if name != "" && imageIDOf(ctx, c, name) == imageIDOf(ctx, c, recordedID) {
				return name, false, "", nil
			}
		}
		return recordedID, false, "", nil
	}
	var pullErr error
	if strings.Contains(digest, "@sha256:") {
		if pullErr = pullImage(ctx, c, digest); pullErr == nil {
			return digest, true, "", nil
		}
	}
	if tag == "" {
		return "", false, "", fmt.Errorf("no image reference to recreate from: %w", errors.Join(ErrRecordedImageUnavailable, pullErr))
	}
	if imageIDOf(ctx, c, tag) == "" {
		if err := pullImage(ctx, c, tag); err != nil {
			return "", false, "", err
		}
		pulled = true
	}
	tagID := imageIDOf(ctx, c, tag)
	if recordedID == "" || tagID == recordedID {
		return tag, pulled, "", nil
	}
	mismatch := &RecordedImageError{Tag: tag, RecordedID: recordedID, TagID: tagID, PullErr: pullErr}
	if !allowDifferent {
		return "", pulled, "", mismatch
	}
	return tag, pulled, "Recreated from a DIFFERENT image than the backup ran, as allowed: " + mismatch.Error(), nil
}

// imageIDOf resolves a reference to the local image id, "" when it is not here.
func imageIDOf(ctx context.Context, c *client.Client, ref string) string {
	img, _, err := c.ImageInspectWithRaw(ctx, ref)
	if err != nil {
		return ""
	}
	return img.ID
}

func shortImageID(id string) string {
	trimmed := strings.TrimPrefix(id, "sha256:")
	if len(trimmed) > 12 {
		return trimmed[:12]
	}
	return trimmed
}
