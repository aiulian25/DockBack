package backup

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// Verify ownership by stat, never by trusting the tar (#14/#15).
//
// R2 §Issue 14 read the source's ownership out of the tar headers and found two
// different owners in one tree — `1026/100` on the directory Docker created and
// `33/33` on the one the app made itself. That is a fact about the ARCHIVE. What
// nothing checked is whether the restored tree, on disk, on this host, actually
// carries those ids afterwards.
//
// It frequently will not. tar restores ownership only when the extracting
// process may set it; a numeric owner the target has never heard of, an
// extraction that dropped privileges, a bind whose host path was created by
// Docker moments earlier — each ends with root-owned data under a manifest that
// says otherwise. The application then starts, reports healthy, and fails on its
// first write.
//
// So the rule is the one #15 states: verify by stat. F147 already does it for
// profile secrets; this does it for every recorded mount root.
//
// Never fatal, same as F117 and #34 either side of it: the data is intact and
// the fix is one chown. A restore that refused over an ownership bit would be
// withholding a recovery over something correctable in a second.

// Stat verdict tags. Only ever a destination and an owner follow.
const (
	ownerStat    = "OWN"
	ownerMissing = "OWNMISS"
)

// ownershipIntent is what the F117 alignment set out to do, carried to the sweep
// so a change DockBack made is not reported as a surprise.
//
// Declared is what separates "the alignment ran and chose these ids" from "it
// did not run at all" — the zero value has to mean the second, because an
// alignment that never happened must leave the recorded owner as the
// expectation rather than 0:0.
type ownershipIntent struct {
	UID      int
	GID      int
	Paths    []string
	Declared bool
}

// intendedFor returns the owner this restore deliberately gave a path, if it
// gave it one.
func (o ownershipIntent) intendedFor(dest string) (string, bool) {
	if !o.Declared {
		return "", false
	}
	want := normalizeMountDest(dest)
	for _, p := range o.Paths {
		if normalizeMountDest(p) == want {
			return formatOwner(o.UID, o.GID), true
		}
	}
	return "", false
}

// ownershipStatScript reads the owner of each destination root.
//
// Read-only by construction: `stat` on a directory creates nothing, changes no
// timestamp and needs no write bit anywhere. A path that is not there is said so
// explicitly rather than left out — silence and absence must not look the same,
// which is the mistake Step 16's parse also exists to avoid.
func ownershipStatScript(dests []string) string {
	if len(dests) == 0 {
		return ""
	}
	var script strings.Builder
	script.WriteString("set --")
	for _, dest := range dests {
		script.WriteString(" '" + shellEscape(dest) + "'")
	}
	script.WriteString(`; for d in "$@"; do `)
	script.WriteString(`o=$(stat -c '%u:%g' "$d" 2>/dev/null) && printf '` + ownerStat + `|%s|%s\n' "$d" "$o" `)
	script.WriteString(`|| printf '` + ownerMissing + `|%s|\n' "$d"; done`)
	return script.String()
}

// parseOwnershipStat reads the sweep's output into destination → owner.
func parseOwnershipStat(out string) (found map[string]string, missing []string) {
	found = map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), "|", 3)
		if len(fields) != 3 || fields[1] == "" {
			continue
		}
		switch fields[0] {
		case ownerStat:
			if owner := strings.TrimSpace(fields[2]); owner != "" {
				found[fields[1]] = owner
			}
		case ownerMissing:
			missing = append(missing, fields[1])
		}
	}
	sort.Strings(missing)
	return found, missing
}

// OwnershipDrift is one destination whose owner on disk is not what it should be.
type OwnershipDrift struct {
	Destination string
	// Expected is what it should carry: the ids the alignment set, when it
	// touched this path, and otherwise the owner the backup recorded.
	Expected string
	// Found is what stat actually read.
	Found string
	// Intended marks a path the alignment meant to change — so a mismatch here is
	// a chown that did not take, not an unexplained one.
	Intended bool
}

// ownershipSweepVerdict compares what is on disk against what should be there.
//
// Pure, and silent for every case it cannot judge:
//
//   - a mount with no recorded owner (a named volume, or a pre-F81 backup) has
//     nothing to be held against;
//   - a path the sweep could not stat is reported as absent by the caller, not
//     as drifted — "not there" and "wrong owner" are different problems with
//     different fixes;
//   - a path the alignment deliberately re-owned, now carrying exactly the ids
//     it chose, is the restore working.
func ownershipSweepVerdict(man *Manifest, found map[string]string, intent ownershipIntent) []OwnershipDrift {
	if man == nil {
		return nil
	}
	var drifted []OwnershipDrift
	seen := map[string]bool{}
	for _, v := range man.Volumes {
		dest := normalizeMountDest(v.Destination)
		if dest == "" || seen[dest] {
			continue
		}
		actual, stated := found[dest]
		if !stated {
			continue // not stat-ed; the caller reports absence separately
		}
		expected, intended := intent.intendedFor(dest)
		if !intended {
			expected = strings.TrimSpace(v.Owner)
		}
		if expected == "" || expected == actual {
			continue
		}
		seen[dest] = true
		drifted = append(drifted, OwnershipDrift{
			Destination: dest, Expected: expected, Found: actual, Intended: intended,
		})
	}
	sort.Slice(drifted, func(i, j int) bool { return drifted[i].Destination < drifted[j].Destination })
	return drifted
}

// statableDestinations are the mount roots worth reading, deduplicated.
//
// Every recorded destination is stat-ed, including read-only ones: a read-only
// bind whose ownership is wrong is still wrong, and the sweep changes nothing by
// looking.
func statableDestinations(man *Manifest) []string {
	if man == nil {
		return nil
	}
	var dests []string
	seen := map[string]bool{}
	for _, v := range man.Volumes {
		dest := normalizeMountDest(v.Destination)
		if dest == "" || dest == "/" || seen[dest] {
			continue
		}
		seen[dest] = true
		dests = append(dests, dest)
	}
	sort.Strings(dests)
	return dests
}

// formatOwner renders an id pair the way stat prints it.
func formatOwner(uid, gid int) string {
	return strconv.Itoa(uid) + ":" + strconv.Itoa(gid)
}

// verifyRestoredOwnership stats every restored mount root and says which ones
// did not come back as recorded.
//
// Runs as ROOT, and that is the difference from #34's write test beside it.
// A write test as root proves nothing — it succeeds whatever the ownership is,
// which is the exact failure it exists to catch — so that one is skipped when
// the application's uid is unknown. Reading metadata has no such problem: stat
// returns the same numbers whoever asks. Skipping the sweep for the same reason
// would lose it precisely on the images that bake a USER into themselves and
// declare no uid, which is R2 §14's BookStack and the case most likely to drift.
func (e *Engine) verifyRestoredOwnership(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions, intent ownershipIntent) {
	if man == nil || !opts.Volumes || opts.AsName != "" {
		return
	}
	dests := statableDestinations(man)
	if len(dests) == 0 {
		return
	}
	out, err := dockercli.CaptureSidecarRO(ctx, cli, opts.TargetID, []string{"/bin/sh", "-c", ownershipStatScript(dests)})
	if err != nil {
		e.logf(b.ID, "INFO", "Could not read the restored data's ownership (%v) — the data is restored, but its owner was not confirmed", err)
		return
	}
	found, missing := parseOwnershipStat(string(out))
	if len(missing) > 0 {
		e.logf(b.ID, "INFO", "%d recorded %s not present to read ownership from: %s",
			len(missing), plural(len(missing), "mount is", "mounts are"), namedTables(missing))
	}
	drifted := ownershipSweepVerdict(man, found, intent)
	if len(drifted) == 0 {
		if len(found) > 0 {
			e.logf(b.ID, "INFO", "Ownership verified by stat on %d restored %s — read from the filesystem, not from the archive's headers", len(found), plural(len(found), "mount", "mounts"))
		}
		e.reportForeignConsistentOwnership(b, found, opts)
		return
	}

	e.logf(b.ID, "WARN", "%d restored %s owned by somebody else than the backup recorded. The data is intact — this is ownership, and the application will fail on its first write until it is fixed:",
		len(drifted), plural(len(drifted), "mount is", "mounts are"))
	for _, drift := range drifted {
		e.logf(b.ID, "WARN", "  %s — expected %s, found %s%s. %s",
			drift.Destination, drift.Expected, drift.Found, intendedNote(drift.Intended), ownershipFixCommand(man, drift))
	}
}

// intendedNote explains a mismatch on a path the restore meant to change.
func intendedNote(intended bool) string {
	if !intended {
		return ""
	}
	return " (this restore tried to set that owner and the change did not take)"
}

// ownershipFixCommand names the exact command for this mount, host bind or named
// volume — the F117 rule that a finding says what to do about it.
func ownershipFixCommand(man *Manifest, drift OwnershipDrift) string {
	if host := hostSourceFor(man, drift.Destination); host != "" {
		return "On the host: chown -R " + drift.Expected + " " + host
	}
	return "It is a named volume; fix it from inside: docker run --rm -v <volume>:/d alpine chown -R " + drift.Expected + " /d"
}

// Everything agrees, and everything agrees on a number from somebody else's
// machine (#N8).
//
// This is the shape the EACCES incident actually had, and why nothing caught it:
// the restore landed data owned 1026:100 and reproduced PUID=1026/PGID=100
// beside it, so the sweep above found no drift and said so. Consistent, correct,
// and silent — the trap only sprang days later when the operator edited
// PUID/PGID to a local account and the linuxserver init chowned only the
// top-level mount directories, leaving `data/cache/` at 1026:100 for a process
// now running as 1000.
//
// So it is said HERE, on the clean path, at the moment somebody is looking at a
// restore log. Info, not a warning: nothing is broken, and calling a working
// restore a problem is how operators learn to skip the log.

// foreignConsistentOwners returns the distinct owners of the restored data whose
// ids belong to a numbering scheme no ordinary Linux host uses.
//
// Pure, so the rule is testable without a daemon. Sorted and deduplicated: the
// same pair on twelve mounts is one fact, not twelve.
func foreignConsistentOwners(found map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, owner := range found {
		if seen[owner] {
			continue
		}
		uid, gid, ok := parseOwnerPair(owner)
		if !ok || idConvention(uid, gid) == "" {
			continue
		}
		seen[owner] = true
		out = append(out, owner)
	}
	sort.Strings(out)
	return out
}

// parseOwnerPair reads a "uid:gid" pair as stat prints it.
func parseOwnerPair(owner string) (uid, gid int, ok bool) {
	uidPart, gidPart, found := strings.Cut(strings.TrimSpace(owner), ":")
	if !found {
		return 0, 0, false
	}
	uid, err := strconv.Atoi(uidPart)
	if err != nil {
		return 0, 0, false
	}
	gid, err = strconv.Atoi(gidPart)
	if err != nil {
		return 0, 0, false
	}
	return uid, gid, true
}

// reportForeignConsistentOwnership says, once, that a working restore is working
// on somebody else's id numbering.
//
// Silent when a pin is set: the operator has already answered this question, and
// the rewrite logs its own line.
func (e *Engine) reportForeignConsistentOwnership(b *store.Backup, found map[string]string, opts RestoreOptions) {
	if _, _, pinned := e.RestoreOwnership(opts.NodeID, b.TargetName); pinned {
		return
	}
	owners := foreignConsistentOwners(found)
	if len(owners) == 0 {
		return
	}
	uid, gid, _ := parseOwnerPair(owners[0])
	e.logf(b.ID, "INFO", "The restored data is owned by %s — %s. The container runs as the same ids, so everything works exactly as restored and there is nothing to fix today. "+
		"Worth knowing for later: if you change PUID/PGID to an account that exists on this host, only the top-level mount directories are re-owned by the image's own startup — the directories beneath them keep these ids, and the application hits `permission denied` on its first write into one. "+
		"Set this container's restore ownership on its container page before that happens, and DockBack rewrites the ids and re-owns the data together, all the way down.",
		strings.Join(owners, ", "), idConvention(uid, gid))
}
