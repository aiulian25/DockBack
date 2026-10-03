package backup

import (
	"context"
	"strings"
	"time"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
)

// What the next `docker pull` would actually do (#29).
//
// R4 §Issue 29: nas01's local `:latest` pointed at Paperless 3.0.0 while the
// registry's `:latest` had moved to 3.1.0. "Any `docker pull …:latest` on nas01,
// any Watchtower run, or any stack recreation from a compose that says `:latest`
// will fetch Paperless 3.1.0 — a minor-version jump that runs database
// migrations on startup."
//
// The pieces to see that already existed and were never put together: the
// registry peek (ImagePullable's manifest lookup) and the image's own recorded
// version. Backup time is the right moment because it is the one moment the tool
// is already looking at this container, and because the answer is only useful
// BEFORE the pull.
//
// R4's core correction is load-bearing here. The first write-up called a missing
// local tag a "dangling reference" and was wrong: upstream `:3.0.0` still
// resolved to exactly the digest that was running. So a missing local tag is
// never reported as broken — it is checked against the registry first, and when
// the registry agrees it is reported as the non-event it is.

// tagDriftKind is what comparing the two states found.
type tagDriftKind int

const (
	// tagDriftNone — nothing worth saying, including every case where the
	// question could not be answered.
	tagDriftNone tagDriftKind = iota
	// tagDrifted — the tag resolves locally to one image and upstream to
	// another. The next pull is an upgrade nobody asked for.
	tagDrifted
	// tagRemovedLocally — the container's reference is not among the local tags.
	// R4's case, and not a defect.
	tagRemovedLocally
)

// tagDrift is the verdict and the facts it rests on.
type tagDrift struct {
	Kind tagDriftKind
	// LocalDigest is what this container is actually running.
	LocalDigest string
	// RegistryDigest is what the same tag resolves to now.
	RegistryDigest string
	// RegistryAgrees reports whether the registry still resolves the container's
	// reference to the digest it is running — the fact that turns "the tag is
	// gone" into "the tag is gone locally, and the restore is unaffected".
	RegistryAgrees bool
}

// tagDriftVerdict compares what the local daemon knows about a container's image
// against what the registry says today.
//
// Pure, and it answers "say nothing" for every case where it cannot be sure:
//
//   - a digest-pinned reference cannot move, so there is nothing to compare;
//   - no registry answer — offline host, private registry, socket proxy with the
//     distribution endpoint disabled — means the question was not asked, and a
//     backup that warned every time it could not reach a registry would be a
//     warning nobody reads;
//   - an image with no RepoDigests was built locally and was never pulled from
//     anywhere, so no registry has an opinion about it.
//
// The missing-tag case is deliberately checked AFTER the registry guard: R4's
// correction is that a missing local tag says nothing on its own, and without a
// registry answer there is nothing to say it with.
func tagDriftVerdict(configImage string, repoTags, repoDigests []string, registryDigest string) tagDrift {
	if !dockercli.MutableRef(configImage) || registryDigest == "" {
		return tagDrift{}
	}
	localDigest := localDigestForRef(configImage, repoDigests)
	if localDigest == "" {
		return tagDrift{}
	}
	agrees := dockercli.DigestOf(localDigest) == dockercli.DigestOf(registryDigest)
	drift := tagDrift{LocalDigest: localDigest, RegistryDigest: registryDigest, RegistryAgrees: agrees}

	if !slicesContainsFold(repoTags, configImage) {
		drift.Kind = tagRemovedLocally
		return drift
	}
	if !agrees {
		drift.Kind = tagDrifted
	}
	return drift
}

// localDigestForRef picks the RepoDigests entry belonging to the same repository
// as the container's reference.
//
// A single image can be tagged from several repositories — a mirror, a rename,
// a registry migration — and each carries its own digest line. Taking the first
// entry would compare this repository's registry answer against another
// repository's digest, which is a mismatch every time.
func localDigestForRef(configImage string, repoDigests []string) string {
	repo := imageRepository(configImage)
	if repo == "" {
		return ""
	}
	for _, entry := range repoDigests {
		if imageRepository(entry) == repo {
			return entry
		}
	}
	return ""
}

// slicesContainsFold reports membership, comparing references case-insensitively
// the way a registry does.
func slicesContainsFold(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.EqualFold(strings.TrimSpace(s), strings.TrimSpace(needle)) {
			return true
		}
	}
	return false
}

// registryDigestTTL is how long a registry's answer for a tag is reused.
//
// Not a performance optimisation — a correctness one about somebody else's
// service. Docker Hub rate-limits anonymous manifest requests, and a host taking
// hourly backups of a twenty-service stack would spend several hundred requests
// a day on a question whose answer changes on the order of days. Exhausting that
// budget would break the operator's real pulls, which is a much worse outcome
// than a finding arriving a few hours late.
const registryDigestTTL = 6 * time.Hour

// registryAnswer is one cached lookup.
type registryAnswer struct {
	digest string
	at     time.Time
}

// registryTagDigestCached asks the registry at most once per reference per TTL.
//
// A miss is cached too, with an empty digest: an unreachable registry is exactly
// the case that must not be retried on every backup.
func (e *Engine) registryTagDigestCached(ctx context.Context, cli *client.Client, ref string) string {
	e.tagDigestMu.Lock()
	cached, ok := e.tagDigestCache[ref]
	e.tagDigestMu.Unlock()
	if ok && time.Since(cached.at) < registryDigestTTL {
		return cached.digest
	}

	digest, _ := dockercli.RegistryTagDigest(ctx, cli, ref)

	e.tagDigestMu.Lock()
	defer e.tagDigestMu.Unlock()
	if e.tagDigestCache == nil {
		e.tagDigestCache = map[string]registryAnswer{}
	}
	// Bounded: a host with a huge number of distinct references must not grow
	// this without limit, and dropping the whole map is a cheaper eviction than
	// tracking ages for something this cheap to rebuild.
	if len(e.tagDigestCache) >= maxTagDigestCache {
		e.tagDigestCache = map[string]registryAnswer{}
	}
	e.tagDigestCache[ref] = registryAnswer{digest: digest, at: time.Now()}
	return digest
}

// maxTagDigestCache bounds the cache. Far above any real host's container count.
const maxTagDigestCache = 512

// reportTagDrift says what a pull of this container's tag would do today.
//
// Best-effort in every direction, and silent whenever it cannot be certain: this
// is information about the future, and a backup is not the place to guess about
// it.
func (e *Engine) reportTagDrift(ctx context.Context, cli *client.Client, cliContainerID string, man *Manifest, logID, name, configImage string) {
	if !dockercli.MutableRef(configImage) {
		return
	}
	repoTags, repoDigests, ok := dockercli.ImageTagState(ctx, cli, cliContainerID)
	if !ok {
		return
	}
	drift := tagDriftVerdict(configImage, repoTags, repoDigests, e.registryTagDigestCached(ctx, cli, configImage))
	switch drift.Kind {
	case tagDrifted:
		e.addFinding(man, logID, findingLocalTagDrifted, FindingWarn, configImage, drift.describeDrift(name, man))
	case tagRemovedLocally:
		e.addFinding(man, logID, findingLocalTagRemoved, FindingInfo, configImage, drift.describeRemoved(name, configImage))
	}
}

// describeDrift is the finding: what a pull would change, and what this backup
// pinned instead.
//
// It names the version it CAN name and not the one it cannot. The registry's
// manifest carries a digest, not the image's labels, so reading the new version
// would mean pulling the config blob — which is the download this whole check
// exists to avoid.
func (d tagDrift) describeDrift(name string, man *Manifest) string {
	running := "the version it runs today"
	if man != nil && man.ImageConfig != nil {
		if v := strings.TrimSpace(man.ImageConfig.Version); v != "" {
			running = v
		}
	}
	return "This tag has moved. " + name + " is running " + running + ", and the same tag in its registry now resolves to a different image (" +
		shortDigest(d.RegistryDigest) + ", where this one is " + shortDigest(d.LocalDigest) + "). " +
		"Nothing is wrong right now, and this backup is unaffected — it pins the digest that is actually running, so restoring it brings back this image and not the new one. " +
		"What it means is that the next `docker pull` of this tag, the next Watchtower run, or the next `docker compose up` that recreates this container is an upgrade nobody scheduled, and an application that migrates its database on start will do so before anyone has decided to let it. " +
		"Take a backup first, or pin the reference to a digest so a pull cannot change what runs."
}

// describeRemoved is R4's companion finding, and its whole job is to NOT alarm.
//
// The report's own correction: "My first write-up called the reference 'dangling'
// and said a restore from .Config.Image would chase a moved or deleted tag.
// Querying GHCR directly shows that is not what happened here… Restoring from
// .Config.Image would have worked perfectly. The tag is missing only locally."
func (d tagDrift) describeRemoved(name, configImage string) string {
	lead := name + " records the image reference " + configImage + ", and that tag no longer exists on this host — the same image now carries a different tag locally, which is why `docker ps` shows a bare image ID for it instead of a name. "
	if d.RegistryAgrees {
		return lead + "This is not a problem, and it is worth saying so plainly: the registry still resolves that exact tag to the exact image running here, and this backup pins the digest in any case. " +
			"Restoring it needs neither the local tag nor the registry to agree about names. Nothing to do."
	}
	return lead + "This backup pins the digest that is running, so restoring it is unaffected either way. Worth knowing: the registry now resolves that tag to a different image (" +
		shortDigest(d.RegistryDigest) + "), so re-pulling by that reference would no longer give you what is running here."
}

// shortDigest renders a digest the way a person reads one — enough to compare
// two of them at a glance, and not a wall of hex.
func shortDigest(digest string) string {
	d := dockercli.DigestOf(digest)
	if hex := strings.TrimPrefix(d, "sha256:"); len(hex) > 12 {
		return "sha256:" + hex[:12] + "…"
	}
	return d
}
