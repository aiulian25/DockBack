package backup

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// Refusing to start an application on data that did not arrive (#1).
//
// R1's end state is the most dangerous outcome this tool has: the container
// starts, finds an empty data directory, initialises itself fresh, and the
// operator sees a working site with all their content gone. Nothing about it
// looks like a failure — the restore reported success, the health check passes,
// the application is up. It is only wrong.
//
// The materialiser creates the bind sources and the untar fills them, but
// nothing ever checked that the filling happened. A member that failed to
// extract, a remap pointing at the wrong pair of paths, or a parent captured
// empty all reach the same place.
//
// So: before the start, every bind the backup says HAD content is checked for
// content. Empty means stop, not start — an application that has not yet run
// against an empty directory can still be restored again, and one that has
// cannot.

// bindFillState is what the probe found at one destination.
const (
	bindFilled  = "FILLED"
	bindEmpty   = "EMPTY"
	bindMissing = "MISSING"
)

// expectedNonEmptyDestinations lists the bind destinations this backup captured
// with content in them.
//
// The file index is the authority, and the only one: it is the record of what
// was actually there at capture, taken by the same walk that produced the
// archive. A destination with no entries under it was empty on the SOURCE — the
// geoip directory an application populates on first run is the standard case —
// and must never block a restore, because reproducing empty is correct.
//
// File-rooted binds (F81) are excluded. Their bytes travel as their own member
// rather than in the volume archive, and their destination is a file, so asking
// whether it "contains" anything would report every one of them as empty.
func expectedNonEmptyDestinations(man *Manifest, idx VolIndex) []string {
	if man == nil || len(idx.Entries) == 0 {
		return nil
	}
	// Judge only what the archive actually carried. A dumped database's data
	// directory is in the index but never in the archive, so after the files are
	// restored it is empty BY DESIGN until the dump is imported — and treating
	// that as "data that did not arrive" is what stopped a real recovery between
	// clearing the directory and importing the dump.
	idx = withoutArchiveExcluded(idx, archiveExcludedPaths(man))
	var out []string
	for _, v := range man.Volumes {
		if v.Type != "bind" || v.Destination == "" {
			continue
		}
		if v.Kind == dockercli.MountKindFile || v.Archive != "" {
			continue
		}
		prefix := strings.TrimPrefix(normalizeMountDest(v.Destination), "/")
		if prefix == "" {
			continue // a mount at "/" cannot be reasoned about this way
		}
		if indexHasEntriesUnder(idx, prefix) {
			out = append(out, v.Destination)
		}
	}
	return out
}

// archiveExcludedPaths is everything a backup deliberately kept out of its
// volume archive. Backups that record it say so directly. Older ones are
// reconstructed from what they do record: the bundled database whose dump
// replaced its data directory (only when that dump exists — a failed dump means
// the directory was captured raw), the regenerable folders, and the paths the
// app's profile never backs up.
func archiveExcludedPaths(man *Manifest) []string {
	if len(man.ArchiveExcluded) > 0 {
		return man.ArchiveExcluded
	}
	var out []string
	if d := embeddedDumpFor(man); d != nil && d.DataDir != "" && len(man.Databases) > 0 {
		out = append(out, d.DataDir)
	}
	for _, r := range man.ExcludedRegenerable {
		if r.Path != "" {
			out = append(out, r.Path)
		}
	}
	if p := ProfileFor(man.Image); p != nil {
		for _, nb := range p.NeverBackup {
			out = append(out, nb.Path)
		}
	}
	return out
}

// withoutArchiveExcluded drops the index entries the archive never held.
func withoutArchiveExcluded(idx VolIndex, excluded []string) VolIndex {
	if len(excluded) == 0 {
		return idx
	}
	kept := VolIndex{}
	for _, entry := range idx.Entries {
		if !excludedByAny(entry.Path, excluded) {
			kept.Entries = append(kept.Entries, entry)
		}
	}
	return kept
}

// excludedByAny matches an index path against archive exclusions the way the
// capture applied them: a path itself, everything beneath it, and globs — a
// glob that matches a directory covers everything under it.
func excludedByAny(rel string, patterns []string) bool {
	for _, raw := range patterns {
		pattern := strings.Trim(strings.TrimSpace(raw), "/")
		if pattern == "" {
			continue
		}
		if rel == pattern || strings.HasPrefix(rel, pattern+"/") {
			return true
		}
		if !strings.ContainsAny(pattern, "*?[") {
			continue
		}
		for candidate := rel; candidate != "." && candidate != ""; candidate = path.Dir(candidate) {
			if matched, _ := path.Match(pattern, candidate); matched {
				return true
			}
		}
	}
	return false
}

// indexHasEntriesUnder reports whether the index recorded any file under a
// destination. Index paths are relative, the same convention as tar members.
func indexHasEntriesUnder(idx VolIndex, prefix string) bool {
	for _, entry := range idx.Entries {
		if strings.HasPrefix(entry.Path, prefix+"/") {
			return true
		}
	}
	return false
}

// emptyProbeScript asks one sidecar about every destination at once.
//
// -mindepth 1 -maxdepth 1 stops at the first child: this asks whether anything
// is there, not how much, and walking a 58 GB library to answer that would cost
// more than the restore it is guarding.
func emptyProbeScript(dests []string) string {
	var script strings.Builder
	script.WriteString("for d in")
	for _, d := range dests {
		script.WriteString(" '" + shellEscape(d) + "'")
	}
	script.WriteString(`; do if [ ! -d "$d" ]; then s=` + bindMissing + `; ` +
		`elif [ -n "$(find "$d" -mindepth 1 -maxdepth 1 2>/dev/null | head -n 1)" ]; then s=` + bindFilled + `; ` +
		`else s=` + bindEmpty + `; fi; printf '%s|%s\n' "$s" "$d"; done`)
	return script.String()
}

// parseEmptyProbe reads the probe's report.
//
// A destination the probe said nothing about is absent from the map, and the
// caller treats that as unknown rather than as empty: a truncated or garbled
// report must not refuse a restore that was fine.
func parseEmptyProbe(out string) map[string]string {
	found := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		state, dest, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok || dest == "" {
			continue
		}
		switch state {
		case bindFilled, bindEmpty, bindMissing:
			found[dest] = state
		}
	}
	return found
}

// assertDataBindsFilled refuses to start a container whose restored bind mounts
// are empty where the backup says they held data.
//
// Returns an error rather than repairing anything. Re-running the untar would be
// guessing at which of several causes applied — a failed member, a mis-paired
// remap, a parent captured empty — and each needs a different fix from the
// operator. The one thing that must not happen is the start.
func (e *Engine) assertDataBindsFilled(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions) error {
	if man == nil || !opts.Volumes {
		return nil
	}
	if opts.AsName != "" {
		// A clone's binds were stripped for fresh anonymous volumes, so the
		// source's destinations describe nothing this container has.
		return nil
	}
	if man.VolIndex == "" {
		// Taken before the file index existed. There is no record of what was
		// there, so there is no expectation to check — and inventing one from the
		// archive's size would refuse restores on a guess.
		return nil
	}
	idx, err := e.loadVolIndex(ctx, b)
	if err != nil {
		e.logf(b.ID, "WARN", "Could not read this backup's file index (%v) — starting without checking that the restored data arrived", err)
		return nil
	}
	return e.checkBindsFilled(ctx, cli, b, opts.TargetID, expectedNonEmptyDestinations(man, idx))
}

// checkBindsFilled probes the destinations and turns the answer into a verdict.
//
// Separate from the resolution above because the two answer different questions
// — what SHOULD hold data, and what DOES — and only the second one needs a
// daemon.
func (e *Engine) checkBindsFilled(ctx context.Context, cli *client.Client, b *store.Backup, targetID string, dests []string) error {
	if len(dests) == 0 {
		return nil
	}

	out, perr := dockercli.CaptureSidecarRO(ctx, cli, targetID,
		[]string{"/bin/sh", "-c", emptyProbeScript(dests)})
	if perr != nil {
		e.logf(b.ID, "WARN", "Could not check whether the restored data arrived (%v) — the data was restored, but its presence on the target was not confirmed", perr)
		return nil
	}
	states := parseEmptyProbe(string(out))

	var offenders []string
	for _, dest := range dests {
		switch states[dest] {
		case bindEmpty:
			offenders = append(offenders, dest+" is empty")
		case bindMissing:
			offenders = append(offenders, dest+" is not mounted on this container")
		}
	}
	if len(offenders) == 0 {
		e.logf(b.ID, "INFO", "Checked %d restored data mount(s): all hold data, as the backup recorded", len(dests))
		return nil
	}

	e.logf(b.ID, "ERROR", "%d restored mount(s) hold no data, though this backup recorded data in them — the container was NOT started:", len(offenders))
	for _, offender := range offenders {
		e.logf(b.ID, "ERROR", "  %s", offender)
	}
	e.logf(b.ID, "ERROR", "Starting it now is what turns a failed restore into a lost one: the application would initialise itself against the empty directory and look like it is working. Check the host path remap in the restore dialog points at the paths this backup was taken from, then restore again.")
	return fmt.Errorf("restored data is missing from %d mount(s) — %s. The container was NOT started; see the run log for the full list",
		len(offenders), offenders[0])
}
