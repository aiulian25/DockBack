package backup

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Incremental volume backups (F61). An opt-in per container: after a FULL
// baseline, each run captures only the entries that changed against the parent's
// stored inventory, plus a deletion list — packaged as `volumes-delta.tar`
// alongside a complete post-backup `volumes-index.json.zst`. Every Nth backup
// forces a fresh full so chains stay short and bounded. Restore/verify/drills/
// retention/the recover tool are all chain-aware. Databases are never incremental
// (they're dumped in full — already the compact representation). This file holds
// the PURE, Docker-free core so the diff/chain/retention logic is unit-tested.

// FileEntry is one entry's identity in a volume index: its volume-relative path
// plus what changedSinceIndex compares to decide whether it changed. Field
// tags are short to keep the compressed index small on large volumes.
type FileEntry struct {
	Path      string `json:"p"`
	Size      int64  `json:"s"`
	MtimeUnix int64  `json:"m"`
	Mode      uint32 `json:"o"`
	CtimeUnix int64  `json:"c,omitempty"`
	UID       uint32 `json:"u,omitempty"`
	GID       uint32 `json:"g,omitempty"`
	// PathB64 carries a Path that is not UTF-8, which JSON would otherwise alter.
	PathB64 string `json:"pb,omitempty"`

	// Volatile marks a file the application rewrites by itself (#41/#19), so a
	// later content comparison can report it apart from real differences instead
	// of letting it drown the result.
	//
	// R5 §9.3 is the case: six 13-byte `.immich` markers, rewritten by a clone on
	// first boot, made 58 GB of identical photographs compare as different. And
	// because the marker is a FIXED WIDTH, a path+size index cannot see the change
	// at all — which is why the classification is recorded here, at capture,
	// rather than left to whatever compares later.
	Volatile bool `json:"v,omitempty"`

	// MD5 is the file's CONTENT hash, taken from the archive stream at capture
	// (#41).
	//
	// Path, size and mtime above are a manifest, and R5 §9.3 proved a manifest is
	// not proof: Immich's `.immich` marker is a FIXED 13 bytes, so a clone that
	// rewrote all six produced an identical path+size listing and a different
	// tree. "The check that passed perfectly is the one that could not see the
	// change."
	//
	// Computed from the bytes already flowing into the spool — the archive is
	// read in full either way, so this costs a hash and not a second walk.
	//
	// Empty on an index built before this existed, and on entries the archive
	// does not carry; a comparison degrades to size and mtime there rather than
	// claiming a proof it does not have.
	MD5 string `json:"h,omitempty"`
}

// VolIndex is the complete file listing of a backup's captured volume payload,
// stored (zstd-compressed) inside the archive as `volumes-index.json.zst`. The
// NEXT incremental backup diffs its freshly-built index against this one.
type VolIndex struct {
	// Version is volIndexVersion, or 0 on an index that predates versions.
	Version int `json:"version,omitempty"`
	// WalkedAt is the node's clock when the walk began.
	WalkedAt int64 `json:"walked_at,omitempty"`
	// Entries is the regular files.
	Entries []FileEntry `json:"entries"`
	// Others is every other entry: directories, links, devices, FIFOs.
	Others []FileEntry `json:"others,omitempty"`
}

// IncrementalFullEveryDefault forces a fresh full baseline every N backups so a
// chain never grows unbounded; clamped to [IncrementalFullEveryMin,Max].
const (
	IncrementalFullEveryDefault = 7
	IncrementalFullEveryMin     = 2
	IncrementalFullEveryMax     = 30
)

// ClampFullEvery clamps a "full every N" setting into the supported range,
// falling back to the default for a zero/unset value. Shared by the API
// coercion and the engine so both agree.
func ClampFullEvery(n int) int {
	if n == 0 {
		return IncrementalFullEveryDefault
	}
	if n < IncrementalFullEveryMin {
		return IncrementalFullEveryMin
	}
	if n > IncrementalFullEveryMax {
		return IncrementalFullEveryMax
	}
	return n
}

// volIndexVersion is the inventory a delta can be built on: every entry, its owner
// and its ctime. An index without it cannot say what a delta would need to, so the
// next run takes a true full rather than guess.
const volIndexVersion = 2

// Stat's raw mode bits for the entry types an inventory tells apart.
const (
	modeTypeMask = 0o170000
	modeDir      = 0o040000
	modeSocket   = 0o140000
	modeRegular  = 0o100000
)

// inventoryCmd lists every entry under dests, leaving the excluded sub-paths out.
//
// A name can hold anything but a NUL, and neither stat nor BusyBox's find can frame
// on one, so each record starts with marker, a random string no name will contain.
// The first line is the node's clock, and the last is marker-END, printed only when
// find and stat finished without a word on stderr: a listing that missed an entry
// would read as that entry deleted. The destinations travel as literal argv, never
// through the shell. Pure.
func inventoryCmd(dests, excludes []string, marker string) []string {
	script := `date +%s; "$@" 2>/tmp/dockback-walk.err && [ ! -s /tmp/dockback-walk.err ] && echo ` + marker + walkEndSuffix
	cmd := []string{"/bin/sh", "-c", script, "sh", "find"}
	for _, dest := range dests {
		if dest != "" {
			cmd = append(cmd, dest)
		}
	}
	cmd = append(cmd, pruneExpression(excludes)...)
	return append(cmd, "-exec", "stat", "-c", marker+walkStatFormat, "{}", "+")
}

// parseInventory reads inventoryCmd's output into entries named as the archive
// names them, without the leading slash. Any record it cannot read, or a missing
// end, refuses the whole listing: one that is not complete must not be used. Pure.
func parseInventory(out []byte, marker string) (map[string]walkEntry, int64, error) {
	body, complete := bytes.CutSuffix(out, []byte(marker+walkEndSuffix+"\n"))
	if !complete {
		return nil, 0, errWalkIncomplete
	}
	chunks := bytes.Split(body, []byte(marker))
	walkedAt, err := strconv.ParseInt(strings.TrimSpace(string(chunks[0])), 10, 64)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: unreadable clock", errWalkIncomplete)
	}
	walk := make(map[string]walkEntry, len(chunks)-1)
	for _, chunk := range chunks[1:] {
		path, entry, ok := parseInventoryRecord(chunk)
		if !ok {
			return nil, 0, fmt.Errorf("%w: unreadable record", errWalkIncomplete)
		}
		walk[path] = entry
	}
	return walk, walkedAt, nil
}

// parseInventoryRecord reads one walkStatFormat record, whose name is last and may
// hold anything, including the separator and newlines. Pure.
func parseInventoryRecord(record []byte) (string, walkEntry, bool) {
	fields := strings.SplitN(strings.TrimPrefix(strings.TrimSuffix(string(record), "\n"), "|"), "|", walkFieldCount)
	if len(fields) != walkFieldCount || !strings.HasPrefix(fields[walkFieldCount-1], "/") {
		return "", walkEntry{}, false
	}
	size, sizeErr := strconv.ParseInt(fields[0], 10, 64)
	mtime, mtimeErr := strconv.ParseInt(fields[1], 10, 64)
	mode, modeErr := strconv.ParseUint(fields[2], 16, 32)
	ctime, ctimeErr := strconv.ParseInt(fields[3], 10, 64)
	uid, uidErr := strconv.ParseUint(fields[4], 10, 32)
	gid, gidErr := strconv.ParseUint(fields[5], 10, 32)
	if errors.Join(sizeErr, mtimeErr, modeErr, ctimeErr, uidErr, gidErr) != nil {
		return "", walkEntry{}, false
	}
	entry := walkEntry{size: size, mtime: mtime, ctime: ctime, mode: uint32(mode), uid: uint32(uid), gid: uint32(gid)}
	return strings.TrimPrefix(fields[walkFieldCount-1], "/"), entry, true
}

// indexFromWalk is the stored index of a walk: files in Entries, which everything
// that reads a file list expects, and directories, links and the like in Others.
// Sockets are left out, since tar cannot archive them. Pure.
func indexFromWalk(walk map[string]walkEntry, walkedAt int64) VolIndex {
	idx := VolIndex{Version: volIndexVersion, WalkedAt: walkedAt}
	for path, entry := range walk {
		if entry.isSocket() || path == "" {
			continue
		}
		fe := FileEntry{Path: path, Size: entry.size, MtimeUnix: entry.mtime, Mode: entry.mode, CtimeUnix: entry.ctime, UID: entry.uid, GID: entry.gid}
		if entry.isRegular() {
			idx.Entries = append(idx.Entries, fe)
		} else {
			idx.Others = append(idx.Others, fe)
		}
	}
	sort.Slice(idx.Entries, func(i, j int) bool { return idx.Entries[i].Path < idx.Entries[j].Path })
	sort.Slice(idx.Others, func(i, j int) bool { return idx.Others[i].Path < idx.Others[j].Path })
	return idx
}

// encodeOddPaths gives a name JSON would alter a form that survives it.
func encodeOddPaths(entries []FileEntry) {
	for i := range entries {
		if !utf8.ValidString(entries[i].Path) {
			entries[i].PathB64 = base64.StdEncoding.EncodeToString([]byte(entries[i].Path))
		}
	}
}

// decodeOddPaths undoes encodeOddPaths. A value that will not decode leaves the
// path as stored, which at worst reads as a change and copies the file again.
func decodeOddPaths(entries []FileEntry) {
	for i := range entries {
		if entries[i].PathB64 == "" {
			continue
		}
		if raw, err := base64.StdEncoding.DecodeString(entries[i].PathB64); err == nil {
			entries[i].Path = string(raw)
		}
	}
}

// allEntries is every entry an index records, files or not.
func (idx VolIndex) allEntries() []FileEntry {
	return append(append(make([]FileEntry, 0, len(idx.Entries)+len(idx.Others)), idx.Entries...), idx.Others...)
}

// changedSinceIndex is the content-change policy: anything recorded differs. ctime
// is what catches a write under a preserved mtime, a chmod, a chown and a replaced
// link, because nothing but the kernel sets it. It counts seconds, so an entry whose
// ctime reached the parent's own walk could have changed again within that second
// without showing, and is copied again: the same race git's index has.
func changedSinceIndex(prev, cur FileEntry, prevWalkedAt int64) bool {
	return prev.Size != cur.Size || prev.MtimeUnix != cur.MtimeUnix || prev.Mode != cur.Mode ||
		prev.UID != cur.UID || prev.GID != cur.GID || prev.CtimeUnix != cur.CtimeUnix ||
		prev.CtimeUnix >= prevWalkedAt
}

// DiffIndex computes the incremental delta between a parent index and the current
// one: `changed` = entries that are new or differ from the parent (these go into
// `volumes-delta.tar`); `deleted` = entries in the parent that are gone (removed
// before the delta is applied). Both sorted for determinism. Pure.
func DiffIndex(prev, cur VolIndex) (changed, deleted []string) {
	prevByPath := make(map[string]FileEntry, len(prev.Entries)+len(prev.Others))
	for _, e := range prev.allEntries() {
		prevByPath[e.Path] = e
	}
	curByPath := make(map[string]struct{}, len(cur.Entries)+len(cur.Others))
	for _, e := range cur.allEntries() {
		curByPath[e.Path] = struct{}{}
		p, ok := prevByPath[e.Path]
		if !ok || changedSinceIndex(p, e, prev.WalkedAt) {
			changed = append(changed, e.Path)
		}
	}
	for path := range prevByPath {
		if _, ok := curByPath[path]; !ok {
			deleted = append(deleted, path)
		}
	}
	sort.Strings(changed)
	sort.Strings(deleted)
	return changed, deleted
}

// replacedEntries lists the paths that exist in both indexes as different types of
// entry. Extracting over those fails or merges, so they go before the delta does.
// Pure.
func replacedEntries(prev, cur VolIndex) []string {
	prevMode := make(map[string]uint32, len(prev.Entries)+len(prev.Others))
	for _, e := range prev.allEntries() {
		prevMode[e.Path] = e.Mode & modeTypeMask
	}
	var replaced []string
	for _, e := range cur.allEntries() {
		if mode, ok := prevMode[e.Path]; ok && mode != e.Mode&modeTypeMask {
			replaced = append(replaced, e.Path)
		}
	}
	sort.Strings(replaced)
	return replaced
}

// ContentChangedFiles is the part of a delta the ransomware tripwire judges: files
// that are new or whose size or mtime moved. An application that runs chown -R at
// every start touches every ctime, which is worth copying and is no attack. Pure.
func ContentChangedFiles(prev, cur VolIndex) []string {
	prevByPath := make(map[string]FileEntry, len(prev.Entries))
	for _, e := range prev.Entries {
		prevByPath[e.Path] = e
	}
	var changed []string
	for _, e := range cur.Entries {
		if p, ok := prevByPath[e.Path]; !ok || p.Size != e.Size || p.MtimeUnix != e.MtimeUnix {
			changed = append(changed, e.Path)
		}
	}
	return changed
}

// DeletedFiles is the files among a delta's deleted entries, for the same reason.
func DeletedFiles(prev VolIndex, deleted []string) []string {
	gone := make(map[string]bool, len(deleted))
	for _, path := range deleted {
		gone[path] = true
	}
	var files []string
	for _, e := range prev.Entries {
		if gone[e.Path] {
			files = append(files, e.Path)
		}
	}
	return files
}

// deltaCannotCarry names why a delta could not faithfully hold these paths, or says
// nothing when it can. The list handed to tar -T is split on newlines, and the
// manifest's deleted list is JSON, which would alter a name that is not UTF-8. A
// full carries any name, so it stands in. Pure.
func deltaCannotCarry(changed, removed []string) string {
	for _, path := range changed {
		if strings.Contains(path, "\n") {
			return "a changed name holds a newline"
		}
	}
	for _, path := range removed {
		if strings.Contains(path, "\n") || !utf8.ValidString(path) {
			return "a removed name cannot be recorded exactly"
		}
	}
	return ""
}

// resolveChain walks parent pointers from `target` up to the FULL baseline,
// returning the manifests ordered oldest(full)→newest(target) so a restore can
// apply the full first and each delta in sequence. It fails closed on any broken
// or tampered link: a missing manifest, a cycle, a delta whose recorded
// ParentCipherSHA256 doesn't match the parent's actual CipherSHA256 (the
// cryptographic chain pin), or a chain that doesn't bottom out in a full. Pure —
// driven off an id→manifest map so it's unit-testable without a store.
func resolveChain(target string, byID map[string]*Manifest) ([]*Manifest, error) {
	var chain []*Manifest
	seen := map[string]bool{}
	cur := target
	for cur != "" {
		if seen[cur] {
			return nil, fmt.Errorf("backup chain has a cycle at %s", short(cur))
		}
		seen[cur] = true
		m := byID[cur]
		if m == nil {
			return nil, fmt.Errorf("backup chain is broken: %s is missing or unverified", short(cur))
		}
		chain = append(chain, m)
		if m.Parent == "" {
			break // reached the full baseline
		}
		parent := byID[m.Parent]
		if parent == nil {
			return nil, fmt.Errorf("backup chain is broken: parent %s of %s is missing or unverified", short(m.Parent), short(cur))
		}
		if m.ParentCipherSHA256 != "" && parent.CipherSHA256 != "" && m.ParentCipherSHA256 != parent.CipherSHA256 {
			return nil, fmt.Errorf("backup chain integrity check failed: parent %s does not match the pin recorded in %s", short(m.Parent), short(cur))
		}
		cur = m.Parent
	}
	// Reverse to oldest→newest.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	if len(chain) == 0 || chain[0].Incremental || chain[0].Parent != "" {
		return nil, fmt.Errorf("backup chain has no full baseline")
	}
	return chain, nil
}

// sanitizeVolPaths filters a deletion list down to benign volume-relative paths
// before they reach a read-write sidecar `rm` (F61, defense in depth). It drops
// anything empty, absolute, or containing a ".." segment so a tampered manifest
// can never delete outside the restored volumes — even though these paths are
// produced by our own DiffIndex over indexes we built. Pure.
func sanitizeVolPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if strings.TrimSpace(p) == "" || strings.HasPrefix(p, "/") {
			continue
		}
		bad := false
		for _, seg := range strings.Split(p, "/") {
			if seg == ".." {
				bad = true
				break
			}
		}
		if !bad {
			out = append(out, p)
		}
	}
	return out
}

// ChainDescendants returns every backup id that transitively depends on `id`
// as a chain ancestor (children, grandchildren, …) — the set a manual delete
// would orphan (F63). parentOf maps child→parent (ChainParentMap /
// Store.BackupParentMap). Sorted for determinism. Pure.
func ChainDescendants(id string, parentOf map[string]string) []string {
	children := map[string][]string{}
	for c, p := range parentOf {
		children[p] = append(children[p], c)
	}
	var out []string
	stack := []string{id}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, c := range children[cur] {
			out = append(out, c)
			stack = append(stack, c)
		}
	}
	sort.Strings(out)
	return out
}

// protectedAncestors returns the set of backup ids that must be RETAINED because a
// backup being kept depends on them as a chain ancestor — pruning a parent would
// orphan its delta and make the child unrestorable. It walks each kept backup's
// parent pointers to the root. Pure, so retention can unit-test that it never
// orphans a delta over random chains.
func protectedAncestors(keepIDs []string, parentOf map[string]string) map[string]bool {
	prot := map[string]bool{}
	for _, id := range keepIDs {
		cur := parentOf[id]
		for cur != "" && !prot[cur] {
			prot[cur] = true
			cur = parentOf[cur]
		}
	}
	return prot
}

// ---- Ransomware tripwire (F69): mass-change detection on incremental deltas ----

// DeltaStats summarizes one incremental delta for mass-change detection (F69).
// Indexed is the PARENT index size — the files that existed before this run —
// so the percentages measure how much of the known volume changed underneath us.
type DeltaStats struct {
	Indexed     int    // files in the parent index
	Changed     int    // files added or modified in this delta
	Deleted     int    // files that vanished since the parent
	NewExtTop   string // most common previously-unseen extension among changed files ("" = none)
	NewExtCount int    // how many changed files carry that extension
}

// deltaExt returns a path's lower-cased extension without the dot ("" if none).
// Pure string work — volume index paths always use '/'.
func deltaExt(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		p = p[i+1:]
	}
	i := strings.LastIndexByte(p, '.')
	if i <= 0 || i == len(p)-1 { // no dot, dotfile (".env"), or trailing dot
		return ""
	}
	return strings.ToLower(p[i+1:])
}

// AnalyzeDelta computes DeltaStats from a parent/current index pair plus the
// exact changed/deleted lists DiffIndex produced. Pure. The extension histogram
// counts changed files whose extension NEVER appeared in the parent index — the
// classic ransomware signature of every file gaining ".locked". Ties break
// lexicographically so the result is deterministic.
func AnalyzeDelta(prev, cur VolIndex, changed, deleted []string) DeltaStats {
	st := DeltaStats{Indexed: len(prev.Entries), Changed: len(changed), Deleted: len(deleted)}
	prevExts := map[string]bool{}
	for _, e := range prev.Entries {
		if x := deltaExt(e.Path); x != "" {
			prevExts[x] = true
		}
	}
	hist := map[string]int{}
	for _, p := range changed {
		if x := deltaExt(p); x != "" && !prevExts[x] {
			hist[x]++
		}
	}
	for x, n := range hist {
		if n > st.NewExtCount || (n == st.NewExtCount && x < st.NewExtTop) {
			st.NewExtTop, st.NewExtCount = x, n
		}
	}
	return st
}

// SuspectDelta applies the tripwire thresholds (F69) and returns whether this
// delta looks like a mass-change event plus the human-readable reason. minFiles
// gates BOTH rules so a small volume can't trip on a handful of files; an empty
// parent index never trips (nothing existed to be mass-encrypted). Extension
// churn toward one previously-unseen suffix strengthens the reason text.
func SuspectDelta(st DeltaStats, minFiles, changedPct, deletedPct int) (bool, string) {
	if st.Indexed <= 0 {
		return false, ""
	}
	extNote := ""
	// A quarter or more of the changed files sharing ONE new extension is the
	// smoking gun worth naming explicitly.
	if st.NewExtCount > 0 && st.NewExtCount*4 >= st.Changed {
		extNote = fmt.Sprintf("; %d of them now share the previously-unseen extension %q", st.NewExtCount, "."+st.NewExtTop)
	}
	if st.Changed >= minFiles && st.Changed*100 > st.Indexed*changedPct {
		return true, fmt.Sprintf("%d of %d indexed files (%d%%) changed in a single run%s",
			st.Changed, st.Indexed, st.Changed*100/st.Indexed, extNote)
	}
	if st.Deleted >= minFiles && st.Deleted*100 > st.Indexed*deletedPct {
		return true, fmt.Sprintf("%d of %d indexed files (%d%%) were deleted in a single run%s",
			st.Deleted, st.Indexed, st.Deleted*100/st.Indexed, extNote)
	}
	return false, ""
}

// Classifying files an application rewrites by itself (#41/#19).

// genericVolatileFiles are names that mean the same thing in every application:
// a lock held by a running process and a file holding its pid. Both are rewritten
// on every start and neither has ever restored anything.
//
// Deliberately short. R5's takeaway also lists `.nomedia`, which is NOT here: it
// is a marker a USER places and an application reads, so its content does not
// drift — and a wrong volatile label silently unguards real data. Index and cache
// directories, also named there, are already handled by Regenerable (F132) and
// NeverBackup (F138).
var genericVolatileFiles = []string{"*.lock", "*.pid"}

// volatileFileGlobs is the set that applies to one image: the generics plus
// whatever its profile declares.
func volatileFileGlobs(image string) []string {
	globs := append([]string(nil), genericVolatileFiles...)
	if profile := ProfileFor(image); profile != nil {
		globs = append(globs, profile.VolatileFiles...)
	}
	return globs
}

// matchesVolatileFile reports whether an archive-relative path is one of them.
//
// A pattern without a "/" matches the file's NAME anywhere in the tree, because
// that is how these actually appear: Immich writes a `.immich` into every media
// folder, not into one known place. A pattern with a "/" is matched against the
// whole path, for the case where the location is the point.
func matchesVolatileFile(rel string, globs []string) bool {
	if rel == "" {
		return false
	}
	base := path.Base(rel)
	for _, glob := range globs {
		if glob == "" {
			continue
		}
		if strings.Contains(glob, "/") {
			if ok, err := path.Match(glob, rel); err == nil && ok {
				return true
			}
			continue
		}
		if ok, err := path.Match(glob, base); err == nil && ok {
			return true
		}
	}
	return false
}

// markVolatileFiles stamps the index entries an application rewrites itself, and
// returns how many were marked.
func markVolatileFiles(idx *VolIndex, globs []string) int {
	if idx == nil || len(globs) == 0 {
		return 0
	}
	marked := 0
	for i := range idx.Entries {
		if matchesVolatileFile(idx.Entries[i].Path, globs) {
			idx.Entries[i].Volatile = true
			marked++
		}
	}
	return marked
}

// Verifying restored FILES the way the database contract verifies rows (#41/#26).

// maxIndexedHashes bounds how many per-file hashes one capture records. Far above
// any real tree — R5's Immich library is 34,288 files — and only there to stop a
// pathological one from putting a hash for every inode into an archive.
const maxIndexedHashes = 250000

// applyIndexHashes stamps content hashes onto the index entries they belong to,
// and returns how many entries got one.
//
// Keyed by archive-relative path, which is what both sides produce: the index
// strips the leading slash and a tar member never has one.
func applyIndexHashes(idx *VolIndex, hashes map[string]string) int {
	if idx == nil || len(hashes) == 0 {
		return 0
	}
	stamped := 0
	for i := range idx.Entries {
		if sum, ok := hashes[idx.Entries[i].Path]; ok && sum != "" {
			idx.Entries[i].MD5 = sum
			stamped++
		}
	}
	return stamped
}

// FileVerdict is the outcome of checking restored files against the index,
// split the same way HashSplit splits tables.
type FileVerdict struct {
	Phase string `json:"phase"`
	At    string `json:"at"`
	// DurableTotal is how many files carried a hash to compare, excluding the
	// ones the application declares it rewrites itself.
	DurableTotal     int `json:"durable_total"`
	DurableIdentical int `json:"durable_identical"`
	// DurableDifferent and DurableMissing are the failures, named individually —
	// R5 §9.3's lesson is to "report the specific items", because one 13-byte
	// marker among 34,288 files is not something a count can point at.
	DurableDifferent []string `json:"durable_different,omitempty"`
	DurableMissing   []string `json:"durable_missing,omitempty"`
	// VolatileDifferent are declared-volatile files that differ — expected.
	VolatileDifferent []string `json:"volatile_different,omitempty"`
	// Unhashed counts entries with no recorded hash, so a partial index reports
	// what it could not check instead of implying it checked everything.
	Unhashed int `json:"unhashed,omitempty"`
}

// Clean reports whether every durable file came back byte-for-byte.
func (v FileVerdict) Clean() bool {
	return len(v.DurableDifferent) == 0 && len(v.DurableMissing) == 0
}

// Headline is the same wording bar the table split uses.
func (v FileVerdict) Headline() string {
	line := fmt.Sprintf("%d/%d durable identical", v.DurableIdentical, v.DurableTotal)
	if len(v.VolatileDifferent) > 0 {
		line += fmt.Sprintf("; %d volatile differ (expected)", len(v.VolatileDifferent))
	}
	return line
}

// CompareFileHashes checks what is on disk against what the index recorded.
//
// Entries without a recorded hash are counted as unchecked, never as passing: an
// index built before content hashing existed must not read as a proof it never
// made.
func CompareFileHashes(idx VolIndex, restored map[string]string) FileVerdict {
	verdict := FileVerdict{At: nowRFC3339()}
	entries := append([]FileEntry(nil), idx.Entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	for _, entry := range entries {
		if entry.MD5 == "" {
			verdict.Unhashed++
			continue
		}
		got, present := restored[entry.Path]
		switch {
		case entry.Volatile:
			if present && got != entry.MD5 {
				verdict.VolatileDifferent = append(verdict.VolatileDifferent, entry.Path)
			}
		case !present:
			verdict.DurableTotal++
			verdict.DurableMissing = append(verdict.DurableMissing, entry.Path)
		case got == entry.MD5:
			verdict.DurableTotal++
			verdict.DurableIdentical++
		default:
			verdict.DurableTotal++
			verdict.DurableDifferent = append(verdict.DurableDifferent, entry.Path)
		}
	}
	return verdict
}
