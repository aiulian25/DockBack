package dockercli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/image"
)

// From the 2026-10-04 recovery: a restore ran a newer image than the one backed up,
// because a tag already updated on the host won over pulling the recorded
// digest. Two images already on the host stand in for "the version the backup
// ran" (alpine:3.20) and "what the tag names now" (alpine:latest). The digest
// points at a registry that never resolves, so nothing is downloaded.
func TestRecreateUsesTheRecordedImage(t *testing.T) {
	c := dockerForTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	recorded := imageIDOf(ctx, c, "alpine:3.20")
	newer := imageIDOf(ctx, c, "alpine:latest")
	if recorded == "" || newer == "" || recorded == newer {
		t.Skip("needs alpine:3.20 and a different alpine:latest on the host")
	}
	const tag = "dockback-test-recorded:latest"
	if err := c.ImageTag(ctx, "alpine:latest", tag); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = c.ImageRemove(context.Background(), tag, image.RemoveOptions{}) })
	const gone = "registry.invalid/dockback/app@sha256:0000000000000000000000000000000000000000000000000000000000000000"

	ref, pulled, _, err := provideRecordedImage(ctx, c, recorded, gone, tag, false)
	if err != nil || imageIDOf(ctx, c, ref) != recorded || pulled {
		t.Fatalf("the recorded image is on the host, so it must be the one used, not the tag: ref %q (%v)", ref, err)
	}

	ref, _, _, err = provideRecordedImage(ctx, c, newer, gone, tag, false)
	if err != nil || ref != tag {
		t.Fatalf("a tag naming the recorded image keeps its readable name: ref %q (%v)", ref, err)
	}

	missing := "sha256:" + strings.Repeat("f", 64)
	_, _, _, err = provideRecordedImage(ctx, c, missing, gone, tag, false)
	var mismatch *RecordedImageError
	if !errors.Is(err, ErrRecordedImageUnavailable) || !errors.As(err, &mismatch) || mismatch.TagID != newer {
		t.Fatalf("with the recorded image gone, a different image behind the tag must be refused: %v", err)
	}

	ref, _, warning, err := provideRecordedImage(ctx, c, missing, gone, tag, true)
	if err != nil || ref != tag || !strings.Contains(warning, "DIFFERENT image") {
		t.Fatalf("when allowed, the tag is used and the log says it is not the recorded image: ref %q warning %q (%v)", ref, warning, err)
	}
}
