package backup

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
)

// Shared-bind ownership (F83). One host directory is often mounted into
// several containers (a photo app's upload/ dir in both the server and its ML
// sidecar). Capturing it once — by a deterministic OWNER — avoids N× capture
// in stack runs, and lets every other container's skip be recorded as covered
// rather than PARTIAL.

// SharedBindMount is one container's view of a shared host source, as fed to
// the ownership rule.
type SharedBindMount struct {
	Container string // container name
	RW        bool   // mounted read-write
	Selected  bool   // the container's stored mount selection includes it
}

// SharedBindOwner picks which container captures a shared host source, with a
// deterministic precedence: (a) an explicitly Selected mounter wins; (b) among
// Selected (or among all when none are Selected), RW beats RO — the writer is
// the natural owner of the data; (c) remaining ties go to the
// lexicographically-first container name, so the answer never flaps between
// runs. Empty input returns "".
func SharedBindOwner(source string, candidates []SharedBindMount) string {
	if len(candidates) == 0 {
		return ""
	}
	best := -1
	better := func(a, b SharedBindMount) bool {
		if a.Selected != b.Selected {
			return a.Selected
		}
		if a.RW != b.RW {
			return a.RW
		}
		return a.Container < b.Container
	}
	for i := range candidates {
		if best < 0 || better(candidates[i], candidates[best]) {
			best = i
		}
	}
	return candidates[best].Container
}

// bindSharer is one container's mount of a shared host source — the container
// itself, so a caller can ask about its image, and the mount that reached it.
type bindSharer struct {
	Container *dockercli.Container
	Mount     dockercli.Mount
}

// bindSharers maps every backupable host bind source to the containers that
// mount it, one entry per container: a container mounting the same source at
// two destinations is one sharer, keeping its first mount.
//
// The single definition of "these containers share this directory", used both
// to pick a capture owner and to compare what the sharers are running.
func bindSharers(containers []*dockercli.Container) map[string][]bindSharer {
	bySource := map[string][]bindSharer{}
	seen := map[string]map[string]bool{} // source -> container names
	for _, c := range containers {
		if c == nil {
			continue
		}
		for _, m := range c.Mounts {
			if m.Type != "bind" || m.Source == "" || !backupableBind(m.Source) {
				continue
			}
			if seen[m.Source] == nil {
				seen[m.Source] = map[string]bool{}
			}
			if seen[m.Source][c.Name] {
				continue
			}
			seen[m.Source][c.Name] = true
			bySource[m.Source] = append(bySource[m.Source], bindSharer{Container: c, Mount: m})
		}
	}
	return bySource
}

// sortedSharedSources returns the sources mounted by TWO OR MORE containers, in
// a stable order — map iteration is random, and a log line that reorders between
// runs reads as a change that did not happen.
func sortedSharedSources(bySource map[string][]bindSharer) []string {
	out := make([]string, 0, len(bySource))
	for source, sharers := range bySource {
		if len(sharers) >= 2 {
			out = append(out, source)
		}
	}
	sort.Strings(out)
	return out
}

// CoverageMap resolves the owner of every host bind source mounted by TWO OR
// MORE of the given containers: source -> owning container name. A source with
// a single mounter is not shared and is excluded. selectedFor returns a
// container's stored mount-selection destination set (nil when it has none) so
// an explicitly-ticked bind outranks a default one.
func CoverageMap(containers []*dockercli.Container, selectedFor func(name string) map[string]bool) map[string]string {
	bySource := bindSharers(containers)
	out := map[string]string{}
	for _, source := range sortedSharedSources(bySource) {
		candidates := make([]SharedBindMount, 0, len(bySource[source]))
		for _, sharer := range bySource[source] {
			sel := selectedFor(sharer.Container.Name)
			candidates = append(candidates, SharedBindMount{
				Container: sharer.Container.Name,
				RW:        sharer.Mount.RW,
				Selected:  sel != nil && sel[sharer.Mount.Destination],
			})
		}
		out[source] = SharedBindOwner(source, candidates)
	}
	return out
}

// Version skew between containers sharing a mount (#23, PLAYBOOK §4.6).
//
// R3 §Issue 23: Nextcloud ran `nextcloud:34.0.2` and its cron container ran
// `nextcloud:apache`, a ROLLING tag that was already 34.0.3. Both bind-mounted
// the same /var/www/html, so "the cron container ships a newer Nextcloud than
// the code it operates on, and the gap widens every time nextcloud:apache is
// rebuilt."
//
// The report is careful about how bad this was, and so is this finding.
// "Practical impact today: none. The finding stands as a fact — two different
// images share one bind-mounted application — but it is a latent risk, not a
// live defect": the runtimes happened to match (PHP 8.5.9 and 56 extensions on
// both sides) and cron overrode its entrypoint, bypassing the script that would
// have upgraded the shared directory. Neither of those is guaranteed to hold
// after the next pull, and neither is visible from outside the containers — so
// this reports the fact and names §4.6's rule rather than declaring the stack
// broken.
//
// §4.6: "Detect containers sharing a bind mount and flag version disagreement.
// App/cron/worker/websocket is the standard quartet — pin them to ONE digest.
// Per-container pinning is insufficient; they must agree."

// SkewMounter is one container's identity in a version disagreement.
type SkewMounter struct {
	Container string `json:"container"`
	Image     string `json:"image"`
	ImageID   string `json:"image_id"`
}

// SharedMountSkew is one shared host directory whose mounters are running
// different builds of the same image.
type SharedMountSkew struct {
	Source     string        `json:"source"`
	Repository string        `json:"repository"`
	Mounters   []SkewMounter `json:"mounters"`
}

// imageRepository strips the tag and digest from an image reference, leaving
// the repository: `nextcloud:apache` and `nextcloud@sha256:…` both give
// `nextcloud`.
//
// The colon is only a tag separator when nothing after it is a path segment, so
// a registry port (`registry:5000/repo`) survives intact.
func imageRepository(image string) string {
	repo := strings.ToLower(strings.TrimSpace(image))
	if i := strings.IndexByte(repo, '@'); i >= 0 {
		repo = repo[:i]
	}
	if i := strings.LastIndexByte(repo, ':'); i >= 0 && !strings.ContainsRune(repo[i+1:], '/') {
		repo = repo[:i]
	}
	return repo
}

// SharedMountSkews finds shared host directories whose mounters run DIFFERENT
// BUILDS OF THE SAME IMAGE.
//
// Two conditions, and the second is what keeps this finding worth reading:
//
//   - The image IDs must differ. Two containers off one image is the ordinary
//     case (a service and its own worker) and says nothing.
//   - The REPOSITORY must be the same. Different applications sharing a data
//     directory is normal architecture — immich-server and
//     immich-machine-learning share the upload directory by design, paperless
//     shares /tmp with gotenberg — and calling that "version skew" would fire on
//     nearly every real stack and teach the operator to skip the report. What R3
//     found was one application at two versions sharing its own installed code,
//     and the shared repository is what says "same application".
//
// The cost of that narrowing is a repository-renamed fork — `linuxserver/x`
// beside `x` — which is missed. A miss is the better failure here: a finding
// nobody trusts catches nothing at all.
//
// Pure and deterministic: sources sorted, mounters sorted by container name.
func SharedMountSkews(containers []*dockercli.Container) []SharedMountSkew {
	bySource := bindSharers(containers)
	var out []SharedMountSkew
	for _, source := range sortedSharedSources(bySource) {
		byRepo := map[string][]SkewMounter{}
		for _, sharer := range bySource[source] {
			repo := imageRepository(sharer.Container.Image)
			if repo == "" {
				continue
			}
			byRepo[repo] = append(byRepo[repo], SkewMounter{
				Container: sharer.Container.Name,
				Image:     sharer.Container.Image,
				ImageID:   sharer.Container.ImageID,
			})
		}
		repos := make([]string, 0, len(byRepo))
		for repo := range byRepo {
			repos = append(repos, repo)
		}
		sort.Strings(repos)
		for _, repo := range repos {
			mounters := byRepo[repo]
			if !distinctImageIDs(mounters) {
				continue
			}
			sort.Slice(mounters, func(i, j int) bool { return mounters[i].Container < mounters[j].Container })
			out = append(out, SharedMountSkew{Source: source, Repository: repo, Mounters: mounters})
		}
	}
	return out
}

// distinctImageIDs reports whether these mounters are running more than one
// build. An unknown id counts as its own build only when another id is known —
// two containers with no recorded id are not evidence of anything.
func distinctImageIDs(mounters []SkewMounter) bool {
	if len(mounters) < 2 {
		return false
	}
	ids := map[string]bool{}
	for _, m := range mounters {
		ids[m.ImageID] = true
	}
	if len(ids) < 2 {
		return false
	}
	return !(len(ids) == 2 && ids[""])
}

// Describe is the finding's message: what disagrees, and §4.6's fix.
func (s SharedMountSkew) Describe() string {
	parts := make([]string, 0, len(s.Mounters))
	for _, m := range s.Mounters {
		parts = append(parts, m.Container+" ("+m.Image+")")
	}
	return joinAnd(parts) + " all mount " + s.Source + ", and they are running different builds of " + s.Repository + ". " +
		"They share one copy of that application's files, so which version those files are is decided by whichever container's entrypoint ran last — and if any of those tags is a rolling one, the gap widens every time it is rebuilt. " +
		"It may well be harmless today: a container that overrides its entrypoint never upgrades the shared directory, and two builds close together often carry the same runtime. Neither of those is visible from outside the containers, and neither survives the next pull on its own. " +
		"Pin every container that mounts this path to ONE image digest — pinning each of them separately is not enough, they have to agree."
}

// joinAnd renders a list the way a sentence needs it: "a and b", "a, b and c".
func joinAnd(parts []string) string {
	if len(parts) < 3 {
		return strings.Join(parts, " and ")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// Containers lists the names in a skew, for a caller that needs them without
// the prose.
func (s SharedMountSkew) Containers() []string {
	out := make([]string, 0, len(s.Mounters))
	for _, m := range s.Mounters {
		out = append(out, m.Container)
	}
	return out
}

// coveredSourcesWindow bounds how old another container's newest backup may be
// to still count as covering a shared source — stale coverage is no coverage.
const coveredSourcesWindow = 14 * 24 * time.Hour

// MountSelection exposes a container's stored explicit mount selection (F83
// stack-run dedup reads it API-side; nil,false = no stored selection).
func (e *Engine) MountSelection(nodeID, name string) ([]string, bool) {
	sel, _, ok := e.loadMountSelection(nodeID, name)
	return sel, ok
}

// CoveredSources scans the node's catalog for host bind sources that RECENT
// successful backups of OTHER containers actually captured: source -> owning
// container name. One newest-success-per-target pass; only those manifests are
// parsed. excludeName is the container asking (its own backups can't cover it).
func (e *Engine) CoveredSources(nodeID, excludeName string) map[string]string {
	list, err := e.Store.ListBackups(nodeID, 5000)
	if err != nil {
		return nil
	}
	cutoff := time.Now().Add(-coveredSourcesWindow).Unix()
	newest := map[string]int{} // target -> index of newest recent success
	for i, b := range list {
		if b.Status != "success" || b.TargetName == "" || b.TargetName == excludeName || b.CreatedAt < cutoff {
			continue
		}
		if j, ok := newest[b.TargetName]; !ok || b.CreatedAt > list[j].CreatedAt {
			newest[b.TargetName] = i
		}
	}
	out := map[string]string{}
	for target, idx := range newest {
		var man Manifest
		if json.Unmarshal([]byte(list[idx].ManifestJSON), &man) != nil {
			continue
		}
		for _, v := range man.Volumes {
			if v.Type == "bind" && v.Source != "" {
				// When several containers all captured the same source recently,
				// the lexicographically-first name wins — deterministic, so the
				// annotation never flaps between runs.
				if cur, ok := out[v.Source]; !ok || target < cur {
					out[v.Source] = target
				}
			}
		}
	}
	return out
}

// reportSharedMountSkew records, at capture, that this container shares a
// directory with another one running a different build of the same image (#23).
//
// Enumerating the node's containers is the only way to see this — the fact is
// about a PAIR, and nothing in one container's own inspect mentions the other.
// Guarded on this container actually having a backupable bind, so a
// volumes-only or bind-less backup pays nothing for a question that cannot
// apply to it.
//
// Best-effort throughout: a listing that fails costs a finding, never a backup.
func (e *Engine) reportSharedMountSkew(ctx context.Context, cli *client.Client, man *Manifest, logID, name string, insp types.ContainerJSON) {
	if !mountsABackupableBind(insp) {
		return
	}
	lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	containers, err := dockercli.ListContainers(lctx, cli)
	if err != nil {
		return
	}
	for _, skew := range SharedMountSkews(containers) {
		if !slices.Contains(skew.Containers(), name) {
			continue // a disagreement between two OTHER containers is their backup's finding
		}
		e.addFinding(man, logID, findingSharedMountVersionSkew, FindingWarn, skew.Source, skew.Describe())
	}
}

// mountsABackupableBind reports whether this container has a host bind at all.
//
// Read from the live inspect rather than the manifest: this runs before the
// mount walk records anything, and the question is about the container as it is
// now — which is also what the comparison below reads.
func mountsABackupableBind(insp types.ContainerJSON) bool {
	for _, m := range insp.Mounts {
		if string(m.Type) == "bind" && m.Source != "" && backupableBind(m.Source) {
			return true
		}
	}
	return false
}
