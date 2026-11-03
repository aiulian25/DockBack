package backup

import (
	"context"
	"fmt"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

// What a capture does with one container, shared by the two ways a container is
// captured: on its own (Run) and as one service of an app-consistent stack snapshot
// (BackupStackConsistent). The two were separate copies, and every feature added to
// one was found missing from the other by someone whose backup lacked it. Each step
// here is used by both, so a saved per-service choice means the same thing in both.
//
// Where a group snapshot cannot honour a choice, it says so: captureOverride below.

// resolveDatabaseCapture decides how a container's database is captured: the engine
// to dump live, and for an all-in-one image the embedded server inside it (F126), or
// "" for files. A stopped container has no live process to dump, so it is captured
// as files (F9): valid for an app kept off between uses, and its data directory is
// then included, since no dump replaces it.
func (e *Engine) resolveDatabaseCapture(ctx context.Context, cli *client.Client, containerID string, insp types.ContainerJSON, running bool, id string) (string, *EmbeddedDump) {
	engineKind := detectDBEngine(insp.Config.Image, insp.Config.Env)
	if !running {
		if engineKind != "" {
			e.logf(id, "INFO", "Target is stopped — backing up volumes/config without a live database dump")
		}
		return "", nil
	}
	if engineKind != "" {
		return engineKind, nil
	}
	// F126: an ALL-IN-ONE image bundling its own database server. detectDBEngine
	// reads the image name, and an image name says nothing about what it bundles —
	// `jwetzell/guacamole` runs a full PostgreSQL and looks like an ordinary app,
	// so its live data directory was raw-copied: a hot file copy of a running
	// database, which is exactly what the dump path exists to prevent.
	p := ProfileFor(insp.Config.Image)
	if p == nil || p.EmbeddedDump == nil {
		return "", nil
	}
	// F166: some applications ship ONE image that can run several ways, so the
	// declaration is conditional. The probe decides; a "no" leaves the ordinary file
	// path in place, which for such a deployment is the correct method rather than a
	// fallback.
	if !e.embeddedDumpApplies(ctx, cli, containerID, p.EmbeddedDump, id, p.Name) {
		return "", nil
	}
	e.logf(id, "INFO", "%s bundles its own %s server — dumping it with native tools instead of copying its data directory", p.Name, p.EmbeddedDump.Engine)
	return p.EmbeddedDump.Engine, p.EmbeddedDump
}

// resolveExclusions records what this container's application never needs backed up
// (F138) and what its operator chose to leave out because it rebuilds itself (F132),
// on opts for the copy and on the manifest for the record, so the two cannot
// disagree: the archive always states exactly what was left out and what
// regenerating it costs.
func (e *Engine) resolveExclusions(opts *Options, man *Manifest, image, name, id string) {
	if p := ProfileFor(image); p != nil && len(p.NeverBackup) > 0 {
		for _, nb := range p.NeverBackup {
			if nb.Path == "" {
				continue
			}
			opts.neverBackup = append(opts.neverBackup, nb.Path)
			man.ExcludedRegenerable = append(man.ExcludedRegenerable, ExcludedPath{Path: nb.Path, Label: nb.Why})
		}
		if len(opts.neverBackup) > 0 {
			e.logf(id, "INFO", "Leaving out %d path(s) with no restore value: %s", len(opts.neverBackup), strings.Join(opts.neverBackup, ", "))
		}
	}
	regen := RegenerablePathsFor(image)
	if len(regen) == 0 || !e.ExcludeRegenerable(opts.NodeID, name) {
		return
	}
	opts.excludeRegenerable = regen
	for _, r := range regen {
		man.ExcludedRegenerable = append(man.ExcludedRegenerable, ExcludedPath{Path: r.Path, Label: r.Label, Cost: r.Cost})
		e.logf(id, "INFO", "Leaving out %s (%s) — %s rebuilds it. %s", r.Path, r.Label, name, r.Cost)
	}
}

// bundleImage saves the container's image into the archive for an air-gapped
// restore (PLAN §0.3 / §8.4), by reference so `docker load` restores its repo:tag.
// Best-effort: a failure logs WARN and the backup still completes, just not
// air-gapped. It materially enlarges the archive, hence opt-in.
func (e *Engine) bundleImage(ctx context.Context, cli *client.Client, imageRef, work string, man *Manifest, id string) {
	if imageRef == "" {
		e.logf(id, "WARN", "Image tarball requested but the container has no resolvable image reference — skipping")
		return
	}
	itBytes, err := e.saveImageTar(ctx, cli, imageRef, work, id)
	if err != nil {
		e.logf(id, "WARN", "Image tarball NOT saved (%v) — this backup is not air-gapped", err)
		return
	}
	man.ImageTar = &ImageTarRef{Ref: imageRef, Bytes: itBytes}
	man.Format.Layout += ", image.tar"
	e.logf(id, "INFO", "Saved image tarball for air-gapped restore: %s (%s)", imageRef, humanBytes(itBytes))
}

// indexWholeCapture stores the complete file listing of a full volume capture inside
// the archive (F70), so browsing lists every file from one small member and file
// search and generation diff become index-only reads. The walk is itself the "after"
// reading of a drift report, which costs it no walk of its own. Best-effort — an
// index failure NEVER fails the backup; index.always=false gives byte-identical
// pre-F70 archives.
func (e *Engine) indexWholeCapture(ctx context.Context, cli *client.Client, containerID string, volDests, excludes []string, fileHashes map[string]string, driftBefore *VolIndex, man *Manifest, work, id string) {
	if !e.indexAlways() {
		return
	}
	idx, err := e.buildVolIndex(ctx, cli, containerID, volDests, excludes, man.Image)
	if err != nil {
		e.logf(id, "WARN", "File indexing failed (%v) — browsing this backup will stream the archive instead", err)
		return
	}
	e.stampIndexHashes(&idx, fileHashes, id)
	e.reportCopyDrift(man, id, driftBefore, idx, man.Image)
	e.storeVolIndex(man, work, id, idx)
}

// captureOverride is a saved per-service choice an app-consistent snapshot cannot
// honour, and why. It is said before anything is held, on the archive and in the
// log, rather than left for the operator to discover in a restore.
type captureOverride struct {
	choice string
	why    string
}

// consistentCaptureOverrides lists what a group snapshot does differently from what
// this service's own settings ask for. App-native export runs the application's own
// tool, which needs it running, and the window holds it still, so the export is
// replaced by its files and databases captured at the group's moment. An
// incremental setting does not apply, because every service of a group is captured
// in full: its point in time is the whole group's. Pure.
func consistentCaptureOverrides(appExport bool, exportTool string, incremental bool) []captureOverride {
	var overrides []captureOverride
	if appExport && exportTool != "" {
		overrides = append(overrides, captureOverride{
			choice: "app-native export",
			why: fmt.Sprintf("an app-consistent snapshot captures this service's files and databases at the group's moment instead of a %s export, "+
				"because the export runs %s's own tool and needs it running while the snapshot holds it still. "+
				"A backup of this container on its own still takes the export", exportTool, exportTool),
		})
	}
	if incremental {
		overrides = append(overrides, captureOverride{
			choice: "incremental",
			why: "an app-consistent snapshot captures every service in full, because its point in time is the whole group's; " +
				"this service's own incremental chain carries on from its own backups",
		})
	}
	return overrides
}

// recordCaptureOverrides puts each override on the archive as a finding and in the log.
func (e *Engine) recordCaptureOverrides(man *Manifest, id string, overrides []captureOverride) {
	for _, o := range overrides {
		e.addFinding(man, id, findingCaptureOverridden, FindingInfo, o.choice, "Saved setting not applied here: "+o.choice+" — "+o.why+".")
	}
}
