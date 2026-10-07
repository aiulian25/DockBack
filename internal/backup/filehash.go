package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// Proving restored FILES came back, before the application touches them
// (#41 proof half, #26 for files, #15's stat-verify gap).
//
// R5 §9.3: six 13-byte `.immich` markers, rewritten by a clone on first boot,
// made 58 GB of identical photographs compare as different — and because the
// marker is a FIXED WIDTH, the path+size manifest could not see the change at
// all. "The check that passed perfectly is the one that could not see the
// change." So the index carries content hashes, and this checks them back at the
// same moment the database contract is checked: after the restore has written
// everything, before anything is running that could write more.

// fileHashPrefix and ownerPrefix tag the two things the probe reports.
const (
	fileHashPrefix = "FHASH|"
	ownerPrefix    = "FOWNER|"
)

// restoredFilesScript hashes every restored file and stats each mount root.
//
// -print0 / -0 because a path may contain spaces or newlines and a broken split
// would report a real file as missing. The stat is #15's other half: ownership
// alignment writes uid:gid and nothing ever read them back, so the mount root's
// owner is reported here where it is already looking.
func restoredFilesScript(dests []string) string {
	var script strings.Builder
	for _, dest := range dests {
		quoted := "'" + shellEscape(dest) + "'"
		script.WriteString(`d=` + quoted + `; if [ -d "$d" ]; then `)
		script.WriteString(`printf '` + ownerPrefix + `%s|%s\n' "$(stat -c '%u:%g' "$d" 2>/dev/null)" "$d"; `)
		// Prefixed by the shell, not by sed: the tag itself contains a pipe, and a
		// sed delimiter that collides with its own replacement is a silent no-op
		// that reports every restored file as missing.
		script.WriteString(`find "$d" -type f -print0 2>/dev/null | xargs -0 -r md5sum 2>/dev/null | ` +
			`while IFS= read -r l; do printf '` + fileHashPrefix + `%s\n' "$l"; done; fi; `)
	}
	return script.String()
}

// parseRestoredFiles reads the probe into archive-relative path → hash, plus the
// owner each mount root reports.
//
// md5sum prints "<hash>  <path>" with the path absolute; the index stores it
// relative, so the leading slash comes off here and the two line up.
func parseRestoredFiles(out string) (hashes map[string]string, owners map[string]string) {
	hashes, owners = map[string]string{}, map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if rest, ok := strings.CutPrefix(line, ownerPrefix); ok {
			if owner, dest, cut := strings.Cut(rest, "|"); cut && dest != "" && owner != "" {
				owners[dest] = owner
			}
			continue
		}
		rest, ok := strings.CutPrefix(line, fileHashPrefix)
		if !ok {
			continue
		}
		sum, path, cut := strings.Cut(rest, "  ")
		if !cut || sum == "" || path == "" {
			continue
		}
		hashes[strings.TrimPrefix(path, "/")] = sum
	}
	return hashes, owners
}

// restoreFileVerdictKey is where a restore's file verdict is kept, beside the
// table one.
func restoreFileVerdictKey(backupID string) string { return "restore.fileverdict:" + backupID }

// verifyRestoredFiles checks the restored tree against the index and says
// whether the restore should be treated as unhealthy.
//
// Runs in a sidecar, never through the container's own binaries: the thing being
// verified must not be the thing doing the verifying.
//
// Fails on a DURABLE content mismatch, which is the entire point of having
// hashes — a file that came back with different bytes is the failure the size
// manifest could not see. Volatile drift never fails: those are the files the
// application declares it rewrites itself.
func (e *Engine) verifyRestoredFiles(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions, dests []string) error {
	if man == nil || man.VolIndex == "" || len(dests) == 0 || !opts.Volumes {
		return nil
	}
	if opts.AsName != "" {
		return nil // a clone's files are its own from the moment it is created
	}
	idx, err := e.loadVolIndex(ctx, b)
	if err != nil || len(idx.Entries) == 0 {
		return nil
	}
	if !indexHasHashes(idx) {
		// An archive from before content hashing. Its size-and-mtime listing is
		// what it is; claiming a content proof from it would be the exact mistake
		// R5 §9.3 documents.
		return nil
	}

	out, perr := dockercli.CaptureSidecarRO(ctx, cli, opts.TargetID,
		[]string{"/bin/sh", "-c", restoredFilesScript(dests)})
	if perr != nil {
		e.logf(b.ID, "INFO", "Could not read the restored files back to check them (%v) — the data was restored, but its contents were not compared against the backup", perr)
		return nil
	}
	hashes, owners := parseRestoredFiles(string(out))
	e.reportRestoredOwners(b, owners)

	// A database laid down from its consistent snapshot is not the raw copy the
	// index hashed, and its stale journal files were removed on purpose; the
	// SQLite check proves those instead.
	if overlaid := sqliteOverlaidPaths(man); len(overlaid) > 0 {
		idx = withoutPaths(idx, overlaid)
		e.logf(b.ID, "INFO", "%d SQLite database(s) came back from their consistent snapshot, so they are checked as databases, not against the raw copy", len(man.SQLiteDumps))
	}

	verdict := CompareFileHashes(idx, hashes)
	verdict.Phase = HashPhasePreStart
	e.persistFileVerdict(b, verdict)

	if verdict.Unhashed > 0 {
		e.logf(b.ID, "INFO", "%d restored %s carried no recorded content hash and %s checked by size and modification time instead",
			verdict.Unhashed, plural(verdict.Unhashed, "file", "files"), plural(verdict.Unhashed, "was", "were"))
	}
	if len(verdict.VolatileDifferent) > 0 {
		e.logf(b.ID, "INFO", "%d restored %s differ that %s declares it rewrites itself: %s",
			len(verdict.VolatileDifferent), plural(len(verdict.VolatileDifferent), "file", "files"),
			b.TargetName, namedTables(verdict.VolatileDifferent))
	}

	if verdict.Clean() {
		e.logf(b.ID, "INFO", "Files verified before %s started: %s — every one came back byte-for-byte, not merely the same size",
			b.TargetName, verdict.Headline())
		return nil
	}

	// Named individually, because a count cannot point at the one file that is
	// wrong among tens of thousands that are not.
	if len(verdict.DurableMissing) > 0 {
		e.logf(b.ID, "ERROR", "%d restored %s in this backup %s not on the target: %s",
			len(verdict.DurableMissing), plural(len(verdict.DurableMissing), "file", "files"),
			plural(len(verdict.DurableMissing), "is", "are"), namedTables(verdict.DurableMissing))
	}
	if len(verdict.DurableDifferent) > 0 {
		e.logf(b.ID, "ERROR", "%d restored %s came back with DIFFERENT CONTENT than this backup holds: %s",
			len(verdict.DurableDifferent), plural(len(verdict.DurableDifferent), "file", "files"),
			namedTables(verdict.DurableDifferent))
	}
	e.logf(b.ID, "ERROR", "Files checked before %s started: %s. The container was NOT started — starting it would let the application write over data that did not come back correctly.",
		b.TargetName, verdict.Headline())
	return fmt.Errorf("%d restored file(s) do not match this backup's contents — the container was not started, see the run log for the list",
		len(verdict.DurableDifferent)+len(verdict.DurableMissing))
}

// indexHasHashes reports whether this index carries any content hash at all.
func indexHasHashes(idx VolIndex) bool {
	for i := range idx.Entries {
		if idx.Entries[i].MD5 != "" {
			return true
		}
	}
	return false
}

// reportRestoredOwners closes #15's other half: ownership alignment wrote uid:gid
// and nothing ever read them back. The probe is already standing in the restored
// tree, so it says what each mount root actually ended up owned by.
func (e *Engine) reportRestoredOwners(b *store.Backup, owners map[string]string) {
	if len(owners) == 0 {
		return
	}
	parts := make([]string, 0, len(owners))
	for _, dest := range sortedTableNames(owners) {
		parts = append(parts, dest+" "+owners[dest])
	}
	e.logf(b.ID, "INFO", "Restored mount ownership, read back from the target: %s", strings.Join(parts, ", "))
}

// persistFileVerdict keeps the file verdict beside the table one.
func (e *Engine) persistFileVerdict(b *store.Backup, verdict FileVerdict) {
	if e.Store == nil {
		return
	}
	raw, err := json.Marshal(verdict)
	if err != nil {
		return
	}
	_ = e.Store.SetSetting(restoreFileVerdictKey(b.ID), string(raw))
}

// restoredScanRoots are the mount destinations the file check walks.
//
// The captured directory mounts, and only those: a file-rooted bind (F81) is
// written to the host before the container exists and is not inside any of these
// trees, and a mount at "/" is too broad to walk usefully.
func restoredScanRoots(man *Manifest) []string {
	if man == nil {
		return nil
	}
	var roots []string
	for _, v := range man.Volumes {
		dest := normalizeMountDest(v.Destination)
		if dest == "" || dest == "/" {
			continue
		}
		if v.Kind == dockercli.MountKindFile || v.Archive != "" {
			continue
		}
		if v.NestedIn != "" {
			continue // its parent's walk already covers it
		}
		roots = append(roots, dest)
	}
	return roots
}

// sqliteOverlaidPaths lists, as the index names them, the files a restore
// replaces or removes when it lays each SQLite snapshot over its raw file: the
// database and its -wal, -shm and -journal. Pure.
func sqliteOverlaidPaths(man *Manifest) map[string]bool {
	paths := map[string]bool{}
	for _, s := range man.SQLiteDumps {
		db := strings.TrimPrefix(s.Source, "/")
		if db == "" {
			continue
		}
		for _, side := range []string{"", "-wal", "-shm", "-journal"} {
			paths[db+side] = true
		}
	}
	return paths
}

// withoutPaths is the index without the given entries. Pure.
func withoutPaths(idx VolIndex, drop map[string]bool) VolIndex {
	kept := make([]FileEntry, 0, len(idx.Entries))
	for _, entry := range idx.Entries {
		if !drop[entry.Path] {
			kept = append(kept, entry)
		}
	}
	return VolIndex{Entries: kept}
}
