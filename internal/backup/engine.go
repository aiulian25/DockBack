package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/time/rate"

	"dockback/internal/crypto"
	"dockback/internal/dockercli"
	"dockback/internal/notify"
	"dockback/internal/storage"
	"dockback/internal/store"
)

// LogFunc receives live progress lines for streaming to the UI (PLAN §4.11).
type LogFunc func(backupID, level, msg string)

// Engine orchestrates backups, verification and restores.
type Engine struct {
	Store   *store.Store
	Reg     *dockercli.Registry
	Storage storage.Backend

	// Key and KeyFP are the master key and its fingerprint. Set once at
	// construction, then replaced only by SetKey when the operator rotates.
	//
	// F16: after construction they must be READ through MasterKey/MasterKeyFP/
	// CurrentKey and WRITTEN only through SetKey. They used to be swapped in
	// place while backups were running: an unsynchronised write to a slice header
	// and a string that other goroutines were reading, and worse than a race in
	// the abstract, because a run that straddled the swap could wrap its data key
	// under one master key while recording the OTHER key's fingerprint. That
	// backup is permanently "Key mismatch" and cannot be restored. Rotation now
	// also refuses to start while any backup is in flight, so every read within
	// one run answers with the same key.
	Key   []byte // 32-byte master key — read via MasterKey()
	KeyFP string // key fingerprint (PLAN §3.3) — read via MasterKeyFP()
	keyMu sync.RWMutex

	Log LogFunc
	// WorkDir is DISK-backed scratch where the raw volume/DB/export tar is spooled
	// before the streaming compress+encrypt+store step — so a multi-GB volume
	// doesn't exhaust the RAM-backed /tmp (PLAN §11).
	WorkDir string
	// Notify, if set, is called on backup success/failure and verification
	// failure to deliver pluggable notifications (PLAN §4.10). Nil-safe; the
	// implementation must not block (it dispatches asynchronously).
	Notify func(kind, title, message string)

	// UpLimiter, if set, caps the AGGREGATE offsite upload rate across all
	// concurrent backups (PLAN §4.13/§9.9). Shared (one limiter on the single
	// Engine), so N concurrent mirrors together stay under the cap. Nil = no cap.
	UpLimiter *rate.Limiter

	// F70: small bounded cache of parsed volume file indexes (backup id → index)
	// so repeated file searches / generation diffs don't re-decrypt the archive
	// member every time. Guarded by idxMu; evicted oldest-first at the cap.
	idxMu    sync.Mutex
	idxCache map[string]VolIndex
	idxOrder []string

	// #29: what a registry last said a tag resolves to, so the question is asked
	// at most once per reference per TTL. Docker Hub rate-limits anonymous
	// manifest requests, and asking on every backup of every service would spend
	// that budget on an answer that changes on the order of days — exhausting it
	// would break the operator's real pulls. Guarded by tagDigestMu; dropped
	// wholesale at the cap.
	tagDigestMu    sync.Mutex
	tagDigestCache map[string]registryAnswer

	// F86 write-only mode: the offline private key supplied for the CURRENT
	// restore. Set at the top of Restore, cleared on return, never persisted and
	// never logged. Engine is shared, so the GATE is held for the whole restore —
	// restores are already stack-exclusive (locks.acquireRestore), so this only
	// serializes the rare concurrent-different-stack case.
	//
	// F209: the gate and the value are deliberately separate, and it matters.
	// They used to be one sync.Mutex guarding a plain string, which made every
	// write-only restore a PERMANENT HANG: Restore takes the lock and holds it for
	// its whole duration, then reads the key back through restorePrivFor() —
	// archiveKey does exactly that on the first archive read, and Verify does it
	// again inside a safety snapshot. A Go mutex is not reentrant, so the second
	// Lock never returns. Not an error, not a timeout: the goroutine parks
	// forever, the deferred lock releases never run, and that stack can never be
	// restored again until the process restarts.
	//
	// So privGate stays a plain mutual-exclusion gate (only Restore touches it),
	// and the value moves to an atomic the holder can read back freely.
	privGate    sync.Mutex
	restorePriv atomic.Pointer[string]
}

// CurrentKey returns the master key and its fingerprint as ONE consistent pair,
// for callers that record both against the same artifact.
func (e *Engine) CurrentKey() ([]byte, string) {
	e.keyMu.RLock()
	defer e.keyMu.RUnlock()
	return e.Key, e.KeyFP
}

// MasterKey returns the master key in force right now.
func (e *Engine) MasterKey() []byte {
	e.keyMu.RLock()
	defer e.keyMu.RUnlock()
	return e.Key
}

// MasterKeyFP returns the fingerprint of the master key in force right now.
func (e *Engine) MasterKeyFP() string {
	e.keyMu.RLock()
	defer e.keyMu.RUnlock()
	return e.KeyFP
}

// SetKey installs a rotated master key (F16). The caller must have established
// that no backup is in flight — see the note on Key.
func (e *Engine) SetKey(key []byte, fingerprint string) {
	e.keyMu.Lock()
	defer e.keyMu.Unlock()
	e.Key, e.KeyFP = key, fingerprint
}

// notify fires a notification if a notifier is wired (nil-safe).
func (e *Engine) notify(kind, title, message string) {
	if e.Notify != nil {
		e.Notify(kind, title, message)
	}
}

// resumeContainer brings a quiesced container back after the volume snapshot —
// unpause or start — confirms it actually came up, and LOGS the outcome so a
// failed restart (or a crash loop) is visible instead of leaving the app
// silently down (PLAN §4.2). Uses a fresh context so it runs even if the backup
// context is near its deadline.
func (e *Engine) resumeContainer(cli *client.Client, containerID, logID string, paused bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if paused {
		e.logf(logID, "INFO", "Unpausing container")
		if err := cli.ContainerUnpause(ctx, containerID); err != nil {
			e.logf(logID, "ERR", "Failed to unpause container: %v — unpause it manually", err)
			return
		}
	} else {
		e.logf(logID, "INFO", "Restarting container after volume snapshot")
		if err := cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
			e.logf(logID, "ERR", "Failed to restart container after backup: %v — start it manually", err)
			return
		}
	}
	// Confirm it's actually up (catch a crash-loop / immediate exit).
	time.Sleep(2 * time.Second)
	insp, err := cli.ContainerInspect(ctx, containerID)
	if err != nil || insp.State == nil {
		return
	}
	switch insp.State.Status {
	case "running":
		e.logf(logID, "INFO", "Container is running again")
	case "restarting":
		e.logf(logID, "WARN", "Container is restarting (it may be crash-looping) — check its logs")
	default:
		e.logf(logID, "WARN", "Container state is %q after restart — check it", insp.State.Status)
	}
}

// New builds an Engine and derives the key fingerprint. workDir must be a
// disk-backed, writable path (NOT a small RAM tmpfs) — large backups spool there.
func New(st *store.Store, reg *dockercli.Registry, sb storage.Backend, key []byte, workDir string, log LogFunc) *Engine {
	if log == nil {
		log = func(string, string, string) {}
	}
	return &Engine{
		Store: st, Reg: reg, Storage: sb, Key: key,
		KeyFP: KeyFingerprint(key), Log: log, WorkDir: workDir,
	}
}

// KeyFingerprint derives the short, non-secret fingerprint that identifies which
// master key wrapped a backup's DEK (PLAN §3.3). Stable — the same key always maps
// to the same value — so a rotated key yields a new fingerprint and a manifest's
// key_fingerprint can be compared against the running key to detect a mismatch.
func KeyFingerprint(key []byte) string {
	sum := sha256.Sum256(append([]byte("dockback-keyfp"), key...))
	return fmt.Sprintf("%x", sum[:6])
}

// Options controls a single backup run.
type Options struct {
	// embeddedDataDir is the data directory of a database server bundled INSIDE
	// this application's container (F126), set by Run from the app's profile.
	//
	// Unexported: it is derived, never supplied by a caller. It exists because
	// mount-level selection cannot express it — an all-in-one image keeps its
	// database inside the same directory as its configuration, so "skip the data
	// directory" is a path within a captured mount rather than a mount.
	embeddedDataDir string

	// excludeRegenerable are the app-declared regenerable directories the
	// operator chose to leave out (F132), resolved by Run from the profile plus
	// the per-container setting. Unexported: derived, never supplied by a caller.
	excludeRegenerable []RegenerablePath

	// neverBackup are paths with no restore value at all — logs, temp, scratch
	// (F138). Always applied; no operator choice, because there isn't one to
	// make. Unexported: derived from the app's profile, never supplied.
	neverBackup []string

	// BackupID, if set, is used as the backup id (so the caller can register a
	// cancel handle before Run starts). Empty means generate one.
	BackupID    string
	NodeID      string
	ContainerID string
	StopApp     bool   // legacy: stop app container during volume copy (never DBs); superseded by PauseMode
	PauseMode   string // "none"|"pause"|"stop" quiesce during volume copy (never DBs, PLAN §4.2); "" = use remembered/StopApp
	Compression string // "fast" | "balanced" | "max" (zstd, PLAN §8.3)
	// CompressionExplicit marks Compression as a deliberate user choice (a saved
	// non-default, or a stack-run override — F84). When false, an effective
	// "balanced" is the implicit default and the engine may autotune it to
	// "fast" for a selection the learned ratio proves incompressible.
	CompressionExplicit bool

	// Destinations selects which external destinations to mirror to (by ID).
	// DestinationsExplicit distinguishes "no selection made" (mirror to all
	// enabled — the default for quick/full-server backups) from an explicit
	// empty selection (local only).
	Destinations         []string
	DestinationsExplicit bool

	// AppExport requests an application-native export (Paperless etc.) instead
	// of raw volumes/DB — a portable, version-independent archive (PLAN §9.4).
	AppExport bool

	// SaveImage bundles a `docker save` of the container's image (image.tar) for
	// fully air-gapped restore where re-pull is impossible (PLAN §0.3 / §8.4).
	// Off by default; materially enlarges the backup.
	SaveImage bool

	// IncludeMounts, if non-nil, is the explicit set of mount destinations to
	// back up (chosen in the UI). nil means "use the remembered selection for
	// this container, or the size-based default (named volumes + small binds,
	// auto-skipping large media binds)".
	IncludeMounts []string
	// SelectionEphemeral marks IncludeMounts as a one-run computed selection
	// (F83 stack-run shared-bind dedup) that must NOT overwrite the container's
	// remembered selection — otherwise a dedup'd stack run would silently drop
	// a user-ticked bind from every future individual run.
	SelectionEphemeral bool

	// Label, if set, is a free-text label applied to the created backup (F2's
	// label field) — e.g. "auto: pre-change" for an event-triggered protective
	// snapshot (F7). Purely descriptive; shown on the row and matched by search.
	Label string

	// Databases, if non-empty, narrows a Postgres/MySQL dump to just these
	// databases (F8) so one app's DB on a shared engine can be backed up and
	// restored independently. Empty = the full-cluster dump (default). Ignored for
	// non-SQL engines.
	Databases []string

	// Attempt is the 0-based auto-retry attempt number of this run (F26): 0 is the
	// first try, 1/2 are backing-off retries the queue enqueued after a transient
	// failure. Persisted in the queued-job opts JSON so a restart resumes mid-retry.
	Attempt int

	// VolumeOnly, when set, backs up a STANDALONE named volume (no container) —
	// data left behind by a removed container (F23). ContainerID is ignored; the
	// volume's contents are tarred directly via the sidecar and the backup is
	// recorded as TargetName "volume:<name>".
	VolumeOnly string

	// ForceFull disables incremental volume capture for THIS run even when the
	// container has it enabled (F61) — used for pre-restore safety snapshots and
	// event-triggered auto snapshots, which must be self-contained rollback points
	// that never depend on an in-flight chain.
	ForceFull bool

	// SkipRetention suppresses the post-backup retention sweep for THIS run
	// (F208). Set by the pre-restore safety snapshot, on both the container and
	// the standalone-volume path.
	//
	// THE BUG THIS CLOSES
	//
	// A safety snapshot carries the SAME TargetName as the backup being restored —
	// it is, by construction, another backup of that same thing. The sweep that
	// runs at the end of every verified backup (storeAndVerify below) selects every
	// successful backup of that target and deletes the losers outright, archive and
	// row. So with auto-pruning on and `retention.generations` at its default 3, a
	// restore of the OLDEST of three generations went: snapshot created -> four
	// rows -> GFS keeps the newest three -> the fourth, which is the archive the
	// operator asked to restore, is deleted from the catalog and from every
	// location it lived in. The restore then failed to find its own source.
	//
	// Suppressing the sweep for this one run is the whole fix, and it costs
	// nothing: retention is not a deadline. The next ordinary backup of that
	// target, and the scheduled fleet-wide sweep, both apply the policy normally.
	// What must never happen is a prune running in the middle of a destructive
	// operation, deciding the fate of the very data that operation depends on.
	SkipRetention bool
}

// retentionSweepWanted reports whether this run should end with a retention
// sweep of its target.
//
// Two conditions, and the second is F208. A backup that did not verify is not a
// generation and must not evict one. And a run that asked to be left out — a
// pre-restore safety snapshot — must not sweep at all, because its sweep would
// be deciding the fate of the very archive the in-flight restore is about to
// read. See Options.SkipRetention.
func (o Options) retentionSweepWanted(verified string) bool {
	return verified == "verified" && !o.SkipRetention
}

func (e *Engine) logf(id, level, format string, a ...any) {
	e.Log(id, normalizeLogLevel(level), fmt.Sprintf(format, a...))
}

// normalizeLogLevel folds the one spelling that never reached anybody.
//
// This package emits both "ERR" and "ERROR" — 32 sites and 27 sites — and every
// consumer keys on "ERR" alone: the run consoles colour it red, and three pages
// end a run on it. An "ERROR" line therefore rendered as ordinary grey text and
// never ended anything, so a refused stack restore (`Stack restore refused: …`,
// logged at "ERROR") left the spinner turning with no indication it had stopped.
//
// Folded here rather than at 27 call sites: this is the single function every
// log line in the package already passes through, so one rule covers the ones
// written since as well.
func normalizeLogLevel(level string) string {
	if level == "ERROR" {
		return "ERR"
	}
	return level
}

// addFinding records something discovered about the SOURCE and logs it once.
//
// The single channel every "report, don't silently reproduce" check writes to
// (PLAYBOOK §10.3), so a finding reaches the manifest, the run log and the UI
// through one call rather than each check inventing a field and a log format.
//
// Deduplicated on (code, subject): the checks run per mount, per container and
// per shared source, and the same defect is reachable from several of them —
// three identical lines about one my.cnf is how a real finding starts looking
// like noise.
//
// Never fails and never blocks a backup. A finding describes the deployment,
// not this archive's integrity, and the run that noticed it is still a good
// backup of a flawed source — which is precisely the situation worth reporting.
func (e *Engine) addFinding(man *Manifest, logID, code, severity, subject, message string) {
	if man == nil || code == "" || message == "" {
		return
	}
	if severity != FindingInfo && severity != FindingWarn && severity != FindingDanger {
		severity = FindingWarn
	}
	for _, f := range man.Findings {
		if f.Code == code && f.Subject == subject {
			return
		}
	}
	man.Findings = append(man.Findings, Finding{
		Code: code, Severity: severity, Message: message, Subject: subject,
	})

	// One line, one format. INFO for something merely worth knowing; WARN for a
	// real defect — including danger, because the run log has no louder level and
	// the severity travels in the manifest where the UI can render it properly.
	level := "WARN"
	if severity == FindingInfo {
		level = "INFO"
	}
	if subject != "" {
		e.logf(logID, level, "Finding on the source (%s): %s — %s", code, subject, message)
		return
	}
	e.logf(logID, level, "Finding on the source (%s): %s", code, message)
}

// Run performs a backup of a single container: db dump (if any) + volumes +
// config, compressed, encrypted, stored, with a self-describing manifest, then
// verifies it (always-on, PLAN §4.3). It returns the backup id.
func (e *Engine) Run(ctx context.Context, nodeName string, opts Options) (string, error) {
	id := opts.BackupID
	if id == "" {
		id = newID()
	}
	b := &store.Backup{ID: id, NodeID: opts.NodeID, Status: "running"}
	start := time.Now() // F28: wall-clock run time, stamped on success for drift detection

	// F23: a standalone named volume (no container) takes a dedicated capture path.
	if opts.VolumeOnly != "" {
		return e.runVolumeOnly(ctx, nodeName, opts, id, b, start)
	}

	cli, err := e.Reg.Get(opts.NodeID)
	if err != nil {
		return id, err
	}
	insp, err := cli.ContainerInspect(ctx, opts.ContainerID)
	if err != nil {
		return id, fmt.Errorf("inspect target: %w", err)
	}
	name := strings.TrimPrefix(insp.Name, "/")
	stack := insp.Config.Labels["com.docker.compose.project"]
	service := insp.Config.Labels["com.docker.compose.service"]
	b.TargetName, b.Stack = name, stack
	if err := e.Store.CreateBackup(b); err != nil {
		return id, err
	}
	// F163: a container marked as one that must only ever be backed up with
	// write-only encryption, at a moment when write-only is off. Checked here,
	// before anything is captured, so the refusal costs nothing — and refused
	// rather than downgraded, because the entire content of that setting is "I
	// would rather have no backup than a readable one".
	if werr := e.writeOnlyRequirement(opts.NodeID, name, insp.Config.Image); werr != nil {
		return id, e.fail(b, werr)
	}
	// Apply an initial label if requested (e.g. "auto: pre-change" from an
	// event-triggered snapshot, F7). CreateBackup doesn't take a label, and the
	// end-of-run UpdateBackup doesn't touch the label column, so this persists.
	if opts.Label != "" {
		b.Label = opts.Label
		_ = e.Store.SetBackupLabel(id, opts.Label)
	}
	e.logf(id, "INFO", "Starting backup of %q (stack=%q) on node %s", name, stack, nodeName)

	// Self-heal: reap any backup sidecars a previous run leaked on this node
	// (e.g. an interrupted run or a teardown that failed on a network blip), so
	// they can't accumulate (PLAN §2.12/§9.10).
	if n, _ := dockercli.RemoveOrphanSidecars(ctx, cli); n > 0 {
		e.logf(id, "INFO", "Cleaned %d orphaned backup sidecar(s) from a previous run", n)
	}

	// Pre-flight: free space (PLAN §4.4).
	if free, err := e.Storage.FreeBytes(ctx); err == nil && free > 0 {
		e.logf(id, "INFO", "Destination free space: %s", humanBytes(int64(free)))
	}

	imageRef, imageDigest, _ := dockercli.ImageRef(ctx, cli, opts.ContainerID)
	engineKind := detectDBEngine(insp.Config.Image, insp.Config.Env)
	// F126: an ALL-IN-ONE image bundling its own database server. detectDBEngine
	// reads the image name, and an image name says nothing about what it bundles —
	// `jwetzell/guacamole` runs a full PostgreSQL and looks like an ordinary app,
	// so its live data directory was raw-copied: a hot file copy of a running
	// database, which is exactly what the dump path exists to prevent.
	var embedded *EmbeddedDump
	if engineKind == "" {
		if p := ProfileFor(insp.Config.Image); p != nil && p.EmbeddedDump != nil {
			// F166: some applications ship ONE image that can run several ways, so
			// the declaration is conditional. The probe decides; a "no" leaves the
			// ordinary file path in place, which for such a deployment is the
			// correct method rather than a fallback.
			if e.embeddedDumpApplies(ctx, cli, opts.ContainerID, p.EmbeddedDump, id, p.Name) {
				embedded = p.EmbeddedDump
				engineKind = embedded.Engine
				// NOTE: opts.embeddedDataDir is deliberately NOT set here. The
				// exclusion is armed only once the dump has actually succeeded —
				// excluding a data directory with no dump to supersede it would turn
				// a wasteful backup into an empty one.
				e.logf(id, "INFO", "%s bundles its own %s server — dumping it with native tools instead of copying its data directory", p.Name, embedded.Engine)
			}
		}
	}

	// A stopped container can't run a live database dump (the dump execs a client
	// inside a running process) and has no live writes to quiesce. Back it up as a
	// file/volume snapshot instead — valid for an app kept off between uses, and it
	// keeps a scheduled whole-node/specific run from failing on a stopped target
	// (F9). Its data dir is NOT excluded from the volume tar, since no dump replaces
	// it, so the raw files are captured.
	running := insp.State != nil && insp.State.Running
	if !running && engineKind != "" {
		e.logf(id, "INFO", "Target is stopped — backing up volumes/config without a live database dump")
		engineKind = ""
	}

	// Pre-flight (PLAN §4.4): encryption key present, storage writable, DB tools
	// present — fail fast before any expensive/destructive work. (Disk-space
	// estimate runs later as guardFreeSpace, once the mount selection is known.)
	dbFallback, err := e.preflight(ctx, cli, opts, engineKind, id)
	if err != nil {
		return id, e.fail(b, err)
	}
	algo, zlevel, zwindow, compLabel := parseCompression(opts.Compression)

	man := &Manifest{
		// F103: carried from pre-flight — a database whose dump tools are absent
		// will be captured as raw files, which the grade and runbook must reflect.
		DBFallback:     dbFallback,
		Version:        ManifestVersion,
		BackupID:       id,
		CreatedAt:      nowRFC3339(),
		NodeID:         opts.NodeID,
		NodeName:       nodeName,
		Stack:          stack,
		Service:        service,
		DependsOn:      parseDependsOn(insp.Config.Labels["com.docker.compose.depends_on"]),
		TargetName:     name,
		ContainerID:    insp.ID,
		Image:          imageRef,
		ImageDigest:    imageDigest,
		KeyFingerprint: e.MasterKeyFP(),
		Format: Format{
			Encryption:  "AES-256-GCM (DBACKv1 chunked)",
			Compression: compLabel,
			Algorithm:   algo,
			Archive:     "tar",
			Layout:      "manifest.json, config/inspect.json, config/docker-compose.yml, db/<service>.dump, volumes.tar",
		},
		HasConfig: true,
	}

	// #11: a value this application can never re-create must be IN the backup, or
	// the backup is worthless in the disaster it exists for. Asserted here —
	// before the dump, before the volume copy — so a refusal costs nothing.
	if insp.Config != nil {
		if nerr := e.assertNeverRegenerate(man, id, name, insp.Config.Image, insp.Config.Env); nerr != nil {
			return id, e.fail(b, nerr)
		}
		// The same assertion for an application that keeps its irreplaceable
		// value in a configuration file instead of the environment.
		if nerr := e.assertNeverRegenerateFiles(ctx, cli, opts.ContainerID, man, id, name, insp.Config.Image, running); nerr != nil {
			return id, e.fail(b, nerr)
		}
	}
	// Record the on-host compose project layout (working dir + compose filename) so
	// a DR restore can rebuild the organized <base>/<stack>/ folder, not just the
	// container. Empty for non-compose containers.
	man.StackWorkingDir, man.ComposeFile = composeProjectLayout(insp.Config.Labels)

	work, err := os.MkdirTemp(e.WorkDir, "dback-"+id+"-*")
	if err != nil {
		return id, e.fail(b, err)
	}
	defer os.RemoveAll(work)

	// 0) Application-aware quiesce hooks (PLAN §9.5): run pre-hooks so the files
	// and database are captured at one consistent instant; post-hooks always run
	// afterward (e.g. Nextcloud maintenance mode off), even on failure.
	pre, post := e.gatherHooks(insp.Config.Image, opts.NodeID, name)
	ranPre := false
	defer func() {
		if !ranPre {
			return
		}
		pctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		for _, h := range post {
			_ = e.runHook(pctx, cli, opts.ContainerID, h, id, "post")
		}
	}()
	for _, h := range pre {
		if err := e.runHook(ctx, cli, opts.ContainerID, h, id, "pre"); err != nil {
			return id, e.fail(b, fmt.Errorf("pre-backup hook failed: %w", err))
		}
	}
	if len(pre) > 0 {
		ranPre = true
		e.logf(id, "INFO", "Application quiesced for a consistent snapshot")
	}

	// Decide capture strategy: portable app-native export vs raw volumes/DB.
	profile := e.exportProfile(insp.Config.Image, opts.NodeID, name)
	useExport := opts.AppExport && profile.Available

	// 1) Data capture.
	if useExport {
		e.logf(id, "INFO", "App-native export via %s (%s)", profile.Tool, strings.Join(profile.ExportCmd, " "))
		if _, eerr := dockercli.ExecHook(ctx, cli, opts.ContainerID, profile.ExportCmd, profile.User, ""); eerr != nil {
			return id, e.fail(b, fmt.Errorf("app export: %w", eerr))
		}
		out := filepath.Join(work, "appexport.tar")
		f, ferr := os.Create(out)
		if ferr != nil {
			return id, e.fail(b, ferr)
		}
		terr := dockercli.ExecStream(ctx, cli, opts.ContainerID, []string{"tar", "-cf", "-", "-C", profile.Dir, "."}, f)
		f.Close()
		if terr != nil {
			return id, e.fail(b, fmt.Errorf("capturing export: %w", terr))
		}
		fi, _ := os.Stat(out)
		man.AppExport = &AppExportRef{
			Tool: profile.Tool, Dir: profile.Dir, ImportCmd: profile.ImportCmd, Bytes: fi.Size(),
			// F152: recorded so a restore knows the application's own check
			// without consulting settings that may have changed since.
			VerifyCmd: profile.VerifyCmd,
		}
		man.Format.Layout = "manifest.json, config/inspect.json, config/docker-compose.yml, appexport.tar (portable " + profile.Tool + " export)"
		e.logf(id, "INFO", "Portable export captured (%s)", humanBytes(fi.Size()))
		// F151: the export directory still holds a PLAINTEXT copy of everything
		// the application knows. Emptying it is opt-in and happens only here, on
		// the success path — a half-made backup is never a reason to delete the
		// thing it was made from.
		if e.CleanupExport(opts.NodeID, name) {
			man.AppExport.Cleaned = e.cleanupExportDir(ctx, cli, opts.ContainerID, profile.Dir, profile.User, id)
		} else {
			e.logf(id, "INFO", "A plaintext copy of this export remains in %s inside the container. DockBack's archive is encrypted; that directory is not — turn on \"Empty the export directory after each backup\" on this container if it should not linger", profile.Dir)
		}
	} else if engineKind != "" {
		// Database dump (live, never pause the DB — PLAN §4.1/§4.2).
		e.logf(id, "INFO", "Detected %s database; dumping live with native tools", engineKind)
		dump, derr := e.dumpDatabaseWith(ctx, cli, ContainerRef{NodeID: opts.NodeID, Name: name}, opts.ContainerID, engineKind, embedded, insp.Config.Env, work, service, opts.Databases)
		switch {
		case derr == nil:
			// #13: which credential actually authenticated, and therefore what this
			// dump can and cannot contain. Probed rather than assumed — a declared
			// root password that never applied produces a schema dump that would
			// otherwise be recorded as a server one.
			e.recordDumpScope(ctx, cli, opts.ContainerID, engineKind, insp.Config.Env, man, &dump, id)
			// F168: the application's own key tables, counted through the same
			// connection the dump used, so a restore can be held to the ROWS and
			// not only to the schema.
			e.recordAppTableCounts(ctx, cli, opts.ContainerID, embedded, &dump, id)
			man.Databases = append(man.Databases, dump)
			e.logf(id, "INFO", "Database dump complete: %s (%s)", dump.Path, humanBytes(dump.Bytes))
			// F126: the dump exists, so the raw data directory it supersedes can
			// now be left out of the file capture. Armed HERE and nowhere else —
			// on every failure path below the directory is still captured, because
			// a torn copy beats no copy at all.
			if embedded != nil {
				opts.embeddedDataDir = embedded.DataDir
				e.logf(id, "INFO", "Excluding the raw %s data directory %s from the file capture — the dump replaces it", embedded.Engine, embedded.DataDir)
			}
		case redisAuthUnavailable(engineKind, derr):
			// F185: Redis, and ONLY Redis, may fall back here.
			//
			// The rule everywhere else is that a database we cannot dump must fail
			// loudly rather than be quietly recorded as files, and it stays that
			// way: a Postgres or MySQL data directory copied out from under a
			// running server is torn, and a torn copy that grades like a backup is
			// worse than no backup.
			//
			// Redis is genuinely different. It writes a COMPLETE, self-consistent
			// RDB to /data on its own save schedule — that file is exactly the
			// artifact a restore replays, and capturing it is the same operation,
			// just at Redis's chosen moment rather than ours. So the honest
			// outcome is a slightly older snapshot, not a broken one.
			//
			// Recorded, not swallowed: DBFallback puts it on the archive's face, so
			// the grade and the runbook say a consistent snapshot was not taken and
			// why. The failure this replaces was worse in the way that matters —
			// the whole container had no backup at all.
			e.logf(id, "WARN", "Could not authenticate to Redis, so no consistent snapshot was taken — capturing its /data directory as files instead. That holds the RDB Redis last wrote on its own save schedule, so a restore works from a slightly older point in time. Set REDIS_PASSWORD on this container for a point-in-time snapshot.")
			e.logf(id, "WARN", "Redis said: %s", redisServerReply(derr))
			man.DBFallback = redisAuthFallbackNote
			engineKind = ""
		case dumpToolMissing(derr):
			// The DB CLI isn't in this container — it's not really a database
			// server (e.g. an app that merely connects to one). Don't fail the
			// backup; capture its files/volumes instead.
			e.logf(id, "WARN", "%s tools not found in this container — it isn't a database server; backing up its files instead", engineKind)
			// F103: record the fallback. If this container really IS a database
			// server whose image simply lacks the client tools, its files were just
			// copied live and may be torn — the grade and the runbook have to say
			// so rather than let it pass as an ordinary app backup.
			man.DBFallback = dbFallbackNote(engineKind)
			engineKind = ""
		default:
			return id, e.fail(b, fmt.Errorf("database dump: %w", derr))
		}
	}

	// F95: record what the IMAGE declares, so a later restore can tell whether the
	// image it is about to run expects configuration this backup never had.
	// Best-effort — an image that can't be inspected simply records nothing.
	if ic, ierr := dockercli.InspectImageConfig(ctx, cli, insp.Image); ierr == nil {
		man.ImageConfig = &ImageConfig{
			EnvKeys: ic.EnvKeys, Entrypoint: ic.Entrypoint, Cmd: ic.Cmd,
			Volumes: ic.Volumes, Healthcheck: ic.Healthcheck, Version: ic.Version, User: ic.User,
		}
	}
	// #8: a policy that will not bring this container back after a reboot. Said
	// at capture, where the operator can still act on it before they need it.
	if insp.HostConfig != nil {
		e.reportRestartPolicy(man, id, name, string(insp.HostConfig.RestartPolicy.Name))
	}
	// #7: the engine this backup is being taken on, so a restore can tell an
	// environmental rule that applies from one that would be churn.
	if v, verr := cli.ServerVersion(ctx); verr == nil {
		man.DockerVersion = v.Version
	}
	// #35: a probe that cannot fail flows a false green through every gate that
	// trusts it, including this tool's own. Audited here, where the environment
	// that decides the verdict is in the same inspect.
	e.auditHealthcheck(man, id, insp)
	// #16: and the absence of one. A database that reports only "running" hands
	// `depends_on: service_started` a green the instant the container exists,
	// which is what makes the stack race its own database on every boot.
	e.reportMissingHealthcheck(man, id, name, insp)
	// #23: and the fact that only a PAIR of containers can show — one directory
	// shared by two different builds of the same image. Nothing in this
	// container's own inspect mentions the other one, so it is asked here, where
	// the recorded mounts are already known.
	e.reportSharedMountSkew(ctx, cli, man, id, name, insp)
	// #24: and the third way a container says which user it runs as — a raw id in
	// the stack file, which is invisible from both the image and the environment.
	e.reportComposeUser(man, id, name, insp)
	// #5: and the same question of the env-configurable model — the pair is
	// handled end to end, but the numbers in it can still be a NAS's.
	e.reportRunAsConvention(man, id, name, insp)
	// #29: and what a `docker pull` of this container's tag would do today. Asked
	// here because this is the one moment the tool is already looking at this
	// image, and because the answer is only useful BEFORE the pull.
	e.reportTagDrift(ctx, cli, opts.ContainerID, man, id, name, insp.Config.Image)
	if insp.Config != nil {
		man.ContainerEnvKeys = dockercli.EnvKeys(insp.Config.Env)
		// F117: which uid/gid this app drops privileges to. Two small integers —
		// never a secret — recorded so a restore onto a host configured with
		// different ids can align the restored files instead of leaving the app
		// unable to write its own database.
		if uid, gid, key, ok := dockercli.RunAsIDs(insp.Config.Env); ok {
			man.RunAsIDs = &RunAs{UID: uid, GID: gid, Key: key}
		}
	}

	// F94: record what this container needs FROM ITS HOST, so a cross-host restore
	// can warn before creating something the target cannot start.
	man.Requires = hostRequirementsOf(insp)
	// F144: and which host ports it needs free there.
	man.PublishedPorts = publishedPortsOf(insp)

	// F146: is this container one service of an application whose services are
	// only meaningful together? A single-service backup of such a stack is the
	// documented way to end up with archives that each verify perfectly and
	// cannot restore a working deployment, so it is recorded in the archive and
	// said out loud while the operator is still here.
	//
	// Gated on the compose project: a container that belongs to no stack cannot
	// be a member of anything, which is the common case and costs nothing.
	if stack != "" && insp.Config != nil {
		if set, members := e.stackAtomicContext(ctx, cli, stack); set != nil && len(members) >= 2 {
			man.StackAtomic = &StackAtomicRef{Why: set.Why, Symptom: set.Symptom, SoloRestore: set.SoloRestore, Members: members}
			e.logf(id, "WARN", "%s — and this backup covers only %q. Back up the stack %q as one app-consistent snapshot instead: %s.",
				set.Why, name, stack, set.Symptom)
		}
	}

	// F141: paths whose data is only meaningful together. Recorded so the archive
	// states its own contract, and so a restore can refuse a half of it rather
	// than producing a broken or factory-fresh application.
	if insp.Config != nil {
		if set := AtomicVolumesFor(insp.Config.Image); set != nil {
			var mounted []string
			for _, m := range insp.Mounts {
				mounted = append(mounted, m.Destination)
			}
			required, absent := atomicSetRequired(set, mounted)
			man.AtomicVolumes = required
			// A member this container does not mount at all is not something the
			// backup can be blamed for — but it is worth saying, because it means
			// that data lives in the container's writable layer and disappears the
			// next time the container is recreated, backup or no backup.
			if len(absent) > 0 {
				e.logf(id, "WARN", "%s is not mounted as a volume on this container, so its contents live in the container's writable layer and cannot be captured — or survive a recreate. %s",
					strings.Join(absent, " and "), set.Why)
			}
		}
	}

	// F132: regenerable directories the operator chose to drop. Resolved here so
	// the exclusion and the manifest record cannot disagree — the archive always
	// states exactly what was left out and what regenerating it costs.
	// F138: paths always left out — logs, temp, scratch. No toggle: these have
	// no restore value at all, unlike the regenerable data below, and keeping
	// them costs archive space and widens what a leak exposes.
	if p := ProfileFor(insp.Config.Image); p != nil && len(p.NeverBackup) > 0 {
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

	if regen := RegenerablePathsFor(insp.Config.Image); len(regen) > 0 && e.ExcludeRegenerable(opts.NodeID, name) {
		opts.excludeRegenerable = regen
		for _, r := range regen {
			man.ExcludedRegenerable = append(man.ExcludedRegenerable, ExcludedPath{Path: r.Path, Label: r.Label, Cost: r.Cost})
			e.logf(id, "INFO", "Leaving out %s (%s) — %s rebuilds it. %s", r.Path, r.Label, name, r.Cost)
		}
	}

	// F89: record the network topology — each attached network's definition and
	// this container's endpoint on it. Best-effort: a failure leaves the field
	// empty and the restore falls back to the pre-F89 behaviour, never a failed
	// backup over metadata.
	man.Networks = networkRefsFrom(dockercli.InspectNetworks(ctx, cli, insp))
	if len(man.Networks) > 0 {
		e.logf(id, "INFO", "Recorded %d network(s) for restore: %s", len(man.Networks), networkSummary(man.Networks))
	}

	// 2) Config (container inspect = our compose/config record, PLAN §4 / §9.3).
	inspBytes, _ := json.MarshalIndent(insp, "", "  ")
	if err := os.WriteFile(filepath.Join(work, "inspect.json"), inspBytes, 0o600); err != nil {
		return id, e.fail(b, err)
	}
	// F73: config-drift fingerprints of the captured config, so later page loads
	// can detect "changed since this backup" without opening the archive.
	man.ConfigFP = ConfigFingerprint(inspBytes)
	man.ConfigFPLite = configFPLiteFromInspect(insp)

	// 2b) Reconstructed Compose file (PLAN §0.3 "Always: Compose files" / §9.3).
	// The real compose YAML lives on the host (unreachable via the socket-proxy),
	// so we synthesize a functional equivalent from the inspect for hand-restore.
	// Best-effort: a synthesis failure never fails the backup.
	if composeBytes, cerr := composeFromInspect(insp, imageRef, man.Networks, nil, dockercli.ImageEnv(ctx, cli, insp.Image)); cerr == nil {
		if werr := os.WriteFile(filepath.Join(work, "docker-compose.yml"), composeBytes, 0o600); werr == nil {
			man.HasCompose = true
		} else {
			e.logf(id, "WARN", "Could not write reconstructed compose file: %v", werr)
		}
	} else {
		e.logf(id, "WARN", "Could not reconstruct compose file: %v", cerr)
	}

	// 2b-ii) Also capture the GENUINE host compose file(s) and the stack's .env
	// (F57/F231) — from every transport, not only over SSH.
	if hasCompose, anyFile := e.captureOriginalCompose(ctx, cli, id, opts.NodeID, insp, work); anyFile {
		man.HasOriginalCompose = hasCompose
		man.Format.Layout += ", config/original-compose/*"
	}
	if man.ProjectFolder = e.captureProjectFolder(ctx, cli, id, insp, work); man.ProjectFolder != nil && man.ProjectFolder.Entries > 0 {
		man.Format.Layout += ", " + layoutPath(projectFolderMember)
	}

	// 2c) Optional image tarball for air-gapped restore (PLAN §0.3 / §8.4). Saved
	// by the image reference so `docker load` restores its repo:tag. Best-effort:
	// a failure logs WARN and the state backup still completes (just not
	// air-gapped). Materially enlarges the archive, hence opt-in.
	if opts.SaveImage {
		if imageRef == "" {
			e.logf(id, "WARN", "Image tarball requested but the container has no resolvable image reference — skipping")
		} else if itBytes, ierr := e.saveImageTar(ctx, cli, imageRef, work, id); ierr != nil {
			e.logf(id, "WARN", "Image tarball NOT saved (%v) — this backup is not air-gapped", ierr)
		} else {
			man.ImageTar = &ImageTarRef{Ref: imageRef, Bytes: itBytes}
			man.Format.Layout += ", image.tar"
			e.logf(id, "INFO", "Saved image tarball for air-gapped restore: %s (%s)", imageRef, humanBytes(itBytes))
		}
	}

	// 3) Volumes via --volumes-from sidecar (PLAN §2.12). Optionally stop the
	//    app container first for consistency (never for DBs). Skipped entirely in
	//    app-native export mode (the export is the portable data).
	var volDests []string
	var uncompressedSel int64 // measured uncompressed selection bytes (F17: learn the ratio on success)
	if !useExport {
		var refs []VolumeRef
		// When a consistent DB dump was actually taken (engineKind survives the
		// no-tools fallback), exclude that engine's raw data dir from the volume
		// capture (PLAN §4.1).
		dumpedDataDir := ""
		if engineKind != "" {
			dumpedDataDir = dockercli.DBDataDir(engineKind)
			// F126: an embedded server keeps its data wherever the APP puts it
			// (Guacamole: /config/postgres), not at the engine image's default —
			// so exclude the declared path, or the raw datadir ships alongside
			// the dump that supersedes it.
			if embedded != nil && embedded.DataDir != "" {
				dumpedDataDir = embedded.DataDir
			}
		}
		var skipped []SkippedMount
		volDests, refs, skipped = e.selectMounts(ctx, cli, insp, opts, id, dumpedDataDir)
		// F81: what every host bind of this container IS — kind, owner, mode —
		// captured or not, so a restore onto a machine that has none of these paths
		// can create each one as the right kind of thing instead of letting Docker
		// invent a root-owned directory where a file belongs. Recorded before the
		// captured refs so both are stamped from the same single probe.
		var fileSkips []SkippedMount
		volDests, refs, fileSkips = e.captureBindRoots(ctx, cli, insp, opts.ContainerID, work, id, man, volDests, refs)
		skipped = append(skipped, fileSkips...)
		// F226: and the identity of EVERY named volume it mounts, captured or not,
		// so the restore creates each one itself instead of letting Docker
		// auto-create an unlabelled replacement.
		man.MountedVolumes = e.recordMountedVolumes(ctx, cli, insp)
		man.SkippedMounts = skipped
		e.logPartialSkips(id, skipped)
		// Bind destinations among the selection — used to warn on UID/GID/permission
		// mismatches the reader can't access (PLAN §2.13). Keep each bind's host
		// source so an unreadable one can be recorded with its path.
		var bindDests []string
		bindSrc := map[string]string{}
		for _, r := range refs {
			if r.Type == "bind" {
				bindDests = append(bindDests, r.Destination)
				bindSrc[r.Destination] = r.Source
			}
		}
		// Pre-flight: make sure the selection plausibly fits before we stop the
		// container and start streaming (avoids filling the disk / long downtime).
		unreadableBinds, uncompressed, err := e.guardFreeSpace(ctx, cli, opts.NodeID, name, opts.ContainerID, volDests, bindDests, id, opts.Compression)
		if err != nil {
			return id, e.fail(b, err)
		}
		uncompressedSel = uncompressed
		// F84: now that the selection is measured, an implicitly-balanced run over
		// data the learned ratio proves incompressible switches to fast for this
		// run — the compressor only starts in the store tail, so re-resolving here
		// is safe. The manifest labels follow so the archive self-describes.
		if newPreset, learned, switched := e.autotuneCompression(opts, name, uncompressedSel); switched {
			algo, zlevel, zwindow, compLabel = parseCompression(newPreset)
			man.Format.Compression, man.Format.Algorithm = compLabel, algo
			e.logf(id, "INFO", "Selection is incompressible (learned ratio %.2f) — using fast compression for this run", learned)
		}
		// F3: a selected bind whose files the reader couldn't access (UID/GID
		// mismatch / NFS root_squash) drops data silently — record it as a PARTIAL
		// skipped-mount so the manifest, Backups "Partial" chip, and DR runbook all
		// reflect the gap instead of only a log line.
		if part := unreadableSkippedMounts(unreadableBinds, bindSrc); len(part) > 0 {
			man.SkippedMounts = append(man.SkippedMounts, part...)
			e.logf(id, "WARN", "%d selected bind mount(s) had files the reader could not access — this backup is PARTIAL; see the backup's details", len(part))
		}
	}
	// Smart pause policy (PLAN §4.2): quiesce an APP container during the volume
	// copy for a consistent snapshot — never a database (it's dumped live, §4.1)
	// and only when there's volume data to capture. The container is brought back
	// up as soon as the volume tar is captured — NOT held down through
	// compress/encrypt/store/verify — so downtime is just the copy.
	var resume func()
	resumed := false
	doResume := func() {
		if resume != nil && !resumed {
			resumed = true
			resume()
		}
	}
	defer doResume() // safety net: always resume, even on an error/early return
	pm, pauseByOperator := e.pauseModeFor(opts, insp.Config.Image, name)
	// #3: a database whose image nobody recognises is still a database. Quiesce
	// the copy window rather than tear its data directory, and record in the
	// manifest that this went out as a file copy and not a dump.
	pm = e.guardStatefulVolumes(ctx, cli, man, opts.ContainerID, id, name, engineKind, pm, pauseByOperator, volDests)
	// F145: when an application declares its own quiesce default and that is what
	// this run is using, say so and say why. A default that differs from the
	// shipped one must be visible, or an operator cannot disagree with it.
	if mode, why := AppPauseDefault(insp.Config.Image); mode == pm && why != "" && mode == PauseNone && len(volDests) > 0 && running {
		e.logf(id, "INFO", "Not pausing %s during the copy — %s.", name, why)
	}
	if shouldPause(pm, engineKind, running, len(volDests)) {
		switch pm {
		case PauseStop:
			e.logf(id, "INFO", "Stopping container for a consistent volume snapshot")
			_ = cli.ContainerStop(ctx, opts.ContainerID, container.StopOptions{})
			resume = func() { e.resumeContainer(cli, opts.ContainerID, id, false) }
		case PausePause:
			e.logf(id, "INFO", "Pausing container for a consistent volume snapshot (brief freeze)")
			if perr := cli.ContainerPause(ctx, opts.ContainerID); perr != nil {
				e.logf(id, "WARN", "Pause failed (%v) — continuing with a live copy", perr)
			} else {
				resume = func() { e.resumeContainer(cli, opts.ContainerID, id, true) }
			}
		}
	}
	// #25: what the tree looked like as the copy began. Compared against the walk
	// the index already takes afterwards, it says what moved underneath the
	// archive — the one thing a hot copy can be honest about.
	driftBefore := e.driftBaseline(ctx, cli, opts.ContainerID, volDests, pm, man.Image, id)
	if len(volDests) > 0 {
		// #40: the bulk transfer starts HERE, and everything this backup needs to
		// know about the container was read above — the inspect, the image config,
		// the mounts, the findings, the database dump. R5 §6 measured why the order
		// matters: with a 58 GB stream running through the socket proxy "a plain
		// `docker inspect` call timed out after 120 seconds… all API access to the
		// source is effectively lost for the duration". A metadata read moved below
		// this line would not be slow, it would fail. Pinned by
		// TestIntrospectionPrecedesBulkTransfer.
		if verr := e.captureVolumes(ctx, cli, opts, man, work, volDests, id, name, driftBefore); verr != nil {
			return id, e.fail(b, fmt.Errorf("volume archive: %w", verr))
		}
		// F22: capture any SQLite databases under the selected mounts as CONSISTENT
		// snapshots (sqlite3 online backup) alongside the raw copy, so a live WAL/
		// journal can't leave a torn database in the backup. Fail-safe: the raw file
		// is already in the volume payload, so any snapshot failure just falls back to
		// it. Runs on the changed-set only for a delta — a SQLite file that didn't
		// change isn't re-snapshotted; the newest generation that touched it holds it.
		sqerr := e.snapshotSQLite(ctx, cli, opts.ContainerID, volDests, work, man, id)
		// Snapshot captured — bring the app back up NOW; the rest of the pipeline
		// (compress/encrypt/store/verify) works from the spooled file. The resume
		// happens BEFORE acting on a corruption verdict (F116): an app must never be
		// left paused because its database turned out to be damaged.
		doResume()
		if sqerr != nil {
			return id, e.fail(b, sqerr)
		}
		// F143: record the TLS certificates that travelled with the data — what
		// they are and when they expire — so an operator can tell a restorable
		// copy from one whose certificate died months ago. Read-only, and only for
		// applications that declare certificate directories, so it costs nothing
		// for everything else. Runs after the resume: these are static files, and
		// nothing is gained by holding the app down for them.
		e.inventoryCertificates(ctx, cli, opts.ContainerID, insp.Config.Image, man, id)
		// F147: and the permissions of files whose exposure matters — a private
		// key left world-readable is invisible precisely because everything works.
		// Reads modes only; the files themselves are never opened.
		e.auditSecretFiles(ctx, cli, opts.ContainerID, insp.Config.Image, man, id)
		// F157: leftovers from a write that was interrupted, still sitting in the
		// tree. Reported, never removed — a backup tool does not get to decide
		// that something which looks like debris is.
		e.scanStagingDebris(ctx, cli, opts.ContainerID, insp.Config.Image, volDests, man, id)
	} else {
		e.logf(id, "INFO", "No named volumes to archive")
	}

	// 4-7) Shared tail: pack -> encrypt -> store -> mirror -> verify -> retention.
	return e.storeAndVerify(ctx, id, nodeName, stack, name, b, &man, opts, work, algo, zlevel, zwindow, uncompressedSel, start)
}

// volumeTargetPrefix marks a backup whose target is a standalone named volume
// rather than a container (F23): TargetName is "volume:<name>".
const volumeTargetPrefix = "volume:"

// runVolumeOnly backs up a STANDALONE named volume — data left behind by a removed
// container (F23). It tars the volume's contents via the sidecar into volumes.tar,
// records a single VolumeRef + TargetName "volume:<name>", then runs the same
// encrypt/store/verify/mirror/retention tail as a container backup.
// volumeSelfRef describes a standalone volume backup's own volume (F23) —
// including, since F226, its driver, options and LABELS.
//
// Those were not recorded, so restoring an orphan volume produced a bare local
// volume: the wrong backing store for anything on NFS or CIFS, and — for a
// volume that once belonged to a compose project — one without the
// com.docker.compose.* labels, which `docker compose up` then refuses to adopt.
// Best-effort: a volume that cannot be inspected still yields its name, which is
// exactly what was recorded before.
func volumeSelfRef(ctx context.Context, cli *client.Client, vol string) VolumeRef {
	ref := VolumeRef{Name: vol, Type: "volume", Destination: "/"}
	if cli == nil {
		return ref
	}
	if drv, opts, labels, err := dockercli.InspectVolume(ctx, cli, vol); err == nil {
		ref.Driver = drv
		ref.Options, ref.OptionsRedacted = dockercli.RedactVolumeOptions(opts)
		ref.Labels = labels
	}
	return ref
}

func (e *Engine) runVolumeOnly(ctx context.Context, nodeName string, opts Options, id string, b *store.Backup, start time.Time) (string, error) {
	vol := opts.VolumeOnly
	name := volumeTargetPrefix + vol
	b.TargetName = name
	if err := e.Store.CreateBackup(b); err != nil {
		return id, err
	}
	if opts.Label != "" {
		b.Label = opts.Label
		_ = e.Store.SetBackupLabel(id, opts.Label)
	}
	e.logf(id, "INFO", "Starting backup of standalone volume %q on node %s", vol, nodeName)

	cli, err := e.Reg.Get(opts.NodeID)
	if err != nil {
		return id, e.fail(b, err)
	}
	// Pre-flight: the encryption key must be present (mirrors the container path's
	// key check) — fail fast before spooling.
	if len(e.MasterKey()) != 32 {
		return id, e.fail(b, fmt.Errorf("encryption key not configured"))
	}
	if free, ferr := e.Storage.FreeBytes(ctx); ferr == nil && free > 0 {
		e.logf(id, "INFO", "Destination free space: %s", humanBytes(int64(free)))
	}

	algo, zlevel, zwindow, compLabel := parseCompression(opts.Compression)
	man := &Manifest{
		Version:        ManifestVersion,
		BackupID:       id,
		CreatedAt:      nowRFC3339(),
		NodeID:         opts.NodeID,
		NodeName:       nodeName,
		TargetName:     name,
		KeyFingerprint: e.MasterKeyFP(),
		Format: Format{
			Encryption:  "AES-256-GCM (DBACKv1 chunked)",
			Compression: compLabel,
			Algorithm:   algo,
			Archive:     "tar",
			Layout:      "manifest.json, volumes.tar (standalone named volume, contents-relative)",
		},
		Volumes: []VolumeRef{volumeSelfRef(ctx, cli, vol)},
	}

	work, err := os.MkdirTemp(e.WorkDir, "dback-"+id+"-*")
	if err != nil {
		return id, e.fail(b, err)
	}
	defer os.RemoveAll(work)

	// Capture the volume's contents via a read-only sidecar into volumes.tar.
	e.logf(id, "INFO", "Archiving volume %q via sidecar", vol)
	rc, terr := dockercli.TarNamedVolume(ctx, cli, vol)
	if terr != nil {
		return id, e.fail(b, fmt.Errorf("volume archive: %w", terr))
	}
	volTar := filepath.Join(work, "volumes.tar")
	f, cerr := os.Create(volTar)
	if cerr != nil {
		rc.Close()
		return id, e.fail(b, cerr)
	}
	h := sha256.New()
	uncompressed, werr := io.Copy(io.MultiWriter(f, h), ctxReader(ctx, rc))
	f.Close()
	rc.Close()
	if werr != nil {
		return id, e.fail(b, fmt.Errorf("volume archive: %w", werr))
	}
	man.VolumesSHA256 = fmt.Sprintf("%x", h.Sum(nil))
	e.logf(id, "INFO", "Volume captured (%s)", humanBytes(uncompressed))

	// Shared tail — stack is empty for a standalone volume.
	return e.storeAndVerify(ctx, id, nodeName, "", name, b, &man, opts, work, algo, zlevel, zwindow, uncompressed, start)
}

// storeAndVerify is the container-agnostic backup tail shared by a normal container
// backup (Run) and a standalone-volume backup (runVolumeOnly, F23): it packs the
// spooled work dir into the encrypted archive, writes the manifest sidecar, mirrors
// to offsite destinations, records the backup row, runs always-on verification, and
// applies retention. name is the display/target name ("<container>" or
// "volume:<vol>"); stack scopes the storage-key folder ("" for a volume).
func (e *Engine) storeAndVerify(ctx context.Context, id, nodeName, stack, name string, b *store.Backup, manp **Manifest, opts Options, work string, algo string, zlevel zstd.EncoderLevel, zwindow int, uncompressedSel int64, start time.Time) (string, error) {
	man := *manp
	// #37: carry the measured selection into the manifest, so a restore can size
	// its payload from what was actually there instead of guessing backwards
	// from the compressed archive.
	man.SelectionBytes = uncompressedSel
	// 4) Assemble outer tar -> zstd -> AES-256-GCM -> storage (streaming).
	key := backupKey(nodeName, stack, name, id)
	man.ArchiveKey = key
	e.logf(id, "INFO", "Compressing (zstd) + encrypting (AES-256-GCM) and storing")

	cipherSHA, size, err := e.packEncryptStore(ctx, work, man, key, algo, zlevel, zwindow)
	if err != nil {
		// A partial object under this backup's own (unique) key is junk — nothing
		// else can ever reference it. Sweep it so a canceled/failed run doesn't
		// leave dead bytes on the storage volume.
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = e.Storage.Delete(dctx, key)
		dcancel()
		if ctx.Err() != nil {
			e.logf(id, "WARN", "Backup canceled while packing/storing — the partial archive was removed")
		}
		return id, e.fail(b, fmt.Errorf("pack/encrypt/store: %w", err))
	}
	man.CipherSHA256, man.CipherSize = cipherSHA, size

	// 5) Manifest sidecar beside the archive (PLAN §9.3).
	manBytes, _ := json.MarshalIndent(man, "", "  ")
	if err := e.writeManifestSidecar(ctx, e.Storage, key, manBytes); err != nil {
		return id, e.fail(b, fmt.Errorf("writing manifest sidecar: %w", err))
	}

	// Mirror to enabled external destinations (offsite copies, PLAN §9.1).
	// Best-effort: a failed mirror never fails the (verified) local backup.
	locs := []Location{{Kind: "local", Name: "local", Type: "local"}}
	// Automatic path: honour each destination's upload window (overrideWindow=false).
	locs = append(locs, e.mirror(ctx, id, key, manBytes, opts.Destinations, opts.DestinationsExplicit, false)...)

	// Surface a degraded result: the backup is a verified-good local copy, but one
	// or more intended offsite copies didn't make it (3-2-1 not satisfied). These
	// failed destinations are recorded on the backup so the UI can flag it; they
	// are retried on the next run (PLAN §6.5/§9.1).
	var okExternal, failedExternal, deferredExternal int
	for _, l := range locs {
		if l.Kind != "dest" {
			continue
		}
		switch l.Status {
		case "failed":
			failedExternal++
		case "deferred": // outside its upload window (F14) — postponed, not a failure
			deferredExternal++
		default:
			okExternal++
		}
	}
	if deferredExternal > 0 {
		e.logf(id, "INFO", "%d offsite destination(s) deferred to their upload window — this backup is verified locally and will be mirrored when the window opens (or via Send offsite now).", deferredExternal)
	}
	if failedExternal > 0 {
		e.logf(id, "WARN", "DEGRADED offsite copies: %d destination(s) failed — this backup has %d good copy/copies (local + %d offsite). Not fully protected (3-2-1); will retry next run.",
			failedExternal, 1+okExternal, okExternal)
		// Operational alert (PLAN §9.15): the local copy is good but 3-2-1 isn't
		// satisfied — surface it instead of leaving it in the log only. Throttled
		// server-side so a persistent outage doesn't alert on every backup.
		e.notify(notify.KindNoOffsite, "Offsite copy incomplete: "+name,
			fmt.Sprintf("%s on %s backed up and verified locally, but %d offsite destination(s) failed — not fully protected (3-2-1). Will retry next run.", name, nodeName, failedExternal))
	}

	// If the run was canceled (e.g. during a slow mirror), stop here so the
	// caller marks it canceled rather than success.
	if err := ctx.Err(); err != nil {
		return id, err
	}

	b.Status = "success"
	b.SizeBytes = size
	b.CipherSHA256 = cipherSHA
	b.StorageKey = key
	// F17: learn this container's actual compressed-vs-selected ratio so the next
	// backup's free-space pre-flight uses observed compressibility, not a fixed
	// preset constant. Only when the uncompressed selection was measured (volume
	// backups); DB-only/app-export runs have no measured selection to learn from.
	if uncompressedSel > 0 {
		e.recordRatio(opts.NodeID, name, uncompressedSel, size)
	}
	b.LocationsJSON = mustJSON(locs)
	b.ManifestJSON = string(manBytes)
	b.CompletedAt = time.Now().Unix()
	b.DurationMs = time.Since(start).Milliseconds() // F28: wall-clock run time
	if err := e.Store.UpdateBackup(b); err != nil {
		return id, err
	}
	e.logf(id, "INFO", "Backup stored: %s (%s)", key, humanBytes(size))

	// F69 ransomware tripwire: a suspect delta flags the row and freezes this
	// target's retention, so clean pre-event generations can't be pruned away
	// while a schedule keeps capturing encrypted garbage. The API layer raises
	// the critical alert off the stored flag; the operator clears the hold in
	// the UI after review.
	if man.Suspect != "" {
		_ = e.Store.SetBackupSuspect(id, man.Suspect)
		_ = e.Store.SetSetting(RetentionHoldKey(opts.NodeID, name),
			man.Suspect+"|"+strconv.FormatInt(time.Now().Unix(), 10))
		e.logf(id, "WARN", "Retention for %s is ON HOLD after a possible mass-change event — review its backups, then clear the hold on the container page", name)
	}

	// 6) Always-on verification (PLAN §4.3 — non-negotiable).
	e.logf(id, "INFO", "Verifying backup (always-on)")
	rep := e.Verify(ctx, b, man, "local")
	b.VerificationJSON = mustJSON(rep)
	// Embed the report so the manifest is the full contract (PLAN §4.12): update
	// the manifest-of-record (DB) and drop a verification sidecar beside the local
	// archive. The immutable signed sidecar (§3.5) stays as written at capture.
	man.Verification = rep
	if mb, err := json.MarshalIndent(man, "", "  "); err == nil {
		b.ManifestJSON = string(mb)
	}
	if _, err := e.Storage.Put(ctx, key+".verification.json", strings.NewReader(mustJSON(rep))); err != nil {
		e.logf(id, "WARN", "Could not write verification sidecar: %v", err)
	}
	if rep.OK {
		b.Verified = "verified"
		b.LastVerifiedAt = time.Now().Unix() // seeds the scrub "last verified" clock (§9.4)
		e.logf(id, "INFO", "Verification PASSED")
		e.notify(notify.KindBackupSuccess, "Backup verified: "+name, fmt.Sprintf("%s on %s — %s, verified OK.", name, nodeName, humanBytes(size)))
	} else {
		b.Verified = "failed"
		e.logf(id, "ERR", "Verification FAILED: %s", rep.Summary())
		// The headline alert (PLAN §4.10): a backup exists but did NOT verify.
		e.notify(notify.KindVerifyFailed, "VERIFICATION FAILED: "+name, fmt.Sprintf("%s on %s did not pass verification — do NOT trust this backup. %s", name, nodeName, rep.Summary()))
	}
	_ = e.Store.UpdateBackup(b)

	// 7) Enforce the user's retention policy across ALL locations (PLAN §4.6):
	//    keep only the newest N successful backups of this target.
	if opts.retentionSweepWanted(b.Verified) {
		e.applyRetention(ctx, opts.NodeID, name)
	}
	return id, nil
}

// Scrub re-verifies an already-stored backup (PLAN §9.4) — re-reads the archive,
// re-checks the ciphertext hash, decrypts, walks the tar and checks the manifest
// — catching bit-rot or a destination that silently went bad since creation. It
// updates the verified state + last-verified timestamp and alerts on a
// regression. Returns the report. Read-only; never mutates the stored archive.
// `source` selects which copy to re-verify (F44): ""/"local" → the local archive,
// a destination ID → that destination's copy. The result is stamped onto that
// copy's Location (VerifiedAt/VerifyOK); only a LOCAL scrub also updates the
// backup's global verified badge + last-verified clock, so the UI list badge keeps
// its existing meaning. A failed scrub names the exact copy in its alert.
func (e *Engine) Scrub(ctx context.Context, b *store.Backup, source string) *VerificationReport {
	man := &Manifest{}
	_ = json.Unmarshal([]byte(b.ManifestJSON), man)
	label := e.locationLabel(source, b)
	rep := e.Verify(ctx, b, man, source)
	now := time.Now().Unix()

	// Stamp the per-copy result (all cases).
	e.recordLocationVerify(b, source, now, rep.OK)
	// A local scrub also drives the legacy global verified/last-verified fields
	// (unchanged UI-list semantics). A destination scrub touches ONLY its location.
	if source == "" || source == "local" {
		if rep.OK {
			b.Verified = "verified"
		} else {
			b.Verified = "failed"
		}
		b.VerificationJSON = mustJSON(rep)
		b.LastVerifiedAt = now
	}
	_ = e.Store.UpdateBackup(b)

	if rep.OK {
		e.logf(b.ID, "INFO", "Scrub OK: %s (%s copy) still verifies", b.TargetName, label)
	} else {
		e.logf(b.ID, "ERR", "Scrub FAILED: %s (%s copy) no longer verifies — %s", b.TargetName, label, rep.Summary())
		e.notify(notify.KindScrubFailed, "SCRUB FAILED: "+b.TargetName+" ("+label+")",
			fmt.Sprintf("The %s copy of %s no longer verifies (possible bit-rot or a destination gone bad). %s", label, b.TargetName, rep.Summary()))
	}
	return rep
}

// Offsite upload guarding (PLAN §4.5/§9.1). Instead of a fixed total cap (which
// wrongly kills a slow-but-steady large upload — a 10 GB Jellyfin config over a
// home upstream legitimately takes a while), we abort only when an upload makes
// NO progress for stallTimeout. So large scheduled backups complete as long as
// bytes keep moving, while a genuinely hung/dead target still fails fast.
const (
	stallTimeout     = 10 * time.Minute // abort if no upload progress for this long
	progressInterval = 60 * time.Second // progress-log + stall-check cadence
)

// Mirror retry/back-off for transient offsite failures (proxy 5xx, dropped
// connections) so a momentary blip doesn't lose an offsite copy for the run
// (PLAN §4.5). WebDAV/Nextcloud also retries at the chunk level, so this
// whole-file retry rarely re-uploads in practice.
const (
	maxMirrorAttempts  = 3
	mirrorRetryBackoff = 5 * time.Second
)

// progressReader tracks bytes read and the time of the last read, so the upload
// guard can distinguish a slow-but-live transfer from a stalled one.
// throttleChunk bounds each throttled read so a single WaitN never exceeds the
// limiter's burst (PLAN §9.9).
const throttleChunk = 32 << 10 // 32 KiB

// NewUploadLimiter builds the shared aggregate upload limiter from a megabits/sec
// cap, or nil when mbps<=0 (unlimited). Burst is one second of bytes (min one
// chunk) so steady throughput is allowed without starving on the first read.
func NewUploadLimiter(mbps int) *rate.Limiter {
	if mbps <= 0 {
		return nil
	}
	bytesPerSec := float64(mbps) * 1_000_000 / 8 // megabits/sec -> bytes/sec
	burst := int(bytesPerSec)
	if burst < throttleChunk {
		burst = throttleChunk
	}
	return rate.NewLimiter(rate.Limit(bytesPerSec), burst)
}

// uploadPolicy is a destination's optional per-target throttle + allowed upload
// window (F14), parsed from its sealed config map. Zero values = no restriction.
type uploadPolicy struct {
	Mbps  int    // per-destination cap in megabits/sec (0 = unlimited)
	Start string // "HH:MM" local, inclusive window start ("" = no window)
	End   string // "HH:MM" local, exclusive window end   ("" = no window)
}

// parseUploadPolicy reads the per-destination upload policy from a config map.
func parseUploadPolicy(cfg map[string]string) uploadPolicy {
	p := uploadPolicy{Start: strings.TrimSpace(cfg["upload_window_start"]), End: strings.TrimSpace(cfg["upload_window_end"])}
	if n, err := strconv.Atoi(strings.TrimSpace(cfg["max_upload_mbps"])); err == nil && n > 0 {
		p.Mbps = n
	}
	return p
}

// parseHHMM parses "HH:MM" into minutes-of-day; ok=false on any malformed value.
func parseHHMM(s string) (int, bool) {
	var h, m int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &h, &m); err != nil {
		return 0, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// inUploadWindow reports whether now falls inside [start,end) (local time).
// An unset or malformed/degenerate window means "always allowed". Supports a
// wrap-around window like 22:00–06:00 (F14).
func inUploadWindow(start, end string, now time.Time) bool {
	s, ok1 := parseHHMM(start)
	e, ok2 := parseHHMM(end)
	if !ok1 || !ok2 || s == e {
		return true // no window / degenerate → unrestricted
	}
	cur := now.Hour()*60 + now.Minute()
	if s < e {
		return cur >= s && cur < e
	}
	return cur >= s || cur < e // wrap-around (e.g. 22:00–06:00)
}

// NewSharedUploadLimiter returns the engine's always-present aggregate upload
// limiter, initialised to unlimited (rate.Inf). It's tuned live via
// SetUploadLimit; keeping it non-nil lets the cap be changed at runtime without a
// racy nil↔limiter pointer swap (F15).
func NewSharedUploadLimiter() *rate.Limiter { return rate.NewLimiter(rate.Inf, throttleChunk) }

// uploadMbps reads the shared aggregate cap back in megabits/sec (0 = unlimited)
// — the inverse of SetUploadLimit, for effective-cap reporting (F76).
func (e *Engine) uploadMbps() int {
	if e.UpLimiter == nil || e.UpLimiter.Limit() == rate.Inf {
		return 0
	}
	return int(float64(e.UpLimiter.Limit()) * 8 / 1_000_000)
}

// effectiveMbps resolves the cap that actually governs one transfer (F76): the
// tighter of the global aggregate and the destination's own cap; 0 on either
// side means that side is unlimited. Pure — the limiters themselves are chained
// in the upload path, so this is the REPORTING view of the same composition.
func effectiveMbps(global, dest int) int {
	switch {
	case global <= 0 && dest <= 0:
		return 0
	case global <= 0:
		return dest
	case dest <= 0:
		return global
	case dest < global:
		return dest
	default:
		return global
	}
}

// SetUploadLimit updates the shared aggregate upload cap in place (megabits/sec;
// <=0 = unlimited). Safe to call at runtime — rate.Limiter guards its own state,
// and in-flight uploads simply pick up the new rate as they read on (F15).
func (e *Engine) SetUploadLimit(mbps int) {
	if e.UpLimiter == nil {
		e.UpLimiter = NewSharedUploadLimiter()
	}
	if mbps <= 0 {
		e.UpLimiter.SetLimit(rate.Inf)
		return
	}
	bytesPerSec := float64(mbps) * 1_000_000 / 8
	burst := int(bytesPerSec)
	if burst < throttleChunk {
		burst = throttleChunk
	}
	e.UpLimiter.SetBurst(burst)
	e.UpLimiter.SetLimit(rate.Limit(bytesPerSec))
}

// throttle wraps r with the shared upload limiter when a finite cap is set, so the
// aggregate offsite egress across all concurrent backups stays under the cap
// (PLAN §9.9). Unlimited (nil or rate.Inf) returns r unwrapped for a fast path.
// ctx bounds the wait so a canceled backup doesn't block on tokens.
func (e *Engine) throttle(ctx context.Context, r io.Reader) io.Reader {
	if e.UpLimiter == nil || e.UpLimiter.Limit() == rate.Inf {
		return r
	}
	return throttleWith(ctx, r, e.UpLimiter)
}

// throttleWith wraps r with a token-bucket limiter (nil = unlimited). Chaining
// two — the shared aggregate cap plus a per-destination cap (F14) — gates the
// same bytes through both, so the tighter of the two wins.
func throttleWith(ctx context.Context, r io.Reader, lim *rate.Limiter) io.Reader {
	if lim == nil {
		return r
	}
	return &throttledReader{r: r, lim: lim, ctx: ctx}
}

// throttledReader rate-limits reads against a shared token-bucket limiter.
type throttledReader struct {
	r   io.Reader
	lim *rate.Limiter
	ctx context.Context
}

func (t *throttledReader) Read(b []byte) (int, error) {
	if len(b) > throttleChunk {
		b = b[:throttleChunk]
	}
	n, err := t.r.Read(b)
	if n > 0 {
		if werr := t.lim.WaitN(t.ctx, n); werr != nil && err == nil {
			err = werr // ctx canceled/expired while waiting for tokens
		}
	}
	return n, err
}

type progressReader struct {
	r        io.Reader
	n        int64 // atomic: total bytes read
	lastNano int64 // atomic: unix-nanos of the last non-empty read
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		atomic.AddInt64(&p.n, int64(n))
		atomic.StoreInt64(&p.lastNano, time.Now().UnixNano())
	}
	return n, err
}
func (p *progressReader) bytes() int64 { return atomic.LoadInt64(&p.n) }
func (p *progressReader) idle() time.Duration {
	return time.Since(time.Unix(0, atomic.LoadInt64(&p.lastNano)))
}

// uploadGuard logs upload progress every progressInterval and cancels the upload
// if it makes no progress for stallTimeout (setting *stalled). It exits when the
// upload finishes (done) or the run is canceled.
func (e *Engine) uploadGuard(ctx context.Context, cancel context.CancelFunc, pr *progressReader, total int64, name, id string, done <-chan struct{}, stalled *int32) {
	t := time.NewTicker(progressInterval)
	defer t.Stop()
	start := time.Now()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			if pr.idle() > stallTimeout {
				atomic.StoreInt32(stalled, 1)
				e.logf(id, "ERR", "Mirror to %s: no progress for %s — aborting (link too slow or target unresponsive; local copy is safe)", name, stallTimeout)
				cancel()
				return
			}
			n := pr.bytes()
			rate := int64(float64(n) / time.Since(start).Seconds())
			if total > 0 {
				e.logf(id, "INFO", "Mirror to %s: %s / %s (%d%%) at %s/s", name, humanBytes(n), humanBytes(total), 100*n/total, humanBytes(rate))
			} else {
				e.logf(id, "INFO", "Mirror to %s: %s uploaded at %s/s", name, humanBytes(n), humanBytes(rate))
			}
		}
	}
}

// mirrorFailReason converts an upload failure into a short, human reason for the
// backups list (e.g. "no progress (stalled)", "server error (502)").
func mirrorFailReason(err error, stalled bool) string {
	if stalled {
		return "no progress (stalled)"
	}
	if err == nil {
		return "failed"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "413"):
		return "rejected: file too large for server (413)"
	case strings.Contains(s, "401") || strings.Contains(s, "403") || strings.Contains(s, "credential") || strings.Contains(s, "auth"):
		return "authentication/permission denied"
	case strings.Contains(s, "502") || strings.Contains(s, "503") || strings.Contains(s, "504"):
		return "server/proxy error (5xx)"
	case strings.Contains(s, "no such host") || strings.Contains(s, "connection refused") || strings.Contains(s, "timeout") || strings.Contains(s, "tls"):
		return "unreachable (network/TLS)"
	case strings.Contains(s, "quota") || strings.Contains(s, "insufficient") || strings.Contains(s, "space"):
		return "out of space on destination"
	}
	// Fall back to a trimmed single-line form of the error.
	msg := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	if len(msg) > 80 {
		msg = msg[:77] + "…"
	}
	return msg
}

// isTransientErr reports whether a mirror error is worth retrying. Timeouts and
// cancellation are NOT retried (a too-slow target won't get faster, and a
// canceled run should stop).
// IsTransient reports whether a backup error is a transient failure worth an
// automatic retry (F26) — a brief node/DB/registry blip — rather than a permanent
// problem (bad selection, out of space, a user cancel/timeout). It reuses the exact
// classifier the offsite mirror already uses for its own retry (PLAN §4.5), so the
// backup and mirror layers agree on what "transient" means.
func IsTransient(err error) bool { return isTransientErr(err) }

func isTransientErr(err error) bool {
	if err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, sub := range []string{"502", "503", "504", "429", "connection reset", "broken pipe", "unexpected eof", "connection refused", "i/o timeout", "tls handshake"} {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// mirror copies the stored archive + manifest to every enabled external
// destination (PLAN §9.1). Best-effort and isolated per destination; returns
// the locations successfully written so they're recorded on the backup. Each
// destination is bounded by mirrorTimeout and honors run cancellation, and
// every step is logged so the user can see exactly where a mirror is.
// overrideWindow ignores per-destination upload windows (a deliberate on-demand
// "Send offsite now" pushes now regardless); the automatic backup-run path passes
// false so a metered target's window is honoured (F14).
func (e *Engine) mirror(ctx context.Context, id, key string, manBytes []byte, sel []string, explicit, overrideWindow bool) []Location {
	var written []Location
	dests, err := e.Store.ListDestinations()
	if err != nil || len(dests) == 0 {
		return written
	}
	selSet := map[string]bool{}
	for _, s := range sel {
		selSet[s] = true
	}
	// Archive size for progress reporting (0 = unknown).
	var totalSize int64
	if n, ok, serr := e.Storage.Stat(ctx, key); serr == nil && ok {
		totalSize = n
	}
	for _, d := range dests {
		if !d.Enabled {
			continue
		}
		if explicit && !selSet[d.ID] {
			continue // user chose a specific set; this one isn't in it
		}
		// Stop entirely if the backup was canceled.
		if ctx.Err() != nil {
			e.logf(id, "INFO", "Mirroring canceled")
			return written
		}
		failedLoc := Location{Kind: "dest", DestID: d.ID, Name: d.Name, Type: d.Type, Status: "failed"}

		cfg, err := e.destConfig(d)
		if err != nil {
			e.logf(id, "ERR", "Mirror to %s: %v", d.Name, err)
			_ = e.Store.SetDestinationStatus(d.ID, "error")
			failedLoc.Detail = mirrorFailReason(err, false)
			written = append(written, failedLoc)
			continue
		}
		policy := parseUploadPolicy(cfg)

		// Upload window (F14): outside its window, defer this destination instead of
		// uploading — recorded as a "deferred" (not failed) location, retried by the
		// next in-window run or an explicit "Send offsite now". A deliberate on-demand
		// push (overrideWindow) uploads regardless.
		if !overrideWindow && !inUploadWindow(policy.Start, policy.End, time.Now()) {
			e.logf(id, "INFO", "Deferring mirror to %s — now is outside its upload window (%s–%s)", d.Name, policy.Start, policy.End)
			written = append(written, Location{Kind: "dest", DestID: d.ID, Name: d.Name, Type: d.Type, Status: "deferred",
				Detail: fmt.Sprintf("outside upload window %s–%s", policy.Start, policy.End)})
			continue
		}

		backend, err := storage.NewFromConfig(d.Type, cfg)
		if err != nil {
			e.logf(id, "ERR", "Mirror to %s: %v", d.Name, err)
			_ = e.Store.SetDestinationStatus(d.ID, "error")
			failedLoc.Detail = mirrorFailReason(err, false)
			written = append(written, failedLoc)
			continue
		}
		// Per-destination rate limiter (F14), chained inside the shared aggregate
		// cap. The log reports the EFFECTIVE cap — min(global, destination) — so
		// "global 10, destination 20" honestly reads as 10 (F76).
		perLim := NewUploadLimiter(policy.Mbps)
		if eff := effectiveMbps(e.uploadMbps(), policy.Mbps); eff > 0 {
			e.logf(id, "INFO", "Mirroring to %s (%s), capped at %d Mbit/s…", d.Name, d.Type, eff)
		} else {
			e.logf(id, "INFO", "Mirroring to %s (%s)…", d.Name, d.Type)
		}
		start := time.Now()

		// Upload with bounded retry/back-off on transient failures (PLAN §4.5) and
		// a no-progress (stall) guard instead of a fixed cap, so a slow-but-steady
		// large upload completes while a hung target still fails (PLAN §4.5/§9.1).
		var perr error
		var stalled int32
		backoff := mirrorRetryBackoff
		for attempt := 1; attempt <= maxMirrorAttempts; attempt++ {
			atomic.StoreInt32(&stalled, 0)
			dctx, cancel := context.WithCancel(ctx)
			rc, gerr := e.Storage.Get(dctx, key)
			if gerr != nil {
				cancel()
				perr = fmt.Errorf("reading local copy: %w", gerr)
				break // local read failure isn't a transient remote issue
			}
			// Throttle the offsite upload against the shared aggregate cap (PLAN
			// §9.9) AND this destination's own cap (F14); the progress/stall guard
			// then measures the real (throttled) rate.
			pr := &progressReader{r: throttleWith(dctx, e.throttle(dctx, rc), perLim)}
			atomic.StoreInt64(&pr.lastNano, time.Now().UnixNano())
			done := make(chan struct{})
			go e.uploadGuard(dctx, cancel, pr, totalSize, d.Name, id, done, &stalled)
			_, perr = backend.Put(dctx, key, pr)
			close(done)
			rc.Close()
			if perr == nil {
				// F78: seal the sidecar per this destination's preference (metadata
				// privacy where an untrusted third party holds the bytes).
				if merr := e.writeManifestSidecarSealed(dctx, backend, key, manBytes, e.sealForDest(cfg)); merr != nil {
					e.logf(id, "ERR", "Mirror to %s manifest failed: %v", d.Name, merr)
				}
				cancel()
				break
			}
			cancel()
			if ctx.Err() != nil { // the whole backup was canceled by the user
				e.logf(id, "INFO", "Mirror to %s canceled", d.Name)
				return written
			}
			if atomic.LoadInt32(&stalled) == 1 {
				break // stall already logged; a dead/too-slow link won't recover this run
			}
			if attempt < maxMirrorAttempts && isTransientErr(perr) {
				e.logf(id, "INFO", "Mirror to %s failed (%v) — retry %d/%d in %s", d.Name, perr, attempt, maxMirrorAttempts-1, backoff)
				select {
				case <-ctx.Done():
					e.logf(id, "INFO", "Mirror to %s canceled", d.Name)
					return written
				case <-time.After(backoff):
				}
				backoff *= 3
				continue
			}
			break // permanent error, or out of attempts
		}

		if perr != nil {
			if atomic.LoadInt32(&stalled) == 0 { // a stall already logged its own ERR
				e.logf(id, "ERR", "Mirror to %s failed: %v", d.Name, perr)
			}
			_ = e.Store.SetDestinationStatus(d.ID, "error")
			failedLoc.Detail = mirrorFailReason(perr, atomic.LoadInt32(&stalled) == 1)
			written = append(written, failedLoc)
			continue
		}
		_ = e.Store.SetDestinationStatus(d.ID, "active")
		loc := Location{Kind: "dest", DestID: d.ID, Name: d.Name, Type: d.Type}
		// Record WORM/object-lock so the UI can badge it and prune can retain it
		// while the lock is live (PLAN §9.1).
		if im, ok := backend.(storage.Immutabler); ok && im.Immutable() {
			until := time.Now().Add(time.Duration(im.LockDays()) * 24 * time.Hour)
			// F97: a filesystem-like destination has to be TOLD to lock — S3 applies
			// Object Lock server-side at write time, but local/SMB/SFTP need an
			// explicit post-write pass over the archive and its sidecars.
			//
			// The location is only marked immutable if that pass SUCCEEDS. Recording
			// a lock that was never applied would make prune skip a copy nothing is
			// actually protecting — a false sense of safety, which is worse here than
			// no lock at all.
			locked := true
			if rl, ok := backend.(storage.RetentionLocker); ok {
				if lerr := e.applyRetentionLock(ctx, rl, key, until); lerr != nil {
					e.logf(id, "WARN", "Mirrored to %s but could not apply its retention lock (%v) — this copy is NOT protected and prune may remove it on schedule", d.Name, lerr)
					locked = false
				}
			}
			if locked {
				loc.Immutable = true
				loc.LockUntil = until.Unix()
				e.logf(id, "INFO", "Mirrored to %s (immutable / WORM-locked ~%dd)", d.Name, im.LockDays())
			}
		}
		written = append(written, loc)
		e.logf(id, "INFO", "Mirrored to %s in %s", d.Name, time.Since(start).Round(time.Second))
	}
	return written
}

// MirrorExisting re-runs ONLY the offsite mirror step for an already-stored,
// verified local backup — reusing the existing encrypted archive instead of
// re-capturing/compressing/encrypting/verifying. It completes a DEGRADED backup's
// 3-2-1 copies or pushes a local-only backup to a destination added later,
// without wasting resources (PLAN §9.1/§6.5). destIDs limits the mirror to a
// subset; empty means every enabled destination that doesn't already hold a good
// copy. Progress is logged under the backup id so the UI's log stream shows it.
func (e *Engine) MirrorExisting(ctx context.Context, backupID string, destIDs []string) error {
	b, err := e.Store.GetBackup(backupID)
	if err != nil {
		return err
	}
	if b.Status != "success" || b.StorageKey == "" {
		return fmt.Errorf("only a completed local backup can be mirrored")
	}
	// The mirror reads the local archive; make sure it's actually there.
	//
	// An UNANSWERED stat is not a missing archive (Backend.Stat): a blip on the
	// backups volume would otherwise be reported to the operator as their local
	// copy having disappeared. Log it and carry on — the mirror's own Get is the
	// real test, and it fails with the actual reason.
	_, ok, serr := e.Storage.Stat(ctx, b.StorageKey)
	if serr != nil {
		e.logf(backupID, "WARN", "Could not check the local archive before mirroring (%v) — continuing; the copy step will report any real problem", serr)
	}
	if serr == nil && !ok {
		return fmt.Errorf("the local archive for this backup is missing — cannot mirror")
	}

	var existing []Location
	_ = unmarshal(b.LocationsJSON, &existing)
	good := map[string]bool{}
	for _, l := range existing {
		if l.Kind == "dest" && !noData(l.Status) {
			good[l.DestID] = true // already holds a copy — don't re-upload (deferred/failed still need one)
		}
	}
	dests, _ := e.Store.ListDestinations()
	want := map[string]bool{}
	for _, id := range destIDs {
		want[id] = true
	}
	var sel []string
	for _, d := range dests {
		if !d.Enabled || good[d.ID] {
			continue
		}
		if len(destIDs) > 0 && !want[d.ID] {
			continue
		}
		sel = append(sel, d.ID)
	}
	if len(sel) == 0 {
		e.logf(backupID, "INFO", "Nothing to mirror — every enabled destination already has a copy of this backup")
		return fmt.Errorf("all enabled destinations already have a copy of this backup")
	}

	e.logf(backupID, "INFO", "Mirroring the existing local backup to %d destination(s) — reusing the stored archive, no re-capture", len(sel))
	// On-demand push: a deliberate user action, so override upload windows (F14).
	newLocs := e.mirror(ctx, backupID, b.StorageKey, []byte(b.ManifestJSON), sel, true, true)

	// Reload before saving so a concurrent scrub/verify's fields aren't clobbered;
	// only the locations are ours to update here.
	if fresh, gerr := e.Store.GetBackup(backupID); gerr == nil {
		b = fresh
		_ = unmarshal(b.LocationsJSON, &existing)
	}
	b.LocationsJSON = mustJSON(mergeLocations(existing, newLocs))
	if uerr := e.Store.UpdateBackup(b); uerr != nil {
		return uerr
	}

	okN, failN := 0, 0
	for _, l := range newLocs {
		if l.Kind != "dest" {
			continue
		}
		if l.Status == "failed" {
			failN++
		} else {
			okN++
		}
	}
	if failN == 0 {
		e.logf(backupID, "INFO", "Mirror complete — %d destination(s) now hold this backup", okN)
		return nil
	}
	e.logf(backupID, "WARN", "Mirror finished: %d succeeded, %d still failing — the local copy remains safe", okN, failN)
	return nil
}

// fail marks a backup failed and logs it. It deliberately does NOT emit the
// backup.failed notification: the queue (the sole caller of Run) owns that alert
// so a TRANSIENT failure that will be auto-retried stays quiet and only the final,
// exhausted or permanent failure notifies (F26). See jobqueue.runQueued.
func (e *Engine) fail(b *store.Backup, err error) error {
	b.Status = "failed"
	b.Error = err.Error()
	b.CompletedAt = time.Now().Unix()
	_ = e.Store.UpdateBackup(b)
	e.logf(b.ID, "ERR", "Backup failed: %v", err)
	return err
}

// excludeSubPaths lists paths to leave out of the volume tar.
//
// Two sources, and they mean different things:
//
//   - the embedded database's data directory (F126), armed only once a dump of
//     it has actually been taken — a raw copy beats nothing when there is no
//     dump to supersede it;
//   - regenerable directories the operator chose to drop (F132), which the
//     application rebuilds on its own;
//   - paths with no restore value at all (F138), always excluded.
func (o Options) excludeSubPaths() []string {
	var out []string
	if o.embeddedDataDir != "" {
		out = append(out, o.embeddedDataDir)
	}
	for _, r := range o.excludeRegenerable {
		if r.Path != "" {
			out = append(out, r.Path)
		}
	}
	out = append(out, o.neverBackup...)
	return out
}

// dumpDatabase runs the native dump tool inside the container and spools to a
// temp file (PLAN §4.1). Returns the DBDump manifest entry.
func (e *Engine) dumpDatabase(ctx context.Context, cli *client.Client, owner ContainerRef, id, engineKind string, env []string, work, service string, dbs []string) (DBDump, error) {
	return e.dumpDatabaseWith(ctx, cli, owner, id, engineKind, nil, env, work, service, dbs)
}

// dumpDatabaseWith is dumpDatabase with an optional EMBEDDED server description
// (F126). When embedded is non-nil the dump targets a database bundled inside an
// application's own container, which needs its own command: the standalone
// command dumps the whole cluster with pg_dumpall, and an embedded server's
// application user is routinely not a superuser, so that would fail on the
// globals and take the backup with it.
//
// The result is recorded as a per-database (subset) dump, which is exactly what
// it is — and that makes every downstream path already correct: the restore
// imports it into the RUNNING engine without wiping a data directory, which is
// the only sane thing to do when the "engine" is somebody's application.
func (e *Engine) dumpDatabaseWith(ctx context.Context, cli *client.Client, owner ContainerRef, id, engineKind string, embedded *EmbeddedDump, env []string, work, service string, dbs []string) (DBDump, error) {
	if service == "" {
		service = engineKind
	}
	dbs = cleanDBNames(dbs)
	fname := "db_" + sanitize(service) + dbExt(engineKind)
	out := filepath.Join(work, fname)
	f, err := os.Create(out)
	if err != nil {
		return DBDump{}, err
	}
	defer f.Close()

	// Per-database selection (F8) only applies to the SQL engines that dump per DB;
	// mongodb/redis always capture the whole instance.
	sel := dbs
	if engineKind != "postgres" && engineKind != "mysql" {
		sel = nil
	}
	if len(sel) > 0 {
		e.logf(id, "INFO", "Dumping only selected database(s): %s", strings.Join(sel, ", "))
	}
	cmd := dbDumpCommand(engineKind, env, sel)
	if embedded != nil {
		cmd = embeddedDumpCommand(embedded, env)
		// Recorded as a subset of exactly the database that was dumped, so the
		// restore takes the import-into-running-engine path rather than trying to
		// wipe and re-initialise an application's container.
		if embedded.DBName != "" {
			sel = []string{embedded.DBName}
		}
	}
	// F87: tally the dump AS IT IS WRITTEN — structure, completion marker and
	// SHA-256 — in the same pass that spools it to disk. No extra read, no second
	// implementation: this is the tally the restore path uses.
	tally := NewDumpTallyFor(engineKind)
	// F180: a Redis password given as `redis-server --requirepass X` exists only
	// in the container's recorded configuration — redis overwrites its own argv
	// with a process title, so nothing inside the container can still see it.
	// Read it from outside and hand it over through the exec's environment, which
	// keeps it off every command line.
	var execEnv []string
	if engineKind == "redis" && embedded == nil {
		// F205: a password the operator recorded for THIS container comes first.
		// It is the only thing that reaches a broker whose password was set at
		// runtime with CONFIG SET — invisible to the environment, to any file, and
		// to the command line, so without it that broker falls back to a raw file
		// capture on every single run. Resolved HERE, in the one function both the
		// ordinary and the app-consistent dump paths call, so the two cannot
		// diverge the way they did before F191.
		if insp, ierr := cli.ContainerInspect(ctx, id); ierr == nil {
			execEnv = redisDumpExecEnv(insp)
		}
		if pw := e.redisAuthEnv(owner.NodeID, owner.Name); len(pw) > 0 {
			// The recorded password REPLACES any REDISCLI_AUTH recovered from the
			// container's configuration rather than being listed alongside it.
			// Which of two duplicate entries an exec's environment ends up with is
			// not something to rely on, and the operator typed this one on purpose:
			// if it disagrees with a stale --requirepass in the recorded argv, the
			// operator is the one who knows which server is actually running. Any
			// REDIS_CONF hint survives — it points at a file, not a credential.
			execEnv = append(pw, withoutEnvKey(execEnv, "REDISCLI_AUTH")...)
			// The FACT, never the value. This line exists so a passing dump is
			// attributable to the recorded password rather than looking like luck.
			e.logf(id, "INFO", "Using the Redis password recorded for %q (sealed at rest, passed to redis-cli through the environment — never on a command line)", owner.Name)
		}
	}
	// #18: what each table's modification counter reads BEFORE the dump. Compared
	// after the hash pass, it says which tables were written to across the window
	// — and those are exactly the ones whose hash would not describe this dump.
	activityBefore := e.tableQuiescenceEvidence(ctx, cli, id, engineKind, embedded)
	if err := dockercli.ExecStreamEnv(ctx, cli, id, cmd, execEnv, io.MultiWriter(f, tally)); err != nil {
		return DBDump{}, err
	}
	exp := tally.Expect()

	// A dump that began as a pg_dump and never reached its completion marker was
	// CUT SHORT. Storing it would store a backup that cannot restore, so fail now
	// — while the source database is still right there — rather than let someone
	// discover it during a recovery. Only Postgres is gated: it is the engine whose
	// dumps carry a completion marker we can rely on.
	if engineKind == "postgres" && exp.Truncated() {
		return DBDump{}, fmt.Errorf(
			"the database dump ended before its completion marker — not storing an incomplete dump (got %s, %d primary key(s), %d foreign key(s); the dump command may have been killed, hit a disk limit, or lost its connection)",
			humanBytes(exp.Bytes), exp.PrimaryKeys, exp.ForeignKeys)
	}
	if exp.PrimaryKeys > 0 || exp.ForeignKeys > 0 {
		e.logf(id, "INFO", "Dump captured complete: %d primary key(s), %d foreign key(s), %s",
			exp.PrimaryKeys, exp.ForeignKeys, humanBytes(exp.Bytes))
	}
	// F99: MySQL declares its structure in the dump text, so the tally already has
	// it. MongoDB and Redis dump a BINARY stream that declares nothing, so their
	// expectation has to be asked of the live source — best-effort, right after the
	// snapshot, while the source is still there.
	switch engineKind {
	case "mysql":
		if exp.Tables > 0 {
			e.logf(id, "INFO", "Dump captured: %d table(s), %d insert statement(s), %s", exp.Tables, exp.Rows, humanBytes(exp.Bytes))
		}
	case "mongodb", "redis":
		e.recordLiveCounts(ctx, cli, id, engineKind, &exp)
	}

	fi, _ := f.Stat()
	dump := DBDump{
		Service: service, Engine: engineKind, Version: e.dbVersion(ctx, cli, id, engineKind),
		Path: "db/" + fname[len("db_"):], Bytes: fi.Size(),
		Extensions:      e.dbExtensions(ctx, cli, id, engineKind, env),
		Databases:       sel,
		DumpPrimaryKeys: exp.PrimaryKeys, DumpForeignKeys: exp.ForeignKeys,
		DumpComplete: exp.Complete, DumpSHA256: exp.SHA256,
		DumpTables: exp.Tables, DumpRows: exp.Rows,
		DumpCollections: exp.Collections, DumpKeys: exp.Keys, DumpVolatileKeys: exp.VolatileKeys,
	}
	// #39: the cluster the dump came out of, which the dump itself does not
	// describe. Recorded HERE rather than at the two call sites because both the
	// ordinary capture and the app-consistent one arrive through this function —
	// and the counts recorded at only one of those call sites are the standing
	// demonstration of what happens when a capture is wired per-path.
	e.recordPGClusterConfig(ctx, cli, id, embedded, &dump, id)
	// #18: a content baseline beside the dump, on the same exec channel and the
	// same credentials, so a restore can prove the ROWS came back and not merely
	// that there are the same number of them.
	e.recordTableHashes(ctx, cli, id, engineKind, embedded, &dump, id, activityBefore)
	return dump, nil
}

// recordLiveCounts asks a MongoDB or Redis container how much it holds, so the
// restore has something to check its import against (F99).
//
// Entirely best-effort: a count that cannot be taken leaves the expectation at
// zero, which the restore reads as "nothing to verify" rather than "zero
// expected". Failing a backup because a supplementary count did not run would
// trade a real backup for a diagnostic.
func (e *Engine) recordLiveCounts(ctx context.Context, cli *client.Client, id, engineKind string, exp *DumpExpect) {
	cmd := dbCountCmd(engineKind)
	if cmd == nil {
		return
	}
	out, err := dockercli.ExecCapture(ctx, cli, id, cmd)
	if err != nil {
		e.logf(id, "INFO", "Could not record a %s object count for this dump (%v) — the backup is fine; the restore will simply have nothing to cross-check against", engineKind, err)
		return
	}
	switch engineKind {
	case "mongodb":
		if n, ok := parseNameCount(string(out), "collections"); ok {
			exp.Collections = int(n)
			e.logf(id, "INFO", "Dump captured: %d collection(s), %s", n, humanBytes(exp.Bytes))
		}
	case "redis":
		if n, ok := parseNameCount(string(out), "keys"); ok {
			exp.Keys = n
			// F150: how many of them could expire on their own. Recorded in the
			// same breath, from the same INFO line, because without it a restore
			// cannot tell a time-to-live from data loss.
			v, _ := parseNameCount(string(out), "volatile")
			exp.VolatileKeys = v
			if v > 0 {
				e.logf(id, "INFO", "Dump captured: %d key(s) — %d with an expiry, which Redis will drop as it loads this snapshot if their time has passed — %s", n, v, humanBytes(exp.Bytes))
			} else {
				e.logf(id, "INFO", "Dump captured: %d key(s), %s", n, humanBytes(exp.Bytes))
			}
		}
	}
}

// dbExtensions best-effort captures a Postgres container's installed extension
// set ("name version" pairs) for the manifest, so a restore can flag an
// extension/vector incompatibility (e.g. pgvecto.rs vs VectorChord). Returns nil
// for non-Postgres engines or on any error — it never fails the backup.
func (e *Engine) dbExtensions(ctx context.Context, cli *client.Client, id, engine string, env []string) []string {
	if engine != "postgres" {
		return nil
	}
	out, err := dockercli.ExecCapture(ctx, cli, id, PGExtensionsCmd(env))
	if err != nil {
		return nil
	}
	return ParsePGExtensions(string(out))
}

// dbVersion best-effort captures the database engine's version string for the
// manifest (PLAN §4.12). Empty if it can't be determined.
func (e *Engine) dbVersion(ctx context.Context, cli *client.Client, id, engine string) string {
	cmd := DBVersionCmd(engine)
	if cmd == nil {
		return ""
	}
	out, err := dockercli.ExecCapture(ctx, cli, id, cmd)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line
}

// volumeIndexMember is the archive member holding a backup's COMPLETE post-backup
// volume file index (F61) — zstd-compressed JSON, diffed by the next incremental
// backup. Present on every full-or-delta captured with incremental mode on.
const volumeIndexMember = "volumes-index.json.zst"

// volumeDeltaMember is the archive member holding an incremental backup's
// CHANGED-FILES-ONLY tar (F61). Present instead of volumes.tar on a delta.
const volumeDeltaMember = "volumes-delta.tar"

// volPayloadMember returns the archive member that carries a backup's volume
// payload — the delta member for an incremental backup, else the full volumes.tar
// (F61). Shared by capture, verify, and restore so they agree on the member name.
func volPayloadMember(man *Manifest) string {
	if man != nil && man.Incremental {
		return volumeDeltaMember
	}
	return "volumes.tar"
}

// captureVolumes captures the container's volume payload, choosing a FULL
// self-contained `volumes.tar` or an incremental CHANGED-FILES-ONLY
// `volumes-delta.tar` against a parent, per the container's setting (F61). It sets
// the manifest's volume fields (VolumesSHA256, and for a delta Parent/
// ParentCipherSHA256/Incremental/ChainDepth/Deleted/VolIndex). With incremental
// OFF (or forced full, or no valid parent) it takes today's full path — plus,
// by default, the same complete file index incrementals store (F70), so browse/
// search/diff never have to decrypt the whole archive just for a listing.
func (e *Engine) captureVolumes(ctx context.Context, cli *client.Client, opts Options, man *Manifest, work string, volDests []string, id, name string, driftBefore *VolIndex) error {
	inc, fullEvery := e.incrementalPolicy(opts.NodeID, name)
	if !inc || opts.ForceFull {
		// Non-incremental: full volumes.tar as always.
		e.logf(id, "INFO", "Archiving %d volume path(s) via sidecar", len(volDests))
		sha, fileHashes, err := e.spoolVolumeTar(ctx, cli, id, opts.ContainerID, volDests, filepath.Join(work, "volumes.tar"), opts.excludeSubPaths()...)
		if err != nil {
			return err
		}
		man.VolumesSHA256 = sha
		man.ArchiveExcluded = opts.excludeSubPaths()
		// F70 universal file index: store the complete listing inside the archive
		// so browsing lists every file from one small member (no 20k truncation),
		// and cross-backup file search / generation diff become index-only reads.
		// Best-effort — an index failure NEVER fails the backup; set index.always
		// to "false" for byte-identical pre-F70 archives.
		if e.indexAlways() {
			if idx, ierr := e.buildVolIndex(ctx, cli, opts.ContainerID, volDests, man.Image); ierr == nil {
				e.stampIndexHashes(&idx, fileHashes, id)
				// The index walk is itself the "after" reading, so the drift
				// report costs no walk of its own.
				e.reportCopyDrift(man, id, driftBefore, idx, man.Image)
				if werr := writeVolIndex(work, idx); werr == nil {
					man.VolIndex = volumeIndexMember
				} else {
					e.logf(id, "WARN", "Could not write the file index (%v) — browsing this backup will stream the archive instead", werr)
				}
			} else {
				e.logf(id, "WARN", "File indexing failed (%v) — browsing this backup will stream the archive instead", ierr)
			}
		}
		return nil
	}

	// Incremental mode: build the current index (inside the quiesce window, so it
	// reflects exactly what the tar captures), then decide full-baseline vs delta.
	e.logf(id, "INFO", "Incremental mode: indexing volume files")
	curIdx, ierr := e.buildVolIndex(ctx, cli, opts.ContainerID, volDests, man.Image)
	if ierr != nil {
		// Indexing is a hard prerequisite for a correct delta; fall back to a full
		// baseline (still with an index next time) rather than risk a wrong delta.
		e.logf(id, "WARN", "Volume indexing failed (%v) — capturing a full baseline this run", ierr)
		return e.captureFullBaseline(ctx, cli, opts, man, work, volDests, id, VolIndex{}, driftBefore)
	}

	parent, atCap := e.incrementalParent(opts.NodeID, name, volDests, fullEvery, id)
	if parent == nil {
		e.logf(id, "INFO", "Incremental: no reusable parent — capturing a full baseline (chain reset)")
		return e.captureFullBaseline(ctx, cli, opts, man, work, volDests, id, curIdx, driftBefore)
	}
	if atCap {
		// F85: the "full every N" boundary. When enabled, build the new baseline
		// LOCALLY from the chain — the source transfers only the changed files.
		// App-native exports are out of scope; any error falls back to the
		// source-read full, so this can only ever be an optimization.
		if e.syntheticFullEnabled() && man.AppExport == nil {
			if serr := e.captureSyntheticFull(ctx, cli, opts, man, work, id, parent, curIdx); serr != nil {
				e.logf(id, "WARN", "Synthetic full failed (%v) — falling back to a full capture from the source", serr)
				return e.captureFullBaseline(ctx, cli, opts, man, work, volDests, id, curIdx, driftBefore)
			}
			return nil
		}
		e.logf(id, "INFO", "Incremental: chain reached its full-every-%d boundary — capturing a full baseline", fullEvery)
		return e.captureFullBaseline(ctx, cli, opts, man, work, volDests, id, curIdx, driftBefore)
	}

	prevIdx, perr := e.loadVolIndex(ctx, parent)
	if perr != nil {
		e.logf(id, "WARN", "Incremental: could not read parent index (%v) — capturing a full baseline", perr)
		return e.captureFullBaseline(ctx, cli, opts, man, work, volDests, id, curIdx, driftBefore)
	}
	var parentMan Manifest
	_ = unmarshal(parent.ManifestJSON, &parentMan)

	changed, deleted := DiffIndex(prevIdx, curIdx)
	e.logf(id, "INFO", "Incremental delta vs %s: %d changed, %d deleted (chain depth %d)",
		short(parent.ID), len(changed), len(deleted), parentMan.ChainDepth+1)

	// F69 ransomware tripwire: a delta touching an abnormal share of the volume
	// marks the backup suspect (never fails it — capturing the evidence matters).
	if e.tripwireEnabled() {
		st := AnalyzeDelta(prevIdx, curIdx, changed, deleted)
		if sus, reason := SuspectDelta(st,
			e.settingIntClamped("tripwire.min_files", 200, 10, 100000),
			e.settingIntClamped("tripwire.changed_pct", 60, 10, 100),
			e.settingIntClamped("tripwire.deleted_pct", 40, 10, 100)); sus {
			man.Suspect = reason
			e.logf(id, "WARN", "TRIPWIRE: possible mass-change/ransomware event — %s. Marking this backup suspect; retention for %s goes ON HOLD.", reason, name)
		}
	}

	sha, terr := e.spoolVolumeTarFiles(ctx, cli, opts.ContainerID, changed, filepath.Join(work, volumeDeltaMember))
	if terr != nil {
		return terr
	}
	if werr := writeVolIndex(work, curIdx); werr != nil {
		return werr
	}
	man.VolumesSHA256 = sha
	man.Incremental = true
	man.Parent = parent.ID
	man.ParentCipherSHA256 = parent.CipherSHA256
	man.ChainDepth = parentMan.ChainDepth + 1
	man.Deleted = deleted
	man.VolIndex = volumeIndexMember
	man.Format.Layout = "manifest.json, config/…, db/…, volumes-delta.tar (changed files only), volumes-index.json.zst; parent chain restores full→deltas"
	return nil
}

// captureFullBaseline captures a full volumes.tar AND stores the complete file
// index (F61), so the next run can delta against it. Used for the first
// incremental backup and every forced full. curIdx may be pre-built (reused) or
// empty (then it's built from the freshly-written tar's view).
func (e *Engine) captureFullBaseline(ctx context.Context, cli *client.Client, opts Options, man *Manifest, work string, volDests []string, id string, curIdx VolIndex, driftBefore *VolIndex) error {
	e.logf(id, "INFO", "Archiving %d volume path(s) via sidecar (full baseline)", len(volDests))
	sha, fileHashes, err := e.spoolVolumeTar(ctx, cli, id, opts.ContainerID, volDests, filepath.Join(work, "volumes.tar"), opts.excludeSubPaths()...)
	if err != nil {
		return err
	}
	man.ArchiveExcluded = opts.excludeSubPaths()
	if len(curIdx.Entries) == 0 {
		if built, berr := e.buildVolIndex(ctx, cli, opts.ContainerID, volDests, man.Image); berr == nil {
			e.stampIndexHashes(&built, fileHashes, id)
			e.reportCopyDrift(man, id, driftBefore, built, man.Image)
			curIdx = built
		} else {
			e.logf(id, "WARN", "Baseline index build failed (%v) — next run will re-baseline", berr)
		}
	}
	if werr := writeVolIndex(work, curIdx); werr != nil {
		return werr
	}
	man.VolumesSHA256 = sha
	man.Incremental = false
	man.ChainDepth = 0
	man.VolIndex = volumeIndexMember
	return nil
}

// incrementalParent picks the newest successful backup of this (node,target) that
// can be extended by a delta (F61): it must carry a stored file index, its captured
// mount set must match the current one (a changed selection forces a fresh full so
// a dropped mount is never mistaken for mass deletions), and appending would keep
// the chain within fullEvery. Returns (nil, false) when a fresh full baseline is
// required for structural reasons (no usable parent / selection changed), and
// (tip, true) when the ONLY reason is the depth cap — the chain tip is otherwise
// healthy, which is exactly what a synthetic full (F85) merges from.
func (e *Engine) incrementalParent(nodeID, name string, volDests []string, fullEvery int, selfID string) (*store.Backup, bool) {
	list, err := e.Store.ListBackupsForTarget(nodeID, name, 5000)
	if err != nil {
		return nil, false
	}
	want := destSet(volDests)
	for _, b := range list {
		if b.ID == selfID || b.Status != "success" || b.ManifestJSON == "" {
			continue
		}
		var m Manifest
		if unmarshal(b.ManifestJSON, &m) != nil || m.VolIndex == "" {
			continue
		}
		// F86: a delta is built by diffing against the PARENT's file index, which
		// lives inside the parent's ciphertext. A write-only parent cannot be
		// opened by this instance, so there is no diff base — capture a full
		// baseline instead. Returning "no parent" (rather than letting the index
		// read fail downstream) keeps the run log honest: it says the chain
		// restarted, not that something went wrong.
		if IsWriteOnly(&m) {
			return nil, false
		}
		if !sameDestSet(want, m.Volumes) { // mount selection changed — full baseline
			return nil, false
		}
		if m.ChainDepth+1 >= fullEvery { // chain would exceed the cap — boundary
			return b, true
		}
		return b, false // newest usable parent (list is newest-first)
	}
	return nil, false
}

// destSet builds a set of leading-slash-stripped destinations for comparison.
func destSet(dests []string) map[string]bool {
	s := map[string]bool{}
	for _, d := range dests {
		s[strings.TrimPrefix(d, "/")] = true
	}
	return s
}

// sameDestSet reports whether a manifest's captured volume destinations match the
// current selection (order-independent), so a delta is only built over an
// unchanged mount set.
func sameDestSet(want map[string]bool, vols []VolumeRef) bool {
	got := map[string]bool{}
	for _, v := range vols {
		if v.Destination != "" {
			got[strings.TrimPrefix(v.Destination, "/")] = true
		}
	}
	if len(got) != len(want) {
		return false
	}
	for k := range want {
		if !got[k] {
			return false
		}
	}
	return true
}

// buildVolIndex builds the current volume file index via the read-only sidecar (F61).
func (e *Engine) buildVolIndex(ctx context.Context, cli *client.Client, containerID string, volDests []string, image string) (VolIndex, error) {
	out, err := dockercli.CaptureSidecarRO(ctx, cli, containerID, BuildIndexCmd(volDests))
	if err != nil {
		return VolIndex{}, err
	}
	idx, perr := ParseIndexOutput(out)
	if perr != nil {
		return idx, perr
	}
	// #41: stamp the files this application rewrites itself, at capture, while the
	// image that decides the answer is known. A fixed-width marker is invisible to
	// a path+size comparison, so the classification cannot be recovered later.
	markVolatileFiles(&idx, volatileFileGlobs(image))
	return idx, nil
}

// indexAlways reports whether non-incremental backups also store the complete
// file index (F70, default on). Incremental backups always store it — it IS
// their diff base (F61).
func (e *Engine) indexAlways() bool {
	v, _ := e.Store.GetSetting("index.always", "true")
	return v != "false"
}

// volIndexCacheCap bounds the in-memory parsed-index cache (F70) — each entry
// is one backup's complete file listing, so keep the set small.
const volIndexCacheCap = 32

// VolIndexCached returns a backup's stored complete file index, memoized in a
// small bounded cache so repeated searches/diffs don't re-decrypt the archive
// member (F70). ok=false when the backup has no index (legacy/non-indexed) or
// it can't be read — callers fall back or skip, never fail.
func (e *Engine) VolIndexCached(ctx context.Context, b *store.Backup) (VolIndex, bool) {
	man := &Manifest{}
	if b.ManifestJSON != "" {
		_ = unmarshal(b.ManifestJSON, man)
	}
	if man.VolIndex == "" {
		return VolIndex{}, false
	}
	e.idxMu.Lock()
	if idx, ok := e.idxCache[b.ID]; ok {
		e.idxMu.Unlock()
		return idx, true
	}
	e.idxMu.Unlock()

	idx, err := e.loadVolIndex(ctx, b)
	if err != nil {
		return VolIndex{}, false
	}
	e.idxMu.Lock()
	if e.idxCache == nil {
		e.idxCache = map[string]VolIndex{}
	}
	if _, dup := e.idxCache[b.ID]; !dup {
		e.idxCache[b.ID] = idx
		e.idxOrder = append(e.idxOrder, b.ID)
		if len(e.idxOrder) > volIndexCacheCap {
			delete(e.idxCache, e.idxOrder[0])
			e.idxOrder = e.idxOrder[1:]
		}
	}
	e.idxMu.Unlock()
	return idx, true
}

// loadVolIndex reads and decodes a parent backup's stored file index (F61).
func (e *Engine) loadVolIndex(ctx context.Context, parent *store.Backup) (VolIndex, error) {
	data, err := e.extractEntry(ctx, parent, "", volumeIndexMember)
	if err != nil {
		return VolIndex{}, err
	}
	zr, err := zstd.NewReader(bytes.NewReader(data))
	if err != nil {
		return VolIndex{}, err
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		return VolIndex{}, err
	}
	var idx VolIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		return VolIndex{}, err
	}
	return idx, nil
}

// writeVolIndex zstd-compresses the index JSON to work/volumes-index.json.zst so
// it rides inside the (encrypted) archive as the diff base for the next delta (F61).
func writeVolIndex(work string, idx VolIndex) error {
	raw, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(work, volumeIndexMember))
	if err != nil {
		return err
	}
	defer f.Close()
	zw, err := zstd.NewWriter(f)
	if err != nil {
		return err
	}
	if _, err := zw.Write(raw); err != nil {
		zw.Close()
		return err
	}
	return zw.Close()
}

// spoolVolumeTarFiles streams a CHANGED-FILES-ONLY tar (the incremental delta
// payload, F61) into dst, returning the SHA-256 of the plaintext payload for the
// manifest. An empty file list yields an empty tar (a delta that only deletes).
func (e *Engine) spoolVolumeTarFiles(ctx context.Context, cli *client.Client, id string, files []string, dst string) (string, error) {
	rc, err := dockercli.TarVolumesFromList(ctx, cli, id, files)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), ctxReader(ctx, rc)); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// spoolVolumeTar streams the sidecar volume tar into a temp file, returning the
// SHA-256 of the (plaintext) volume payload for the manifest (PLAN §4.12).
func (e *Engine) spoolVolumeTar(ctx context.Context, cli *client.Client, logID, containerID string, dests []string, dst string, excludes ...string) (string, map[string]string, error) {
	rc, err := e.volumeTarStream(ctx, cli, containerID, dests, excludes, logID)
	if err != nil {
		return "", nil, err
	}
	defer rc.Close()
	f, err := os.Create(dst)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()

	// #41: hash each file as it goes past. The archive is read in full either
	// way, so a content baseline for every file costs a hash and not a second
	// walk of the container.
	pr, pw := io.Pipe()
	hashes := map[string]string{}
	hashed := make(chan struct{})
	go func() {
		defer close(hashed)
		hashTarMembers(pr, hashes)
		// Always drain to the end, whatever the parse did. The pipe's writer is
		// the copy below, and a reader that stops reading would stall the backup
		// — a hash is an extra, and an extra must never be able to block the
		// thing it is extra to.
		_, _ = io.Copy(io.Discard, pr)
	}()

	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(f, h, pw), ctxReader(ctx, rc))
	_ = pw.Close()
	<-hashed
	if copyErr != nil {
		return "", nil, copyErr
	}
	return fmt.Sprintf("%x", h.Sum(nil)), hashes, nil
}

// volumeTarStream opens the volume archive, falling back to the archive API on a
// daemon that will create a sidecar and not start it (#2).
//
// The sidecar is tried FIRST and always. R1 §3.1's estate is the exception, not
// the rule, and the sidecar path is better wherever it works: one stream for
// every mount in one pass, and `tar --exclude` filtering at the source instead
// of after the bytes have already crossed the socket.
//
// The branch is on the ATTEMPT's error and nothing else — PLAYBOOK §3's rule,
// learned from the report's own retraction: the proxy's declared environment
// said ALLOW_START=0 and that "was wrong" about what the proxy actually did.
// A create that failed is a different problem with a different answer and is not
// caught here.
//
// Deliberately NOT memoised per node. A memo is a cached branch, which is the
// same class of assumption the doctrine forbids one step removed — and the cost
// it would save is one create-and-remove per service, a few seconds across a
// stack backup that runs for minutes.
func (e *Engine) volumeTarStream(ctx context.Context, cli *client.Client, id string, dests, excludes []string, logID string) (io.ReadCloser, error) {
	rc, err := dockercli.TarVolumesExcluding(ctx, cli, id, dests, excludes)
	if err == nil || !dockercli.SidecarStartRefused(err) {
		return rc, err
	}
	if logID != "" {
		e.logf(logID, "WARN", "This node's Docker API allows creating a helper container but refuses to START it (%v), so sidecars are unavailable here — falling back to the archive API, which reads files without running anything. "+
			"The files are captured with their ownership and modes intact. Database dumps still require exec and are unaffected by this; if exec is also refused, the dumps will fail separately and say so.", err)
	}
	return dockercli.TarViaArchiveAPI(ctx, cli, id, dests, excludes)
}

// hashTarMembers reads an archive and records an MD5 per regular file.
//
// MD5 rather than something stronger because this answers "did these bytes
// arrive", not "did an adversary substitute them" — the archive's own AES-GCM
// tag already answers the second, and md5sum is what every sidecar image has for
// the restore side to answer with.
//
// A malformed or truncated archive simply stops the walk: whatever was hashed
// stands, and the entries that were not are recorded as unchecked rather than as
// passing.
func hashTarMembers(r io.Reader, into map[string]string) {
	tr := tar.NewReader(r)
	for len(into) < maxIndexedHashes {
		hdr, err := tr.Next()
		if err != nil {
			return
		}
		if hdr == nil || hdr.Typeflag != tar.TypeReg {
			continue
		}
		sum := md5.New()
		if _, err := io.Copy(sum, tr); err != nil {
			return
		}
		into[strings.TrimPrefix(hdr.Name, "/")] = hex.EncodeToString(sum.Sum(nil))
	}
}

// sqliteDbkName matches the snapshot filenames the sidecar produces (F22), so a
// (trusted) sidecar output can only ever land as a numbered .dbk — defense in depth.
var sqliteDbkName = regexp.MustCompile(`^[0-9]+\.dbk$`)

// sqliteRowsName matches the per-snapshot per-table count files (F124), held to
// the same strict shape as the .dbk names so a sidecar output can only ever land
// as a numbered rows file.
var sqliteRowsName = regexp.MustCompile(`^rows-[0-9]+\.txt$`)

// snapshotSQLite finds SQLite databases under the captured mounts and stores a
// CONSISTENT snapshot of each into work/sqlite/<n>.dbk, recording them in the
// manifest (F22). It is entirely best-effort and fail-safe: the raw file is always
// already in volumes.tar, so any failure (no SQLite files, no sqlite3 in the
// sidecar, a snapshot that doesn't verify) simply leaves the raw capture in place.
func (e *Engine) snapshotSQLite(ctx context.Context, cli *client.Client, containerID string, volDests []string, work string, man *Manifest, id string) error {
	dbs, err := dockercli.DetectSQLiteFiles(ctx, cli, containerID, volDests)
	if err != nil {
		e.logf(id, "INFO", "SQLite scan skipped (%v) — volumes captured as-is", err)
		return nil
	}
	if len(dbs) == 0 {
		return nil
	}
	e.logf(id, "INFO", "Found %d SQLite database(s) under the captured volumes — snapshotting consistently", len(dbs))
	rc, err := dockercli.SnapshotSQLite(ctx, cli, containerID, dbs)
	if err != nil {
		e.logf(id, "WARN", "SQLite consistent snapshot unavailable (%v) — the raw file(s) are still captured", err)
		return nil
	}
	defer rc.Close()

	sqliteDir := filepath.Join(work, "sqlite")
	if err := os.MkdirAll(sqliteDir, 0o750); err != nil {
		_, _ = io.Copy(io.Discard, rc)
		return nil
	}
	// Extract the sidecar's tar (numbered .dbk files + index.txt) into work/sqlite.
	// filepath.Base + a strict name check anchor path-traversal out (defense in depth).
	tr := tar.NewReader(rc)
	for {
		h, terr := tr.Next()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			e.logf(id, "WARN", "SQLite snapshot read failed (%v) — raw file(s) captured instead", terr)
			os.RemoveAll(sqliteDir)
			return nil
		}
		if h.FileInfo().IsDir() {
			continue
		}
		base := filepath.Base(h.Name)
		if base != "index.txt" && base != "stats.txt" && base != "failed.txt" &&
			!sqliteRowsName.MatchString(base) && !sqliteDbkName.MatchString(base) {
			continue
		}
		out, cerr := os.OpenFile(filepath.Join(sqliteDir, base), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if cerr != nil {
			continue
		}
		_, _ = io.Copy(out, tr)
		out.Close()
	}

	// stats.txt maps "<n>\t<tables>\t<rows>" for each snapshot (F109) — read
	// first so the index loop below can attach each database's contract. Absent
	// when the sidecar predates the feature, in which case no contract is
	// recorded and every later check simply skips.
	stats := parseSQLiteStats(readFileString(filepath.Join(sqliteDir, "stats.txt")))
	_ = os.Remove(filepath.Join(sqliteDir, "stats.txt"))

	// failed.txt lists databases that produced no usable snapshot (F116).
	failures := parseSQLiteFailures(readFileString(filepath.Join(sqliteDir, "failed.txt")))
	_ = os.Remove(filepath.Join(sqliteDir, "failed.txt"))

	// index.txt maps "<n>\t<source-db-path>" for each verified snapshot.
	idxBytes, _ := os.ReadFile(filepath.Join(sqliteDir, "index.txt"))
	_ = os.Remove(filepath.Join(sqliteDir, "index.txt"))
	kept := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(idxBytes)), "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		idx := strings.TrimSpace(parts[0])
		name := idx + ".dbk"
		src := parts[1]
		path := filepath.Join(sqliteDir, name)
		fi, serr := os.Stat(path)
		if serr != nil || fi.Size() == 0 {
			continue
		}
		ref := SQLiteRef{Source: src, Archive: "sqlite/" + name, Bytes: fi.Size()}
		// F109: hash the snapshot as stored, so a scrub can later prove the .dbk
		// in storage is still the one capture wrote. A hash we cannot compute is
		// simply not recorded — an absent contract is honest, a wrong one is not.
		if sum, herr := sha256File(path); herr == nil {
			ref.SHA256 = sum
		}
		if st, ok := stats[idx]; ok {
			ref.Tables, ref.Rows = st.tables, st.rows
		}
		// F124: this snapshot's per-table counts, written beside it by the
		// sidecar. Absent for a database that was too large to scan or has more
		// tables than the cap — the aggregate above still applies then.
		rowsFile := filepath.Join(sqliteDir, "rows-"+idx+".txt")
		ref.TableRows = dockercli.ParseSQLiteTableRows(readFileString(rowsFile))
		_ = os.Remove(rowsFile)
		// #18: the content baseline beside the counts, from the same pass.
		hashFile := filepath.Join(sqliteDir, "hash-"+idx+".txt")
		if hashes := parseTableHashes(readFileString(hashFile)); len(hashes) > 0 {
			ref.TableHashes = hashes
		}
		_ = os.Remove(hashFile)
		man.SQLiteDumps = append(man.SQLiteDumps, ref)
		kept[name] = true
	}
	// Drop any .dbk that wasn't in the verified index, so only good snapshots ship.
	if entries, derr := os.ReadDir(sqliteDir); derr == nil {
		for _, de := range entries {
			if !kept[de.Name()] {
				_ = os.Remove(filepath.Join(sqliteDir, de.Name()))
			}
		}
	}
	// F116: report what did NOT snapshot, and why, before deciding the outcome.
	// Until this existed both failure kinds were swallowed identically and the
	// raw file shipped in their place — so a corrupt database produced a green
	// backup and nobody found out until they needed it.
	corrupt := e.reportSQLiteFailures(man, failures, id)

	if len(man.SQLiteDumps) == 0 {
		os.RemoveAll(sqliteDir)
		// F154: record it, not just log it. Until now a run that found databases
		// and snapshotted none produced a manifest identical to one for an app
		// with no database — so the backup graded A while holding a file copied
		// out from under a live writer.
		man.SQLiteFallbackCount = len(dbs)
		if len(failures) == 0 {
			man.SQLiteFallback = "the volume sidecar image has no sqlite3, so no consistent snapshot could be taken"
			e.logf(id, "WARN", "%d SQLite database(s) were found but captured as RAW FILES — %s. A database that is being written to can be torn by a raw copy; point the sidecar at an image that has sqlite3 (Settings → Backups) for a guaranteed-consistent snapshot", len(dbs), man.SQLiteFallback)
		} else {
			man.SQLiteFallback = "no database produced a usable consistent snapshot"
			e.logf(id, "WARN", "%d SQLite database(s) were found and %s — the raw file(s) are captured instead, which can be torn if they were mid-write", len(dbs), man.SQLiteFallback)
		}
	}

	// A database MEASURED as corrupt fails the backup, so it is found now — while
	// the source is still there and can be repaired — rather than during a
	// recovery. Only a definite integrity verdict does this: a snapshot that
	// could not be TAKEN says nothing about the data and never fails a run.
	if len(corrupt) > 0 && e.failOnCorruptDB() {
		return fmt.Errorf("database corruption detected — refusing to store this backup: %s. Repair or replace the database and run the backup again, or turn off \"Fail a backup when a database is corrupt\" in Settings to store it anyway", strings.Join(corrupt, "; "))
	}

	if len(man.SQLiteDumps) == 0 {
		return nil
	}
	e.logf(id, "INFO", "Captured %d SQLite database(s) as consistent snapshots", len(man.SQLiteDumps))
	// F109: state the contract in the log too. A restore that later comes back
	// short has something contemporaneous to be compared against, without anyone
	// having to open the archive.
	for _, s := range man.SQLiteDumps {
		switch {
		case s.Tables > 0 && s.Rows > 0:
			e.logf(id, "INFO", "  %s: %d table(s), %d row(s), %s", s.Source, s.Tables, s.Rows, humanBytes(s.Bytes))
		case s.Tables > 0:
			e.logf(id, "INFO", "  %s: %d table(s), %s (too large to count rows cheaply)", s.Source, s.Tables, humanBytes(s.Bytes))
		}
	}
	return nil
}

// sqliteFailure is one database that produced no usable snapshot (F116).
type sqliteFailure struct {
	kind   string // "corrupt" (measured) | "snapshot" (couldn't be taken)
	path   string
	detail string
}

// parseSQLiteFailures reads the sidecar's "<kind>\t<path>\t<detail>" lines.
// Pure, so the distinction the gate depends on is unit-tested without Docker.
func parseSQLiteFailures(s string) []sqliteFailure {
	var out []sqliteFailure
	for _, line := range strings.Split(s, "\n") {
		f := strings.SplitN(strings.TrimRight(line, "\r"), "\t", 3)
		if len(f) < 2 || strings.TrimSpace(f[1]) == "" {
			continue
		}
		kind := strings.TrimSpace(f[0])
		if kind != "corrupt" && kind != "snapshot" {
			continue // unknown verdicts are ignored, never guessed at
		}
		fl := sqliteFailure{kind: kind, path: strings.TrimSpace(f[1])}
		if len(f) == 3 {
			fl.detail = strings.TrimSpace(f[2])
		}
		out = append(out, fl)
	}
	return out
}

// reportSQLiteFailures logs each failure at the level its certainty deserves and
// records the corrupt ones in the manifest, returning their human descriptions
// for the gate.
//
// A corrupt database is recorded even when the gate is off, so a backup that was
// deliberately stored anyway still says so on its own face rather than looking
// like a clean one.
func (e *Engine) reportSQLiteFailures(man *Manifest, failures []sqliteFailure, id string) []string {
	var corrupt []string
	for _, f := range failures {
		if f.kind == "corrupt" {
			e.logf(id, "ERR", "Database %s FAILED its integrity check: %s — this database is damaged AT THE SOURCE; the backup would preserve the damage, not repair it", f.path, f.detail)
			man.CorruptDatabases = append(man.CorruptDatabases, CorruptDB{Path: f.path, Detail: f.detail})
			corrupt = append(corrupt, f.path+" ("+f.detail+")")
			continue
		}
		e.logf(id, "WARN", "Database %s could not be snapshotted consistently (%s) — its raw file is still captured, but it may be a torn copy if it was mid-write", f.path, f.detail)
	}
	return corrupt
}

// failOnCorruptDBKey gates the F116 corruption failure. Default ON: a measured
// corrupt database is a real finding, and storing it as a green backup is the
// silent failure this exists to end.
//
// The escape hatch is deliberate. The SQLite scan walks every captured mount by
// header magic, so it can find an abandoned cache or browser database whose
// corruption nobody cares about — and a gate with no way out would leave that
// user unable to back up at all.
const failOnCorruptDBKey = "backup.fail_on_corrupt_db"

func (e *Engine) failOnCorruptDB() bool {
	v, _ := e.Store.GetSetting(failOnCorruptDBKey, "true")
	return v != "false"
}

// sqliteStat is one database's capture-time contract, read from the sidecar's
// stats.txt (F109).
type sqliteStat struct {
	tables int
	rows   int64
}

// parseSQLiteStats parses the sidecar's "<n>\t<tables>\t<rows>" lines, keyed by
// index. A row count of -1 means the snapshot was too large to scan, and is
// stored as 0 = "not counted" — deliberately NOT conflated with a genuine zero,
// which the checks treat as a signal worth failing on. Pure, so it is unit-
// tested without Docker.
func parseSQLiteStats(s string) map[string]sqliteStat {
	out := map[string]sqliteStat{}
	for _, line := range strings.Split(s, "\n") {
		f := strings.Split(strings.TrimSpace(line), "\t")
		if len(f) != 3 || f[0] == "" {
			continue
		}
		var st sqliteStat
		if n, err := strconv.Atoi(strings.TrimSpace(f[1])); err == nil && n >= 0 {
			st.tables = n
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(f[2]), 10, 64); err == nil && n >= 0 {
			st.rows = n
		}
		out[f[0]] = st
	}
	return out
}

// readFileString reads a file, returning "" when it is missing — the sidecar's
// stats.txt is optional and its absence is not an error.
func readFileString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// sha256File returns the hex SHA-256 of a file on disk.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// saveImageTar streams `docker save <ref>` into work/image.tar for air-gapped
// restore (PLAN §0.3 / §8.4). Returns the tarball size.
func (e *Engine) saveImageTar(ctx context.Context, cli *client.Client, ref, work, id string) (int64, error) {
	rc, err := cli.ImageSave(ctx, []string{ref})
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	out := filepath.Join(work, "image.tar")
	f, err := os.Create(out)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := io.Copy(f, rc); err != nil {
		return 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// encryptManifest reports whether the sidecar manifest should be encrypted
// rather than written readable+signed — a per-deployment preference for
// confidentiality over a self-describing beside-copy (PLAN §3.5). Read live so
// the toggle takes effect without a restart.
func (e *Engine) encryptManifest() bool {
	v, _ := e.Store.GetSetting("manifest.encrypt", "false")
	return v == "true"
}

// writeManifestSidecar writes the backup's manifest beside the archive on a
// backend, sealed or readable per the GLOBAL manifest.encrypt setting — the
// local-archive path and any caller without a per-destination preference.
func (e *Engine) writeManifestSidecar(ctx context.Context, be storage.Backend, key string, manBytes []byte) error {
	return e.writeManifestSidecarSealed(ctx, be, key, manBytes, e.encryptManifest())
}

// writeManifestSidecarSealed writes the backup's manifest beside the archive on
// a backend. seal=false: the readable manifest + an HMAC signature
// (tamper-evident, self-describing for hand-restore — PLAN §3.5/§9.3).
// seal=true: an AES-256-GCM sealed manifest (`.manifest.json.enc`) instead —
// confidential AND tamper-evident (GCM auth), so no separate signature is
// needed and volume/DB names + digests are no longer exposed in the clear.
func (e *Engine) writeManifestSidecarSealed(ctx context.Context, be storage.Backend, key string, manBytes []byte, seal bool) error {
	if seal {
		sealed, err := crypto.SealString(string(manBytes), e.MasterKey())
		if err != nil {
			return err
		}
		_, err = be.Put(ctx, key+".manifest.json.enc", bytes.NewReader(sealed))
		return err
	}
	if _, err := be.Put(ctx, key+".manifest.json", bytes.NewReader(manBytes)); err != nil {
		return err
	}
	_, err := be.Put(ctx, key+".manifest.json.sig", strings.NewReader(crypto.SignManifest(manBytes, e.MasterKey())))
	return err
}

// applyRetentionLock makes an uploaded copy read-only on a filesystem-like
// destination (F97): the archive itself, plus whichever manifest sidecars were
// actually written beside it.
//
// The ARCHIVE is the one that must succeed — it holds the data. A sidecar is
// regenerable from the archive, and which ones exist depends on the destination's
// sealing preference (F78), so a missing sidecar is not an error. Locking them
// anyway matters because a manifest is the map to the archive: leaving it
// writable would let an attacker rewrite the description of a copy they cannot
// touch.
func (e *Engine) applyRetentionLock(ctx context.Context, rl storage.RetentionLocker, key string, until time.Time) error {
	if err := rl.LockUntil(ctx, key, until); err != nil {
		return err
	}
	for _, suffix := range []string{".manifest.json", ".manifest.json.sig", ".manifest.json.enc"} {
		_ = rl.LockUntil(ctx, key+suffix, until) // absent sidecars are expected
	}
	return nil
}

// sealForDest resolves whether a destination's sidecar manifest is sealed (F78):
// its own seal_manifests config wins when set; unset falls back to the global
// manifest.encrypt setting — so pre-feature destinations behave exactly as
// before, and the local archive is never affected by a destination's choice.
func (e *Engine) sealForDest(cfg map[string]string) bool {
	switch cfg["seal_manifests"] {
	case "true":
		return true
	case "false":
		return false
	}
	return e.encryptManifest()
}

// archiveKey resolves the key that decrypts a backup's archive (PLAN §3.3): the
// per-backup DEK unwrapped from the manifest with the master key, or — for
// legacy backups with no wrapped key — the master key itself. A wrong master key
// fails to unwrap (GCM auth), surfacing a clear "encrypted with a key you no
// longer have" error.
func (e *Engine) archiveKey(man *Manifest) ([]byte, error) {
	// F86: a write-only backup's DEK is sealed to a public key. The master key is
	// useless here BY DESIGN — this branch is the reason a compromised instance
	// cannot read its own history. It comes first so a write-only manifest can
	// never fall through to a master-key path.
	if IsWriteOnly(man) {
		priv := e.restorePrivFor()
		if priv == "" {
			return nil, ErrPrivateKeyRequired
		}
		if err := CheckPrivateKey(man, priv); err != nil {
			return nil, err
		}
		dek, err := crypto.UnwrapKeyPriv(man.WrappedKeyPub, priv)
		if err != nil {
			return nil, err
		}
		return dek, nil
	}
	if man != nil && man.WrappedKey != "" {
		dek, err := crypto.UnwrapKey(man.WrappedKey, e.MasterKey())
		if err != nil {
			return nil, fmt.Errorf("cannot unwrap archive key — this backup was encrypted with a different master key: %w", err)
		}
		return dek, nil
	}
	return e.MasterKey(), nil // legacy: archive encrypted directly with the master key
}

// archiveKeyFor is archiveKey for a stored backup record (parses its manifest).
// archiveCodec resolves the decrypt key AND the compression algorithm for a
// stored backup from its manifest, so restore/download decompress correctly
// regardless of the algorithm the backup was written with (PLAN §4.15).
func (e *Engine) archiveCodec(b *store.Backup) (key []byte, algorithm string, err error) {
	var man Manifest
	if b != nil && b.ManifestJSON != "" {
		_ = unmarshal(b.ManifestJSON, &man)
	}
	k, err := e.archiveKey(&man)
	return k, man.Format.Algorithm, err
}

// packEncryptStore tars work dir contents (manifest + files), compresses (algo),
// AES-256-GCM encrypts, and stores under key. Returns ciphertext sha + size.
func (e *Engine) packEncryptStore(ctx context.Context, work string, man *Manifest, key, algo string, zlevel zstd.EncoderLevel, zwindow int) (string, int64, error) {
	// Envelope encryption (PLAN §3.3): a fresh per-backup data key (DEK) encrypts
	// the archive; the DEK is wrapped with the master key and recorded in the
	// manifest. Master-key rotation then only re-wraps DEKs, never re-encrypts
	// archives. The DEK never leaves memory unwrapped.
	dek, err := crypto.NewDataKey()
	if err != nil {
		return "", 0, err
	}
	defer func() {
		for i := range dek {
			dek[i] = 0
		}
	}()
	// F86: wrapDEK picks the envelope — symmetric (master key, today's default) or
	// write-only (sealed to an offline public key). One decision point, so the two
	// modes cannot diverge.
	if err := e.wrapDEK(dek, man); err != nil {
		return "", 0, err
	}

	// Write manifest.json into the work dir so it's inside the archive too (now
	// carrying the wrapped DEK + key fingerprint).
	manInner, _ := json.MarshalIndent(man, "", "  ")
	if err := os.WriteFile(filepath.Join(work, "manifest.json"), manInner, 0o600); err != nil {
		return "", 0, err
	}

	pr, pw := io.Pipe()
	go func() {
		zw, err := newCompressWriter(pw, algo, zlevel, zwindow)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		tw := tar.NewWriter(zw)
		err = filepath.Walk(work, func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			// Compressing a large spool takes minutes; a canceled backup must stop
			// here rather than finish packing an archive nobody wants.
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			if fi.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(work, path)
			rel = layoutPath(rel)
			hdr, err := tar.FileInfoHeader(fi, "")
			if err != nil {
				return err
			}
			hdr.Name = rel
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			// ctxReader so a single multi-GB member can't outlive a cancel either.
			_, err = io.Copy(tw, ctxReader(ctx, f))
			return err
		})
		if err == nil {
			err = tw.Close()
		}
		if err == nil {
			err = zw.Close()
		}
		pw.CloseWithError(err)
	}()

	// Encrypt pr -> ctPipe, store ctPipe.
	ctR, ctW := io.Pipe()
	shaCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		sha, err := crypto.Encrypt(ctW, pr, dek)
		ctW.CloseWithError(err)
		shaCh <- sha
		errCh <- err
	}()
	size, putErr := e.Storage.Put(ctx, key, ctR)
	if putErr != nil {
		// Put gave up without draining the stream, so the encryptor is parked on a
		// ctW.Write (and the packer on a pw.Write) that nobody will ever read.
		// Closing both read ends turns those writes into errors: the goroutines
		// exit and the channel reads below complete instead of wedging this
		// backup — and its queue slot and container lock — forever.
		ctR.CloseWithError(putErr)
		pr.CloseWithError(putErr)
	}
	encErr := <-errCh
	sha := <-shaCh
	if putErr != nil { // the encryptor's error is only a consequence of the failed Put
		return "", 0, putErr
	}
	if encErr != nil {
		return "", 0, encErr
	}
	return sha, size, nil
}

// maxBindFileBytes caps one captured file bind. These are configuration files,
// keys, licences and tokens — the things a new machine cannot regenerate — and
// they are held whole in memory on both the capture and the restore side, so the
// cap is what stops a mount that turns out to be a database file from being read
// into RAM. Anything larger is recorded as uncaptured rather than truncated.
const maxBindFileBytes int64 = 1 << 20 // 1 MiB

// captureFileBinds reads the contents of every bind whose root is a FILE into
// its own archive member, returning the refs that were captured (with Archive
// set) and skipped-mount records for the ones that were not.
//
// Read through ONE sidecar attached --volumes-from the target read-only, the
// same way every other mount is read — which also means it works against a
// STOPPED container, where an exec into the target would not. Capturing these
// was never the hard part: a file bind tars perfectly well, and it is only
// extraction back onto its own live mount point that cannot work.
func (e *Engine) captureFileBinds(ctx context.Context, cli *client.Client, containerID string, fileRefs []VolumeRef, work, id string) ([]VolumeRef, []SkippedMount) {
	if len(fileRefs) == 0 {
		return nil, nil
	}
	dests := make([]string, 0, len(fileRefs))
	for _, r := range fileRefs {
		dests = append(dests, r.Destination)
	}

	contents, rerr := e.readFileBinds(ctx, cli, containerID, dests)
	if rerr != nil {
		e.logf(id, "WARN", "Could not read the file-backed mount(s) of this container (%v) — they are recorded as not captured", rerr)
	}

	var captured []VolumeRef
	var skipped []SkippedMount
	for i, r := range fileRefs {
		data, ok := contents[r.Destination]
		if !ok {
			reason := fileBindUnreadableReason
			if rerr == nil {
				reason = fileBindOversizeReason
			}
			skipped = append(skipped, SkippedMount{Destination: r.Destination, Source: r.Source, Type: "bind", Reason: reason})
			e.logf(id, "WARN", "Not capturing the file mounted at %s (%s): %s", r.Destination, r.Source, reason)
			continue
		}
		dir := filepath.Join(work, bindFileWorkDir)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			e.logf(id, "WARN", "Could not stage the file mounted at %s (%v) — recorded as not captured", r.Destination, err)
			skipped = append(skipped, SkippedMount{Destination: r.Destination, Source: r.Source, Type: "bind", Reason: fileBindUnreadableReason})
			continue
		}
		member := strconv.Itoa(i)
		if err := os.WriteFile(filepath.Join(dir, member), data, 0o600); err != nil {
			e.logf(id, "WARN", "Could not stage the file mounted at %s (%v) — recorded as not captured", r.Destination, err)
			skipped = append(skipped, SkippedMount{Destination: r.Destination, Source: r.Source, Type: "bind", Reason: fileBindUnreadableReason})
			continue
		}
		r.Archive = bindFileArchivePrefix + member
		captured = append(captured, r)
	}
	if len(captured) > 0 {
		e.logf(id, "INFO", "Captured %d file-backed mount(s) separately from the volume archive — they are restored to the host before the container is recreated", len(captured))
	}
	return captured, skipped
}

// Reasons a file bind's contents are not in the archive. Stated in terms of what
// the operator has to do, because for these mounts — a key, a licence, a token —
// the answer is always "get it from the source machine", never "run it again".
const (
	fileBindOversizeReason   = "a file-backed mount larger than 1 MiB — not captured; copy it from the source host after a restore"
	fileBindUnreadableReason = "a file-backed mount whose contents could not be read — not captured; copy it from the source host after a restore"
)

// readFileBinds streams the given file destinations out of the container and
// returns their contents, keyed by destination.
//
// Oversized members are consumed and dropped rather than read: the point of the
// cap is to bound memory, so it has to be enforced before the bytes are held.
func (e *Engine) readFileBinds(ctx context.Context, cli *client.Client, containerID string, dests []string) (map[string][]byte, error) {
	rc, err := dockercli.TarVolumesFrom(ctx, cli, containerID, dests)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	out := map[string][]byte{}
	tr := tar.NewReader(rc)
	for {
		hdr, nerr := tr.Next()
		if nerr == io.EOF {
			return out, nil
		}
		if nerr != nil {
			// Whatever was read before the stream broke is still good; the caller
			// records the rest as uncaptured.
			return out, nerr
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if hdr.Size > maxBindFileBytes {
			continue // left out of `out`, so the caller records it as oversize
		}
		data, rerr := io.ReadAll(io.LimitReader(tr, maxBindFileBytes))
		if rerr != nil {
			return out, rerr
		}
		out["/"+strings.TrimPrefix(hdr.Name, "./")] = data
	}
}

// hostFileReader reads one small file from a node's host filesystem, however
// that node is reached.
type hostFileReader func(ctx context.Context, hostPath string, maxBytes int64) ([]byte, error)

// hostFileReaderFor picks how this node's host files are read.
//
// An SSH node keeps the registry's own reader: it already holds authenticated
// credentials and a pinned host key for that machine, and reusing the live
// tunnel is both cheaper and better attested than starting a container.
// Everything else reads through a short-lived read-only sidecar.
//
// The two enforce the same path grammar and the same size cap, so which one a
// node gets changes how the bytes travel and nothing about what may be read.
func (e *Engine) hostFileReaderFor(cli *client.Client, nodeID string) hostFileReader {
	if e.Reg != nil && e.Reg.TransportFor(nodeID) == dockercli.TransportSSH {
		return func(ctx context.Context, hostPath string, maxBytes int64) ([]byte, error) {
			return e.Reg.FetchNodeFile(ctx, nodeID, hostPath, maxBytes)
		}
	}
	return func(ctx context.Context, hostPath string, maxBytes int64) ([]byte, error) {
		return dockercli.ReadHostFile(ctx, cli, hostPath, maxBytes)
	}
}

// captureOriginalCompose reads the GENUINE host compose file(s) named in the
// container's compose config_files label and writes them under
// work/original-compose/<basename> (F57).
//
// It runs for EVERY transport. It once ran only over SSH, on the grounds that
// "the socket-proxy transports deliberately can't read host files" — which had
// stopped being true: the Machine page, the bind-source probe and the stack
// reconstruction all reach a host filesystem through an ordinary sidecar on any
// transport. What the restriction actually achieved was that a socket-proxy
// node's backup carried no .env, so a cross-host restore had no file to remap
// and `docker compose up` reinstated the old machine's address.
//
// Best-effort: any failure logs a WARN and the backup proceeds with the
// reconstruction. Returns whether at least one file was captured (drives
// Manifest.HasOriginalCompose). Both readers enforce the strict path validator
// and the size cap; the SSH one additionally pins the host key.
//
// anyFile reports whether ANYTHING landed under original-compose/ — a compose
// file or just the .env — so the caller can describe the archive's layout
// truthfully even when only the .env was captured.
func (e *Engine) captureOriginalCompose(ctx context.Context, cli *client.Client, id, nodeID string, insp types.ContainerJSON, work string) (hasCompose, anyFile bool) {
	if e.Reg == nil || insp.Config == nil {
		return false, false
	}
	readHostFile := e.hostFileReaderFor(cli, nodeID)
	raw := strings.TrimSpace(insp.Config.Labels["com.docker.compose.project.config_files"])
	if raw == "" {
		return false, false
	}
	dir := filepath.Join(work, "original-compose")
	captured := 0
	written := map[string]bool{}
	for _, p := range strings.Split(raw, ",") {
		if captured >= 3 { // cap files per container
			break
		}
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		name, skip := archiveComposeName(filepath.Base(p), written)
		if skip {
			e.logf(id, "WARN", "Skipping compose file %q — its name is reserved for the stack's captured .env", p)
			continue
		}
		data, ferr := readHostFile(ctx, p, dockercli.MaxComposeFetchBytes)
		if ferr != nil {
			e.logf(id, "WARN", "Could not read original compose file %q from the source host: %v", p, ferr)
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			e.logf(id, "WARN", "Could not create original-compose dir: %v", err)
			return captured > 0, captured > 0
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			e.logf(id, "WARN", "Could not write original compose file: %v", err)
			continue
		}
		if name != filepath.Base(p) {
			e.logf(id, "INFO", "Stored %q as %s — another compose file in this project has the same name", p, name)
		}
		written[name] = true
		captured++
	}
	// F231: and the .env BESIDE them.
	//
	// It was never captured, because it is not named in config_files — and that
	// omission is why a cross-host restore kept the old domain. A compose file
	// that says ${DOMAIN} has no literal to remap; the value lives here. So the
	// remap rewrote the running container correctly, the operator then ran
	// `docker compose up`, compose re-read the OLD .env, and recreated the
	// container with the address it had just been moved off.
	//
	// Same SSH-only boundary, same size cap, same path validator, same
	// best-effort: a stack with no .env simply captures none. The file lands in
	// the ENCRYPTED archive, like the compose file it sits beside — never in the
	// manifest, which travels to every destination in the clear.
	//
	// Attempted INDEPENDENTLY of the compose files above. A compose file over the
	// fetch cap, or unreadable by the SSH user, must not also cost the .env: it is
	// its own small fetch, and the restore extracts it by path without consulting
	// HasOriginalCompose (stackEnvFromArchive). Losing the file that carries the
	// domain because a different file was too big is the worst of both outcomes.
	gotEnv := e.captureStackEnv(ctx, readHostFile, id, raw,
		insp.Config.Labels["com.docker.compose.project.working_dir"],
		insp.Config.Labels["com.docker.compose.project.environment_file"], dir)
	if gotEnv {
		e.logf(id, "INFO", "Captured the stack's .env from the source host")
	}
	if captured > 0 {
		e.logf(id, "INFO", "Captured %d genuine host compose file(s) from the source host", captured)
	}
	// hasCompose is deliberately NOT influenced by the .env: HasOriginalCompose
	// means a genuine COMPOSE FILE is in the archive, which is what the runbook
	// reports. anyFile drives the layout string, which must list the directory
	// whenever the archive actually holds one.
	return captured > 0, captured > 0 || gotEnv
}

// stackEnvArchiveName is where a captured .env lands inside original-compose/.
// Prefixed so it can never collide with a compose file's basename and is
// obviously not itself a compose document.
const stackEnvArchiveName = "dockback-stack.env"

// archiveComposeName decides what one captured compose file is called inside
// original-compose/, given the names already written. Pure, so both rules are
// testable without an SSH node.
//
// Two collisions to close, and neither is hypothetical:
//
//   - `-f base.yml -f /other/dir/base.yml` are two DIFFERENT files with one
//     basename. Writing both as "base.yml" meant the second silently replaced
//     the first, so the archive claimed two originals and held one. A numeric
//     prefix keeps both; nothing reads these by name (the restore lists the
//     directory), so the prefix costs nothing.
//   - a config file literally named dockback-stack.env would take the slot the
//     captured .env is restored from — and with no real .env to overwrite it, a
//     compose document would be laid down on the target host AS the stack's
//     .env. Refused outright rather than renamed: it is not a name we can
//     honour, and the operator should know it was skipped.
func archiveComposeName(base string, written map[string]bool) (name string, skip bool) {
	if base == stackEnvArchiveName {
		return "", true
	}
	if !written[base] {
		return base, false
	}
	// Collision: prefix until free. Bounded by the caller's per-container cap,
	// so this cannot spin.
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%d-%s", i, base)
		if !written[candidate] {
			return candidate, false
		}
	}
}

// captureStackEnv fetches the .env `docker compose` interpolates from: the
// explicit --env-file when the project recorded one, else the file in the
// project's working directory, else beside the first compose file. Returns
// whether it got one.
func (e *Engine) captureStackEnv(ctx context.Context, readHostFile hostFileReader, id, configFiles, workingDir, envFileLabel, dir string) bool {
	// An explicit --env-file is not a guess: compose recorded the exact path it
	// was told to read, so it wins over anything derived. It goes through the
	// same reader as the compose files beside it — same path validator, same cap.
	envPath := strings.TrimSpace(strings.Split(envFileLabel, ",")[0])
	if envPath == "" {
		envPath = stackEnvSourcePath(configFiles, workingDir)
	}
	if envPath == "" {
		return false
	}
	data, ferr := readHostFile(ctx, envPath, dockercli.MaxComposeFetchBytes)
	if ferr != nil {
		// Absent is the common case and not worth a warning; anything else is
		// worth one line, because its absence is what silently keeps an old
		// address alive after a move.
		if !strings.Contains(strings.ToLower(ferr.Error()), "no such file") {
			e.logf(id, "WARN", "Could not read the stack's .env at %q from the source host: %v", envPath, ferr)
		}
		return false
	}
	if len(data) == 0 {
		return false
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	// 0600: this file exists to hold the values the compose file should not.
	return os.WriteFile(filepath.Join(dir, stackEnvArchiveName), data, 0o600) == nil
}

// layoutPath maps work-dir relative names into the documented archive layout.
func layoutPath(rel string) string {
	switch {
	case rel == "manifest.json":
		return "manifest.json"
	case rel == "inspect.json":
		return "config/inspect.json"
	case rel == "docker-compose.yml":
		return "config/docker-compose.yml"
	case strings.HasPrefix(rel, "original-compose/"):
		// Genuine host compose file(s) captured from the source host (F57).
		return "config/" + rel
	case rel == projectFolderMember:
		// The compose project's own folder, without its bind-mounted data (step 24).
		return "config/" + rel
	case strings.HasPrefix(rel, bindFileWorkDir+"/"):
		// Contents of the binds whose root is a file (F81), which cannot ride in
		// volumes.tar — see splitFileBinds.
		return "config/" + rel
	case rel == "volumes.tar":
		return "volumes.tar"
	case rel == volumeDeltaMember: // incremental changed-files-only payload (F61)
		return volumeDeltaMember
	case rel == volumeIndexMember: // complete post-backup file index (F61)
		return volumeIndexMember
	case rel == "image.tar":
		return "image.tar"
	case strings.HasPrefix(rel, "db_"):
		return "db/" + strings.TrimPrefix(rel, "db_")
	default:
		return rel
	}
}

// backupKey lays archives out as <node>/<stack>/<container>/<timestamp>_<id>.dback
// on EVERY location, so a stack's containers are grouped under a single folder
// (instead of one flat folder per container) and the host a backup belongs to is
// obvious even when the same container runs on multiple machines. Standalone
// containers with no compose stack skip the stack level:
// <node>/<container>/<timestamp>_<id>.dback.
//
// The timestamp is the absolute UTC date+time to the second (e.g.
// 2026-07-05_14-30-22); the short id keeps two backups taken in the same second
// distinct. node, stack and container are each sanitized as SEPARATE path
// segments (never a combined string) so no segment can escape its directory —
// the storage backend's safePath still confines the whole key to the root.
func backupKey(nodeName, stack, name, id string) string {
	t := time.Now().UTC().Format("2006-01-02_15-04-05")
	file := fmt.Sprintf("%s_%s.dback", t, id[:8])
	return layoutKey(nodeName, stack, name, file) // ONE layout definition (F77)
}

// stampIndexHashes writes the per-file content hashes taken from the archive
// stream onto the index entries they belong to, and says what got covered.
//
// Best-effort, like the index itself: a file the walk saw and the archive did not
// carry (an excluded path, a file that vanished between the two) simply has no
// hash, and a comparison reports it as unchecked rather than as a mismatch.
func (e *Engine) stampIndexHashes(idx *VolIndex, hashes map[string]string, logID string) {
	if idx == nil || len(hashes) == 0 {
		return
	}
	stamped := applyIndexHashes(idx, hashes)
	if stamped == 0 {
		return
	}
	if missing := len(idx.Entries) - stamped; missing > 0 {
		e.logf(logID, "INFO", "Recorded a content hash for %d of %d indexed files — a restore can prove those came back byte-for-byte; the other %d are checked by size and modification time",
			stamped, len(idx.Entries), missing)
		return
	}
	e.logf(logID, "INFO", "Recorded a content hash for all %d indexed files — a restore can prove every one came back byte-for-byte, not merely the same size", stamped)
}
