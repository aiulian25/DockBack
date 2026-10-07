package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/klauspost/compress/zstd"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// App-consistent stack snapshot (F33). An ordinary stack backup runs each
// service independently and concurrently, so a database and the app that writes
// to it are captured at slightly different instants. For apps that can't be
// dumped through their own tooling, this opt-in mode instead quiesces the app
// tier and captures every service — DB dump(s) and app volumes — inside ONE
// pause window, giving a single coherent point-in-time. Every per-service
// backup is tagged with a shared ConsistencyGroup id + timestamp.
//
// The pause window is kept tight: services are fully PREPARED first (inspect,
// manifest, config, mount selection — no pause), then the window only spans the
// DB dump(s) and volume tars, and the slow pack/encrypt/store/verify tail runs
// AFTER apps are resumed. Downtime is just the copy, not the whole pipeline.

// consistencyMember is one service's classification used to plan the window
// ordering. isDB marks a service captured by a live database dump (never
// paused); running marks a service that is up (only a running app can be paused
// and only a running DB can be dumped).
type consistencyMember struct {
	service string
	isDB    bool
	running bool
}

// consistencyOp is one step in the quiesce window. kind is
// "pause" | "dump" | "tar" | "resume".
type consistencyOp struct {
	kind    string
	service string
}

// planConsistency returns the ordered op sequence for one consistency window:
//
//  1. pause every running app (non-DB) service — freeze the writers,
//  2. dump every running DB service live (databases are never paused),
//  3. tar every service's volumes while the app tier is frozen, then
//  4. resume the paused apps in reverse order.
//
// It is pure (no Docker I/O) so the ordering — the safety-critical part: apps
// pause before any dump, and resume only after the last tar — is unit-testable
// with a fake exec recorder.
func planConsistency(members []consistencyMember) []consistencyOp {
	var ops []consistencyOp
	// 1) Pause running apps first, so no writer mutates state during capture.
	for _, m := range members {
		if !m.isDB && m.running {
			ops = append(ops, consistencyOp{"pause", m.service})
		}
	}
	// 2) Dump running databases live (they were never paused).
	for _, m := range members {
		if m.isDB && m.running {
			ops = append(ops, consistencyOp{"dump", m.service})
		}
	}
	// 3) Tar every service's selected volumes inside the frozen window.
	for _, m := range members {
		ops = append(ops, consistencyOp{"tar", m.service})
	}
	// 4) Resume the paused apps in reverse order (mirror of the pause order).
	for i := len(members) - 1; i >= 0; i-- {
		m := members[i]
		if !m.isDB && m.running {
			ops = append(ops, consistencyOp{"resume", m.service})
		}
	}
	return ops
}

// readdDataDirForFallback puts a database's data directory back into a
// service's capture set after its dump fell through to a file capture (F191).
//
// The directory was excluded — and deliberately not recorded as skipped —
// because the dump was going to supersede it. Once the dump is off the table,
// leaving it excluded would capture a database container with NO database in
// it, silently. The mount is restored to both halves the exclusion touched: the
// tar list, and the manifest's volume refs, so a restore knows the mount exists.
func (e *Engine) readdDataDirForFallback(sc *serviceCapture, dest string) {
	if dest == "" {
		return
	}
	for _, d := range sc.volDests {
		if d == dest {
			return // explicitly opted in; already everywhere it needs to be
		}
	}
	for _, m := range sc.insp.Mounts {
		if m.Destination != dest {
			continue
		}
		sc.volDests = append(sc.volDests, dest)
		ref := VolumeRef{Destination: dest, Type: string(m.Type), Source: m.Source}
		if string(m.Type) == "volume" {
			ref.Name, ref.Driver = m.Name, m.Driver
		}
		sc.man.Volumes = append(sc.man.Volumes, ref)
		e.logf(sc.id, "INFO", "Re-including %s in the file capture — the dump that was going to replace it did not run", dest)
		return
	}
	e.logf(sc.id, "WARN", "The data directory %s is not a mount on %q — nothing to fall back to, so this backup holds no database data", dest, sc.name)
}

// serviceCapture holds one service's prepared, pre-pause state plus the results
// captured inside the window, carried through to the finalize (storeAndVerify)
// phase.
type serviceCapture struct {
	id           string
	key          string // plan/lookup key: compose service, or container name if unlabeled
	b            *store.Backup
	man          *Manifest
	work         string
	nodeID       string // the node this service runs on (per-container settings key on it)
	name         string // target (container) name
	service      string // compose service label (for the manifest)
	stack        string
	containerID  string
	insp         types.ContainerJSON
	engineKind   string // non-empty => a live DB dump will be taken
	env          []string
	volDests     []string
	uncompressed int64
	algo         string
	zlevel       zstd.EncoderLevel
	zwindow      int
	start        time.Time
	pre, post    []Hook
	ranPre       bool
	running      bool
	opts         Options // per-service opts (ContainerID set)

	// window state
	paused     bool
	pauseKind  string        // PausePause | PauseStop
	sqlite     sqliteCapture // the SQLite copies the window took, recorded after it
	captureErr error         // set if a window step failed => skip finalize, mark failed
}

// prepareServiceCapture does everything that does NOT require the app to be
// paused: inspect, classify, build the manifest (tagged with the group id),
// create the backup row, write config, and select mounts. It mirrors the
// pre-pause portion of Run so a consistent-group service is byte-identical to a
// normal backup apart from the ConsistencyGroup tag. Returns a ready capture;
// the caller owns cleanup of work on success (storeAndVerify reads it).
func (e *Engine) prepareServiceCapture(ctx context.Context, cli *client.Client, nodeName, nodeID, containerID, groupID string, groupAt int64, opts Options) (*serviceCapture, error) {
	id := opts.BackupID
	if id == "" {
		id = newID()
	}
	b := &store.Backup{ID: id, NodeID: nodeID, Status: "running"}
	start := time.Now()

	insp, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("inspect target: %w", err)
	}
	name := strings.TrimPrefix(insp.Name, "/")
	stack := insp.Config.Labels["com.docker.compose.project"]
	service := insp.Config.Labels["com.docker.compose.service"]
	b.TargetName, b.Stack = name, stack
	if err := e.Store.CreateBackup(b); err != nil {
		return nil, err
	}
	// F163: same refusal on the stack path. A service that must be sealed to an
	// offline key does not stop needing that because it was captured as part of a
	// group.
	if werr := e.writeOnlyRequirement(nodeID, name, insp.Config.Image); werr != nil {
		return nil, e.fail(b, werr)
	}
	e.logf(id, "INFO", "Preparing consistent capture of %q (service=%q)", name, service)

	imageRef, imageDigest, _ := dockercli.ImageRef(ctx, cli, containerID)
	engineKind := detectDBEngine(insp.Config.Image, insp.Config.Env)
	running := insp.State != nil && insp.State.Running
	if !running && engineKind != "" {
		// A stopped DB has no live process to dump — capture its files instead.
		e.logf(id, "INFO", "Service %q is stopped — capturing volumes/config without a live database dump", name)
		engineKind = ""
	}
	// A DB image without its dump CLI isn't really a database server (e.g. an app
	// that merely connects to one). Decide up front — BEFORE selectMounts — so the
	// data dir is INCLUDED in the volume tar, matching Run's no-tools fallback
	// (which there happens lazily at dump time, too late for a group where mount
	// selection must be final before the window opens).
	if engineKind != "" && !e.probeDBTools(ctx, cli, containerID, engineKind) {
		e.logf(id, "WARN", "%s tools not found in %q — it isn't a database server; capturing its files instead", engineKind, name)
		engineKind = ""
	}

	algo, zlevel, zwindow, compLabel := parseCompression(opts.Compression)
	man := &Manifest{
		Version:          ManifestVersion,
		BackupID:         id,
		CreatedAt:        nowRFC3339(),
		NodeID:           nodeID,
		NodeName:         nodeName,
		Stack:            stack,
		Service:          service,
		DependsOn:        parseDependsOn(insp.Config.Labels["com.docker.compose.depends_on"]),
		ConsistencyGroup: groupID,
		ConsistencyAt:    groupAt,
		TargetName:       name,
		ContainerID:      insp.ID,
		Image:            imageRef,
		ImageDigest:      imageDigest,
		KeyFingerprint:   e.MasterKeyFP(),
		Format: Format{
			Encryption:  "AES-256-GCM (DBACKv1 chunked)",
			Compression: compLabel,
			Algorithm:   algo,
			Archive:     "tar",
			Layout:      "manifest.json, config/inspect.json, config/docker-compose.yml, db/<service>.dump, volumes.tar",
		},
		HasConfig: true,
	}
	// Record the on-host compose project layout (working dir + compose filename) so
	// a DR restore can rebuild the organized <base>/<stack>/ folder, not just the
	// container. Empty for non-compose containers.
	man.StackWorkingDir, man.ComposeFile = composeProjectLayout(insp.Config.Labels)

	work, err := os.MkdirTemp(e.WorkDir, "dback-"+id+"-*")
	if err != nil {
		return nil, e.fail(b, err)
	}

	// F89: record the network topology — each attached network's definition and
	// this container's endpoint on it. Best-effort: a failure leaves the field
	// empty and the restore falls back to the pre-F89 behaviour, never a failed
	// backup over metadata.
	//
	// This path never did. An app-consistent capture built its manifest here and
	// left Networks nil, so every one of its backups restored its networks as
	// bare bridges — no subnet, no static address, no `internal` flag — and its
	// reconstruction declared each of them `external: true`, which is the
	// fallback for "nothing is known about this network". The ordinary capture
	// has recorded all of it since F89; the two paths simply drifted.
	//
	// Ordering matters: the reconstruction below reads man.Networks, so this has
	// to happen before it or the compose file keeps saying external.
	man.Networks = networkRefsFrom(dockercli.InspectNetworks(ctx, cli, insp))
	if len(man.Networks) > 0 {
		e.logf(id, "INFO", "Recorded %d network(s) for restore: %s", len(man.Networks), networkSummary(man.Networks))
	}

	// Config: inspect.json + reconstructed compose (mirrors Run steps 2/2b).
	inspBytes, _ := json.MarshalIndent(insp, "", "  ")
	if err := os.WriteFile(filepath.Join(work, "inspect.json"), inspBytes, 0o600); err != nil {
		os.RemoveAll(work)
		return nil, e.fail(b, err)
	}
	// F73: config-drift fingerprints (mirrors Run step 2).
	man.ConfigFP = ConfigFingerprint(inspBytes)
	man.ConfigFPLite = configFPLiteFromInspect(insp)
	if composeBytes, cerr := composeFromInspect(insp, imageRef, man.Networks, nil, dockercli.ImageEnv(ctx, cli, insp.Image)); cerr == nil {
		if werr := os.WriteFile(filepath.Join(work, "docker-compose.yml"), composeBytes, 0o600); werr == nil {
			man.HasCompose = true
		} else {
			e.logf(id, "WARN", "Could not write reconstructed compose file: %v", werr)
		}
	} else {
		e.logf(id, "WARN", "Could not reconstruct compose file: %v", cerr)
	}
	// Also capture the GENUINE host compose file(s) and the stack's .env (F57/F231).
	if hasCompose, anyFile := e.captureOriginalCompose(ctx, cli, id, nodeID, insp, work); anyFile {
		man.HasOriginalCompose = hasCompose
		man.Format.Layout += ", config/original-compose/*"
	}
	if man.ProjectFolder = e.captureProjectFolder(ctx, cli, id, insp, work); man.ProjectFolder != nil && man.ProjectFolder.Entries > 0 {
		man.Format.Layout += ", " + layoutPath(projectFolderMember)
	}

	// Volume selection (pre-pause). Exclude the dumped DB data dir only when a
	// live dump will actually replace it (engineKind survived the no-tools check).
	sopts := opts
	sopts.ContainerID = containerID
	dumpedDataDir := ""
	if engineKind != "" {
		dumpedDataDir = dockercli.DBDataDir(engineKind)
	}
	volDests, refs, skipped := e.selectMounts(ctx, cli, insp, sopts, id, dumpedDataDir)
	// F81: the same bind work the ordinary capture does — what each bind's root
	// IS, and the file-rooted ones held out of volumes.tar into their own
	// members. An app-consistent snapshot is still a capture of the same
	// container, and everything downstream of it (the materialiser, the
	// ownership rules, the pre-restore plan) reads what this records.
	var fileSkips []SkippedMount
	volDests, refs, fileSkips = e.captureBindRoots(ctx, cli, insp, containerID, work, id, man, volDests, refs)
	skipped = append(skipped, fileSkips...)
	// F226: and the identity of EVERY named volume it mounts, captured or not,
	// so the restore creates each one itself instead of letting Docker
	// auto-create an unlabelled replacement.
	man.MountedVolumes = e.recordMountedVolumes(ctx, cli, insp)
	man.SkippedMounts = skipped
	// #23: one directory shared by two different builds of the same image. An
	// app-consistent capture is a capture of the same container, and a stack
	// snapshot is exactly where the pair this asks about is in front of us.
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
	e.reportTagDrift(ctx, cli, containerID, man, id, name, insp.Config.Image)
	e.logPartialSkips(id, skipped)
	var bindDests []string
	bindSrc := map[string]string{}
	for _, r := range refs {
		if r.Type == "bind" {
			bindDests = append(bindDests, r.Destination)
			bindSrc[r.Destination] = r.Source
		}
	}
	unreadableBinds, uncompressed, gerr := e.guardFreeSpace(ctx, cli, nodeID, name, containerID, volDests, bindDests, id, opts.Compression)
	if gerr != nil {
		os.RemoveAll(work)
		return nil, e.fail(b, gerr)
	}
	if part := unreadableSkippedMounts(unreadableBinds, bindSrc); len(part) > 0 {
		man.SkippedMounts = append(man.SkippedMounts, part...)
		e.logf(id, "WARN", "%d selected bind mount(s) unreadable for %q — this service's backup is PARTIAL", len(part), name)
	}

	pre, post := e.gatherHooks(insp.Config.Image, nodeID, name)

	key := service
	if key == "" {
		key = name
	}
	return &serviceCapture{
		id: id, key: key, b: b, man: man, work: work, nodeID: nodeID, name: name, service: service, stack: stack,
		containerID: containerID, insp: insp, engineKind: engineKind, env: insp.Config.Env,
		volDests: volDests, uncompressed: uncompressed, algo: algo, zlevel: zlevel, zwindow: zwindow,
		start: start, pre: pre, post: post, running: running, opts: sopts,
	}, nil
}

// BackupStackConsistent captures every service of a compose project as one
// app-consistent snapshot (F33): prepare all services, quiesce the app tier,
// dump databases + tar volumes inside that single window, resume, then finalize
// each service through the shared store/verify tail. Every service's manifest
// carries the same ConsistencyGroup id + timestamp. It logs under
// "stack:<project>" and returns the first per-service error (best-effort: other
// services still complete). Compared with the default concurrent stack backup,
// this trades brief app downtime for a coherent point-in-time.
//
// perService (F79) resolves each member's own Options — the caller merges the
// container's remembered manual choices (compression / app-export / save-image)
// plus any per-run overrides, so a consistent stack run produces the same
// archive per service a manual or scheduled run would. nil = every service uses
// the opts template verbatim (previous behavior). Group-wide fields (the
// per-run PauseMode override) still come from the template. A BackupID it sets
// becomes that service's backup id, so the caller can tag the service's log
// lines before the first one is written.
func (e *Engine) BackupStackConsistent(ctx context.Context, nodeID, project string, opts Options, perService func(containerID, name string) Options) error {
	logID := "stack:" + project
	cli, err := e.Reg.Get(nodeID)
	if err != nil {
		return err
	}
	nodeName := nodeID
	if n, nerr := e.Store.GetNode(nodeID); nerr == nil && n != nil && n.Name != "" {
		nodeName = n.Name
	}

	// Global pre-flight once for the whole group (per-service prepare skips these):
	// encryption key present and storage writable — fail fast before any capture.
	if len(e.MasterKey()) != 32 {
		return fmt.Errorf("encryption key not configured (set DOCKBACK_ENCRYPTION_KEY) — refusing to back up unencryptable data")
	}
	if err := e.probeStorageWritable(ctx); err != nil {
		return fmt.Errorf("backup storage %q not writable: %w", e.Storage.Name(), err)
	}

	cs, err := dockercli.ListContainers(ctx, cli)
	if err != nil {
		return err
	}
	var members []*dockercli.Container
	for _, c := range cs {
		if c.Stack == project {
			members = append(members, c)
		}
	}
	if len(members) == 0 {
		return fmt.Errorf("no containers found for stack %q on this node", project)
	}
	// Deterministic order (by compose service) so the pause/tar/resume sequence
	// and the group's logs are reproducible run to run.
	sort.Slice(members, func(i, j int) bool { return memberKey(members[i]) < memberKey(members[j]) })

	// F146: does this stack anchor an application whose services are only
	// meaningful together? If so the whole run becomes all-or-nothing, and every
	// member's archive records the set it belongs to.
	atomicSet, _ := StackAtomicAnchor(members)
	atomicMembers := []StackMember(nil)
	if atomicSet != nil {
		atomicMembers = stackAtomicMembers(members)
	}

	groupAt := time.Now().Unix()
	groupID := fmt.Sprintf("cg-%d-%s", groupAt, short(newID()))
	e.logf(logID, "INFO", "App-consistent snapshot of stack %q — group %s (%d service(s))", project, groupID, len(members))

	// Self-heal any leaked sidecars once for the whole group.
	if n, _ := dockercli.RemoveOrphanSidecars(ctx, cli); n > 0 {
		e.logf(logID, "INFO", "Cleaned %d orphaned backup sidecar(s) from a previous run", n)
	}

	// PHASE 1 — prepare every service with NO pause. A service that fails to
	// prepare is skipped with a WARN; the remaining services still form a group.
	var caps []*serviceCapture
	// missed names every member that ends the run without a backup, and firstErr
	// says why the first of them failed.
	var missed []string
	var firstErr error
	for _, c := range members {
		svcOpts := opts
		if perService != nil {
			svcOpts = perService(c.ID, c.Name)
		}
		sc, perr := e.prepareServiceCapture(ctx, cli, nodeName, nodeID, c.ID, groupID, groupAt, svcOpts)
		if perr != nil {
			// F146: for an atomic set, skipping a member is exactly the archive
			// this whole feature exists to prevent — a group that looks complete
			// and cannot be restored. Fail the run instead, loudly, while the
			// operator is still watching.
			if atomicSet != nil {
				return fmt.Errorf("service %q could not be prepared, and %s. Refusing to write a snapshot missing one of its members — %s: %w",
					memberKey(c), atomicSet.Why, atomicSet.Symptom, perr)
			}
			e.logf(logID, "WARN", "Skipping service %q — preparation failed: %v", c.Service, perr)
			missed = append(missed, memberKey(c))
			if firstErr == nil {
				firstErr = fmt.Errorf("service %q: %w", memberKey(c), perr)
			}
			continue
		}
		if atomicSet != nil {
			sc.man.StackAtomic = &StackAtomicRef{
				Why: atomicSet.Why, Symptom: atomicSet.Symptom, SoloRestore: atomicSet.SoloRestore,
				GroupID: groupID, Members: atomicMembers,
			}
		}
		caps = append(caps, sc)
	}
	if len(caps) == 0 {
		return fmt.Errorf("no services could be prepared for a consistent snapshot of %q", project)
	}
	defer func() {
		for _, sc := range caps {
			os.RemoveAll(sc.work)
		}
	}()

	byKey := map[string]*serviceCapture{}
	for _, sc := range caps {
		byKey[sc.key] = sc
	}
	plan := planConsistency(capsToMembers(caps))

	// PHASE 2 — the quiesce window. Deferred safety nets always resume every app
	// and run post-hooks, even on an early error/panic path.
	resumed := map[string]bool{}
	resumeAll := func() {
		for i := len(caps) - 1; i >= 0; i-- {
			sc := caps[i]
			if sc.paused && !resumed[sc.key] {
				resumed[sc.key] = true
				e.resumeContainer(cli, sc.containerID, sc.id, sc.pauseKind == PausePause)
			}
		}
	}
	ranPost := false
	runPost := func() {
		if ranPost {
			return
		}
		ranPost = true
		pctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		for _, sc := range caps {
			if !sc.ranPre {
				continue
			}
			for _, h := range sc.post {
				_ = e.runHook(pctx, cli, sc.containerID, h, sc.id, "post")
			}
		}
	}
	defer runPost()
	defer resumeAll()

	// Application-layer pre-hooks run BEFORE any pause (a paused container can't
	// exec). A failing pre-hook fails only that service.
	for _, sc := range caps {
		for _, h := range sc.pre {
			if herr := e.runHook(ctx, cli, sc.containerID, h, sc.id, "pre"); herr != nil {
				sc.captureErr = fmt.Errorf("pre-backup hook: %w", herr)
				e.logf(sc.id, "ERR", "Pre-backup hook failed: %v", herr)
				break
			}
			sc.ranPre = true
		}
	}

	for _, op := range plan {
		sc := byKey[op.service]
		if sc == nil || sc.captureErr != nil {
			continue
		}
		switch op.kind {
		case "pause":
			// The whole point of a consistent snapshot is to quiesce the app tier,
			// so an unset/none policy is upgraded to Pause here (a per-run "stop"
			// still wins). Databases never reach a pause op.
			mode, pauseByOperator := e.pauseModeFor(sc.opts, sc.insp.Config.Image, sc.name)
			if opts.PauseMode != "" {
				mode, pauseByOperator = opts.PauseMode, true
			}
			// #3: the same marker check the ordinary capture does. It lives in both
			// because these two paths are two implementations of one capture, and
			// every time a guard has been added to only one of them it was found
			// missing from the other by an incident rather than by a test.
			mode = e.guardStatefulVolumes(ctx, cli, sc.man, sc.containerID, sc.id, sc.name, sc.engineKind, mode, pauseByOperator, sc.volDests)
			if mode == PauseNone {
				mode = PausePause
			}
			if mode == PauseStop {
				e.logf(sc.id, "INFO", "Stopping %q for the consistency window", sc.name)
				if serr := cli.ContainerStop(ctx, sc.containerID, container.StopOptions{}); serr == nil {
					sc.paused, sc.pauseKind = true, PauseStop
				} else {
					e.logf(sc.id, "WARN", "Stop failed (%v) — capturing %q live", serr, sc.name)
				}
			} else {
				e.logf(sc.id, "INFO", "Pausing %q for the consistency window (brief freeze)", sc.name)
				if perr := cli.ContainerPause(ctx, sc.containerID); perr == nil {
					sc.paused, sc.pauseKind = true, PausePause
				} else {
					e.logf(sc.id, "WARN", "Pause failed (%v) — capturing %q live", perr, sc.name)
				}
			}
		case "dump":
			e.logf(sc.id, "INFO", "Dumping %s database live for %q", sc.engineKind, sc.name)
			// F205: the same operator-recorded Redis password the ordinary path
			// uses, resolved inside dumpDatabase from this ref — one lookup, one
			// code path, so a stack backup and a single-container backup of the
			// same broker cannot disagree about whether a snapshot is possible.
			dump, derr := e.dumpDatabase(ctx, cli, ContainerRef{NodeID: sc.nodeID, Name: sc.name}, sc.containerID, sc.engineKind, sc.env, sc.work, sc.service, sc.opts.Databases)
			if derr != nil {
				// F191: the SAME Redis fallback the ordinary path has (F185). This
				// path failed the whole service — and with it the group looked
				// broken — for exactly the failure the fallback exists for, because
				// the fallback was added to one dump call site and this is the
				// other. Redis only, for the reasons documented at that case: an
				// RDB in /data is a complete artifact at Redis's chosen moment,
				// where a torn SQL data directory is not a backup at all.
				if redisAuthUnavailable(sc.engineKind, derr) {
					e.logf(sc.id, "WARN", "Could not authenticate to Redis, so no consistent snapshot was taken — capturing its /data directory as files instead. That holds the RDB Redis last wrote on its own save schedule, so a restore works from a slightly older point in time. Set REDIS_PASSWORD on this container for a point-in-time snapshot.")
					e.logf(sc.id, "WARN", "Redis said: %s", redisServerReply(derr))
					sc.man.DBFallback = redisAuthFallbackNote
					sc.engineKind = ""
					// The data dir was EXCLUDED from the volume selection because
					// this dump was going to replace it. It no longer is, so put it
					// back — its tar op runs after every dump op, which is what
					// makes this recoverable at all.
					e.readdDataDirForFallback(sc, dockercli.DBDataDir("redis"))
					continue
				}
				sc.captureErr = fmt.Errorf("database dump: %w", derr)
				e.logf(sc.id, "ERR", "Database dump failed for %q: %v", sc.name, derr)
				continue
			}
			sc.man.Databases = append(sc.man.Databases, dump)
			e.logf(sc.id, "INFO", "Database dump complete: %s (%s)", dump.Path, humanBytes(dump.Bytes))
		case "tar":
			if len(sc.volDests) == 0 {
				continue
			}
			e.logf(sc.id, "INFO", "Archiving %d volume path(s) for %q via sidecar", len(sc.volDests), sc.name)
			volTar := filepath.Join(sc.work, "volumes.tar")
			// The app-consistent path writes no file index, so the per-file hashes
			// this returns have nowhere to be recorded.
			volSHA, _, verr := e.spoolVolumeTar(ctx, cli, sc.id, sc.containerID, sc.volDests, volTar)
			if verr != nil {
				sc.captureErr = fmt.Errorf("volume archive: %w", verr)
				e.logf(sc.id, "ERR", "Volume archive failed for %q: %v", sc.name, verr)
				continue
			}
			sc.man.VolumesSHA256 = volSHA
			sc.sqlite = e.captureSQLite(ctx, cli, sc.containerID, sc.volDests, sc.work, sc.id, sc.paused || !sc.running)
		case "resume":
			if sc.paused && !resumed[sc.key] {
				resumed[sc.key] = true
				e.resumeContainer(cli, sc.containerID, sc.id, sc.pauseKind == PausePause)
			}
		}
	}
	// Window over: end downtime NOW (resume + post-hooks) before the slow
	// snapshot/pack/encrypt/store/verify tail, which works entirely from the
	// spooled files.
	resumeAll()
	runPost()

	// PHASE 3 — finalize each captured service OUTSIDE the pause window.
	ok := 0
	for _, sc := range caps {
		// F116: a corruption verdict fails THIS service only. The group's other
		// members are already captured and still form a coherent point-in-time;
		// discarding them because a sibling's database is damaged would turn one
		// problem into several.
		if sc.captureErr == nil {
			sc.captureErr = e.recordSQLite(sc.man, sc.sqlite, sc.id)
		}
		// F143: certificates are static files, so their inventory belongs out here
		// rather than inside the window — a stack snapshot should hold every
		// service still for as short a time as it can.
		if sc.captureErr == nil && len(sc.volDests) > 0 && sc.insp.Config != nil {
			e.inventoryCertificates(ctx, cli, sc.containerID, sc.insp.Config.Image, sc.man, sc.id)
			// F147: and the permissions of the files whose exposure matters.
			e.auditSecretFiles(ctx, cli, sc.containerID, sc.insp.Config.Image, sc.man, sc.id)
			// F157: and the leftovers of writes that never finished.
			e.scanStagingDebris(ctx, cli, sc.containerID, sc.insp.Config.Image, sc.volDests, sc.man, sc.id)
		}
		if sc.captureErr != nil {
			_ = e.fail(sc.b, sc.captureErr)
			missed = append(missed, sc.key)
			if firstErr == nil {
				firstErr = fmt.Errorf("service %q: %w", sc.service, sc.captureErr)
			}
			continue
		}
		man := sc.man
		if _, serr := e.storeAndVerify(ctx, sc.id, nodeName, sc.stack, sc.name, sc.b, &man, sc.opts, sc.work, sc.algo, sc.zlevel, sc.zwindow, sc.uncompressed, sc.start); serr != nil {
			// Under the service's own id: an error under the stack's id ends the
			// stack's console while the other services are still finishing.
			e.logf(sc.id, "ERR", "Finalizing the backup of %q failed: %v", sc.name, serr)
			missed = append(missed, sc.key)
			if firstErr == nil {
				firstErr = fmt.Errorf("service %q: %w", sc.service, serr)
			}
			continue
		}
		ok++
	}
	if len(missed) == 0 {
		e.logf(logID, "INFO", "App-consistent snapshot complete — all %d service(s) captured in group %s", ok, groupID)
		return nil
	}
	e.logf(logID, "ERR", "App-consistent snapshot failed for %d of %d service(s): %s — %d captured in group %s", len(missed), len(members), strings.Join(missed, ", "), ok, groupID)
	return firstErr
}

// capsToMembers projects prepared captures onto the pure planning input. A
// service counts as a DB only if a live dump will actually be taken
// (engineKind survived the stopped/no-tools checks), matching Run's semantics.
func capsToMembers(caps []*serviceCapture) []consistencyMember {
	m := make([]consistencyMember, len(caps))
	for i, sc := range caps {
		m[i] = consistencyMember{service: sc.key, isDB: sc.engineKind != "", running: sc.running}
	}
	return m
}

// memberKey is the stable ordering/lookup key for a stack member: its compose
// service name, or the container name when unlabeled.
func memberKey(c *dockercli.Container) string {
	if c.Service != "" {
		return c.Service
	}
	return c.Name
}
