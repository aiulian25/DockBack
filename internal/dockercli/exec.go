package dockercli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// defaultSidecarRef is the tiny helper image used for raw volume tar / du (PLAN
// §2.12). Configurable so an air-gapped or private-registry-only node can point it
// at a mirror (F25).
const defaultSidecarRef = "alpine:3.20"

// sidecarImgOverride, when set, replaces the default sidecar image at runtime,
// applied live from the in-app setting / env default (F25). Empty = the default.
var sidecarImgOverride atomic.Value // holds string

// SetSidecarImage sets (or clears, with "") the runtime volume-sidecar image ref.
// A blank value restores the compiled-in default. Mirrors SetDBReadyTimeout.
func SetSidecarImage(ref string) {
	sidecarImgOverride.Store(strings.TrimSpace(ref))
}

// sidecarRef returns the effective volume-sidecar image: the runtime override when
// set, else the default (alpine:3.20).
func sidecarRef() string {
	if v, ok := sidecarImgOverride.Load().(string); ok && v != "" {
		return v
	}
	return defaultSidecarRef
}

// SidecarImage returns the effective volume-sidecar image ref, so the API can show
// what a volume backup will actually pull (F25).
func SidecarImage() string { return sidecarRef() }

// sidecarLabelKey marks every ephemeral DockBack sidecar so leftovers can be
// found and swept on the target node, guaranteeing no accumulation even when an
// individual teardown fails (PLAN §2.12/§9.10).
const sidecarLabelKey = "com.dockback.sidecar"

func sidecarLabels() map[string]string { return map[string]string{sidecarLabelKey: "1"} }

// RemoveOrphanSidecars force-removes leftover DockBack sidecar containers that
// are no longer running (created/exited/dead) on the given node — defense in
// depth so a sidecar whose explicit removal failed (network blip, app restart
// mid-run, proxy hiccup) can't pile up (PLAN §2.12/§9.10). Running sidecars are
// deliberately left alone so a concurrent backup isn't disturbed. Returns the
// number removed.
func RemoveOrphanSidecars(ctx context.Context, c *client.Client) (int, error) {
	f := filters.NewArgs(
		filters.Arg("label", sidecarLabelKey+"=1"),
		filters.Arg("status", "created"),
		filters.Arg("status", "exited"),
		filters.Arg("status", "dead"),
	)
	list, err := c.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, ct := range list {
		if removeContainer(c, ct.ID) == nil {
			n++
		}
	}
	return n, nil
}

// ExecStream runs cmd inside container id, writing stdout to w. Used for
// database dumps (pg_dump/mysqldump/mongodump) — the dump runs inside the
// target's own namespaces and we only handle the bytes (PLAN §2.11). stderr is
// captured and surfaced on a non-zero exit.
func ExecStream(ctx context.Context, c *client.Client, id string, cmd []string, w io.Writer) error {
	return ExecStreamEnv(ctx, c, id, cmd, nil, w)
}

// execIdleTimeout bounds INACTIVITY on an exec, not its total duration.
//
// A pg_dumpall of a large cluster — and the import that puts it back — streams
// for as long as it takes, and a fixed 30-minute cap failed it after every byte
// had already been written. The biggest databases, the ones most worth backing
// up, were the ones that could never be backed up or restored, and the failure
// surfaced as a generic exec-inspect error rather than as a timeout, so it did
// not even read as one. Progress restarts the clock; genuine silence still ends
// the command. The caller-level run deadlines (a restore's 2h, a stack's 3h, a
// node rebuild's 6h) stay as the absolute bound above this.
const execIdleTimeout = 30 * time.Minute

// idleContext derives a context cancelled after `idle` with no progress
// reported. keepAlive restarts the clock, stop releases the timer and the
// context.
func idleContext(parent context.Context, idle time.Duration) (ctx context.Context, keepAlive func(), stop func()) {
	ctx, cancel := context.WithCancel(parent)
	timer := time.AfterFunc(idle, cancel)
	return ctx, func() { timer.Reset(idle) }, func() {
		timer.Stop()
		cancel()
	}
}

// keepAliveReader restarts the idle clock on every chunk that actually moves, so
// a slow-but-steady stream is never mistaken for a hung one.
type keepAliveReader struct {
	r         io.Reader
	keepAlive func()
}

func (k keepAliveReader) Read(p []byte) (int, error) {
	n, err := k.r.Read(p)
	if n > 0 {
		k.keepAlive()
	}
	return n, err
}

// ExecStreamEnv is ExecStream with extra environment for the exec'd process
// only — it is not added to the container and does not outlive the command.
//
// This exists for one reason: a secret that has to reach a CLI must not travel
// on its argv, where anything inside the container can read it out of the
// process list. Redis is the case that forced it (F180) — its password can be
// on the server's own command line, which redis then overwrites with its
// process title, so the only way to hand it to redis-cli is from outside.
func ExecStreamEnv(ctx context.Context, c *client.Client, id string, cmd, env []string, w io.Writer) error {
	ctx, keepAlive, stop := idleContext(ctx, execIdleTimeout)
	defer stop()

	resp, err := c.ContainerExecCreate(ctx, id, container.ExecOptions{
		Cmd:          cmd,
		Env:          env,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return fmt.Errorf("exec create: %w", err)
	}
	att, err := c.ContainerExecAttach(ctx, resp.ID, container.ExecStartOptions{})
	if err != nil {
		return fmt.Errorf("exec attach: %w", err)
	}
	defer att.Close()

	var stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(w, &stderr, keepAliveReader{r: att.Reader, keepAlive: keepAlive}); err != nil {
		return fmt.Errorf("exec read: %w", err)
	}
	insp, err := c.ContainerExecInspect(ctx, resp.ID)
	if err != nil {
		return fmt.Errorf("exec inspect: %w", err)
	}
	if insp.ExitCode != 0 {
		return fmt.Errorf("command %q exited %d: %s", strings.Join(cmd, " "), insp.ExitCode, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// maxExecOutputTail bounds what an exec's output can cost in memory. A database
// client prints a line per problem, so a badly-broken import is chatty and an
// unbounded buffer would hold all of it. The window is large enough that no real
// import reaches it — the classifier downstream reads this text to decide
// whether every statement applied, so trimming it aggressively would trade a
// memory bound for the risk of certifying an incomplete restore as clean.
const maxExecOutputTail = 1 << 20 // 1 MiB

// tailWriter keeps only the LAST limit bytes written to it, and says so when it
// has dropped anything, so a truncated capture can never be mistaken for the
// whole story.
type tailWriter struct {
	buf     []byte
	limit   int
	dropped bool
}

func (t *tailWriter) Write(p []byte) (int, error) {
	written := len(p)
	if len(p) > t.limit { // one write larger than the whole window
		p = p[len(p)-t.limit:]
		t.buf = t.buf[:0]
		t.dropped = true
	}
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.limit:]...)
		t.dropped = true
	}
	return written, nil
}

func (t *tailWriter) String() string {
	if !t.dropped {
		return string(t.buf)
	}
	return "…(earlier output dropped)…\n" + string(t.buf)
}

// execStream is the half-closable byte stream an exec attach hands back: stdin
// goes in through Write, the multiplexed output comes out of Output, and Close
// is the ONLY thing that unblocks a read — a hijacked connection does not watch
// a context.
type execStream struct {
	Write      io.Writer
	Output     io.Reader
	CloseWrite func() error
	Close      func()
}

// streamExecIO feeds stdin into an attached exec while draining its output
// CONCURRENTLY, and returns once the command's output ends or ctx does.
//
// The concurrency is the whole point. An exec's output pipe is small, so a
// client that says anything at all while a multi-GB dump is still being written
// fills it; with nobody reading, the stdin copy blocks forever, and because the
// hijacked connection ignores the context nothing ever times it out. That hang
// held the restore lock and the run registration until the process was
// restarted. Same shape as the sqlite overlay sidecar further down this file.
func streamExecIO(ctx context.Context, s execStream, stdin io.Reader, out io.Writer) error {
	drained := make(chan struct{})
	go func() { _, _ = stdcopy.StdCopy(out, out, s.Output); close(drained) }()

	// Closing the connection is what ends the drain; waiting for it afterwards
	// means the caller can read `out` without racing the goroutine.
	stop := func() {
		s.Close()
		<-drained
	}

	if _, err := io.Copy(s.Write, stdin); err != nil {
		stop()
		return fmt.Errorf("writing stdin: %w", err)
	}
	_ = s.CloseWrite()

	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		stop()
		return ctx.Err()
	}
}

// ExecStdin runs cmd inside container id, feeding r to the command's stdin.
// Used to re-import database dumps on restore (psql/mysql/mongorestore,
// PLAN §4.8).
func ExecStdin(ctx context.Context, c *client.Client, id string, cmd []string, r io.Reader) error {
	_, err := ExecStdinCapture(ctx, c, id, cmd, r)
	return err
}

// ExecStdinCapture is ExecStdin that also RETURNS the command's combined
// stdout+stderr. A database client's exit code alone is not a verdict — psql
// (without ON_ERROR_STOP) and mysql --force both keep going after a failed
// statement and still exit 0 — so the import path must read what the client
// actually said to know whether every statement applied.
func ExecStdinCapture(ctx context.Context, c *client.Client, id string, cmd []string, r io.Reader) (string, error) {
	// Either direction counts as progress: a dump being written in and whatever
	// the client says back are both proof the command is alive.
	ctx, keepAlive, stop := idleContext(ctx, execIdleTimeout)
	defer stop()

	resp, err := c.ContainerExecCreate(ctx, id, container.ExecOptions{
		Cmd:          cmd,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return "", fmt.Errorf("exec create: %w", err)
	}
	att, err := c.ContainerExecAttach(ctx, resp.ID, container.ExecStartOptions{})
	if err != nil {
		return "", fmt.Errorf("exec attach: %w", err)
	}
	defer att.Close()

	out := &tailWriter{limit: maxExecOutputTail}
	stream := execStream{
		Write: att.Conn, Output: keepAliveReader{r: att.Reader, keepAlive: keepAlive},
		CloseWrite: att.CloseWrite, Close: att.Close,
	}
	if err := streamExecIO(ctx, stream, keepAliveReader{r: r, keepAlive: keepAlive}, out); err != nil {
		// Whatever the client managed to say usually explains why: returning it
		// lets the caller classify a broken import instead of only reporting that
		// the pipe went away.
		return out.String(), err
	}

	insp, err := c.ContainerExecInspect(ctx, resp.ID)
	if err != nil {
		return out.String(), fmt.Errorf("exec inspect: %w", err)
	}
	if insp.ExitCode != 0 {
		return out.String(), fmt.Errorf("import exited %d: %s", insp.ExitCode, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

// ExecHook runs an application quiesce/unquiesce hook inside the container,
// optionally as a specific user and working directory (e.g. Nextcloud's
// `occ maintenance:mode` as www-data in /var/www/html — PLAN §9.5). Returns the
// combined output; a non-zero exit is an error.
func ExecHook(ctx context.Context, c *client.Client, id string, cmd []string, user, workdir string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	resp, err := c.ContainerExecCreate(ctx, id, container.ExecOptions{
		Cmd: cmd, User: user, WorkingDir: workdir, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("exec create: %w", err)
	}
	att, err := c.ContainerExecAttach(ctx, resp.ID, container.ExecStartOptions{})
	if err != nil {
		return nil, fmt.Errorf("exec attach: %w", err)
	}
	defer att.Close()
	var out bytes.Buffer
	_, _ = stdcopy.StdCopy(&out, &out, att.Reader)
	insp, err := c.ContainerExecInspect(ctx, resp.ID)
	if err != nil {
		return out.Bytes(), err
	}
	if insp.ExitCode != 0 {
		return out.Bytes(), fmt.Errorf("exit %d: %s", insp.ExitCode, strings.TrimSpace(out.String()))
	}
	return out.Bytes(), nil
}

// ExecCapture runs cmd and returns its stdout buffered in memory.
func ExecCapture(ctx context.Context, c *client.Client, id string, cmd []string) ([]byte, error) {
	var buf bytes.Buffer
	if err := ExecStream(ctx, c, id, cmd, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// safeExecName limits executable names probed by HasExecutable to a conservative
// charset (matches every real tool: pg_dumpall, mysqldump, mariadb-dump, …), so a
// future caller can't smuggle shell metacharacters into the probe (SEC-5).
var safeExecName = regexp.MustCompile(`^[a-z0-9_-]+$`)

// shellEscape neutralises single quotes for safe, lossless embedding in a '...'
// shell literal (each "'" becomes '\”). Mirrors backup.shellEscape; duplicated
// here because dockercli must not import backup (import cycle).
func shellEscape(s string) string {
	return strings.ReplaceAll(s, "'", `'\''`)
}

// HasExecutable reports whether bin exists on the container's PATH (used to
// detect pg_dump/mysqldump availability in pre-flight, PLAN §4.4).
func HasExecutable(ctx context.Context, c *client.Client, id, bin string) bool {
	if !safeExecName.MatchString(bin) {
		return false
	}
	// Pass bin as a positional parameter ($1) — the script text is a constant, so
	// bin is never re-parsed as shell syntax even if the charset guard changes.
	out, err := ExecCapture(ctx, c, id, []string{"/bin/sh", "-c", `command -v "$1" || which "$1"`, "sh", bin})
	return err == nil && len(bytes.TrimSpace(out)) > 0
}

// Mount kinds. A bind's root is one or the other, and a restore that has to
// create a missing source on a new host must know which: the daemon's own
// behaviour for an absent source is to invent a DIRECTORY, even where the
// container expects a file, which starts the application pointing at a directory
// where its secret should be.
const (
	MountKindDir  = "dir"
	MountKindFile = "file"
)

// mountStatTimeout bounds the stats-only probe. It is a `stat` per path, not a
// directory walk, so this is a generous ceiling rather than a working budget.
const mountStatTimeout = 60 * time.Second

// MountStat describes what a mount's root IS, as distinct from how big it is:
// the filesystem behind it, whether it is a file or a directory, and the
// ownership and permission bits it carries.
//
// Every field is empty when the probe could not establish it, and that is
// load-bearing rather than tidy. These values are what a cross-host restore
// creates absent bind sources from, and a guessed kind or a guessed owner is
// precisely the failure being prevented — so a value that does not parse is left
// blank and the caller is left with nothing to act on, which is recoverable.
type MountStat struct {
	FSType string // backing filesystem (zfs/btrfs/ext4/…) — capability detect
	Kind   string // MountKindDir | MountKindFile
	Owner  string // "uid:gid" of the mount root, validated against ownerRe
	Mode   string // octal permission bits, e.g. "700", validated against modeRe
}

// mountProbeScript builds the per-path probe the mount sidecar runs.
//
// withSizes adds the `du` walk, and that is the whole difference between a
// sub-second read of what these mounts ARE and a scan that takes minutes on a
// large bind. The identity fields are wanted on every backup; the sizes are not,
// so the two are separable.
//
// Paths are embedded as single-quoted shell literals with any embedded quote
// escaped (SEC-8), so a path containing a "'" is probed rather than mangled.
func mountProbeScript(paths []string, withSizes bool) string {
	var script strings.Builder
	script.WriteString("for p in")
	for _, p := range paths {
		script.WriteString(" '")
		script.WriteString(shellEscape(p))
		script.WriteString("'")
	}
	script.WriteString("; do ")
	if withSizes {
		// Size, plus a read-permission flag when du hit one underneath — a UID/GID
		// mismatch / NFS root_squash that would leave files MISSING from the backup
		// (PLAN §2.13).
		script.WriteString(`s=$(du -sb "$p" 2>/tmp/dserr | cut -f1); echo "${s:-0}|$p"; [ -s /tmp/dserr ] && echo "ERR|$p"; rm -f /tmp/dserr; `)
	}
	// Backing filesystem — best-effort, empty when stat -f is unsupported.
	script.WriteString(`t=$(stat -f -c %T "$p" 2>/dev/null); echo "FS|$p|${t:-}"; `)
	// Kind, owner and mode, emitted ONLY for a path that exists. A missing path
	// must produce no verdict at all: "not measured" and "this is a file" are
	// different answers, and defaulting the second would have a restore create the
	// wrong kind of thing.
	script.WriteString(`if [ -e "$p" ]; then k=` + MountKindDir + `; [ -d "$p" ] || k=` + MountKindFile +
		`; echo "KIND|$k|$(stat -c %u:%g "$p" 2>/dev/null)|$(stat -c %a "$p" 2>/dev/null)|$p"; fi; `)
	script.WriteString("done")
	return script.String()
}

// cutMountKindLine splits a KIND payload of "<kind>|<owner>|<mode>|<path>".
//
// The path comes LAST on purpose. The three fixed fields are cut off the front
// and whatever remains is the path, so a mount path containing '|' is read whole
// instead of being truncated at a character that is legal in a filename.
func cutMountKindLine(s string) (kind, owner, mode, path string, ok bool) {
	var rest string
	if kind, rest, ok = strings.Cut(s, "|"); !ok {
		return "", "", "", "", false
	}
	if owner, rest, ok = strings.Cut(rest, "|"); !ok {
		return "", "", "", "", false
	}
	if mode, path, ok = strings.Cut(rest, "|"); !ok || path == "" {
		return "", "", "", "", false
	}
	return kind, owner, mode, path, true
}

// parseMountProbe reads the probe's output back into sizes, the unreadable set,
// and the per-path stats.
//
// Each field is judged on its own: an owner that is not numeric uid:gid, or a
// mode that is not octal, is dropped while the rest of the line is kept. A line
// it cannot make sense of contributes nothing. Pure, so every shape is testable
// without a daemon.
func parseMountProbe(out string) (map[string]int64, []string, map[string]MountStat) {
	sizes := map[string]int64{}
	stats := map[string]MountStat{}
	var unreadable []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "ERR|"):
			unreadable = append(unreadable, line[len("ERR|"):])
		case strings.HasPrefix(line, "FS|"):
			// FS|<path>|<fstype> — fstype may be empty if stat -f is unsupported.
			rest := line[len("FS|"):]
			j := strings.LastIndexByte(rest, '|')
			if j < 0 {
				continue
			}
			if t := strings.TrimSpace(rest[j+1:]); t != "" {
				st := stats[rest[:j]]
				st.FSType = t
				stats[rest[:j]] = st
			}
		case strings.HasPrefix(line, "KIND|"):
			kind, owner, mode, path, ok := cutMountKindLine(line[len("KIND|"):])
			if !ok {
				continue
			}
			st := stats[path]
			if kind == MountKindDir || kind == MountKindFile {
				st.Kind = kind
			}
			if ownerRe.MatchString(owner) {
				st.Owner = owner
			}
			if modeRe.MatchString(mode) {
				st.Mode = mode
			}
			stats[path] = st
		default:
			i := strings.IndexByte(line, '|')
			if i < 0 {
				continue
			}
			var n int64
			fmt.Sscan(line[:i], &n)
			sizes[line[i+1:]] = n
		}
	}
	return sizes, unreadable, stats
}

// runMountProbe runs one probe sidecar over the given paths, attached
// --volumes-from target read-only. The sidecar is always removed.
func runMountProbe(ctx context.Context, c *client.Client, targetID string, paths []string, withSizes bool) (map[string]int64, []string, map[string]MountStat, error) {
	sizes := map[string]int64{}
	stats := map[string]MountStat{}
	if len(paths) == 0 {
		return sizes, nil, stats, nil
	}
	if err := ensureSidecar(ctx, c); err != nil {
		return nil, nil, nil, err
	}

	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: []string{"sh", "-c", mountProbeScript(paths, withSizes)}, AttachStdout: true, AttachStderr: true, Labels: sidecarLabels()},
		&container.HostConfig{VolumesFrom: []string{targetID + ":ro"}}, nil, nil, "")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mount probe sidecar create: %w", err)
	}
	sidecarID := created.ID
	defer removeContainer(c, sidecarID)

	att, err := c.ContainerAttach(ctx, sidecarID, container.AttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mount probe sidecar attach: %w", err)
	}
	if err := c.ContainerStart(ctx, sidecarID, container.StartOptions{}); err != nil {
		att.Close()
		return nil, nil, nil, fmt.Errorf("mount probe sidecar start: %w", err)
	}

	var stdout, stderr bytes.Buffer
	copyDone := make(chan error, 1)
	go func() { _, e := stdcopy.StdCopy(&stdout, &stderr, att.Reader); copyDone <- e }()
	select {
	case <-copyDone:
	case <-ctx.Done():
		att.Close()
		return sizes, nil, stats, ctx.Err() // partial/none; caller treats unknown as large
	}
	att.Close()

	sizes, unreadable, stats := parseMountProbe(stdout.String())
	return sizes, unreadable, stats, nil
}

// MountSizesFrom measures the on-disk size (bytes) of each path inside the
// target container's volumes using a temporary Alpine sidecar (du -sb). Paths
// that can't be measured (or if du is cut short) come back as 0. It also returns
// the paths under which the reader hit a read-permission error (du stderr) — a
// UID/GID mismatch / NFS root_squash that would leave files MISSING from the
// backup, so the caller can warn instead of failing silently (PLAN §2.13) — and
// what each mount root IS (MountStat). Used to show sizes in the backup UI and to
// auto-skip large bind mounts. The sidecar is always removed.
func MountSizesFrom(ctx context.Context, c *client.Client, targetID string, paths []string) (map[string]int64, []string, map[string]MountStat, error) {
	return runMountProbe(ctx, c, targetID, paths, true)
}

// MountStatsFrom reads what each mount root IS — file or directory, its owner
// and its mode — WITHOUT the du walk.
//
// Separate from MountSizesFrom because the cost is different by orders of
// magnitude and so is the cadence: sizes are measured when the operator opens the
// picker or when a selection has to be defaulted, whereas the identity of a bind
// root is wanted on every single backup so a later cross-host restore can
// recreate it. Charging every backup a directory scan for three `stat` fields
// would be paying minutes for milliseconds of information.
func MountStatsFrom(ctx context.Context, c *client.Client, targetID string, paths []string) (map[string]MountStat, error) {
	sctx, cancel := context.WithTimeout(ctx, mountStatTimeout)
	defer cancel()
	_, _, stats, err := runMountProbe(sctx, c, targetID, paths, false)
	return stats, err
}

// sqliteMagic is the first 16 bytes of every SQLite database file ("SQLite
// format 3\000"). Detection is by this header magic, never by filename.
const sqliteMagic = "SQLite format 3"

// maxSQLiteFiles caps how many SQLite files a single backup snapshots, so a volume
// full of them can't spawn an unbounded amount of sidecar work.
const maxSQLiteFiles = 200

// DetectSQLiteFiles returns the absolute paths of SQLite database files found under
// the given mount destinations in the target's volumes, identified by the "SQLite
// format 3" header magic (F22) — so the backup can capture them CONSISTENTLY rather
// than copying a live file mid-write. Read-only sidecar, bounded and best-effort;
// WAL/shm/journal side-files are excluded (they aren't standalone databases).
func DetectSQLiteFiles(ctx context.Context, c *client.Client, targetID string, mountDests []string) ([]string, error) {
	if len(mountDests) == 0 {
		return nil, nil
	}
	if err := ensureSidecar(ctx, c); err != nil {
		return nil, err
	}
	// For each dest: find regular files >100 bytes whose first 16 bytes are the
	// SQLite magic, printing each path. Paths are single-quoted shell literals with
	// embedded quotes escaped (SEC-8). `grep -qa` treats the header bytes as text.
	var script strings.Builder
	script.WriteString("n=0; for d in")
	for _, d := range mountDests {
		script.WriteString(" '")
		script.WriteString(shellEscape(d))
		script.WriteString("'")
	}
	script.WriteString(`; do [ -e "$d" ] || continue; find "$d" -type f -size +100c ! -name '*-wal' ! -name '*-shm' ! -name '*-journal' 2>/dev/null | while IFS= read -r f; do head -c16 "$f" 2>/dev/null | grep -qa '`)
	script.WriteString(sqliteMagic)
	script.WriteString(`' && printf '%s\n' "$f"; done; done | head -n `)
	fmt.Fprintf(&script, "%d", maxSQLiteFiles)

	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: []string{"sh", "-c", script.String()}, AttachStdout: true, AttachStderr: true, Labels: sidecarLabels()},
		&container.HostConfig{VolumesFrom: []string{targetID + ":ro"}}, nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("sqlite detect sidecar create: %w", err)
	}
	sidecarID := created.ID
	defer removeContainer(c, sidecarID)

	att, err := c.ContainerAttach(ctx, sidecarID, container.AttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		return nil, fmt.Errorf("sqlite detect attach: %w", err)
	}
	if err := c.ContainerStart(ctx, sidecarID, container.StartOptions{}); err != nil {
		att.Close()
		return nil, fmt.Errorf("sqlite detect start: %w", err)
	}
	var stdout, stderr bytes.Buffer
	copyDone := make(chan error, 1)
	go func() { _, e := stdcopy.StdCopy(&stdout, &stderr, att.Reader); copyDone <- e }()
	select {
	case <-copyDone:
	case <-ctx.Done():
		att.Close()
		return nil, ctx.Err()
	}
	att.Close()

	var out []string
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if p := strings.TrimSpace(line); p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

// maxSQLiteRowCountBytesStr caps how big a database may be before its row count
// is skipped (F109), as a shell literal.
//
// Counting rows is a full scan of every table; on a multi-GB database that would
// add minutes to each backup for a number that is only a corroborating signal.
// Above the cap the table count and checksum are still recorded, so the contract
// degrades rather than disappearing.
//
// Deliberately just under 2^31 rather than a round 2 GiB: the sidecar's shell
// may do 32-bit arithmetic, and a comparison that overflows would silently pick
// the wrong branch.
const maxSQLiteRowCountBytesStr = "2000000000"

// sqliteCountScript is the shared body that measures ONE SQLite database — used
// at capture against the snapshot and at restore against the file that landed on
// disk, so both sides count the same way and their numbers are comparable.
//
// $db must be set to the file to measure. It sets:
//
//	tb — user tables (sqlite_% excluded), or -1 when unreadable
//	rw — total rows across those tables, or -1 when unreadable or too large
//
// -1 is deliberately distinct from 0: "not counted" and "counted, and empty" call
// for opposite reactions, and conflating them would turn a genuinely empty
// database into a silent pass.
//
// The row query is generated by SQL because the table list is per-database; each
// name is double-quote-escaped so a table called `weird"name` can neither break
// nor inject into the generated query. Every value is guarded for emptiness,
// because a client that succeeds while printing nothing is a real case.
const sqliteCountScript = `tb=$(sqlite3 "$db" "select count(*) from sqlite_master where type='table' and name not like 'sqlite_%'" 2>/dev/null); ` +
	`[ -n "$tb" ] || tb=-1; ` +
	`sz=$(wc -c < "$db" 2>/dev/null | tr -dc '0-9'); [ -n "$sz" ] || sz=0; rw=-1; ` +
	`if [ "$sz" -gt 0 ] && [ "$sz" -le ` + maxSQLiteRowCountBytesStr + ` ]; then ` +
	`q=$(sqlite3 "$db" "select coalesce(group_concat('select count(*) as n from \"'||replace(name,'\"','\"\"')||'\"',' union all '),'select 0 as n') from sqlite_master where type='table' and name not like 'sqlite_%'" 2>/dev/null); ` +
	`if [ -n "$q" ]; then rw=$(sqlite3 "$db" "select coalesce(sum(n),0) from ($q)" 2>/dev/null); fi; ` +
	`fi; [ -n "$rw" ] || rw=-1; `

// maxSQLiteTablesStr caps how many tables get an individual row count (F124), as
// a shell literal. Every self-hosted app is far below it — Gotify has 5,
// Dockhand 30, BookStack 41 — and the cap only stops a pathological schema from
// putting thousands of entries into a manifest. Above it the aggregate still
// applies, so the contract degrades rather than disappearing.
const maxSQLiteTablesStr = "100"

// sqliteTableRowsScript emits one "TBLROW<TAB><table><TAB><rows>" line per user
// table of $db (F124), shared by capture and restore so both sides count
// identically.
//
// A single aggregate row count hides the failure that matters. Gotify's 115,000
// rows are almost entirely message history, so losing all 32 rows of `clients` —
// every device that receives notifications — is a 0.03% shortfall, and if any
// other table gained rows in the meantime it nets out to nothing at all. Per
// table, that same loss reads "clients came back with 0 of 32 rows".
//
// Requires $tb and $rw from sqliteCountScript, so it runs only when the database
// was readable and small enough to scan.
//
// The prefix and separators are produced by the SQL itself rather than by
// post-processing, so no assumption is made about which sed or awk the sidecar
// image ships. Table names are double-quote-escaped exactly as above.
const sqliteTableRowsScript = `if [ "$tb" -gt 0 ] && [ "$tb" -le ` + maxSQLiteTablesStr + ` ] && [ "$rw" -ge 0 ]; then ` +
	`qt=$(sqlite3 "$db" "select coalesce(group_concat('select ''TBLROW''||char(9)||'||quote(name)||'||char(9)||count(*) from \"'||replace(name,'\"','\"\"')||'\"',' union all '),'') from sqlite_master where type='table' and name not like 'sqlite_%'" 2>/dev/null); ` +
	`[ -n "$qt" ] && sqlite3 "$db" "$qt" 2>/dev/null; fi; `

// sqliteStatsScript records "<i>\t<tables>\t<rows>" into /out/stats.txt for the
// snapshot at /out/$i.dbk (F109), so the backup states what the database
// CONTAINED at capture and a restore can prove it got the same thing back.
const sqliteStatsScript = `db="/out/$i.dbk"; ` + sqliteCountScript +
	`printf '%s\t%s\t%s\n' "$i" "$tb" "$rw" >> /out/stats.txt; ` +
	// F124: per-table counts, one file per snapshot so the index needs no
	// in-band encoding.
	`{ ` + sqliteTableRowsScript + `} > "/out/rows-$i.txt" 2>/dev/null; ` +
	// #18: and this snapshot's per-table CONTENT hashes, so a restore can prove
	// the rows came back unchanged rather than merely equally numerous.
	`{ ` + sqliteTableHashScript + `} > "/out/hash-$i.txt" 2>/dev/null; `

// SnapshotSQLite produces a CONSISTENT snapshot of each SQLite file in dbPaths via
// a read-only sidecar (F22) and streams a tar of the results. Because the source
// volume is mounted read-only (and so is never modified), each database's file set
// is first copied into the sidecar's writable scratch space, then snapshotted there
// with `sqlite3 … VACUUM INTO` and validated with `PRAGMA integrity_check`; only a
// database that snapshots AND verifies clean is included. The returned tar holds
// `<n>.dbk` files, an `index.txt` mapping `<n>\t<source-path>`, (F109) a
// `stats.txt` mapping `<n>\t<tables>\t<rows>`, and (F116) a `failed.txt` mapping
// `<kind>\t<source-path>\t<detail>` for every database that did NOT produce a
// usable snapshot. If the sidecar image has no sqlite3, the tar is empty
// (index.txt only) and the caller falls back to the raw capture already in
// volumes.tar — never a torn or missing database.
func SnapshotSQLite(ctx context.Context, c *client.Client, targetID string, dbPaths []string) (io.ReadCloser, error) {
	if len(dbPaths) == 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	if len(dbPaths) > maxSQLiteFiles {
		dbPaths = dbPaths[:maxSQLiteFiles]
	}
	if err := ensureSidecar(ctx, c); err != nil {
		return nil, err
	}

	// Build the snapshot script. For each db (1-based index i): stage a copy of the
	// db + its -wal/-shm into a writable dir so sqlite3 can open a WAL database (the
	// source is read-only), VACUUM INTO a single consistent /out/<i>.dbk, verify it,
	// and record the mapping only on success. The source files are never written.
	var script strings.Builder
	script.WriteString("set -e; mkdir -p /out; i=0; ")
	script.WriteString("if ! command -v sqlite3 >/dev/null 2>&1; then tar -cf - -C /out . ; exit 0; fi; ")
	script.WriteString("for db in")
	for _, p := range dbPaths {
		script.WriteString(" '")
		script.WriteString(shellEscape(p))
		script.WriteString("'")
	}
	// Note: VACUUM INTO fails if the destination exists, so /out/<i>.dbk is always fresh.
	script.WriteString(`; do i=$((i+1)); [ -f "$db" ] || continue; st="/tmp/s$i"; mkdir -p "$st"; ` +
		`cp "$db" "$st/d" 2>/dev/null || continue; ` +
		`[ -f "$db-wal" ] && cp "$db-wal" "$st/d-wal" 2>/dev/null || true; ` +
		`[ -f "$db-shm" ] && cp "$db-shm" "$st/d-shm" 2>/dev/null || true; ` +
		// F116: the two ways this can fail are NOT the same thing, and until now
		// both were swallowed identically — the snapshot was dropped and the raw
		// file quietly shipped instead.
		//
		//   snapshot — VACUUM INTO could not run (locked, unreadable, out of
		//              space). Says nothing about the data; the raw copy stands.
		//   corrupt  — the snapshot WAS produced and PRAGMA integrity_check
		//              rejected it. That is a measured statement about the
		//              database itself, and shipping a backup of it as green is
		//              how a corrupt database is discovered a year later during
		//              a recovery.
		//
		// They are recorded separately in failed.txt so the caller can react to
		// each on its own terms.
		`if sqlite3 "$st/d" ".timeout 5000" "VACUUM INTO '/out/$i.dbk'" >/dev/null 2>&1; then ` +
		`ic=$(sqlite3 "/out/$i.dbk" 'PRAGMA integrity_check' 2>/dev/null | head -1); ` +
		`if [ "$ic" = "ok" ]; then ` +
		`printf '%s\t%s\n' "$i" "$db" >> /out/index.txt; ` + sqliteStatsScript +
		`else rm -f "/out/$i.dbk"; ` +
		`[ -n "$ic" ] || ic="integrity check produced no result"; ` +
		`printf 'corrupt\t%s\t%s\n' "$db" "$ic" >> /out/failed.txt; fi; ` +
		`else rm -f "/out/$i.dbk"; ` +
		`printf 'snapshot\t%s\t%s\n' "$db" "could not be snapshotted (locked or unreadable)" >> /out/failed.txt; fi; ` +
		`rm -rf "$st"; done; tar -cf - -C /out .`)

	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: []string{"sh", "-c", script.String()}, AttachStdout: true, AttachStderr: true, Labels: sidecarLabels()},
		&container.HostConfig{VolumesFrom: []string{targetID + ":ro"}, AutoRemove: false}, nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("sqlite snapshot sidecar create: %w", err)
	}
	sidecarID := created.ID

	att, err := c.ContainerAttach(ctx, sidecarID, container.AttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		_ = removeContainer(c, sidecarID)
		return nil, fmt.Errorf("sqlite snapshot attach: %w", err)
	}
	if err := c.ContainerStart(ctx, sidecarID, container.StartOptions{}); err != nil {
		att.Close()
		_ = removeContainer(c, sidecarID)
		return nil, fmt.Errorf("sqlite snapshot start: %w", err)
	}
	pr, pw := io.Pipe()
	go func() {
		var stderr bytes.Buffer
		_, copyErr := stdcopy.StdCopy(pw, &stderr, att.Reader)
		att.Close()
		waitCh, errCh := c.ContainerWait(context.Background(), sidecarID, container.WaitConditionNotRunning)
		finalErr := copyErr
		select {
		case st := <-waitCh:
			if st.StatusCode != 0 && finalErr == nil {
				finalErr = fmt.Errorf("sqlite snapshot sidecar exited %d: %s", st.StatusCode, strings.TrimSpace(stderr.String()))
			}
		case e := <-errCh:
			if finalErr == nil {
				finalErr = e
			}
		case <-time.After(30 * time.Minute):
			if finalErr == nil {
				finalErr = fmt.Errorf("sqlite snapshot timed out")
			}
		}
		_ = removeContainer(c, sidecarID)
		pw.CloseWithError(finalErr)
	}()
	return pr, nil
}

// SQLiteRestoreCheck is what the overlay sidecar observed about ONE database
// after writing the consistent snapshot back over it (F109).
//
// The three states are kept distinct because they call for different action:
// Integrity "ok" is proof, a non-"ok" value is a measured failure, and an empty
// Integrity means the sidecar could not read the file at all — which must never
// be reported as either.
type SQLiteRestoreCheck struct {
	Path      string
	Integrity string // "ok", the sqlite failure text, or "" when unreadable
	Tables    int
	Rows      int64
	RowsKnown bool
	// TableRows is the per-table count (F124), empty when the database was
	// unreadable or has more tables than the cap.
	TableRows map[string]int64
	// SHA256 of the file as restored (F134), empty when it could not be hashed.
	SHA256 string
}

// OverlaySQLiteRestore writes each consistent `.dbk` snapshot back over its source
// database and removes the now-stale WAL/shm/journal side-files (F22), so the
// consistent copy — not the raw file already restored from volumes.tar — is what
// the app opens. overlayTar is a tar whose members are named as the (leading-slash-
// stripped) source paths with the `.dbk` content; sources lists those paths so the
// stale side-files can be pruned. Runs in a read-WRITE sidecar on the target volumes.
//
// It then re-reads each restored database in the SAME sidecar and returns what it
// found (F109): `PRAGMA integrity_check` plus table/row counts, for the caller to
// compare against what the manifest recorded at capture. Folding the check in here
// rather than running a second sidecar keeps it free — the container, the volume
// attachment and the sqlite3 binary are already in hand.
func OverlaySQLiteRestore(ctx context.Context, c *client.Client, targetID string, overlayTar io.Reader, sources []string) ([]SQLiteRestoreCheck, error) {
	if len(sources) == 0 {
		return nil, nil
	}
	if err := ensureSidecar(ctx, c); err != nil {
		return nil, err
	}
	// tar -xf - lays each .dbk over its source db; then drop the stale side-files so
	// SQLite doesn't replay an old WAL over the freshly-restored database. Finally
	// each restored database is re-read and its state printed as
	// "DBCHK\t<path>\t<integrity>\t<tables>\t<rows>" for the caller to parse.
	var rm strings.Builder
	rm.WriteString("tar -xf - -C / && ")
	rm.WriteString("for s in")
	for _, s := range sources {
		rm.WriteString(" '")
		rm.WriteString(shellEscape(s))
		rm.WriteString("'")
	}
	rm.WriteString(`; do rm -f "$s-wal" "$s-shm" "$s-journal"; done; `)
	// Verification pass. Every field defaults to the "could not read" value, so a
	// missing sqlite3 or an unreadable file reports honestly instead of silently
	// looking like a pass.
	rm.WriteString("command -v sqlite3 >/dev/null 2>&1 || exit 0; ")
	rm.WriteString("for s in")
	for _, s := range sources {
		rm.WriteString(" '")
		rm.WriteString(shellEscape(s))
		rm.WriteString("'")
	}
	rm.WriteString(`; do [ -f "$s" ] || continue; db="$s"; ` +
		`ic=$(sqlite3 "$db" 'PRAGMA integrity_check' 2>/dev/null | head -1); ` +
		sqliteCountScript +
		// DBCHK first, then this database's TBLROW lines: the parser attaches
		// each run of TBLROWs to the DBCHK above it, so the path never has to be
		// repeated on every line.
		// F134: the SHA-256 of the file as it now sits on disk. Compared against
		// the checksum recorded at capture, this proves the restored database is
		// BYTE-IDENTICAL — every row, every column value — which no count or
		// app-specific digest can match.
		`sha=$(sha256sum "$s" 2>/dev/null | cut -d" " -f1); [ -n "$sha" ] || sha=-; ` +
		`printf 'DBCHK\t%s\t%s\t%s\t%s\t%s\n' "$s" "$ic" "$tb" "$rw" "$sha"; ` +
		sqliteTableRowsScript + `done`)

	hostConfig, err := restoreHostConfig(ctx, c, targetID)
	if err != nil {
		return nil, err
	}
	created, err := c.ContainerCreate(ctx,
		&container.Config{
			Image: sidecarRef(), Cmd: []string{"sh", "-c", rm.String()},
			OpenStdin: true, StdinOnce: true, AttachStdin: true, AttachStdout: true, AttachStderr: true,
			Labels: sidecarLabels(),
		},
		hostConfig, nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("sqlite overlay sidecar create: %w", err)
	}
	sidecarID := created.ID
	defer removeContainer(c, sidecarID)

	att, err := c.ContainerAttach(ctx, sidecarID, container.AttachOptions{Stream: true, Stdin: true, Stdout: true, Stderr: true})
	if err != nil {
		return nil, fmt.Errorf("sqlite overlay attach: %w", err)
	}
	defer att.Close()
	if err := c.ContainerStart(ctx, sidecarID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("sqlite overlay start: %w", err)
	}
	// Drain stdout CONCURRENTLY with the tar upload. The verification pass writes
	// to stdout, and an undrained pipe would deadlock the sidecar mid-restore.
	var stdout, stderr bytes.Buffer
	copyDone := make(chan struct{})
	go func() { _, _ = stdcopy.StdCopy(&stdout, &stderr, att.Reader); close(copyDone) }()

	if _, err := io.Copy(att.Conn, overlayTar); err != nil {
		return nil, fmt.Errorf("streaming sqlite overlay: %w", err)
	}
	att.CloseWrite()

	waitCh, errCh := c.ContainerWait(ctx, sidecarID, container.WaitConditionNotRunning)
	select {
	case st := <-waitCh:
		if st.StatusCode != 0 {
			return nil, fmt.Errorf("sqlite overlay exited %d", st.StatusCode)
		}
	case e := <-errCh:
		return nil, e
	case <-time.After(10 * time.Minute):
		return nil, fmt.Errorf("sqlite overlay timed out")
	}
	// The container has exited; give the drain a moment to flush what it wrote.
	select {
	case <-copyDone:
	case <-time.After(30 * time.Second):
	}
	return parseSQLiteRestoreChecks(stdout.String()), nil
}

// parseSQLiteRestoreChecks turns the overlay sidecar's "DBCHK" lines into
// results (F109). Pure, so the parsing is unit-tested without Docker. Anything
// unparseable is skipped rather than guessed at — a malformed line must not
// become a false pass OR a false failure.
func parseSQLiteRestoreChecks(out string) []SQLiteRestoreCheck {
	var checks []SQLiteRestoreCheck
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		// F124: a TBLROW belongs to the DBCHK above it. A TBLROW with no
		// preceding DBCHK is dropped rather than guessed at.
		if name, n, ok := parseTableRow(line); ok {
			if len(checks) > 0 {
				cur := &checks[len(checks)-1]
				if cur.TableRows == nil {
					cur.TableRows = map[string]int64{}
				}
				cur.TableRows[name] = n
			}
			continue
		}
		if !strings.HasPrefix(line, "DBCHK\t") {
			continue
		}
		// Four fields is the pre-F134 shape and five is the current one; both are
		// accepted so a sidecar image mid-upgrade never drops a whole check.
		f := strings.Split(strings.TrimPrefix(line, "DBCHK\t"), "\t")
		if (len(f) != 4 && len(f) != 5) || f[0] == "" {
			continue
		}
		chk := SQLiteRestoreCheck{Path: f[0], Integrity: strings.TrimSpace(f[1]), Tables: -1}
		if n, err := strconv.Atoi(strings.TrimSpace(f[2])); err == nil {
			chk.Tables = n
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(f[3]), 10, 64); err == nil && n >= 0 {
			chk.Rows, chk.RowsKnown = n, true
		}
		// "-" is the sidecar's marker for "could not hash" — distinct from a real
		// digest, and never treated as one.
		if len(f) == 5 {
			if h := strings.TrimSpace(f[4]); h != "" && h != "-" {
				chk.SHA256 = h
			}
		}
		checks = append(checks, chk)
	}
	return checks
}

// parseTableRow reads one "TBLROW<TAB><table><TAB><rows>" line (F124). Pure, and
// shared by the capture and restore parsers so a table name can never be read
// two different ways.
func parseTableRow(line string) (name string, rows int64, ok bool) {
	const prefix = "TBLROW\t"
	if !strings.HasPrefix(line, prefix) {
		return "", 0, false
	}
	// SplitN with 2 so a table name containing a tab keeps its own field intact;
	// the count is always the LAST field.
	rest := strings.TrimPrefix(line, prefix)
	i := strings.LastIndexByte(rest, '\t')
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(rest[i+1:]), 10, 64)
	if err != nil || n < 0 {
		return "", 0, false
	}
	if name = rest[:i]; name == "" {
		return "", 0, false
	}
	return name, n, true
}

// sqliteTableHashScript emits one "TBLHASH|<table>|<hash>" line per user table of
// $db (#18), using the sqlite3 shell's own SHA3 content hash.
//
// SHA3 rather than md5 because sqlite3 has no md5() at all — measured, not
// assumed. The hash was also measured to be independent of insertion order,
// sensitive to a single changed value, and unchanged across the backup-API copy
// this snapshot is made with, which are the three properties a restore proof
// needs.
//
// Guarded by the same $tb bounds as the row counts, so a pathological schema
// cannot put thousands of entries into a manifest.
const sqliteTableHashScript = `if [ "$tb" -gt 0 ] && [ "$tb" -le ` + maxSQLiteTablesStr + ` ]; then ` +
	`for t in $(sqlite3 "$db" "select name from sqlite_master where type='table' and name not like 'sqlite_%'" 2>/dev/null); do ` +
	`h=$(sqlite3 "$db" ".sha3sum $t" 2>/dev/null | cut -d'|' -f1); ` +
	`[ -n "$h" ] && printf 'TBLHASH|%s|%s\n' "$t" "$h"; done; fi; `

// ParseSQLiteTableRows reads the per-table counts written beside a snapshot at
// capture (F124), where each database has its own file so no path prefix is
// needed. Returns nil for empty input.
func ParseSQLiteTableRows(s string) map[string]int64 {
	var out map[string]int64
	for _, line := range strings.Split(s, "\n") {
		if name, n, ok := parseTableRow(strings.TrimRight(line, "\r")); ok {
			if out == nil {
				out = map[string]int64{}
			}
			out[name] = n
		}
	}
	return out
}

// TarVolumesFrom streams a tar of the given destination paths from a temporary
// Alpine sidecar attached with --volumes-from target (PLAN §2.12). The caller
// reads and closes the returned ReadCloser; the sidecar is auto-removed.
func TarVolumesFrom(ctx context.Context, c *client.Client, targetID string, paths []string) (io.ReadCloser, error) {
	return TarVolumesExcluding(ctx, c, targetID, paths, nil)
}

// tarWithExcludes builds a tar invocation that leaves sub-paths out, and FALLS
// BACK to a full capture when the sidecar's tar does not understand --exclude.
//
// The fallback is not optional politeness. --exclude is a GNU tar option that
// Alpine's busybox happens to provide, but the sidecar image is user
// configurable (F25) and other busybox builds reject it outright. Passing it
// blindly would make tar exit with a usage error and take the ENTIRE volume
// capture with it — trading a redundant directory for no backup at all, which is
// far worse than the problem being solved.
//
// So the option is probed in the same invocation, against a path guaranteed to
// exist, and the real capture runs with or without it accordingly. One sidecar,
// no extra round trip, and on a tar that lacks the option the result is exactly
// today's behaviour.
func tarWithExcludes(paths, excludes []string) []string {
	var ex, members strings.Builder
	for _, x := range excludes {
		if x = strings.TrimPrefix(strings.TrimSpace(x), "/"); x == "" {
			continue
		}
		// Both forms: the directory entry itself and everything beneath it.
		ex.WriteString(" --exclude='" + shellEscape(x) + "'")
		ex.WriteString(" --exclude='" + shellEscape(x) + "/*'")
	}
	for _, p := range paths {
		members.WriteString(" '" + shellEscape(strings.TrimPrefix(p, "/")) + "'")
	}
	if ex.Len() == 0 {
		return []string{"/bin/sh", "-c", "exec tar -cf - -C /" + members.String()}
	}
	script := `if tar -cf /dev/null --exclude=probe -C / dev/null >/dev/null 2>&1; then ` +
		`exec tar -cf - -C /` + ex.String() + members.String() + `; ` +
		`else echo "DockBack: this sidecar image's tar does not support --exclude; capturing everything" >&2; ` +
		`exec tar -cf - -C /` + members.String() + `; fi`
	return []string{"/bin/sh", "-c", script}
}

// TarVolumesExcluding is TarVolumesFrom with sub-paths left out (F126).
//
// Mount-level selection cannot express this case. An all-in-one image keeps its
// bundled database INSIDE the same directory as its configuration — Guacamole's
// PGDATA sits at /config/postgres, under the one bind that holds everything —
// so "don't capture the data directory" is a path within a mount, not a mount.
//
// Without it the live data directory ships anyway, next to the logical dump that
// supersedes it, and worse: a restore lays that torn copy down and the
// application starts on it BEFORE the good dump is imported.
//
// --exclude is understood by both GNU tar and the busybox tar in the default
// sidecar, and is placed before the member list as GNU tar requires.
func TarVolumesExcluding(ctx context.Context, c *client.Client, targetID string, paths, excludes []string) (io.ReadCloser, error) {
	if len(paths) == 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	if err := ensureSidecar(ctx, c); err != nil {
		return nil, err
	}

	// tar -cf - -C / <relpaths>  (leading slash stripped so members are relative)
	args := []string{"tar", "-cf", "-", "-C", "/"}
	for _, p := range paths {
		args = append(args, strings.TrimPrefix(p, "/"))
	}
	if len(excludes) > 0 {
		args = tarWithExcludes(paths, excludes)
	}

	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: args, AttachStdout: true, AttachStderr: true, Labels: sidecarLabels()},
		&container.HostConfig{VolumesFrom: []string{targetID + ":ro"}, AutoRemove: false},
		nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("sidecar create: %w", err)
	}
	sidecarID := created.ID

	att, err := c.ContainerAttach(ctx, sidecarID, container.AttachOptions{
		Stream: true, Stdout: true, Stderr: true,
	})
	if err != nil {
		_ = removeContainer(c, sidecarID)
		return nil, fmt.Errorf("sidecar attach: %w", err)
	}
	if err := c.ContainerStart(ctx, sidecarID, container.StartOptions{}); err != nil {
		att.Close()
		_ = removeContainer(c, sidecarID)
		return nil, fmt.Errorf("sidecar start: %w", err)
	}

	pr, pw := io.Pipe()
	// A canceled backup must stop this stream NOW, not after the whole volume
	// has been tarred (see abortSidecarOnCancel).
	streamDone := make(chan struct{})
	abortSidecarOnCancel(ctx, c, sidecarID, pw, streamDone)
	go func() {
		defer close(streamDone)
		var stderr bytes.Buffer
		_, copyErr := stdcopy.StdCopy(pw, &stderr, att.Reader)
		att.Close()
		if ctx.Err() != nil {
			// Canceled: the sidecar was killed, so a non-zero exit is expected —
			// report the cancellation, not a bogus "tar exited 137".
			_ = removeContainer(c, sidecarID)
			pw.CloseWithError(ctx.Err())
			return
		}

		// Wait for tar to finish and check exit code.
		waitCh, errCh := c.ContainerWait(context.Background(), sidecarID, container.WaitConditionNotRunning)
		var finalErr = copyErr
		select {
		case st := <-waitCh:
			if st.StatusCode != 0 && finalErr == nil {
				finalErr = fmt.Errorf("sidecar tar exited %d: %s", st.StatusCode, strings.TrimSpace(stderr.String()))
			}
		case e := <-errCh:
			if finalErr == nil {
				finalErr = e
			}
		case <-time.After(30 * time.Minute):
			if finalErr == nil {
				finalErr = fmt.Errorf("sidecar tar timed out")
			}
		}
		_ = removeContainer(c, sidecarID) // always clean up (PLAN §9.10)
		pw.CloseWithError(finalErr)
	}()
	return pr, nil
}

// maxTarStderrTail bounds what tar's complaints can cost in memory. One useful
// message is a line; a badly corrupted archive prints one per member, so this is
// generous for the former and nowhere near dangerous for the latter.
const maxTarStderrTail = 8 << 10 // 8 KiB

// tarFailureDetail renders tar's own account of a failure for the error message.
// Empty when it said nothing, so the caller's sentence stays clean.
func tarFailureDetail(stderr string) string {
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		return ""
	}
	// One line, so the run log stays readable; tar puts the cause first.
	return ": " + strings.ReplaceAll(msg, "\n", "; ")
}

// UntarToVolumes extracts a tar stream back into the target container's volumes
// via a temporary Alpine sidecar given the target's own mounts, all writable
// (restoreHostConfig) — so a file the container mounts read-only is restored
// too. Used by restore (PLAN §4.8). The sidecar is always removed.
func UntarToVolumes(ctx context.Context, c *client.Client, targetID string, tarStream io.Reader) error {
	if err := ensureSidecar(ctx, c); err != nil {
		return err
	}
	hostConfig, err := restoreHostConfig(ctx, c, targetID)
	if err != nil {
		return err
	}
	created, err := c.ContainerCreate(ctx,
		&container.Config{
			Image:     sidecarRef(),
			Cmd:       []string{"tar", "-xf", "-", "-C", "/"},
			OpenStdin: true,
			StdinOnce: true,
			// Stdout and stderr are attached deliberately: tar's stderr is the only
			// account of WHY a restore failed, and without it the operator gets an
			// exit code and has to reproduce the failure by hand.
			AttachStdin: true, AttachStdout: true, AttachStderr: true,
			Labels: sidecarLabels(),
		},
		hostConfig,
		nil, nil, "")
	if err != nil {
		return fmt.Errorf("restore sidecar create: %w", err)
	}
	sidecarID := created.ID
	defer removeContainer(c, sidecarID)

	att, err := c.ContainerAttach(ctx, sidecarID, container.AttachOptions{
		Stream: true, Stdin: true, Stdout: true, Stderr: true,
	})
	if err != nil {
		return fmt.Errorf("restore sidecar attach: %w", err)
	}
	defer att.Close()

	if err := c.ContainerStart(ctx, sidecarID, container.StartOptions{}); err != nil {
		return fmt.Errorf("restore sidecar start: %w", err)
	}
	// Drain CONCURRENTLY with the upload, the same shape as the sqlite overlay
	// sidecar above. Two reasons, and both matter: an undrained pipe blocks tar
	// as soon as it fills, and what tar writes here — "No space left on device",
	// "Permission denied" — is the only explanation of a failed restore there is.
	stderr := &tailWriter{limit: maxTarStderrTail}
	drained := make(chan struct{})
	go func() { _, _ = stdcopy.StdCopy(io.Discard, stderr, att.Reader); close(drained) }()

	if _, err := io.Copy(att.Conn, tarStream); err != nil {
		return fmt.Errorf("streaming tar to sidecar: %w", err)
	}
	att.CloseWrite() // signal EOF on stdin so tar finishes

	waitCh, errCh := c.ContainerWait(ctx, sidecarID, container.WaitConditionNotRunning)
	select {
	case st := <-waitCh:
		if st.StatusCode != 0 {
			// The container has exited; let the drain flush before its buffer is
			// read, so the message is complete and nothing races the goroutine.
			select {
			case <-drained:
			case <-time.After(30 * time.Second):
			}
			return fmt.Errorf("restore tar exited %d%s", st.StatusCode, tarFailureDetail(stderr.String()))
		}
	case e := <-errCh:
		return e
	case <-time.After(30 * time.Minute):
		return fmt.Errorf("restore tar timed out")
	}
	return nil
}

// ImageRef returns the image reference and digest for a container (recorded in
// the manifest so restore re-pulls the identical image, PLAN §0.3).
func ImageRef(ctx context.Context, c *client.Client, id string) (ref, digest string, err error) {
	insp, err := c.ContainerInspect(ctx, id)
	if err != nil {
		return "", "", err
	}
	ref = insp.Config.Image
	img, _, err := c.ImageInspectWithRaw(ctx, insp.Image)
	if err == nil && len(img.RepoDigests) > 0 {
		digest = img.RepoDigests[0]
	}
	return ref, digest, nil
}

func ensureImage(ctx context.Context, c *client.Client, ref string) error {
	if _, _, err := c.ImageInspectWithRaw(ctx, ref); err == nil {
		return nil
	}
	return pullImage(ctx, c, ref)
}

// pullImage pulls a single reference and drains the progress stream.
func pullImage(ctx context.Context, c *client.Client, ref string) error {
	rc, err := c.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	defer rc.Close()
	_, _ = io.Copy(io.Discard, rc) // drain to completion
	return nil
}

// ensureImageAvailable makes sure at least one of the given references resolves
// to a local image, returning the one to create the container from. It prefers
// a reference already present locally (so an air-gapped `docker load` of the
// saved tarball is used without a network), otherwise pulls them in order (so a
// digest-pinned ref wins over a moving tag — PLAN §0.3). Errors only if none can
// be made available.
func ensureImageAvailable(ctx context.Context, c *client.Client, refs ...string) (string, error) {
	for _, r := range refs {
		if r == "" {
			continue
		}
		if _, _, err := c.ImageInspectWithRaw(ctx, r); err == nil {
			return r, nil
		}
	}
	var lastErr error
	for _, r := range refs {
		if r == "" {
			continue
		}
		if err := pullImage(ctx, c, r); err == nil {
			return r, nil
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no image reference available to pull")
	}
	return "", lastErr
}

// ImagePullable reports whether a restore could still obtain the container image
// for one of the given references WITHOUT pulling layers (F11 — pre-restore
// readiness). It is satisfied two cheap ways: the image is already present
// locally (an air-gapped restore needs nothing more), or — if not local — a
// digest/tag reference still resolves in its registry via a manifest peek
// (DistributionInspect downloads no layers). It never pulls, so it is safe to run
// on demand. The returned detail explains the verdict for the UI. Note: the
// registry peek requires the socket-proxy DISTRIBUTION endpoint; when that is
// disabled the peek errors and only local presence can be confirmed, which the
// detail string makes explicit rather than silently reporting "gone".
func ImagePullable(ctx context.Context, c *client.Client, refs ...string) (bool, string) {
	tried := false
	// 1. Present locally? Then a restore recreates the container with no network.
	for _, r := range refs {
		if r == "" {
			continue
		}
		tried = true
		if _, _, err := c.ImageInspectWithRaw(ctx, r); err == nil {
			return true, "Image is present locally — restores with no registry."
		}
	}
	// 2. Not local — ask the registry for the manifest only (no layer download).
	var lastErr string
	for _, r := range refs {
		if r == "" {
			continue
		}
		dist, err := c.DistributionInspect(ctx, r, "")
		if err != nil {
			lastErr = err.Error()
			continue
		}
		// Step 27: an image with no build for this machine fails late, or pulls
		// the wrong one. Checked against the node it will run on.
		if info, ierr := c.Info(ctx); ierr == nil {
			var offered []string
			for _, p := range dist.Platforms {
				offered = append(offered, p.Architecture)
			}
			if arch := dockerArch(info.Architecture); !platformOffered(offered, arch) {
				return false, fmt.Sprintf("Image %s has no build for this machine's architecture (%s); it offers %s.", r, arch, strings.Join(offered, ", "))
			}
		}
		return true, "Image is available from its registry (" + r + ") and will be pulled on restore."
	}
	return false, imageUnavailableDetail(tried, lastErr)
}

// dockerArch names a machine architecture the way image platforms do: the
// daemon reports `uname -m` (x86_64, aarch64), registries say amd64, arm64. Pure.
func dockerArch(machine string) string {
	switch machine {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	case "armv7l", "armv6l":
		return "arm"
	case "i386", "i686":
		return "386"
	}
	return machine
}

// platformOffered reports whether an image offers a build for arch. A registry
// that lists no platforms (a single-platform manifest) is not taken as a
// refusal: there is nothing to compare. Pure.
func platformOffered(offered []string, arch string) bool {
	return len(offered) == 0 || slices.Contains(offered, arch)
}

// imageUnavailableDetail builds the human explanation when no reference is present
// locally and none resolved in a registry (F11). Kept pure so the wording/verdict
// is unit-testable without a live daemon.
func imageUnavailableDetail(triedRefs bool, registryErr string) string {
	if !triedRefs {
		return "This backup recorded no image reference, so its image cannot be verified."
	}
	msg := "Image is not present locally and could not be resolved in a registry"
	if registryErr != "" {
		msg += " (" + registryErr + ")"
	}
	return msg + "."
}

// RemoveContainerAndAnonVolumes force-removes a container together with the
// ANONYMOUS volumes Docker created for it (F219) — the `docker rm -v` semantics.
//
// Named volumes are never touched by this: Docker only reaps volumes it created
// implicitly for the container, which is precisely what an isolated clone has
// (stripForClone gives it a fresh empty volume at every mount destination and no
// real sources). So the test-clone reaper cannot take a volume anything else
// depends on, whatever it is pointed at.
func RemoveContainerAndAnonVolumes(ctx context.Context, c *client.Client, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return c.ContainerRemove(ctx, id, container.RemoveOptions{Force: true, RemoveVolumes: true})
}

func removeContainer(c *client.Client, id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
}
