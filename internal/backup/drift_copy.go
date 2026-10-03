package backup

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/docker/docker/client"
)

// Measuring what moved while the copy was running (#25).
//
// R3 §Issue 25: 22 GB copied over ~25 minutes with the source live. Comparing
// per-file manifests afterwards, 53,159 files matched and exactly one did not —
// `nextcloud.log`, which grew 3,573 bytes during the copy. Benign there, and
// that is the trap: "a file-level copy of a running application yields a smear
// across the copy window, not a snapshot. A file being rewritten (rather than
// appended) at the moment it is read can be captured torn."
//
// DockBack can quiesce, and by default does. What it never did was MEASURE. A
// backup taken without quiescing is not wrong to take — sometimes it is the only
// one available — but it should say what moved underneath it.
//
// Stat only, never a re-hash: R3 §11.3's other lesson is that verification I/O
// competes with the transfer, and re-reading 22 GB to learn that a log grew is
// the wrong trade.

// genericDriftExpected are names whose change during a copy is ordinary
// anywhere. A log is appended to continuously by definition — R3's single
// drifting file was one.
//
// Wider than VolatileFiles is allowed to be, and safely so: this decides a
// finding's SEVERITY, never whether a restore is verified. Mislabelling here
// costs an info that should have been a warn; mislabelling there would unguard
// real data.
var genericDriftExpected = []string{"*.log", "*.log.*", "*.log-*", "*.pid", "*.lock", "*.sock"}

// driftExpectation is what an application says about its own churn: the paths it
// declares hold logs and scratch (F138), the directories it rebuilds by itself
// (F132), and the files it rewrites on its own (#41).
type driftExpectation struct {
	globs    []string
	prefixes []string
}

// driftExpectationFor gathers it for one image.
func driftExpectationFor(image string) driftExpectation {
	expect := driftExpectation{globs: append([]string(nil), genericDriftExpected...)}
	profile := ProfileFor(image)
	if profile == nil {
		return expect
	}
	expect.globs = append(expect.globs, profile.VolatileFiles...)
	for _, never := range profile.NeverBackup {
		expect.prefixes = append(expect.prefixes, never.Path)
	}
	for _, regen := range profile.Regenerable {
		expect.prefixes = append(expect.prefixes, regen.Path)
	}
	return expect
}

// expected reports whether a path's movement during the copy is ordinary.
func (d driftExpectation) expected(rel string) bool {
	if matchesVolatileFile(rel, d.globs) {
		return true
	}
	for _, prefix := range d.prefixes {
		trimmed := strings.TrimPrefix(strings.TrimSpace(prefix), "/")
		if trimmed == "" {
			continue
		}
		// A declared path may itself be a glob (Mealie's "/app/data/mealie.log*").
		if matchesVolatileFile(rel, []string{trimmed}) {
			return true
		}
		if rel == trimmed || strings.HasPrefix(rel, trimmed+"/") {
			return true
		}
	}
	return false
}

// CopyDrift is one file that changed while the archive was being written.
type CopyDrift struct {
	Path      string
	SizeDelta int64
	// Rewritten marks a file whose modification time moved without its size —
	// the case R3 warns about, because a file being REWRITTEN as it is read can
	// be captured torn, where an append merely arrives short.
	Rewritten bool
}

// describe renders one entry the way R3 reports it: the file, then what moved.
func (d CopyDrift) describe() string {
	switch {
	case d.SizeDelta > 0:
		return fmt.Sprintf("%s +%s", d.Path, humanBytes(d.SizeDelta))
	case d.SizeDelta < 0:
		return fmt.Sprintf("%s -%s", d.Path, humanBytes(-d.SizeDelta))
	default:
		return d.Path + " rewritten in place"
	}
}

// diffCopyDrift compares two stat walks taken either side of the copy.
//
// Only files present in BOTH are drift. One that appeared is not something the
// archive got wrong, and one that vanished is reported by the capture's own
// accounting; conflating either with "changed underneath us" would bury the case
// this exists to surface.
func diffCopyDrift(before, after VolIndex, expect driftExpectation) (ordinary, unexpected []CopyDrift) {
	was := make(map[string]FileEntry, len(before.Entries))
	for _, e := range before.Entries {
		was[e.Path] = e
	}
	for _, now := range after.Entries {
		old, seen := was[now.Path]
		if !seen || (old.Size == now.Size && old.MtimeUnix == now.MtimeUnix) {
			continue
		}
		entry := CopyDrift{
			Path:      now.Path,
			SizeDelta: now.Size - old.Size,
			Rewritten: old.Size == now.Size,
		}
		if expect.expected(now.Path) {
			ordinary = append(ordinary, entry)
			continue
		}
		unexpected = append(unexpected, entry)
	}
	sort.Slice(ordinary, func(i, j int) bool { return ordinary[i].Path < ordinary[j].Path })
	sort.Slice(unexpected, func(i, j int) bool { return unexpected[i].Path < unexpected[j].Path })
	return ordinary, unexpected
}

// describeDrift renders a list, bounded like every other named-item report.
func describeDrift(entries []CopyDrift) string {
	shown := entries
	suffix := ""
	if len(entries) > maxReportedShortTables {
		shown = entries[:maxReportedShortTables]
		suffix = fmt.Sprintf(" and %d more", len(entries)-maxReportedShortTables)
	}
	parts := make([]string, 0, len(shown))
	for _, e := range shown {
		parts = append(parts, e.describe())
	}
	return strings.Join(parts, ", ") + suffix
}

// driftBaseline takes the stat walk the drift report compares against.
//
// Returns nothing when the container is being STOPPED for the copy: its own
// processes cannot write to a filesystem they are not running on, so there is
// nothing to measure and no reason to pay for a walk.
func (e *Engine) driftBaseline(ctx context.Context, cli *client.Client, containerID string, dests []string, pauseMode, image, logID string) *VolIndex {
	if len(dests) == 0 || pauseMode == PauseStop {
		return nil
	}
	idx, err := e.buildVolIndex(ctx, cli, containerID, dests, image)
	if err != nil {
		e.logf(logID, "INFO", "Could not list the files before the copy (%v) — the backup proceeds, but it cannot report what changed underneath it", err)
		return nil
	}
	return &idx
}

// reportCopyDrift says what moved while the archive was being written.
//
// Report-only, always. A backup taken from a live application is not wrong to
// take — often it is the only one available — and refusing it because a log grew
// would be refusing the backup over the thing least worth worrying about. What
// an operator needs is to know WHICH files moved, because "the log grew" and
// "a database file was rewritten mid-read" look identical until they are named.
func (e *Engine) reportCopyDrift(man *Manifest, logID string, before *VolIndex, after VolIndex, image string) {
	if before == nil || len(before.Entries) == 0 || len(after.Entries) == 0 {
		return
	}
	ordinary, unexpected := diffCopyDrift(*before, after, driftExpectationFor(image))
	if len(ordinary) == 0 && len(unexpected) == 0 {
		e.logf(logID, "INFO", "Nothing changed underneath the copy — every file is as it was when the archive started")
		return
	}
	if len(ordinary) > 0 {
		e.addFinding(man, logID, findingChangedDuringCopy, FindingInfo, "", fmt.Sprintf(
			"%d %s changed while the archive was being written, all of them logs, caches or scratch this application writes continuously: %s. Ordinary, and not a reason to distrust the backup.",
			len(ordinary), plural(len(ordinary), "file", "files"), describeDrift(ordinary)))
	}
	if len(unexpected) == 0 {
		return
	}
	e.addFinding(man, logID, findingChangedDuringCopy, FindingWarn, "", fmt.Sprintf(
		"%d %s changed while the archive was being written: %s. A copy of a running application is a smear across the copy window, not a snapshot — a file appended to arrives short, and one REWRITTEN as it was read can be captured torn. "+
			"If any of these matter, back this container up with its pause mode set to stop, which holds it still for the copy.",
		len(unexpected), plural(len(unexpected), "file", "files"), describeDrift(unexpected)))
}
