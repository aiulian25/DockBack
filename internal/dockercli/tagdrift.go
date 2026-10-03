package dockercli

import (
	"context"
	"strings"
	"time"

	"github.com/docker/docker/client"
)

// Local tag state and registry tag state diverge independently (#29).
//
// R4 §Issue 29, after the author corrected their own first write-up: "A missing
// local tag says nothing about whether the reference is still valid upstream,
// and a present local tag says nothing about whether it still matches the
// registry. Here both are true at once — `:3.0.0` is absent locally but valid
// upstream, while `:latest` is present locally pointing at 3.0.0 when the
// registry's `:latest` is now 3.1.0."
//
// These read the two states. Neither judges: the verdict is assembled where both
// answers are in hand, so the rule can be tested without a daemon or a network.

// registryPeekTimeout bounds the manifest lookup. This runs inside a backup that
// has real work to do, and a registry that is slow or unreachable must cost a
// finding, never the backup.
const registryPeekTimeout = 10 * time.Second

// MutableRef reports whether a reference can point somewhere else tomorrow.
//
// A digest-pinned reference cannot, by construction — it names the content — so
// there is nothing to compare and nothing that a pull could change.
func MutableRef(ref string) bool {
	r := strings.TrimSpace(ref)
	return r != "" && !strings.Contains(r, "@sha256:")
}

// DigestOf reduces a reference to its digest, so `repo@sha256:abc` and a bare
// `sha256:abc` compare equal.
//
// The two sides genuinely arrive in different shapes: a local RepoDigests entry
// carries its repository, and the registry's descriptor is the digest alone.
func DigestOf(s string) string {
	d := strings.TrimSpace(s)
	if i := strings.Index(d, "@"); i >= 0 {
		d = d[i+1:]
	}
	return d
}

// ImageTagState reads what the LOCAL daemon knows about a container's image: the
// tags that resolve to it, and the digests it was pulled under.
//
// Both slices are returned raw. Which digest belongs to the container's own
// reference is a judgement about repositories, and it is made where the rest of
// the verdict is.
func ImageTagState(ctx context.Context, c *client.Client, containerID string) (repoTags, repoDigests []string, ok bool) {
	insp, err := c.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, nil, false
	}
	img, _, err := c.ImageInspectWithRaw(ctx, insp.Image)
	if err != nil {
		return nil, nil, false
	}
	return img.RepoTags, img.RepoDigests, true
}

// RegistryTagDigest asks a registry what a tag resolves to TODAY, without
// downloading anything.
//
// The daemon performs the lookup, so this reaches the registry only as far as
// the socket proxy's DISTRIBUTION endpoint allows — with that endpoint disabled
// it simply errors, which the caller treats as "no answer" rather than as news.
//
// Returns ok=false for every failure, deliberately and without distinguishing
// them: an offline host, a private registry needing credentials and a disabled
// endpoint all mean the same thing here, which is that nothing can be said.
func RegistryTagDigest(ctx context.Context, c *client.Client, ref string) (string, bool) {
	if !MutableRef(ref) {
		return "", false
	}
	pctx, cancel := context.WithTimeout(ctx, registryPeekTimeout)
	defer cancel()
	dist, err := c.DistributionInspect(pctx, ref, "")
	if err != nil {
		return "", false
	}
	digest := strings.TrimSpace(dist.Descriptor.Digest.String())
	if digest == "" {
		return "", false
	}
	return digest, true
}
