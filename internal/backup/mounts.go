package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"golang.org/x/sys/unix"

	"dockback/internal/dockercli"
)

// freeBytesAt reports free space (bytes available to non-root) at a filesystem
// path — used to pre-flight the disk-backed backup work dir.
func freeBytesAt(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// Large-bind threshold (F12): bind mounts larger than the effective threshold are
// skipped by default (they're usually media/data libraries that dwarf the backup
// target and are managed separately). Named volumes are always included by
// default; the user can override either way per container by ticking mounts.
//
// The cutoff is configurable in GiB: a per-container override wins, else the global
// setting, else the shipped default — so the boundary matches the user's storage
// instead of a fixed 5 GiB.
const (
	defaultBindSkipGiB = 5    // shipped default cutoff, in GiB
	minBindSkipGiB     = 1    // smallest sane cutoff (0 would auto-include every bind)
	maxBindSkipGiB     = 1024 // generous ceiling to keep a stored value sane
	bindSkipGiBKey     = "backup.bind_skip_gib"
)

// Regenerable-path exclusion (F132).
//
// Default OFF — include everything. Fidelity is the safer default and the size
// trade should be the operator's explicit choice, not something that happens to
// their backup because DockBack decided a directory looked disposable.
const excludeRegenerableKey = "backup.exclude_regenerable"

func excludeRegenerableKeyFor(nodeID, name string) string {
	return excludeRegenerableKey + "." + nodeID + "." + name
}

// ExcludeRegenerable reports whether this container's app-declared regenerable
// directories are being left out of its backups (F132).
func (e *Engine) ExcludeRegenerable(nodeID, name string) bool {
	v, _ := e.Store.GetSetting(excludeRegenerableKeyFor(nodeID, name), "false")
	return v == "true"
}

// SetExcludeRegenerable stores the choice. Applies to this container's next
// backup, scheduled or manual — the same convention every other per-container
// backup option follows.
func (e *Engine) SetExcludeRegenerable(nodeID, name string, on bool) error {
	if !on {
		return e.Store.SetSetting(excludeRegenerableKeyFor(nodeID, name), "")
	}
	return e.Store.SetSetting(excludeRegenerableKeyFor(nodeID, name), "true")
}

// RegenerablePathsFor returns the regenerable directories this container's image
// declares, for the picker to offer. Empty for an image with none, so the option
// is not shown at all.
func RegenerablePathsFor(image string) []RegenerablePath {
	if p := ProfileFor(image); p != nil {
		return p.Regenerable
	}
	return nil
}

// bindSkipGiBKeyFor is the optional per-container override key.
func bindSkipGiBKeyFor(nodeID, name string) string {
	return bindSkipGiBKey + "." + nodeID + "." + name
}

// settingGiB reads a positive integer GiB setting, returning def when it's unset,
// malformed, or non-positive.
func (e *Engine) settingGiB(key string, def int) int {
	v, _ := e.Store.GetSetting(key, "")
	if strings.TrimSpace(v) == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// BindSkipGiBGlobal is the effective global large-bind cutoff (GiB): the stored
// setting when present and valid, else the shipped default (F12).
func (e *Engine) BindSkipGiBGlobal() int { return e.settingGiB(bindSkipGiBKey, defaultBindSkipGiB) }

// BindSkipGiBOverride returns a container's per-container cutoff override (GiB) and
// whether one is set (F12).
func (e *Engine) BindSkipGiBOverride(nodeID, name string) (int, bool) {
	v, _ := e.Store.GetSetting(bindSkipGiBKeyFor(nodeID, name), "")
	if strings.TrimSpace(v) == "" {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// BindSkipGiBEffective is the cutoff (GiB) that actually applies to a container:
// its per-container override if set, else the global value (F12).
func (e *Engine) BindSkipGiBEffective(nodeID, name string) int {
	if n, ok := e.BindSkipGiBOverride(nodeID, name); ok {
		return n
	}
	return e.BindSkipGiBGlobal()
}

// SetBindSkipGiBOverride stores (or, when gib <= 0, clears) a container's
// per-container large-bind cutoff override, clamping a set value to a sane range
// (F12).
func (e *Engine) SetBindSkipGiBOverride(nodeID, name string, gib int) error {
	key := bindSkipGiBKeyFor(nodeID, name)
	if gib <= 0 {
		return e.Store.SetSetting(key, "")
	}
	if gib < minBindSkipGiB {
		gib = minBindSkipGiB
	}
	if gib > maxBindSkipGiB {
		gib = maxBindSkipGiB
	}
	return e.Store.SetSetting(key, strconv.Itoa(gib))
}

// bindSkipThreshold returns the effective large-bind cutoff for a container in
// BYTES (F12) — the per-container override, else the global setting, else the
// shipped 5 GiB default.
func (e *Engine) bindSkipThreshold(nodeID, name string) int64 {
	return int64(e.BindSkipGiBEffective(nodeID, name)) << 30
}

// MountInfo describes one backup-candidate mount and the default/remembered
// decision about whether to include it.
type MountInfo struct {
	Destination  string `json:"destination"`
	Source       string `json:"source"`
	Type         string `json:"type"` // "volume" | "bind"
	Name         string `json:"name,omitempty"`
	Driver       string `json:"driver,omitempty"`
	RW           bool   `json:"rw"`
	SizeBytes    int64  `json:"size_bytes"`
	SizeKnown    bool   `json:"size_known"`
	Selected     bool   `json:"selected"`
	Reason       string `json:"reason,omitempty"`       // why unselected by default
	Unreadable   bool   `json:"unreadable,omitempty"`   // reader hit permission denied (UID/GID mismatch, PLAN §2.13)
	FSType       string `json:"fs_type,omitempty"`      // backing filesystem (zfs/btrfs/ext4/…) — capability detect
	Snapshotable bool   `json:"snapshotable,omitempty"` // backing FS supports atomic snapshots (zfs/btrfs)
	// What this mount's ROOT is — file or directory, and the ids and mode it
	// carries. Binds only, and empty whenever the probe could not read it, so the
	// picker can say what a mount is without ever implying a default.
	Kind  string `json:"kind,omitempty"`  // dockercli.MountKindDir | MountKindFile
	Owner string `json:"owner,omitempty"` // "uid:gid" of the mount root
	Mode  string `json:"mode,omitempty"`  // octal permission bits, e.g. "700"
	// Shared-bind context (F83), filled API-side from the inventory cache:
	// other containers on the node mounting the same host Source, and — for an
	// unselected bind — the container whose recent backups already capture it.
	SharedWith []string `json:"shared_with,omitempty"`
	CoveredBy  string   `json:"covered_by,omitempty"`
}

// snapshotCapableFS reports whether a backing filesystem supports atomic
// point-in-time snapshots. Detection only — taking the snapshot
// needs host-level privilege the hardened, socket-proxy-only app doesn't have.
func snapshotCapableFS(fs string) bool {
	switch strings.ToLower(fs) {
	case "zfs", "btrfs":
		return true
	}
	return false
}

// candidateMounts returns the backup-eligible mounts of a container: every named
// volume, plus writable bind mounts that aren't system paths (PLAN §4/§9).
func candidateMounts(mounts []types.MountPoint) []MountInfo {
	var out []MountInfo
	for _, m := range mounts {
		if m.Destination == "" {
			continue
		}
		switch string(m.Type) {
		case "volume":
			out = append(out, MountInfo{Destination: m.Destination, Source: m.Source, Type: "volume", Name: m.Name, Driver: m.Driver, RW: m.RW})
		case "bind":
			// Read-only binds are candidates too. The reader has always been able
			// to see them — the capture sidecar attaches --volumes-from :ro, so a
			// mount's own read-only flag was never what stopped it being read — and
			// excluding them meant an application's data could be absent from a
			// backup that reported success. One application's web-push private key is
			// mounted read-only, which is exactly right for the application and no
			// reason at all not to keep a copy of it.
			//
			// Being a candidate is not being captured: the size default and the
			// operator's selection still decide, and anything left out is recorded
			// as a skipped mount, which is the honest half that was missing.
			if backupableBind(m.Source) {
				out = append(out, MountInfo{Destination: m.Destination, Source: m.Source, Type: "bind", RW: m.RW})
			}
		}
	}
	return out
}

func mountSelKey(nodeID, name string) string { return "mounts." + nodeID + "." + name }

// MountSelectionKey exposes that settings key, so callers outside this package
// address the remembered mount selection through the engine's own definition
// rather than rebuilding the string — the same reason BackupOptionsKey and
// PauseModeKey are exported.
func MountSelectionKey(nodeID, name string) string { return mountSelKey(nodeID, name) }

// storedMountSelection is what a remembered selection is written as: the mounts
// the operator kept, AND the mounts they were choosing from at the time.
//
// The second half is the load-bearing one. Without it, "not in the selection"
// has two very different meanings — "I unticked this" and "this was not on the
// list when I chose" — and the code cannot tell them apart. That is not
// hypothetical: making read-only binds selectable turned an application's own
// secret into a mount that read as deliberately excluded, in a saved selection
// made before it could ever have been ticked.
type storedMountSelection struct {
	Selected []string `json:"selected"`
	Offered  []string `json:"offered"`
}

// loadMountSelection returns the remembered selection and the candidate set it
// was chosen from. offered is nil for a selection saved before the set was
// recorded, which callers must treat as "unknown", never as "empty".
func (e *Engine) loadMountSelection(nodeID, name string) (sel []string, offered map[string]bool, ok bool) {
	v, _ := e.Store.GetSetting(mountSelKey(nodeID, name), "")
	if v == "" {
		return nil, nil, false
	}
	// The original shape was a bare array. It is still written by nothing, but it
	// is still READ from every installation that predates this.
	if json.Unmarshal([]byte(v), &sel) == nil {
		return sel, nil, true
	}
	var stored storedMountSelection
	if json.Unmarshal([]byte(v), &stored) != nil {
		return nil, nil, false
	}
	return stored.Selected, setOf(stored.Offered), true
}

// saveMountSelection records the choice together with what was on offer when it
// was made. offered may be nil only where the caller genuinely cannot know it.
func (e *Engine) saveMountSelection(nodeID, name string, sel, offered []string) {
	b, _ := json.Marshal(storedMountSelection{Selected: sel, Offered: offered})
	_ = e.Store.SetSetting(mountSelKey(nodeID, name), string(b))
}

// candidateMountDests returns the backup-eligible destinations from a container's
// INVENTORY mounts (dockercli.Mount) — every named volume plus writable non-system
// binds, mirroring candidateMounts. Used by label-driven mount exclusion (F19).
func candidateMountDests(mounts []dockercli.Mount) []string {
	var out []string
	for _, m := range mounts {
		if m.Destination == "" {
			continue
		}
		switch m.Type {
		case "volume":
			out = append(out, m.Destination)
		case "bind":
			// Read-only included, for the reasons candidateMounts gives. The two
			// must agree exactly: this one feeds the label-driven exclusion (F19)
			// and the stack panel's "n of m" count, and a count that disagrees with
			// what a run captures is worse than no count.
			if backupableBind(m.Source) {
				out = append(out, m.Destination)
			}
		}
	}
	return out
}

// CandidateMountDests exposes the backup-eligible mount destinations of a
// container's INVENTORY mounts (F80 stack panel: "n of m" mount counts) —
// every named volume plus writable non-system binds.
func CandidateMountDests(mounts []dockercli.Mount) []string { return candidateMountDests(mounts) }

// SetLabelMountExclusion records an explicit mount selection = all backup-candidate
// mounts MINUS the excluded destinations (F19 dockback.mounts.exclude), so the
// listed paths are left out of the capture. mounts is the container's inventory
// mounts. A selection of exactly the candidates that aren't excluded is stored, so
// the excluded ones are omitted regardless of the size-based default.
func (e *Engine) SetLabelMountExclusion(nodeID, name string, mounts []dockercli.Mount, exclude []string) {
	ex := setOf(exclude)
	offered := candidateMountDests(mounts)
	sel := []string{}
	for _, d := range offered {
		if !ex[d] {
			sel = append(sel, d)
		}
	}
	e.saveMountSelection(nodeID, name, sel, offered)
}

// ClearMountSelection removes a container's remembered mount selection, reverting to
// the size-based default — used when dockback.* labels are removed (F19).
func (e *Engine) ClearMountSelection(nodeID, name string) {
	_ = e.Store.DeleteSetting(mountSelKey(nodeID, name))
}

// SetMountSelection stores a container's mount selection WITHOUT running a
// backup (F115), the counterpart of the backup-options setter the stack panel
// already uses.
//
// It writes the SAME setting a backup run writes, so a selection made in the
// stack panel and one made on the container page are indistinguishable — there
// is one source of truth, and the container page reflects a change immediately.
//
// An EMPTY (but non-nil) selection is meaningful and is stored as such: "capture
// none of this container's mounts" is a legitimate choice for a service whose
// data is entirely covered by a sibling. Passing nil clears the selection
// instead, reverting to the size-based default.
func (e *Engine) SetMountSelection(nodeID, name string, sel, offered []string) error {
	if sel == nil {
		e.ClearMountSelection(nodeID, name)
		return nil
	}
	e.saveMountSelection(nodeID, name, sel, offered)
	return nil
}

// CandidateMountDestsOf inspects a container and returns the destinations that
// are eligible for backup at all — every named volume plus writable non-system
// binds (F115).
//
// Used to VALIDATE an incoming selection. Without it, an arbitrary string could
// be written into the stored selection, and a later run would quietly match
// nothing and capture less than the operator believes — a silent gap in a backup
// is exactly the failure this codebase spends the most effort avoiding.
//
// Deliberately does NOT measure sizes: validation must be cheap, and the `du`
// scan ListMounts performs can take minutes on a large bind.
func CandidateMountDestsOf(insp types.ContainerJSON) []string {
	cands := candidateMounts(insp.Mounts)
	out := make([]string, 0, len(cands))
	for _, m := range cands {
		out = append(out, m.Destination)
	}
	return out
}

// ListMounts inspects a container and returns its backup-candidate mounts with
// sizes and the current selection (remembered, or the size-based default). Used
// by the backup UI so the user sees exactly what will be captured.
func (e *Engine) ListMounts(ctx context.Context, cli *client.Client, nodeID, containerID string) ([]MountInfo, error) {
	insp, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, err
	}
	name := strings.TrimPrefix(insp.Name, "/")
	cands := candidateMounts(insp.Mounts)
	if len(cands) == 0 {
		return cands, nil
	}

	paths := make([]string, 0, len(cands))
	for _, m := range cands {
		paths = append(paths, m.Destination)
	}
	sctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	sizes, unreadable, stats, _ := dockercli.MountSizesFrom(sctx, cli, containerID, paths) // best-effort
	cancel()
	unreadableSet := setOf(unreadable)

	sel, offered, hasSel := e.loadMountSelection(nodeID, name)
	selSet := setOf(sel)
	thr := e.bindSkipThreshold(nodeID, name)
	for i := range cands {
		if sz, ok := sizes[cands[i].Destination]; ok {
			cands[i].SizeBytes, cands[i].SizeKnown = sz, true
		}
		// Backing-FS capability detection: inform the operator when a
		// volume sits on a snapshot-capable filesystem. The same probe reports
		// what the mount root IS, which the picker shows for a bind so a
		// file-backed mount is not mistaken for a folder.
		if st, ok := stats[cands[i].Destination]; ok {
			cands[i].FSType = st.FSType
			cands[i].Snapshotable = snapshotCapableFS(st.FSType)
			cands[i].Kind, cands[i].Owner, cands[i].Mode = st.Kind, st.Owner, st.Mode
		}
		// Flag bind mounts the reader can't fully access so the
		// picker can warn the user before they rely on the backup.
		if cands[i].Type == "bind" && unreadableSet[cands[i].Destination] {
			cands[i].Unreadable = true
		}
		cands[i].Selected, cands[i].Reason = defaultSelected(cands[i], hasSel, selSet, offered, thr)
	}
	// PLAN §4.1: a DB engine's data dir is captured via a consistent dump, not a
	// raw copy — so show it unselected with that reason (unless the user has
	// explicitly kept it in a remembered selection).
	eng := detectDBEngine(insp.Config.Image, insp.Config.Env)
	dir := dockercli.DBDataDir(eng)
	// F126: an all-in-one image keeps its bundled server's data wherever the APP
	// puts it, not at the engine image's default — so the picker has to name the
	// declared path, or the operator sees a large "postgres" folder with no
	// explanation of why it isn't ticked.
	if eng == "" {
		if p := ProfileFor(insp.Config.Image); p != nil && p.EmbeddedDump != nil {
			eng, dir = p.EmbeddedDump.Engine, p.EmbeddedDump.DataDir
		}
	}
	if eng != "" && dir != "" {
		for i := range cands {
			if cands[i].Destination == dir && !selSet[dir] {
				cands[i].Selected = false
				cands[i].Reason = "captured via consistent " + eng + " dump — raw copy skipped"
			}
		}
	}
	return cands, nil
}

// defaultSelected decides whether a mount is checked: a remembered selection
// wins; otherwise named volumes are on, and binds are on only if measured and at
// or under the effective threshold (thr, in bytes — F12).
func defaultSelected(m MountInfo, hasSel bool, sel, offered map[string]bool, thr int64) (bool, string) {
	if !hasSel {
		return sizeDefaultSelected(m, thr)
	}
	if sel[m.Destination] {
		return true, ""
	}
	if !newlyOffered(m, offered) {
		return false, "" // absent because the operator took it out — leave it out
	}
	// It could not have been ticked when that selection was saved, so its absence
	// says nothing. Decide it as if it were new, and SAY so: a mount that turns
	// itself back on without explanation is its own kind of surprise.
	on, reason := sizeDefaultSelected(m, thr)
	if on {
		reason = newlyOfferedReason
	}
	return on, reason
}

// newlyOfferedReason explains a mount that is ticked despite not being in the
// remembered selection.
const newlyOfferedReason = "newly offered — this mount was not on the list when your selection was saved, so it is included rather than silently left out; untick it to exclude it"

// newlyOffered reports whether this mount could not have been part of the saved
// selection, which is the difference between "excluded" and "never asked about".
//
// A selection saved before the offered set was recorded answers by rule instead:
// read-only binds were not candidates then, so a read-only bind missing from an
// old selection was never a choice — it is the previous behaviour showing
// through. Everything else in an old selection IS a choice and is respected.
func newlyOffered(m MountInfo, offered map[string]bool) bool {
	if offered != nil {
		return !offered[m.Destination]
	}
	return m.Type == "bind" && !m.RW
}

// sizeDefaultSelected is the size-based default: named volumes on, binds on only
// if measured and at or under the effective threshold (thr, in bytes — F12).
func sizeDefaultSelected(m MountInfo, thr int64) (bool, string) {
	if m.Type == "volume" {
		return true, ""
	}
	if m.SizeKnown && m.SizeBytes <= thr {
		return true, ""
	}
	if m.SizeKnown {
		return false, fmt.Sprintf("large bind (%s) — skipped by default; tick to include", humanBytes(m.SizeBytes))
	}
	return false, "size unknown — skipped by default; tick to include"
}

func setOf(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

// includeNewlyOffered adds the candidates a saved selection could not have
// covered, deciding each by the ordinary size rule.
//
// It measures them, and that is the whole point of it existing as a step of its
// own. A remembered selection deliberately spares a run the `du` sweep, so the
// candidates here carry no size at all — and the size rule reads "no size" as
// "skip by default". The result was a picker that showed a mount ticked (it
// measures) beside a run that dropped it as unknown-size (it does not), for the
// same container, at the same moment. Measuring just this handful restores the
// one property that matters: a run captures what the picker said it would.
func (e *Engine) includeNewlyOffered(ctx context.Context, cli *client.Client, containerID string, cands []MountInfo, offered, chosen map[string]bool, thr int64, id string) {
	var fresh []MountInfo
	for _, m := range cands {
		if !chosen[m.Destination] && newlyOffered(m, offered) {
			fresh = append(fresh, m)
		}
	}
	if len(fresh) == 0 {
		return
	}
	sizes := map[string]int64{}
	if cli != nil {
		paths := make([]string, 0, len(fresh))
		for _, m := range fresh {
			paths = append(paths, m.Destination)
		}
		sctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
		sizes, _, _, _ = dockercli.MountSizesFrom(sctx, cli, containerID, paths)
		cancel()
	}
	for _, m := range fresh {
		if sz, known := sizes[m.Destination]; known {
			m.SizeBytes, m.SizeKnown = sz, true
		}
		on, why := sizeDefaultSelected(m, thr)
		if !on {
			e.logf(id, "INFO", "Leaving out %s — it was not on the list when this container's mount selection was saved, and %s", m.Destination, why)
			continue
		}
		chosen[m.Destination] = true
		e.logf(id, "INFO", "Including %s — it was not on the list when this container's mount selection was saved, so it is captured rather than silently left out", m.Destination)
	}
}

// captureBindRoots records what every bind of this container IS, holds the
// file-rooted ones out of the volume archive into their own members, and stamps
// the captured refs from that same single probe (F81).
//
// Both capture paths call this. They are two implementations of the same
// capture, and this work lived in only one of them for exactly as long as it
// took to take an app-consistent backup and find the application's key missing
// from it — with the picker showing it ticked. One function, both callers, so
// they cannot drift apart again.
func (e *Engine) captureBindRoots(ctx context.Context, cli *client.Client, insp types.ContainerJSON, containerID, work, id string, man *Manifest, volDests []string, refs []VolumeRef) ([]string, []VolumeRef, []SkippedMount) {
	man.MountedBinds = e.recordMountedBinds(ctx, cli, insp, id)
	var fileRefs []VolumeRef
	volDests, refs, fileRefs = splitFileBinds(volDests, refs, man.MountedBinds)
	captured, fileSkips := e.captureFileBinds(ctx, cli, containerID, fileRefs, work, id)
	refs = append(refs, captured...)
	volDests, refs = e.foldNestedMounts(volDests, refs, id)
	e.reportDuplicateMountTargets(man, id)
	man.Volumes = append(man.Volumes, applyBindStats(refs, man.MountedBinds)...)
	if len(captured) > 0 {
		man.Format.Layout += ", " + bindFileArchivePrefix + "*"
	}
	return volDests, refs, fileSkips
}

// selectMounts decides which mount destinations a backup run captures and the
// VolumeRefs to record, honoring an explicit selection, then a remembered one,
// then the size-based default. The chosen set is persisted so future (including
// scheduled) runs don't re-measure.
// dumpedDataDir, when non-empty, is the DB engine's data directory that was just
// captured via a CONSISTENT dump — so its raw files are excluded from the volume
// capture unless the user explicitly asked to include them.
// recordMountedVolumes records the IDENTITY of every named volume this container
// mounts — name, driver, driver options and labels — whether or not its contents
// are captured (F226).
//
// The distinction matters because "not captured" is common and has nothing to do
// with the volume needing to exist: a database's data directory is captured by
// its dump rather than copied, a shared folder is captured once via another
// container, an app's regenerable cache is deliberately left out, the operator
// unticked something. In every one of those cases the container still mounts the
// volume, so a restore still has to CREATE it.
//
// Before this, only captured volumes were recorded, so on restore Docker
// auto-created the rest the moment the container referenced them: plain local
// driver, and — the part that bites — no labels. A volume Docker made carries
// none of the com.docker.compose.* labels the project expects, so a later
// `docker compose up` refuses to adopt it:
//
//	volume "<project>_<vol>" already exists but was not created by Docker Compose
//
// and both remedies that error suggests are wrong after a restore: `external:
// true` makes the compose file permanently describe a volume it no longer
// manages, and removing the volume deletes the data that was just recovered.
//
// Identity only. This list is never a claim that anything was backed up — the
// captured set stays `Volumes`, and everything that reads "what is in this
// archive" keeps reading that.
// bindMountRoots returns every host bind a restore could be expected to
// MATERIALISE on the target — read-only ones included, whether or not their
// contents are captured.
//
// Deliberately not candidateMounts: that answers "what is worth backing up",
// which is a narrower and different question. A read-only reference set is not
// worth copying and still has to exist for the container to start; a database's
// data directory is superseded by its dump and still has to exist. The one
// exclusion shared with capture is backupableBind, because /etc/localtime, the
// Docker socket and /proc are on every host already and are never ours to create.
func bindMountRoots(mounts []types.MountPoint) []MountInfo {
	var out []MountInfo
	seen := map[string]bool{}
	for _, m := range mounts {
		if string(m.Type) != "bind" || m.Destination == "" || seen[m.Destination] {
			continue
		}
		if !backupableBind(m.Source) {
			continue
		}
		seen[m.Destination] = true
		out = append(out, MountInfo{Destination: m.Destination, Source: m.Source, Type: "bind", RW: m.RW})
	}
	return out
}

// recordMountedBinds records what every host bind of this container IS, for
// Manifest.MountedBinds — see that field for why the set is wider than the
// captured one.
//
// Best-effort, exactly as recordMountedVolumes is: a probe that cannot run
// leaves the kind/owner/mode blank and the sources still recorded, which is
// strictly more than a backup carried before. It never fails a backup — a
// recovery must not be refused over metadata about a directory.
func (e *Engine) recordMountedBinds(ctx context.Context, cli *client.Client, insp types.ContainerJSON, logID string) []VolumeRef {
	roots := bindMountRoots(insp.Mounts)
	if len(roots) == 0 {
		return nil
	}
	stats := map[string]dockercli.MountStat{}
	if cli != nil {
		paths := make([]string, 0, len(roots))
		for _, m := range roots {
			paths = append(paths, m.Destination)
		}
		st, err := dockercli.MountStatsFrom(ctx, cli, containerRef(insp), paths)
		if err != nil {
			e.logf(logID, "INFO", "Could not read what this container's bind mounts are (%v) — their kind and ownership are not recorded in this backup, so a restore onto a machine without those paths will ask you to create them", err)
		} else {
			stats = st
		}
	}
	out := make([]VolumeRef, 0, len(roots))
	for _, m := range roots {
		out = append(out, bindVolumeRef(m, stats[m.Destination]))
	}
	return out
}

// containerRef is the id a sidecar attaches --volumes-from, preferring the id
// over the name so a rename between inspect and probe cannot misdirect it.
func containerRef(insp types.ContainerJSON) string {
	if insp.ContainerJSONBase != nil && insp.ID != "" {
		return insp.ID
	}
	return strings.TrimPrefix(insp.Name, "/")
}

// splitFileBinds separates the binds whose root is a FILE from everything else,
// because the two are captured and restored by different mechanisms.
//
// The volume archive is extracted in a sidecar attached --volumes-from the
// target, so every bind of the container is a live mount inside it. A directory
// member is written THROUGH its mount point and lands on the host, which is the
// whole mechanism. A regular-file member cannot: tar has to unlink the
// destination first, the destination is a bind mount, and the kernel answers
// EBUSY. Busybox tar then exits non-zero and the entire volume restore fails
// over it — one small file costing the operator every volume in the archive.
//
// So a file bind's contents travel as their own archive member and are written
// to the host before the container exists (dockercli.WriteHostFile). Splitting
// them out here is what keeps them out of the tar member list.
//
// Pure, so the partition is tested without a daemon.
func splitFileBinds(dests []string, refs []VolumeRef, roots []VolumeRef) (dirDests []string, dirRefs, fileRefs []VolumeRef) {
	isFile := map[string]bool{}
	for _, r := range roots {
		if r.Kind == dockercli.MountKindFile && r.Destination != "" {
			isFile[r.Destination] = true
		}
	}
	if len(isFile) == 0 {
		return dests, refs, nil
	}

	dirDests = make([]string, 0, len(dests))
	for _, d := range dests {
		if !isFile[d] {
			dirDests = append(dirDests, d)
		}
	}
	dirRefs = make([]VolumeRef, 0, len(refs))
	for _, r := range refs {
		// Only a BIND can have a file root; a named volume is always a directory.
		if r.Type != "bind" || !isFile[r.Destination] {
			dirRefs = append(dirRefs, r)
			continue
		}
		r.Kind = dockercli.MountKindFile
		fileRefs = append(fileRefs, r)
	}
	return dirDests, dirRefs, fileRefs
}

// bindVolumeRef builds the manifest record for one bind, folding in what the
// probe learned about its root. Pure, so the mapping is tested without a daemon.
func bindVolumeRef(m MountInfo, st dockercli.MountStat) VolumeRef {
	return VolumeRef{
		Destination: m.Destination,
		Type:        "bind",
		Source:      m.Source,
		Kind:        st.Kind,
		Owner:       st.Owner,
		Mode:        st.Mode,
		ReadOnly:    !m.RW,
	}
}

// applyBindStats stamps each CAPTURED bind ref with what recordMountedBinds
// learned about the same mount, so the two records agree by construction rather
// than by running the probe twice.
//
// Matched on the container path: it is the same string in both records and on
// both machines, whereas a host source is rewritten by the path remap. Refs that
// have no matching root — every named volume — are returned untouched. Pure.
func applyBindStats(refs []VolumeRef, roots []VolumeRef) []VolumeRef {
	if len(refs) == 0 || len(roots) == 0 {
		return refs
	}
	byDest := make(map[string]VolumeRef, len(roots))
	for _, r := range roots {
		byDest[r.Destination] = r
	}
	for i := range refs {
		if refs[i].Type != "bind" {
			continue
		}
		root, ok := byDest[refs[i].Destination]
		if !ok {
			continue
		}
		refs[i].Kind, refs[i].Owner, refs[i].Mode = root.Kind, root.Owner, root.Mode
	}
	return refs
}

func (e *Engine) recordMountedVolumes(ctx context.Context, cli *client.Client, insp types.ContainerJSON) []VolumeRef {
	out := []VolumeRef{}
	for _, m := range insp.Mounts {
		if string(m.Type) != "volume" || m.Name == "" {
			continue
		}
		ref := VolumeRef{Name: m.Name, Destination: m.Destination, Driver: m.Driver, Type: "volume", Source: m.Source}
		// Best-effort, exactly as the captured path is: a volume that cannot be
		// inspected still contributes its name, which is all a bare create needs.
		if cli != nil {
			if drv, opts, labels, ierr := dockercli.InspectVolume(ctx, cli, m.Name); ierr == nil {
				if drv != "" {
					ref.Driver = drv
				}
				ref.Options, ref.OptionsRedacted = dockercli.RedactVolumeOptions(opts)
				ref.Labels = labels
			}
		}
		out = append(out, ref)
	}
	return out
}

func (e *Engine) selectMounts(ctx context.Context, cli *client.Client, insp types.ContainerJSON, opts Options, id, dumpedDataDir string) ([]string, []VolumeRef, []SkippedMount) {
	name := strings.TrimPrefix(insp.Name, "/")
	cands := candidateMounts(insp.Mounts)
	if len(cands) == 0 {
		return nil, nil, nil
	}
	thr := e.bindSkipThreshold(opts.NodeID, name)

	offeredNow := make([]string, 0, len(cands))
	for _, m := range cands {
		offeredNow = append(offeredNow, m.Destination)
	}

	var chosen map[string]bool
	switch {
	case opts.IncludeMounts != nil:
		chosen = setOf(opts.IncludeMounts)
		// A stack run's shared-bind dedup (F83) computes a one-run selection —
		// never let it overwrite what the user actually chose.
		if !opts.SelectionEphemeral {
			e.saveMountSelection(opts.NodeID, name, opts.IncludeMounts, offeredNow)
		}
	default:
		if sel, offered, ok := e.loadMountSelection(opts.NodeID, name); ok {
			chosen = setOf(sel)
			// A run must capture what the picker would show: a mount that was not
			// on offer when this selection was saved is not an exclusion, and the
			// size default decides it instead.
			e.includeNewlyOffered(ctx, cli, opts.ContainerID, cands, offered, chosen, thr, id)
		} else {
			chosen = e.defaultSelection(ctx, cli, opts.ContainerID, cands, id, thr)
			// Remember it so we don't re-measure large binds every run.
			var keep []string
			for d := range chosen {
				keep = append(keep, d)
			}
			e.saveMountSelection(opts.NodeID, name, keep, offeredNow)
		}
	}

	// PLAN §4.1: a consistent DB dump is authoritative, so don't ALSO ship the
	// engine's raw data directory (a live-file copy is frequently corrupt). Honor
	// an explicit opt-in if the user really wants the raw files too.
	if dumpedDataDir != "" && chosen[dumpedDataDir] {
		explicit := opts.IncludeMounts != nil && setOf(opts.IncludeMounts)[dumpedDataDir]
		if !explicit {
			delete(chosen, dumpedDataDir)
			e.logf(id, "INFO", "Skipping raw DB data dir %s — captured via the consistent dump, not a live-file copy", dumpedDataDir)
		}
	}

	// F141: some paths are only meaningful together. Put back any member of an
	// atomic set the selection left out — whether it was deselected on purpose or
	// simply fell out of the size-based default because its size could not be
	// measured — because half of such a set is not a smaller backup, it is an
	// archive whose restore is refused.
	if insp.Config != nil {
		if set := AtomicVolumesFor(insp.Config.Image); set != nil {
			var mounted []string
			for _, m := range cands {
				mounted = append(mounted, m.Destination)
			}
			if added := enforceAtomicSelection(set, mounted, chosen); len(added) > 0 {
				e.logf(id, "WARN", "Including %s as well: %s. %s.",
					strings.Join(added, " and "), set.Why, set.Symptom)
			}
		}
	}

	var dests []string
	var refs []VolumeRef
	var skipped []SkippedMount
	for _, m := range cands {
		if !chosen[m.Destination] {
			// The DB engine's data dir is captured via the consistent dump instead
			// of a raw file copy — that's a better capture, not a gap, so it isn't
			// recorded as a skipped/uncaptured mount.
			if m.Destination == dumpedDataDir {
				continue
			}
			reason := "excluded from the mount selection"
			switch {
			case m.Type == "bind" && m.SizeKnown && m.SizeBytes > thr:
				reason = fmt.Sprintf("large bind (%s) excluded by default — select it to include", humanBytes(m.SizeBytes))
			case m.Type == "bind" && !m.SizeKnown:
				reason = "bind of unknown size excluded by default — select it to include"
			}
			e.logf(id, "INFO", "Skipping %s %s (%s)", m.Type, m.Destination, reason)
			sm := SkippedMount{Destination: m.Destination, Source: m.Source, Type: m.Type, Reason: reason}
			if m.SizeKnown {
				sm.Bytes = m.SizeBytes
			}
			skipped = append(skipped, sm)
			continue
		}
		dests = append(dests, m.Destination)
		if m.Type == "volume" {
			ref := VolumeRef{Name: m.Name, Destination: m.Destination, Driver: m.Driver, Type: "volume", Source: m.Source}
			// F90: record what makes the driver mean something. Best-effort — a
			// volume that can't be inspected still gets its name and driver, which
			// is what was recorded before this existed.
			if cli != nil && m.Name != "" {
				if drv, opts, labels, ierr := dockercli.InspectVolume(ctx, cli, m.Name); ierr == nil {
					if drv != "" {
						ref.Driver = drv
					}
					ref.Options, ref.OptionsRedacted = dockercli.RedactVolumeOptions(opts)
					ref.Labels = labels
				}
			}
			refs = append(refs, ref)
		} else {
			// Kind/Owner/Mode are stamped on afterwards from the single bind probe
			// (applyBindStats), so the captured record and MountedBinds cannot
			// disagree and the probe runs once per backup rather than twice.
			refs = append(refs, bindVolumeRef(m, dockercli.MountStat{}))
		}
	}
	// F83: a skipped bind whose host Source is captured by ANOTHER container's
	// recent backups is an intentional single-capture of a shared directory —
	// mark it covered so grade/runbook/digest don't count it as PARTIAL. The
	// catalog lookup runs only when there's a bind skip to annotate.
	needCover := false
	for _, sk := range skipped {
		if sk.Type == "bind" && sk.Source != "" {
			needCover = true
			break
		}
	}
	if needCover {
		covered := e.CoveredSources(opts.NodeID, name)
		for i := range skipped {
			if skipped[i].Type != "bind" || skipped[i].Source == "" {
				continue
			}
			if owner := covered[skipped[i].Source]; owner != "" {
				skipped[i].CoveredBy = owner
				e.logf(id, "INFO", "Bind %s is captured by %s's backups — not counted as partial here", skipped[i].Destination, owner)
			}
		}
	}
	return dests, refs, skipped
}

// defaultSelection applies the size-based default: all named volumes + binds at
// or under the effective threshold (thr, in bytes — F12); large/unmeasured binds
// are skipped (logged).
func (e *Engine) defaultSelection(ctx context.Context, cli *client.Client, containerID string, cands []MountInfo, id string, thr int64) map[string]bool {
	var bindPaths []string
	for _, m := range cands {
		if m.Type == "bind" {
			bindPaths = append(bindPaths, m.Destination)
		}
	}
	sizes := map[string]int64{}
	if len(bindPaths) > 0 {
		sctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
		sizes, _, _, _ = dockercli.MountSizesFrom(sctx, cli, containerID, bindPaths)
		cancel()
	}
	chosen := map[string]bool{}
	for _, m := range cands {
		if m.Type == "volume" {
			chosen[m.Destination] = true
			continue
		}
		if sz, ok := sizes[m.Destination]; ok && sz <= thr {
			chosen[m.Destination] = true
		} else if ok {
			e.logf(id, "INFO", "Skipping large bind %s (%s) by default — select it explicitly to include it", m.Destination, humanBytes(sz))
		} else {
			e.logf(id, "INFO", "Skipping bind %s (size unmeasured) by default — select it explicitly to include it", m.Destination)
		}
	}
	return chosen
}

// estimatedCompressionRatio is a conservative fraction of the uncompressed size
// we expect the encrypted+zstd archive to occupy, per compression preset.
// Conservative on purpose: better to slightly over-estimate the destination need
// than to start a backup that overflows mid-write.
func estimatedCompressionRatio(preset string) float64 {
	switch preset {
	case "max":
		return 0.60
	case "fast":
		return 0.85
	default: // balanced (and unset)
		return 0.70
	}
}

// Learned-ratio bounds (F17). When a container has prior successful backups, the
// free-space guard uses that container's OBSERVED stored-vs-selected ratio instead
// of the fixed preset constant — so highly-compressible data isn't over-refused
// and incompressible data (already-zipped media) isn't under-budgeted into a
// mid-write overflow. The used value is clamped to a sane band so one odd run
// can't wildly mis-estimate; the learned value is smoothed across runs.
const (
	minLearnedRatio  = 0.15 // never budget below this (a fluke tiny archive)
	maxLearnedRatio  = 1.0  // incompressible data ~1x selection (+overhead, caught by the 95% headroom)
	learnedRatioSeed = 2.0  // upper cap when RECORDING, so overhead can push a touch over 1x
	ratioSmoothing   = 0.5  // EWMA weight on the newest observation
)

// ratioSettingKey is where a container's learned compression ratio is stored.
func ratioSettingKey(nodeID, name string) string { return "backup.ratio." + nodeID + "." + name }

// learnedRatio returns a container's smoothed observed compression ratio, or 0
// when there's no usable history (so the caller falls back to the preset).
func (e *Engine) learnedRatio(nodeID, name string) float64 {
	v, _ := e.Store.GetSetting(ratioSettingKey(nodeID, name), "")
	if strings.TrimSpace(v) == "" {
		return 0
	}
	r, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || r <= 0 {
		return 0
	}
	return r
}

// recordRatio updates a container's learned compression ratio after a successful
// backup (F17), exponentially smoothing the newest observation (stored /
// uncompressed selection) into the stored value. Called only when the uncompressed
// selection was actually measured (> 0), so it never divides by zero.
func (e *Engine) recordRatio(nodeID, name string, uncompressed, stored int64) {
	if uncompressed <= 0 || stored <= 0 {
		return
	}
	observed := float64(stored) / float64(uncompressed)
	// Clamp the raw observation before smoothing so a pathological run (e.g. a tiny
	// selection dominated by fixed archive overhead) can't poison the average.
	if observed < 0.01 {
		observed = 0.01
	}
	if observed > learnedRatioSeed {
		observed = learnedRatioSeed
	}
	smoothed := observed
	if prev := e.learnedRatio(nodeID, name); prev > 0 {
		smoothed = ratioSmoothing*observed + (1-ratioSmoothing)*prev
	}
	_ = e.Store.SetSetting(ratioSettingKey(nodeID, name), strconv.FormatFloat(smoothed, 'f', 4, 64))
}

// Autotune (F84): an implicitly-"balanced" run over a large selection whose
// learned ratio proves the data incompressible (media libraries — JPEG/HEIC/
// MP4/EPUB already-compressed payloads) is executed as "fast" instead: 2-4x
// the throughput for a ±1% size difference. Guards are deliberately narrow —
// an explicit user choice is never overridden, and small/compressible/unknown
// selections keep today's behavior byte-for-byte.
const (
	autotuneRatioMin = 0.97    // learned stored/uncompressed at or above this = incompressible
	autotuneMinBytes = 1 << 30 // only bother for selections >= 1 GiB
)

// AutotuneCompression is the pure F84 decision: returns ("fast", true) only
// when the toggle is on, the preset is the IMPLICIT default ("balanced" or
// unset — explicit choices always win), the learned ratio says compression
// can't help, and the selection is big enough to matter. Otherwise the preset
// is returned unchanged.
func AutotuneCompression(enabled, explicit bool, preset string, learned float64, selectionBytes int64) (string, bool) {
	if !enabled || explicit || (preset != "" && preset != "balanced") {
		return preset, false
	}
	if learned < autotuneRatioMin || selectionBytes < autotuneMinBytes {
		return preset, false
	}
	return "fast", true
}

// autotuneCompression resolves a run's effective preset (F84): the pure
// decision fed with the container's learned ratio and the in-app toggle
// (backup.autotune_compression, default on).
func (e *Engine) autotuneCompression(opts Options, name string, selectionBytes int64) (preset string, learned float64, switched bool) {
	enabled := true
	if v, _ := e.Store.GetSetting("backup.autotune_compression", "true"); v == "false" {
		enabled = false
	}
	learned = e.learnedRatio(opts.NodeID, name)
	preset, switched = AutotuneCompression(enabled, opts.CompressionExplicit, opts.Compression, learned, selectionBytes)
	return preset, learned, switched
}

// estimateCompressed estimates the stored archive size from the uncompressed
// selection size. It prefers a container's LEARNED ratio (learned > 0), clamped to
// a sane band, and otherwise falls back to the conservative preset constant (F17).
func estimateCompressed(uncompressed int64, preset string, learned float64) int64 {
	ratio := estimatedCompressionRatio(preset)
	if learned > 0 {
		ratio = learned
		if ratio < minLearnedRatio {
			ratio = minLearnedRatio
		}
		if ratio > maxLearnedRatio {
			ratio = maxLearnedRatio
		}
	}
	return int64(float64(uncompressed) * ratio)
}

// guardFreeSpace fails fast (returns an error) if the selected mounts clearly
// won't fit in the work dir or destination free space, instead of filling the
// disk mid-archive. It also returns the selected bind destinations that were
// unreadable (permission denied under them) — in addition to the WARN it logs —
// so the caller records them as PARTIAL skipped mounts (F3) rather than dropping
// those files silently.
func (e *Engine) guardFreeSpace(ctx context.Context, cli *client.Client, nodeID, name, containerID string, dests, binds []string, id, compression string) (unreadableBinds []string, uncompressedBytes int64, err error) {
	if len(dests) == 0 {
		return nil, 0, nil
	}
	sctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	sizes, unreadable, _, _ := dockercli.MountSizesFrom(sctx, cli, containerID, dests)
	cancel()

	// PLAN §2.13: detect bind-mount permission/ownership mismatches and WARN —
	// never let a backup silently omit files the reader couldn't access. The
	// unreadable set is also returned so the caller records it as a PARTIAL
	// skipped-mount in the manifest (F3), not just a log line.
	bindSet := setOf(binds)
	seen := map[string]bool{}
	for _, p := range unreadable {
		if bindSet[p] && !seen[p] {
			seen[p] = true
			unreadableBinds = append(unreadableBinds, p)
			e.logf(id, "WARN", "Permission denied reading parts of bind mount %s — the backup reader can't access some files (likely a UID/GID mismatch, e.g. NFS root_squash). Those files will be MISSING from the backup; align the host ownership with the container's user.", p)
		}
	}

	var total int64
	for _, d := range dests {
		total += sizes[d]
	}
	if total <= 0 {
		return unreadableBinds, 0, nil // unknown — don't block, nothing to learn from
	}

	// 1) Disk-backed work dir must hold the raw volume tar that is spooled there
	//    before the streaming compress+encrypt step. The default work
	//    dir shares the backups volume, so the compressed archive is written
	//    alongside the spool — budget the uncompressed selection plus ~20% headroom.
	if wf, werr := freeBytesAt(e.WorkDir); werr == nil && wf > 0 {
		need := uint64(total) + uint64(total)/5
		if need > wf {
			return unreadableBinds, total, fmt.Errorf("not enough scratch space to spool the backup in %s: need ~%s, only %s free — set DOCKBACK_WORK_DIR to a larger disk or deselect large paths",
				e.WorkDir, humanBytes(int64(need)), humanBytes(int64(wf)))
		}
		e.logf(id, "INFO", "Work dir %s: %s free for ~%s of volume data", e.WorkDir, humanBytes(int64(wf)), humanBytes(total))
	}

	// 2) The destination must hold the (compressed) archive. Estimate the
	//    compressed size from the selection and require it to fit within 95% of free
	//    space. Prefer this container's LEARNED ratio from prior backups (F17) over
	//    the fixed preset constant — so a highly-compressible container isn't
	//    over-refused and incompressible data isn't under-budgeted into a mid-write
	//    overflow. A rare under-estimate still fails gracefully when the Put overflows.
	free, ferr := e.Storage.FreeBytes(ctx)
	if ferr != nil || free == 0 {
		return unreadableBinds, total, nil // unknown free space — don't block (still learnable)
	}
	learned := e.learnedRatio(nodeID, name)
	est := estimateCompressed(total, compression, learned)
	if uint64(est) > free/100*95 {
		return unreadableBinds, total, fmt.Errorf("estimated compressed backup ~%s won't fit in %s free at the backup target (from ~%s of volume data) — free space or deselect large paths",
			humanBytes(est), humanBytes(int64(free)), humanBytes(total))
	}
	if learned > 0 {
		e.logf(id, "INFO", "Estimated compressed size ~%s (from ~%s of volume data, learned ratio %.2f); %s free at target", humanBytes(est), humanBytes(total), learned, humanBytes(int64(free)))
	} else {
		e.logf(id, "INFO", "Estimated compressed size ~%s (from ~%s of volume data); %s free at target", humanBytes(est), humanBytes(total), humanBytes(int64(free)))
	}
	return unreadableBinds, total, nil
}

// unreadableReason is the manifest text for a selected bind mount whose files the
// backup reader couldn't access — recorded so the backup is honestly PARTIAL (F3).
const unreadableReason = "permission denied reading some files (likely a UID/GID mismatch, e.g. NFS root_squash) — those files are MISSING from the backup"

// unreadableSkippedMounts turns the selected bind destinations that were
// unreadable into PARTIAL skipped-mount records (F3), carrying each bind's host
// source path so the UI/runbook can show it. Returns nil when there are none.
func unreadableSkippedMounts(unreadableBinds []string, bindSrc map[string]string) []SkippedMount {
	if len(unreadableBinds) == 0 {
		return nil
	}
	out := make([]SkippedMount, 0, len(unreadableBinds))
	for _, dest := range unreadableBinds {
		out = append(out, SkippedMount{
			Destination: dest,
			Source:      bindSrc[dest],
			Type:        "bind",
			Reason:      unreadableReason,
		})
	}
	return out
}

// Capturing each byte once when mounts are nested inside one another (#20).
//
// tar crosses mount boundaries, and the sidecar sees every one of the target's
// mounts at its own path. So naming a parent AND a child in the same member list
// walks the child twice and writes it twice, under the same member name both
// times. Nextcloud mounts four binds under /var/www/html; 22 GB was captured as
// 44 GB.
//
// The fix is to stop LISTING the child, not to exclude it from the parent.
// --exclude in tarWithExcludes is global to the whole invocation, so excluding a
// child to keep it out of the parent's walk also deletes it from its own member
// — measured, and the child's files disappear from the archive entirely. Since
// the member name a child produces is identical either way
// (var/www/html/data/f), dropping it from the list leaves the archive holding
// exactly one copy at exactly the right path, and a restore extracts it through
// the child's own live mount as before.

// normalizeMountDest puts a destination in the one form the comparisons below
// expect: no trailing slash, except for root, which is only ever "/".
func normalizeMountDest(dest string) string {
	dest = strings.TrimSpace(dest)
	if dest == "/" {
		return dest
	}
	return strings.TrimSuffix(dest, "/")
}

// pathContains reports whether parent is an ancestor of child on a COMPONENT
// boundary. /var/www/html contains /var/www/html/data and does not contain
// /var/www/html2 — a plain string prefix would fold two unrelated mounts into
// one and lose the second.
func pathContains(parent, child string) bool {
	if parent == "/" {
		return child != "/"
	}
	return strings.HasPrefix(child, parent+"/")
}

// nestedWithin maps each destination to the LONGEST other destination that
// contains it. Destinations with no ancestor in the set are absent from the map.
//
// Longest, not any, because mounts nest more than one deep: with /var/www/html,
// /var/www/html/data and /var/www/html/data/files, the innermost one's real
// parent is /var/www/html/data. Only the outermost mount is kept as a member,
// but each child still records the mount it actually sits in.
func nestedWithin(dests []string) map[string]string {
	clean := make([]string, 0, len(dests))
	seen := map[string]bool{}
	for _, d := range dests {
		d = normalizeMountDest(d)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		clean = append(clean, d)
	}

	out := map[string]string{}
	for _, child := range clean {
		longest := ""
		for _, parent := range clean {
			if !pathContains(parent, child) || len(parent) <= len(longest) {
				continue
			}
			longest = parent
		}
		if longest != "" {
			out[child] = longest
		}
	}
	return out
}

// foldNestedMounts drops the captured mounts whose bytes another captured mount
// already carries, and records on each what it is now riding inside.
//
// Only a mount whose parent is REALLY in the archive is folded. A child of an
// unselected parent has no other copy and stays a member of its own; that is the
// case where folding would be the data loss this exists to avoid.
func (e *Engine) foldNestedMounts(volDests []string, refs []VolumeRef, id string) ([]string, []VolumeRef) {
	all := make([]string, 0, len(refs)+len(volDests))
	for _, r := range refs {
		all = append(all, r.Destination)
	}
	all = append(all, volDests...)
	nested := nestedWithin(all)
	if len(nested) == 0 {
		return volDests, refs
	}

	captured := map[string]bool{}
	for _, d := range volDests {
		captured[normalizeMountDest(d)] = true
	}
	// "Nested" is only worth recording when the parent's member really does hold
	// this mount's bytes, so the field never claims a copy that was not taken.
	parentOf := func(dest string) string {
		parent := nested[normalizeMountDest(dest)]
		if parent == "" || !captured[parent] {
			return ""
		}
		return parent
	}

	for i := range refs {
		refs[i].NestedIn = parentOf(refs[i].Destination)
	}

	kept := make([]string, 0, len(volDests))
	children := map[string][]string{}
	for _, d := range volDests {
		parent := parentOf(d)
		if parent == "" {
			kept = append(kept, d)
			continue
		}
		children[parent] = append(children[parent], d)
	}

	parents := make([]string, 0, len(children))
	for parent := range children {
		parents = append(parents, parent)
	}
	sort.Strings(parents)
	for _, parent := range parents {
		kids := children[parent]
		e.logf(id, "INFO", "%s already contains %d nested mount(s) (%s) — they travel inside its archive member rather than being captured a second time",
			parent, len(kids), strings.Join(kids, ", "))
	}
	return kept, refs
}

// One host path mounted at more than one destination in the SAME container (#21).
//
// R3 §4 found a stack whose database directory was bound at both /var/lib/mysql
// and /etc/mysql/conf.d. MariaDB scans conf.d for configuration, so the datadir's
// own my.cnf was being read as server config — and it contained
// skip_grant_tables=ON, inert only because a second misconfiguration (mode 0777)
// made the server reject the file as world-writable. Two bugs cancelling out.
//
// Nothing surfaced this: bind roots are keyed by DESTINATION, so both mounts are
// recorded correctly and the fact they are one directory is never noticed.

// duplicateMountTargets groups a container's bind roots by host source and keeps
// the sources reachable at more than one destination.
//
// Within one container only. The same source mounted by DIFFERENT containers is
// F83's shared-bind pattern — normal, already handled, and not a defect.
func duplicateMountTargets(binds []VolumeRef) map[string][]string {
	bySource := map[string][]string{}
	for _, b := range binds {
		src, dest := strings.TrimSpace(b.Source), normalizeMountDest(b.Destination)
		if src == "" || dest == "" {
			continue
		}
		if !slices.Contains(bySource[src], dest) {
			bySource[src] = append(bySource[src], dest)
		}
	}
	for src, dests := range bySource {
		if len(dests) < 2 {
			delete(bySource, src)
			continue
		}
		sort.Strings(dests)
		bySource[src] = dests
	}
	return bySource
}

// reportDuplicateMountTargets records one finding per host path that this
// container mounts more than once.
func (e *Engine) reportDuplicateMountTargets(man *Manifest, logID string) {
	dupes := duplicateMountTargets(man.MountedBinds)
	if len(dupes) == 0 {
		return
	}
	sources := make([]string, 0, len(dupes))
	for src := range dupes {
		sources = append(sources, src)
	}
	sort.Strings(sources)
	for _, src := range sources {
		dests := dupes[src]
		e.addFinding(man, logID, findingDuplicateMountTarget, FindingWarn, src, fmt.Sprintf(
			"This one host directory is mounted at %d places in this container (%s), so the application can see one copy of these files at %d paths. "+
				"Configuration directories especially may scan data files as configuration. Mount it once, or split the configuration into its own directory.",
			len(dests), strings.Join(dests, ", "), len(dests)))
	}
}
