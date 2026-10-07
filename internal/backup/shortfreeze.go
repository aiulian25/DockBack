package backup

import (
	"archive/tar"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
)

// A short freeze. The volumes are copied while the application keeps running;
// it is then held — paused or stopped — only long enough to copy again what
// changed during that copy. The archive that results is the one a copy taken
// entirely while held would have produced, but the hold lasts as long as the
// changes take to copy instead of as long as the whole volume takes to stream:
// seconds, where a large volume held the application still for many minutes.
//
// "Changed" is judged by each entry's change time — ctime, which nothing in
// user space can set back — and its modification time, against the node's own
// clock when the live copy began. A write, rename, link or delete moves the
// ctime of the entry or of the directory holding it, so an entry whose times
// both predate the live copy is byte for byte what that copy read.

const (
	// freezeClockMarginSeconds is how long before the live copy began a change
	// still counts as made during it: it covers whole-second timestamps and a
	// small step of the node's clock.
	freezeClockMarginSeconds = 5
	// liveCopyAttempts is how many times the live copy is tried before the
	// container is held for the whole copy instead.
	liveCopyAttempts = 2
	// frozenCopyMember is the second pass's archive while it waits to be merged;
	// it never reaches the backup itself.
	frozenCopyMember = "volumes-frozen.tar"
	// walkCompleteMarker ends a walk that listed every entry without an error.
	walkCompleteMarker = "DOCKBACK-WALK-COMPLETE"
	// walkStatFormat is one walk line: path, size, mtime, raw mode in hex, ctime.
	walkStatFormat = "%n|%s|%Y|%f|%Z"
	// walkFieldSeparators is how many separators walkStatFormat puts after the path.
	walkFieldSeparators = 4
	// stat's raw mode bits for the entry types the merge tells apart.
	modeTypeMask = 0o170000
	modeDir      = 0o040000
	modeRegular  = 0o100000
	modeSocket   = 0o140000
	// tarBlockSize is the unit every tar header and run of data is padded to.
	tarBlockSize = 512
	// tarEndBlocks is how many zero blocks end an archive.
	tarEndBlocks = 2
	// mergeCopyBufferBytes is the read size while a merge copies file data.
	mergeCopyBufferBytes = 1 << 20
)

var errWalkIncomplete = errors.New("find or stat reported an error, so the listing may be missing entries")

// walkEntry is what the walk taken while the container is held says about one
// path.
type walkEntry struct {
	size, mtime, ctime int64
	mode               uint32
}

func (w walkEntry) isDir() bool     { return w.mode&modeTypeMask == modeDir }
func (w walkEntry) isRegular() bool { return w.mode&modeTypeMask == modeRegular }
func (w walkEntry) isSocket() bool  { return w.mode&modeTypeMask == modeSocket }

// liveCopy is a two-pass copy in progress: the first pass, taken while the
// application ran, and once it is held, what changed meanwhile.
type liveCopy struct {
	since  int64             // node time the live copy began, less the clock margin
	sha    string            // the live archive's SHA-256 and per-file MD5s — still
	hashes map[string]string // the answer when nothing changed during it
	walk   map[string]walkEntry
	recopy []string // the entries copied again while held, named as the walk names them
}

// shortFreezeFits says whether this capture can take its bulk while the
// container runs. A whole-volume copy can; an incremental delta is already
// only what changed since the last backup, and keeps its hold as it is.
func (e *Engine) shortFreezeFits(opts Options, name string) bool {
	incremental, _ := e.incrementalPolicy(opts.NodeID, name)
	return !incremental || opts.ForceFull
}

// copyLive takes the first pass: the volumes copied while the application
// still runs, after reading the node's clock to judge later what changed
// meanwhile. busybox tar gives up on a file that shrinks while it reads it — a
// race a second copy rarely loses again — so a copy that fails is tried once
// more. nil means the container is to be held for the whole copy instead: the
// clock could not be read, or neither copy finished.
func (e *Engine) copyLive(ctx context.Context, cli *client.Client, containerID string, dests, excludes []string, work, id string) *liveCopy {
	now, err := e.nodeClock(ctx, cli, containerID)
	if err != nil {
		e.logf(id, "INFO", "Could not read the node's clock (%v), so the container is held for the whole copy", err)
		return nil
	}
	e.logf(id, "INFO", "Archiving %d volume path(s) via sidecar while the container keeps running — it is held afterwards only to copy again what changes meanwhile", len(dests))
	for attempt := 1; attempt <= liveCopyAttempts; attempt++ {
		sha, hashes, err := e.spoolVolumeTar(ctx, cli, id, containerID, dests, filepath.Join(work, volumesMember), excludes...)
		if err == nil {
			return &liveCopy{since: now - freezeClockMarginSeconds, sha: sha, hashes: hashes}
		}
		if ctx.Err() != nil {
			return nil
		}
		next := "a file most likely shrank as it was read, so the copy is taken once more"
		if attempt == liveCopyAttempts {
			next = "the container is held for the whole copy instead"
		}
		e.logf(id, "WARN", "The copy taken while the container ran did not finish (%v) — %s", err, next)
	}
	return nil
}

// copyFrozen takes the second pass while the container is held: a walk of
// every entry, then a copy of each one that changed since the live copy
// began. nil means there was no live copy, or the walk or the copy failed and
// the caller copies everything now instead.
func (e *Engine) copyFrozen(ctx context.Context, cli *client.Client, containerID string, dests, excludes []string, work, id string, live *liveCopy) *liveCopy {
	if live == nil {
		return nil
	}
	walk, err := walkHeld(ctx, cli, containerID, dests, excludes)
	if err != nil {
		e.logf(id, "WARN", "Could not list the files while the container was held (%v), so everything is copied again now", err)
		return nil
	}
	live.walk, live.recopy = walk, changedSince(walk, live.since)
	if len(live.recopy) == 0 {
		e.logf(id, "INFO", "Nothing changed during the live copy")
		return live
	}
	e.logf(id, "INFO", "Copying again the %d entries that changed during the live copy", len(live.recopy))
	frozenPath := filepath.Join(work, frozenCopyMember)
	if err := spoolVolumeEntries(ctx, cli, containerID, live.recopy, frozenPath); err != nil {
		_ = os.Remove(frozenPath)
		e.logf(id, "WARN", "Copying the changed entries failed (%v), so everything is copied again now", err)
		return nil
	}
	return live
}

// recordShortFreeze finishes a short freeze once the container runs again:
// the two passes become one archive, recorded the way a whole copy is.
// Nothing to do when the container was held for the whole copy.
func (e *Engine) recordShortFreeze(man *Manifest, opts Options, work, id string, live *liveCopy) error {
	if live == nil {
		return nil
	}
	sha, hashes, err := live.settle(work)
	if err != nil {
		return fmt.Errorf("volume archive: %w", err)
	}
	man.VolumesSHA256 = sha
	man.ArchiveExcluded = opts.excludeSubPaths()
	if !e.indexAlways() {
		return nil
	}
	idx := frozenIndex(live.walk, man.Image)
	e.stampIndexHashes(&idx, hashes, id)
	e.storeVolIndex(man, work, id, idx)
	return nil
}

// settle turns the two passes into the archive a copy taken entirely while
// held would have produced, returning its SHA-256 and per-file MD5s. When
// nothing changed during the live copy, that copy already is that archive.
func (lc *liveCopy) settle(work string) (string, map[string]string, error) {
	frozenPath := filepath.Join(work, frozenCopyMember)
	defer os.Remove(frozenPath)
	if len(lc.recopy) == 0 {
		return lc.sha, lc.hashes, nil
	}
	recopied := make(map[string]bool, len(lc.recopy))
	for _, path := range lc.recopy {
		recopied[path] = true
	}
	return freezeMerge(filepath.Join(work, volumesMember), frozenPath, func(name string, dir bool) bool {
		return lc.superseded(name, dir, recopied)
	})
}

// superseded says whether the live archive's entry for name must go: it was
// copied again, or it no longer exists as what the archive holds. A directory
// that still exists stays — a second entry for it, copied again, only sets its
// attributes again. Pure.
func (lc *liveCopy) superseded(name string, dir bool, recopied map[string]bool) bool {
	// ponytail: a name holding a newline cannot travel in the line-based walk,
	// so it keeps the live copy's version; a NUL-separated walk would cover it.
	if strings.Contains(name, "\n") {
		return false
	}
	entry, exists := lc.walk[name]
	if dir {
		return !exists || !entry.isDir()
	}
	return recopied[name] || !exists || entry.isDir()
}

// nodeClock reads, through a sidecar, the clock of the node the container runs
// on — the one that stamps its files' times. Reaching it also proves sidecars
// run there, which both passes depend on.
func (e *Engine) nodeClock(ctx context.Context, cli *client.Client, containerID string) (int64, error) {
	out, err := dockercli.CaptureSidecarRO(ctx, cli, containerID, []string{"date", "+%s"})
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

// walkHeld lists every entry under dests while the container is held.
func walkHeld(ctx context.Context, cli *client.Client, containerID string, dests, excludes []string) (map[string]walkEntry, error) {
	out, err := dockercli.CaptureSidecarRO(ctx, cli, containerID, frozenWalkCmd(dests, excludes))
	if err != nil {
		return nil, err
	}
	walk, complete := parseFrozenWalk(out)
	if !complete {
		return nil, errWalkIncomplete
	}
	return walk, nil
}

// frozenWalkCmd lists every entry under dests — files, directories, links —
// as walkStatFormat lines, leaving the excluded sub-paths out, and ends with
// walkCompleteMarker only when find and stat both finished without a word on
// stderr: a listing that missed an entry would read as that entry deleted. The
// destinations travel as literal argv, never through the shell. Pure.
func frozenWalkCmd(dests, excludes []string) []string {
	script := `"$@" 2>/tmp/dockback-walk.err && [ ! -s /tmp/dockback-walk.err ] && echo ` + walkCompleteMarker
	cmd := []string{"/bin/sh", "-c", script, "sh", "find"}
	for _, dest := range dests {
		if dest != "" {
			cmd = append(cmd, dest)
		}
	}
	cmd = append(cmd, pruneExpression(excludes)...)
	return append(cmd, "-exec", "stat", "-c", walkStatFormat, "{}", "+")
}

// pruneExpression is find's "( -path A -o -path B ) -prune -o" for the
// excluded sub-paths, or nothing when there are none. Pure.
func pruneExpression(excludes []string) []string {
	var alternatives []string
	for _, exclude := range excludes {
		if exclude = strings.Trim(strings.TrimSpace(exclude), "/"); exclude != "" {
			alternatives = append(alternatives, "-o", "-path", "/"+exclude)
		}
	}
	if len(alternatives) == 0 {
		return nil
	}
	expression := append([]string{"("}, alternatives[1:]...)
	return append(expression, ")", "-prune", "-o")
}

// parseFrozenWalk reads frozenWalkCmd's listing into entries named as the
// archive names them, without the leading slash. complete is false unless the
// listing ends with walkCompleteMarker. Pure.
func parseFrozenWalk(out []byte) (map[string]walkEntry, bool) {
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	last := len(lines) - 1
	if lines[last] != walkCompleteMarker {
		return nil, false
	}
	walk := make(map[string]walkEntry, last)
	for _, line := range lines[:last] {
		if path, entry, ok := parseWalkLine(line); ok {
			walk[path] = entry
		}
	}
	return walk, true
}

// parseWalkLine reads one walkStatFormat line. A line that is not one — the
// tail of a name holding a newline — is refused rather than guessed at. Pure.
func parseWalkLine(line string) (string, walkEntry, bool) {
	fields := splitLastN(line, '|', walkFieldSeparators)
	if len(fields) != walkFieldSeparators+1 || !strings.HasPrefix(fields[0], "/") {
		return "", walkEntry{}, false
	}
	size, sizeErr := strconv.ParseInt(fields[1], 10, 64)
	mtime, mtimeErr := strconv.ParseInt(fields[2], 10, 64)
	mode, modeErr := strconv.ParseUint(fields[3], 16, 32)
	ctime, ctimeErr := strconv.ParseInt(fields[4], 10, 64)
	if errors.Join(sizeErr, mtimeErr, modeErr, ctimeErr) != nil {
		return "", walkEntry{}, false
	}
	return strings.TrimPrefix(fields[0], "/"), walkEntry{size: size, mtime: mtime, ctime: ctime, mode: uint32(mode)}, true
}

// changedSince lists the entries changed at or after since, by either time —
// a filesystem that keeps no ctime still moves mtime — sorted so a directory
// comes before what it holds. Sockets are left out: tar cannot archive them.
// Pure.
func changedSince(walk map[string]walkEntry, since int64) []string {
	var changed []string
	for path, entry := range walk {
		if !entry.isSocket() && max(entry.ctime, entry.mtime) >= since {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	return changed
}

// frozenIndex is the stored file index of the held moment: every regular file
// the walk saw, recorded as buildVolIndex records one. Pure.
func frozenIndex(walk map[string]walkEntry, image string) VolIndex {
	var idx VolIndex
	for path, entry := range walk {
		if entry.isRegular() {
			idx.Entries = append(idx.Entries, FileEntry{Path: path, Size: entry.size, MtimeUnix: entry.mtime, Mode: entry.mode})
		}
	}
	sort.Slice(idx.Entries, func(i, j int) bool { return idx.Entries[i].Path < idx.Entries[j].Path })
	markVolatileFiles(&idx, volatileFileGlobs(image))
	return idx
}

// spoolVolumeEntries spools exactly the given entries — a directory as itself —
// into dst.
func spoolVolumeEntries(ctx context.Context, cli *client.Client, containerID string, entries []string, dst string) error {
	rc, err := dockercli.TarVolumeEntries(ctx, cli, containerID, entries)
	if err != nil {
		return err
	}
	_, err = spoolStream(ctx, rc, dst)
	return err
}

// freezeMerge rewrites the live archive at livePath, in place, into the one a
// copy taken entirely while held would have produced. The entries superseded
// claims are dropped and everything after them slides down over the gap — no
// second copy of a volume that can be tens of gigabytes — then the archive at
// frozenPath is appended. Kept entries travel byte for byte, headers included,
// so the result reads exactly as the sidecar's own tar does. Returns its
// SHA-256 and the MD5 of each regular file, as hashTarMembers records them.
func freezeMerge(livePath, frozenPath string, superseded func(name string, dir bool) bool) (string, map[string]string, error) {
	live, err := os.OpenFile(livePath, os.O_RDWR, 0)
	if err != nil {
		return "", nil, err
	}
	defer live.Close()
	frozen, err := os.Open(frozenPath)
	if err != nil {
		return "", nil, err
	}
	defer frozen.Close()
	rewriter := &archiveRewriter{dst: live, sha: sha256.New(), hashes: map[string]string{}, buf: make([]byte, mergeCopyBufferBytes)}
	if err := rewriter.copyEntries(live, superseded); err != nil {
		return "", nil, err
	}
	if err := rewriter.copyEntries(frozen, keepEveryEntry); err != nil {
		return "", nil, err
	}
	if err := rewriter.finish(); err != nil {
		return "", nil, err
	}
	return hex.EncodeToString(rewriter.sha.Sum(nil)), rewriter.hashes, nil
}

func keepEveryEntry(string, bool) bool { return false }

// archiveRewriter writes archive entries at off in dst, hashing every byte of
// the result. An entry already sitting at off is hashed and left where it is.
type archiveRewriter struct {
	dst     *os.File
	off     int64
	inPlace bool
	sha     hash.Hash
	hashes  map[string]string
	buf     []byte
	header  []byte
}

// zeroBlock is the padding source: whatever of a block an entry leaves unused.
var zeroBlock [tarBlockSize]byte

func (rw *archiveRewriter) Write(p []byte) (int, error) {
	if !rw.inPlace {
		if _, err := rw.dst.WriteAt(p, rw.off); err != nil {
			return 0, err
		}
	}
	rw.sha.Write(p)
	rw.off += int64(len(p))
	return len(p), nil
}

// copyEntries copies every entry of src that drop does not claim. src is read
// straight through, and in place it is never overtaken: an entry is only ever
// written at or before the offset it was read from.
func (rw *archiveRewriter) copyEntries(src *os.File, drop func(name string, dir bool) bool) error {
	counted := &offsetTrackingReader{r: src}
	tr := tar.NewReader(counted)
	dropped := map[string]bool{}
	start := int64(0)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := mergeableEntry(hdr); err != nil {
			return err
		}
		dataStart := counted.n
		if dataStart < start+tarBlockSize {
			return fmt.Errorf("archive entry %s is not where the merge expected it", hdr.Name)
		}
		size := dataSize(hdr)
		end := dataStart + blockAligned(size)
		name := memberName(hdr.Name)
		if drop(name, hdr.Typeflag == tar.TypeDir) {
			dropped[name] = true
			start = end
			continue
		}
		if hdr.Typeflag == tar.TypeLink && dropped[memberName(hdr.Linkname)] {
			return fmt.Errorf("%s is a hard link to %s, which changed during the copy", hdr.Name, hdr.Linkname)
		}
		rw.inPlace = src == rw.dst && rw.off == start
		if err := rw.copyEntry(src, tr, start, dataStart, size, hdr); err != nil {
			return err
		}
		start = end
	}
}

// copyEntry copies one entry: its header blocks as they are in src, its data
// through tr, and the zero padding after it.
func (rw *archiveRewriter) copyEntry(src *os.File, tr *tar.Reader, start, dataStart, size int64, hdr *tar.Header) error {
	headerLen := int(dataStart - start)
	if cap(rw.header) < headerLen {
		rw.header = make([]byte, headerLen)
	}
	header := rw.header[:headerLen]
	if _, err := src.ReadAt(header, start); err != nil {
		return err
	}
	if _, err := rw.Write(header); err != nil {
		return err
	}
	var out io.Writer = rw
	var fileSum hash.Hash
	if hdr.Typeflag == tar.TypeReg && len(rw.hashes) < maxIndexedHashes {
		fileSum = md5.New()
		out = io.MultiWriter(rw, fileSum)
	}
	if _, err := io.CopyBuffer(out, io.LimitReader(tr, size), rw.buf); err != nil {
		return err
	}
	if _, err := rw.Write(zeroBlock[:blockAligned(size)-size]); err != nil {
		return err
	}
	if fileSum != nil {
		rw.hashes[strings.TrimPrefix(hdr.Name, "/")] = hex.EncodeToString(fileSum.Sum(nil))
	}
	return nil
}

// finish ends the archive with its zero blocks and cuts off whatever of the
// live copy now lies beyond them.
func (rw *archiveRewriter) finish() error {
	rw.inPlace = false
	for range tarEndBlocks {
		if _, err := rw.Write(zeroBlock[:]); err != nil {
			return err
		}
	}
	return rw.dst.Truncate(rw.off)
}

// mergeableEntry refuses the entries a byte-for-byte merge cannot place: a
// sparse file, whose data in the archive is not its size, and a global header,
// whose data archive/tar has already read. The sidecar's tar writes neither.
// Pure.
func mergeableEntry(hdr *tar.Header) error {
	if hdr.Typeflag == tar.TypeXGlobalHeader {
		return fmt.Errorf("the archive holds a global header, which a short freeze cannot merge")
	}
	if isSparse(hdr) {
		return fmt.Errorf("%s is stored sparse, which a short freeze cannot merge", hdr.Name)
	}
	return nil
}

// isSparse says whether an entry is stored sparse, in either GNU form. Pure.
func isSparse(hdr *tar.Header) bool {
	if hdr.Typeflag == tar.TypeGNUSparse {
		return true
	}
	for key := range hdr.PAXRecords {
		if strings.HasPrefix(key, "GNU.sparse.") {
			return true
		}
	}
	return false
}

// dataSize is how many bytes of data follow an entry's header blocks: none for
// the types that are only a header, whatever their size field says — the rule
// archive/tar reads by. Pure.
func dataSize(hdr *tar.Header) int64 {
	switch hdr.Typeflag {
	case tar.TypeLink, tar.TypeSymlink, tar.TypeChar, tar.TypeBlock, tar.TypeDir, tar.TypeFifo:
		return 0
	}
	return hdr.Size
}

// blockAligned rounds n up to whole tar blocks. Pure.
func blockAligned(n int64) int64 {
	return (n + tarBlockSize - 1) / tarBlockSize * tarBlockSize
}

// memberName is an archive member's path as the walk names it: no leading
// "./" or "/", no trailing slash. Pure.
func memberName(name string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(name, "./"), "/"), "/")
}

// offsetTrackingReader counts the bytes read through it. It offers only Read, so
// archive/tar reads the archive straight through instead of seeking, and the
// count is the position in the file.
type offsetTrackingReader struct {
	r io.Reader
	n int64
}

func (c *offsetTrackingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
