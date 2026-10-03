package backup

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
)

// Interrupted-write leftovers (F157).
//
// Careful software writes by staging: put the new content in a temporary
// directory beside the target, then rename it into place, so a crash leaves
// either the old content or the new one and never a half of each. Radicale does
// it with `.Radicale.tmp-*`; plenty of others do the same thing under other
// names.
//
// It follows that a staging directory still sitting there long after it was made
// is the fossil of a write that was interrupted — and that nothing will ever
// clear it, because the application is not looking for it. It is inert, which is
// precisely why it survives: two of them sat in one production tree for a year,
// roughly doubling the item count of every backup taken of it.
//
// A backup already walks the whole tree, so it is the natural place to notice —
// and it is one of the few moments the operator is actually thinking about this
// data.
//
// It is REPORTED and never removed. Deleting something because it looks like
// debris is not a decision a backup tool gets to make on its own: the whole
// premise of the thing is that it does not alter what it is copying. So the
// finding names the paths, says what they are costing, and gives the command —
// with the precondition that makes running it safe.

// scanStagingDebris looks for interrupted-write leftovers under the captured
// mounts and records them (F157).
//
// No-op for an image that declares no patterns, which is nearly all of them, and
// entirely best-effort: a scan that cannot run records nothing rather than
// claiming a clean tree.
func (e *Engine) scanStagingDebris(ctx context.Context, cli *client.Client, containerID, image string, volDests []string, man *Manifest, logID string) {
	p := ProfileFor(image)
	if p == nil || len(p.StagingDebris) == 0 || len(volDests) == 0 {
		return
	}
	for _, pat := range p.StagingDebris {
		if strings.TrimSpace(pat.Glob) == "" {
			continue
		}
		age := pat.MinAge
		if age <= 0 {
			age = time.Hour
		}
		found, err := dockercli.FindStaleDirsBounded(ctx, cli, containerID, volDests, []string{pat.Glob}, age)
		if err != nil {
			e.logf(logID, "INFO", "Interrupted-write check skipped (%v) — the data is captured either way", err)
			continue
		}
		if len(found) == 0 {
			continue
		}
		man.StaleStaging = append(man.StaleStaging, staleStagingOf(found, pat)...)
		for _, line := range stagingDebrisFindings(found, pat) {
			e.logf(logID, "WARN", "%s", line)
		}
	}
	sort.Slice(man.StaleStaging, func(i, j int) bool { return man.StaleStaging[i].Path < man.StaleStaging[j].Path })
}

// staleStagingOf turns what the scan measured into what the archive records.
func staleStagingOf(found []dockercli.StaleDir, pat StagingDebrisPattern) []StaleStagingDir {
	out := make([]StaleStagingDir, 0, len(found))
	for _, d := range found {
		// The noun is stored in the form the archive-level message will read it
		// with, so a later reader does not have to re-derive a plural.
		rec := StaleStagingDir{Path: d.Path, Bytes: d.Bytes, What: pat.noun(len(found))}
		if !d.ModTime.IsZero() {
			rec.ModifiedAt = d.ModTime.UTC().Format(time.RFC3339)
		}
		out = append(out, rec)
	}
	return out
}

// stagingDebrisFindings renders the warning: what was found, what it costs every
// backup, and how to remove it safely.
//
// One line, not one per directory. Two of these is a curiosity and forty is a
// pattern, and a warning that scrolls is a warning nobody finishes reading.
func stagingDebrisFindings(found []dockercli.StaleDir, pat StagingDebrisPattern) []string {
	if len(found) == 0 {
		return nil
	}
	var total int64
	oldest := time.Time{}
	paths := make([]string, 0, len(found))
	for _, d := range found {
		total += d.Bytes
		if !d.ModTime.IsZero() && (oldest.IsZero() || d.ModTime.Before(oldest)) {
			oldest = d.ModTime
		}
		paths = append(paths, d.Path)
	}
	sort.Strings(paths)

	msg := fmt.Sprintf("%d %s captured in this backup", len(found), pat.noun(len(found)))
	if total > 0 {
		msg += fmt.Sprintf(", costing %s in every backup that carries %s", humanBytes(total), plural(len(found), "it", "them"))
	}
	if !oldest.IsZero() {
		msg += fmt.Sprintf(" (the oldest dates from %s)", oldest.UTC().Format("2 January 2006"))
	}
	msg += ": " + strings.Join(namedPaths(paths), ", ") + ". "
	if pat.Cleanup != "" {
		msg += pat.Cleanup
	} else {
		msg += "DockBack never deletes anything it finds — removing them is yours to do, and only after a backup you have verified."
	}
	return []string{msg}
}

// maxNamedDebrisPaths bounds how many paths the warning lists before summarising.
const maxNamedDebrisPaths = 4

// namedPaths lists up to a handful of paths and counts the rest.
func namedPaths(paths []string) []string {
	if len(paths) <= maxNamedDebrisPaths {
		return paths
	}
	out := append([]string{}, paths[:maxNamedDebrisPaths]...)
	return append(out, fmt.Sprintf("and %d more", len(paths)-maxNamedDebrisPaths))
}

// StaleStagingFindings is the exported view for the API: what a backup's archive
// carries in the way of interrupted-write leftovers. Empty for the overwhelming
// majority of backups.
func StaleStagingFindings(man *Manifest) []string {
	if man == nil || len(man.StaleStaging) == 0 {
		return nil
	}
	var total int64
	what := ""
	paths := make([]string, 0, len(man.StaleStaging))
	for _, d := range man.StaleStaging {
		total += d.Bytes
		paths = append(paths, d.Path)
		if what == "" {
			what = d.What
		}
	}
	sort.Strings(paths)
	if what == "" {
		what = "leftovers from interrupted writes"
	}
	msg := fmt.Sprintf("this backup carries %d %s", len(man.StaleStaging), what)
	if total > 0 {
		msg += fmt.Sprintf(" (%s)", humanBytes(total))
	}
	return []string{msg + " — " + strings.Join(namedPaths(paths), ", ") +
		". They are inert and the backup is sound; removing them from the live server, after a verified backup, makes every future backup smaller"}
}
