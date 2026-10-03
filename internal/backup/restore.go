package backup

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"dockback/internal/crypto"
	"dockback/internal/dockercli"
	"dockback/internal/notify"
	"dockback/internal/store"
)

// RestoreOptions controls a restore.
type RestoreOptions struct {
	BackupID string
	NodeID   string
	TargetID string // container to restore into (volumes + db)
	Volumes  bool   // restore volume data
	Database bool   // re-import database dumps
	Recreate bool   // force recreate-from-manifest even if a container exists
	// PromoteRestartPolicy rewrites a policy that will not survive a reboot to
	// `unless-stopped` (#8). Off by default — the finding at capture is the
	// default behaviour, and changing an availability setting unasked is the
	// deviation #31 exists to stop.
	PromoteRestartPolicy bool
	// InjectHealthchecks adds a probe to a recreated DATABASE container that has
	// none (#16). Off by default, and never over an existing probe — Step 19
	// reports the weak ones, and replacing an operator's own probe is the same
	// deviation. R2 §16: `service_started` waits only for the container to exist,
	// so everything downstream races the database on every boot.
	InjectHealthchecks bool
	Snapshot           bool // back up the target's CURRENT state before overwriting so a bad restore is reversible
	// Source picks which copy to read from: "" = auto (local first, then
	// offsite), "local", or a destination ID. A chosen copy that is unreachable
	// or corrupt falls back to the others.
	Source string

	// stackMember marks an INTERNAL per-service call made by RestoreStack (F146).
	// The stack path has already judged the whole atomic set — every member
	// present, all from one snapshot — so the per-service solo guard must not
	// fire again and refuse the very restore that guard exists to steer people
	// towards. Not settable from the API (unexported).
	stackMember bool

	// rollback marks an INTERNAL rollback restore of a pre-restore safety snapshot
	// (F4). It skips the post-restore health gate so returning to a
	// previously-unhealthy prior state doesn't recurse into another rollback. Not
	// settable from the API (unexported) — only the engine's own rollback sets it.
	rollback bool

	// AsName, when set, restores as an ISOLATED CLONE into a NEW container of this
	// name instead of overwriting the original (F10) — so a backup can be brought
	// up alongside the live container to inspect data or test an upgrade. The
	// original is never touched: no in-place overwrite, no safety snapshot.
	AsName string
	// Isolated (clone mode) gives the clone fresh empty volumes, no published
	// ports, and a throwaway network, so it can't clash with or affect the original.
	Isolated bool
	// TestCloneTTL, when set, stamps the clone with an expiry label so the reaper
	// removes it (and the volumes Docker made for it) once the time is up — F219.
	// Only meaningful alongside AsName + Isolated; a clone the operator named
	// themselves is theirs to keep, and nothing sweeps it.
	TestCloneTTL time.Duration

	// ReconstructHost, when set, rebuilds the on-host compose project layout after a
	// disaster-recovery RECREATE: it creates the stack directory and writes the
	// reconstructed docker-compose.yml back to where the container lived, so a
	// restore onto a fresh machine reproduces the organized <base>/<stack>/ folder,
	// not just the container. Opt-in; never overwrites an existing compose file
	// (writes a clearly-named sibling instead). Ignored for clones and for in-place
	// restores where the container already exists.
	ReconstructHost bool
	// HostBaseDir is the fallback base directory (e.g. /opt/docker) under
	// which a NON-compose container's stack folder is placed (<HostBaseDir>/<name>)
	// when the backup carries no compose working_dir. Ignored when a working_dir is
	// recorded. Only consulted with ReconstructHost.
	HostBaseDir string

	// RemapFromIP/RemapToIP, when both set to valid IPs, rewrite the SOURCE machine
	// IP to the TARGET machine IP in the recreated container config (published-port
	// host IPs, env vars, extra_hosts) and in the reconstructed compose file — so a
	// service pinned to the old host's IP comes up on the new host instead of
	// failing to bind. Applied only on the recreate path; a no-op when equal/blank.
	RemapFromIP string
	RemapToIP   string

	// RemapFromDomain/RemapToDomain (F195) rewrite a DOMAIN the same way the IP
	// pair rewrites an address: literally, wherever it appears in the recreated
	// container's environment. This is the address mechanism for containers
	// DockBack has no profile for — no guessing which variable "is the address";
	// only values carrying the old name change, so an upstream pointing at a
	// different machine is never touched.
	RemapFromDomain string
	RemapToDomain   string

	// RemapFromPath/RemapToPath (F81), when both set to different absolute base
	// directories, rewrite bind-mount host sources, the reconstructed compose
	// file's paths, and the stack folder location from the SOURCE machine's base
	// to the TARGET machine's (e.g. /home/alice/docker → /opt/stacks) — exact
	// prefix matches only, every substitution logged. Applied only on the
	// recreate path; a no-op when equal/blank.
	RemapFromPath string
	RemapToPath   string

	// NewSiteAddress (F114) is the address the application will be reached at
	// after this restore, supplied by the operator when — and only when — it is
	// changing.
	//
	// Empty is the normal case and means DO NOTHING: a move that keeps the same
	// domain (DNS or the reverse proxy re-pointed at the new host) needs no
	// address change at all, and applying one anyway is its own way to break a
	// working restore.
	//
	// When set, it drives the app's declared address bindings: env values are
	// rewritten as the container is recreated, the app's own CLI is invoked for
	// the settings it owns (Nextcloud's occ), and anything irreversible is
	// printed for the operator instead of being run. Validated with
	// ValidSiteAddress before it reaches any of them.
	NewSiteAddress string

	// NewUpstreamAddress (F160) is the address of a service this application
	// CONNECTS OUT TO, when that service is the thing that moved.
	//
	// The mirror image of NewSiteAddress above, and it is needed at a different
	// moment: that one is for "this app is now reached somewhere else", this one
	// for "the app is exactly where it was, and the thing it talks to is not".
	//
	// Empty is the normal case and means DO NOTHING. Applied before the container
	// starts, to the declared keys only, and validated with ValidSiteAddress
	// first. Nothing is ever re-authenticated: the credential a dependency issued
	// is bound to its identity, not to its address.
	NewUpstreamAddress string

	// PrivateKey is the offline X25519 private key for a WRITE-ONLY backup (F86).
	// Supplied per restore, held in memory for that operation only, never
	// persisted and never logged. Empty for normal (symmetric) backups, which is
	// every backup taken before write-only mode was enabled.
	PrivateKey string
}

// Restore restores volume data and/or database dumps from a backup into a
// target container. Restore is destructive — callers must confirm first
// . Image re-pull-by-digest and full stack recreation are driven by
// the manifest; v1 restores the irreplaceable state (volumes + databases).
func (e *Engine) Restore(ctx context.Context, opts RestoreOptions) error {
	b, err := e.Store.GetBackup(opts.BackupID)
	if err != nil {
		return err
	}
	man := &Manifest{}
	if b.ManifestJSON != "" {
		_ = unmarshal(b.ManifestJSON, man)
	}
	// F86: install the offline private key for the duration of THIS restore only.
	// Validated up front so a wrong or missing key fails before anything is
	// stopped, snapshotted or overwritten — never mid-restore.
	//
	// A ROLLBACK inherits the key rather than being given one. The health gate's
	// rollback restores the pre-restore safety snapshot, and while write-only
	// mode is armed that snapshot is sealed to the offline keypair like every
	// other backup — so the rollback needs a key it was never passed, and the
	// operator cannot be asked for it again halfway through a failing restore.
	// The outer restore already holds the gate and installed the key, so the
	// rollback reads that one. Without this the single safety net write-only mode
	// offers failed every time it was needed, leaving the container in an
	// intermediate state and the operator with a manual recovery.
	priv := opts.PrivateKey
	if priv == "" && opts.rollback {
		priv = e.restorePrivFor()
	}
	if IsWriteOnly(man) {
		if err := CheckPrivateKey(man, priv); err != nil {
			return err
		}
	}
	// A rollback never touches the gate: it runs INSIDE the restore that holds
	// it, and privGate is a plain mutex, so taking it again would park this
	// goroutine forever rather than fail (see Engine.privGate). It inherits the
	// installed key instead of installing its own.
	if opts.PrivateKey != "" && !opts.rollback {
		// F209: the gate serializes key-bearing restores; the key itself is stored
		// atomically so THIS goroutine can read it back (archiveKey, Verify) without
		// re-entering the gate it already holds. See Engine.privGate.
		e.privGate.Lock()
		key := opts.PrivateKey
		e.restorePriv.Store(&key)
		defer func() {
			e.restorePriv.Store(nil)
			e.privGate.Unlock()
		}()
	}
	// F141: some applications keep one configuration in two volumes, and half of
	// it restores into a broken or factory-fresh deployment. Checked FIRST —
	// before the target is resolved, before the safety snapshot, before anything
	// is stopped — so a refusal costs nothing and changes nothing. Applies to
	// clones too: an isolated clone built from half a configuration is a
	// rehearsal that teaches the wrong thing.
	if aerr := AtomicVolumeVerdict(man); aerr != nil {
		e.logf(b.ID, "ERROR", "Restore refused: %v", aerr)
		return aerr
	}

	// F146: this backup is one service of an application whose services are only
	// meaningful together. Restoring it alone gives a healthy container and a
	// deployment that does not work, so it is refused here — before the target is
	// resolved and before anything is snapshotted.
	//
	// An isolated CLONE is exempt: it overwrites nothing, and inspecting a backup
	// must not require permission. A stack restore reaches this through
	// RestoreStack, which has already judged the whole set (StackAtomicGroupVerdict)
	// and marks its per-service calls accordingly.
	if !opts.stackMember {
		if serr := StackAtomicSoloRestoreVerdict(man, opts.AsName != ""); serr != nil {
			e.logf(b.ID, "ERROR", "Restore refused: %v", serr)
			return serr
		}
	}

	// F23: a standalone-volume backup ("volume:<name>") has no container — it
	// restores by recreating the named volume and untarring its contents back in.
	if strings.HasPrefix(b.TargetName, volumeTargetPrefix) {
		return e.restoreVolumeOnly(ctx, b, opts)
	}
	e.logf(b.ID, "INFO", "Restore starting into container %s (volumes=%v db=%v)", short(opts.TargetID), opts.Volumes, opts.Database)
	// Cross-host portability: the backup may be restored onto a
	// different node than it came from. Networks/volumes are recreated by their
	// original names on the target; the image is re-pulled by digest (or loaded
	// from the bundled tarball) on that node.
	if man.NodeID != "" && man.NodeID != opts.NodeID {
		e.logf(b.ID, "INFO", "Cross-host restore: backup from node %s → restoring onto node %s", man.NodeID, opts.NodeID)
	}

	cli, err := e.Reg.Get(opts.NodeID)
	if err != nil {
		return err
	}

	// Clone mode (F10): restore as an isolated copy under a NEW name. The clone
	// doesn't exist yet, so point the target at the new name (forcing the recreate
	// path) and NEVER resolve/overwrite the original container.
	clone := opts.AsName != ""
	if clone {
		e.logf(b.ID, "INFO", "Restore as a copy: bringing %q up as an isolated clone of %q — the original is left untouched", opts.AsName, b.TargetName)
		opts.TargetID = opts.AsName
	}

	// The manifest stores the container's id at backup time, which goes stale
	// once the container is recreated (new id, same name — e.g. a redeploy or a
	// prior restore). Resolve the live container by NAME so we restore INTO it
	// instead of colliding with it on recreate.
	if !clone {
		if _, ierr := cli.ContainerInspect(ctx, opts.TargetID); ierr != nil {
			targetName := man.TargetName
			if targetName == "" {
				targetName = b.TargetName
			}
			if cid, ok := dockercli.FindContainerByName(ctx, cli, targetName); ok {
				e.logf(b.ID, "INFO", "Target %q found under a new id — restoring into it", targetName)
				opts.TargetID = cid
			}
		}
	}

	// Safety snapshot before an in-place overwrite: back up the target's
	// CURRENT state so a bad restore is reversible. Only when the container exists
	// (nothing to capture when recreating from scratch). Abort the restore if the
	// snapshot fails — never destroy data without a rollback point.
	var snapID string                              // pre-restore safety snapshot id ("" = none taken) — the rollback point for the F4 health gate
	if opts.Snapshot && !opts.rollback && !clone { // a clone overwrites nothing, so there's nothing to snapshot
		if _, ierr := cli.ContainerInspect(ctx, opts.TargetID); ierr == nil {
			snapNode := opts.NodeID
			if n, nerr := e.Store.GetNode(opts.NodeID); nerr == nil && n.Name != "" {
				snapNode = n.Name
			}
			e.logf(b.ID, "INFO", "Safety snapshot: backing up current state before overwrite")
			sid, serr := e.Run(ctx, snapNode, Options{
				NodeID: opts.NodeID, ContainerID: opts.TargetID, Compression: "balanced",
				DestinationsExplicit: true, // local-only — an immediate rollback point, not an offsite copy
				ForceFull:            true, // a rollback point must be self-contained, never a delta on an in-flight chain (F61)
				// F208: and it must not drag a retention sweep in behind it. This
				// snapshot shares its TargetName with the backup being restored, so
				// the sweep at the end of a verified backup could prune that very
				// archive — before the restore below has opened it. See
				// Options.SkipRetention for the whole reasoning.
				SkipRetention: true,
			})
			if serr != nil {
				return fmt.Errorf("pre-restore safety snapshot failed; aborting to avoid an unreversible overwrite: %w", serr)
			}
			snapID = sid
			e.logf(b.ID, "INFO", "Safety snapshot created (%s) — restore it to roll back this restore", short(snapID))
		}
	}

	// Disaster recovery: if the target container still doesn't exist, recreate it
	// from the saved config (image, ports, env, binds, volumes, networks,
	// healthcheck, restart policy…) before restoring its data.
	//
	// ONLY a real not-found answer means the container is gone. Any error used to
	// take this branch, so a Docker API timeout or a blip from the socket proxy
	// turned an in-place restore into a recreate — tearing down and rebuilding a
	// container that was there all along. An unanswered question is not an
	// absent container, so it fails instead.
	_, ierr := cli.ContainerInspect(ctx, opts.TargetID)
	missing := ierr != nil && client.IsErrNotFound(ierr)
	if ierr != nil && !missing {
		return fmt.Errorf("inspect %s: %w", opts.TargetID, ierr)
	}
	if missing || opts.Recreate {
		// Say which of the two it is: "not found" on a container the operator
		// deliberately asked to recreate reads as data loss that did not happen.
		if missing {
			e.logf(b.ID, "INFO", "Target container not found — recreating from backup (disaster recovery)")
		} else {
			e.logf(b.ID, "INFO", "Recreating the container from the backup's saved configuration, as requested")
		}
		inspectBytes, err := e.extractEntry(ctx, b, opts.Source, "config/inspect.json")
		if err != nil {
			return fmt.Errorf("cannot recreate: %w", err)
		}
		// Optional host-IP remap (cross-host DR): rewrite the source machine's IP to
		// the target machine's IP in the recreated config, so a port pinned to the old
		// host's address (or an env/extra-host self-reference) doesn't fail on the new
		// host. Best-effort — a bad remap never blocks the restore.
		if opts.RemapFromIP != "" && opts.RemapToIP != "" {
			if out, n, rerr := dockercli.RemapContainerHostIP(inspectBytes, opts.RemapFromIP, opts.RemapToIP); rerr != nil {
				e.logf(b.ID, "WARN", "Host-IP remap skipped: %v", rerr)
			} else if n > 0 {
				inspectBytes = out
				e.logf(b.ID, "INFO", "Remapped host IP %s → %s in %d place(s) of the recreated config (ports/env/extra-hosts)", opts.RemapFromIP, opts.RemapToIP, n)
			} else {
				// F232: a remap that matched nothing on a move is not reassurance.
				e.logf(b.ID, "WARN", "Host-IP remap found nothing to change: %s does not appear literally in this container's ports, environment or extra-hosts. "+
					"If the address reaches it through a ${VAR} in the stack's .env, that file is what carries it — check the .env written beside the compose file, "+
					"because `docker compose up` re-reads it and would put the old address back", opts.RemapFromIP)
			}
		}
		// F195: the domain, by the same rule. For an application with a profile
		// this and the new-address binding can both run; they agree by
		// construction, because both write the same new name.
		if opts.RemapFromDomain != "" && opts.RemapToDomain != "" {
			if out, n, rerr := dockercli.RemapContainerHostDomain(inspectBytes, opts.RemapFromDomain, opts.RemapToDomain); rerr != nil {
				e.logf(b.ID, "WARN", "Domain remap skipped: %v", rerr)
			} else if n > 0 {
				inspectBytes = out
				e.logf(b.ID, "INFO", "Remapped domain %s → %s in %d place(s) of the recreated config (environment)", opts.RemapFromDomain, opts.RemapToDomain, n)
			} else {
				// F232: same. This one read "nothing to change", which an operator
				// moving a proxied app between machines reads as "it is handled".
				e.logf(b.ID, "WARN", "Domain remap found nothing to change: %s does not appear literally in this container's environment. "+
					"If the domain reaches it through a ${VAR} in the stack's .env, that file is what carries it — check the .env written beside the compose file, "+
					"because `docker compose up` re-reads it and would put the old domain back", opts.RemapFromDomain)
			}
		}
		// Optional host-path remap (F81): move bind-mount sources from the source
		// machine's base directory to the target's, so Docker doesn't materialize
		// the old machine's layout on the new host. Best-effort, like the IP remap.
		if opts.RemapFromPath != "" && opts.RemapToPath != "" && opts.RemapFromPath != opts.RemapToPath {
			if out, n, rerr := dockercli.RemapContainerHostPath(inspectBytes, opts.RemapFromPath, opts.RemapToPath); rerr != nil {
				e.logf(b.ID, "WARN", "Host-path remap skipped: %v", rerr)
			} else if n > 0 {
				inspectBytes = out
				e.logf(b.ID, "INFO", "Remapped host path %s → %s in %d place(s) of the recreated config (bind mounts)", opts.RemapFromPath, opts.RemapToPath, n)
			}
		}
		// F114: the operator moved the app to a different address. Env-recorded
		// addresses are applied HERE, while the container is being built — an env
		// change is undone by recreating again, which is what makes it the one
		// class of address fix that can be applied automatically.
		if opts.NewSiteAddress != "" {
			inspectBytes = e.applyEnvAddressBindings(b, man, inspectBytes, opts.NewSiteAddress)
		}
		// #8: the operator's own choice, applied only when they made it.
		inspectBytes = e.applyRestartPolicyPromotion(b, inspectBytes, opts)
		// #16: and the other one. A database with no probe gives the gate below —
		// and every service waiting on it — nothing better than "the process
		// exists", which for a database is not an answer.
		inspectBytes = e.applyHealthcheckInjection(b, man, inspectBytes, opts)
		// #7 / PLAYBOOK §2.1: environmental rules — the ones that apply because of
		// a measured difference between these two machines rather than because a
		// restore is happening. Decided here, beside the other inspect rewrites,
		// and every verdict is reported: a skipped rule an operator never hears
		// about is indistinguishable from one nobody considered.
		inspectBytes = e.applyEnvironmentalRules(ctx, cli, b, man, inspectBytes)
		// #38: the environment as CAPTURED, before any pass edits it. Compared
		// against the document the create actually uses, it names exactly the keys
		// this restore rewrote — which is what lets the check afterwards allow the
		// deviations the restore performed and nothing else. Doing it here, rather
		// than threading a set through every pass, also covers the IP, domain and
		// path remaps, which can touch any key's value and never knew which.
		capturedEnv := dockercli.ContainerEnv(inspectBytes)
		// F189: an ownership pin has to change the CONTAINER as well as the files.
		//
		// The pin says "this container runs as these ids here". Chowning the
		// restored data to them and leaving the container's own USERMAP_UID /
		// PUID at the source machine's values is worse than doing nothing: the
		// application drops to the old uid and then cannot write the files that
		// were just handed to the new one. Both halves, or neither.
		inspectBytes = e.applyRunAsIDs(b, man, inspectBytes, opts)
		// #17: a clone must not be able to act on the real world with the
		// original's authority. Before the create, because environment is fixed at
		// creation — a copy that starts holding the original's mail credentials has
		// already been able to send by the time anything could edit them.
		inspectBytes = e.applyCloneNeutralizations(b, man, inspectBytes, clone)
		// Air-gapped path: if the backup bundled the image (`docker save`), load it
		// so recreate finds it locally and needs no registry (PLAN §0.3 / §8.4).
		if man.ImageTar != nil {
			e.logf(b.ID, "INFO", "Loading bundled image for air-gapped restore: %s (%s)", man.ImageTar.Ref, humanBytes(man.ImageTar.Bytes))
			if lerr := e.loadImageTar(ctx, cli, b, opts.Source); lerr != nil {
				e.logf(b.ID, "WARN", "Image load failed (%v) — falling back to pull", lerr)
			} else {
				e.logf(b.ID, "INFO", "Bundled image loaded")
			}
		} else if man.ImageDigest != "" {
			e.logf(b.ID, "INFO", "Re-pulling identical image by digest: %s", man.ImageDigest)
		}
		var cloneOpts *dockercli.CloneOptions
		if clone {
			cloneOpts = &dockercli.CloneOptions{NewName: opts.AsName, Isolate: opts.Isolated}
			// F219: the clone carries its own expiry. Stamped at CREATE time, from
			// the moment the container actually comes into being rather than from
			// when the request was accepted — a restore that spent twenty minutes
			// pulling an image should still get its full test window.
			if opts.TestCloneTTL > 0 && opts.Isolated {
				cloneOpts.Labels = TestCloneLabels(time.Now().Add(opts.TestCloneTTL))
			}
		}
		// #37: refuse a restore that would fill the destination's filesystem, before
		// a single volume, directory or file is created. A refusal issued after 58
		// GB has landed is not a refusal.
		if cerr := e.guardRestoreCapacity(ctx, cli, b, man, opts, clone); cerr != nil {
			return cerr
		}

		// F90: create named volumes with their RECORDED driver options first.
		// Docker auto-creates a missing named volume as a plain local one the
		// moment the container references it — which is how an NFS/CIFS-backed
		// volume silently became local disk. Creating them up front, with their
		// options, is what makes the backing store survive.
		if !clone {
			for _, w := range e.ensureRecordedVolumes(ctx, cli, man) {
				e.logf(b.ID, "WARN", "Volume: %s", w)
			}
			// A bind whose root is a FILE is written to the host now, before the
			// container exists — it cannot be restored afterwards through its own
			// mount point. Ahead of the preflight below on purpose: the file it
			// writes is one of the sources that preflight would otherwise report
			// as missing.
			e.restoreBindFiles(ctx, cli, b, man, opts)
			// Every bind source has to exist on THIS host before the create, or the
			// daemon will either refuse it one path at a time or invent empty
			// directories in their place — including where a file belongs. The ones
			// this backup describes are created here; anything left is refused with
			// the whole list. A clone is exempt: stripForClone drops every bind, so
			// it has no host paths to need.
			if err := e.materializeBindSources(ctx, cli, b, man, inspectBytes); err != nil {
				return err
			}
		}

		// F89: hand the recorded topology to the recreate so networks come back with
		// their subnet, internal flag and this container's static address. A clone is
		// deliberately given NONE of it — an isolated clone must stay on its throwaway
		// network and must never claim the original's pinned addresses.
		var netSpecs []dockercli.NetworkSpec
		if !clone {
			netSpecs = networkSpecsFrom(man.Networks)
		}
		newID, name, netWarnings, err := dockercli.RecreateContainer(ctx, cli, inspectBytes, man.ImageDigest, cloneOpts, netSpecs)
		if err != nil {
			return fmt.Errorf("recreate container: %w", err)
		}
		// Anything that could not be honored is stated plainly — a silently
		// different network is exactly the failure this feature exists to end.
		for _, w := range netWarnings {
			e.logf(b.ID, "WARN", "Network: %s", w)
		}
		// #38: prove the environment came back — what was captured, plus exactly
		// the deviations this restore performed. Before the start, because a
		// container missing the variable that gives its bind mount meaning starts
		// perfectly well and is wrong.
		if eerr := e.assertEnvRestored(ctx, cli, b, man, newID, capturedEnv, changedEnvKeys(capturedEnv, dockercli.ContainerEnv(inspectBytes))); eerr != nil {
			return eerr
		}
		if clone {
			e.logf(b.ID, "INFO", "Created isolated clone %q (%s)", name, short(newID))
		} else {
			e.logf(b.ID, "INFO", "Recreated container %q (%s)", name, short(newID))
			// Optionally rebuild the on-host stack directory + compose file so a
			// restore onto a fresh machine reproduces the organized layout, not just
			// the container. Non-fatal: the container is already up either way.
			// F190: skipped for a stack member. Every service in a project shares
			// one working directory and one compose filename, so writing per
			// service means five files racing for one path and the last one
			// winning — a compose file describing one service out of five. The
			// stack restore writes a single merged file once, after every member
			// is in place.
			if opts.ReconstructHost && !opts.stackMember {
				e.reconstructHostStack(ctx, cli, b, man, opts, inspectBytes)
			}
			if !opts.stackMember {
				e.noteUnwrittenOriginals(b.ID, man, opts.ReconstructHost)
			}
		}
		opts.TargetID = newID
	}

	// Run the appropriate restore path, then gate on health (F4).
	var rerr error
	switch {
	case man.AppExport != nil:
		// Portable app-native export → restore via the application's own importer
		// (gold standard, PLAN §9.4).
		rerr = e.restoreAppExport(ctx, cli, b, man, opts)
	case opts.Database && len(man.Databases) > 0 && embeddedDumpFor(man) != nil:
		// F126: an APPLICATION that bundles its own database server. It needs BOTH
		// halves — the other two branches are either/or, and either alone is
		// useless here: the files without the dump give an app with no data, the
		// dump without the files gives data with nothing to read it.
		rerr = e.restoreEmbeddedDBApp(ctx, cli, b, man, opts)
	case opts.Database && len(man.Databases) > 0:
		// A database container is restored from its CONSISTENT logical dump, never
		// from the raw (live-copied) data directory — guarantees healthy data.
		// Application containers restore their files/volumes.
		rerr = e.restoreDatabaseFromDump(ctx, cli, b, man, opts)
	default:
		rerr = e.restoreFiles(ctx, cli, b, man, opts)
	}
	if rerr != nil {
		return rerr
	}

	// F140: post-restore application steps, BEFORE the health gate. For Nextcloud
	// this is what takes the restored instance out of the maintenance mode the
	// backup deliberately captured it in — without it every page returns 503, the
	// gate fails, and the restore rolls back. The gate is the thing this defeats,
	// so it has to run first.
	e.runRestoreHooks(ctx, cli, b.ID, opts.TargetID, manifestImage(man, b), opts.NodeID, b.TargetName)

	// A clone runs ISOLATED (no network to its dependencies), so it often can't
	// reach "healthy" even though it restored fine — the health gate (and its
	// rollback) don't apply. Report the clone as done; the original is untouched.
	if clone {
		if opts.TestCloneTTL > 0 && opts.Isolated {
			e.logf(b.ID, "INFO", "Restore completed — test clone %q is up with the restored data (no published ports; on the %q network). The original container was not touched, and this clone is removed automatically in %s along with the volumes created for it.", opts.AsName, "dockback-restore", humanDuration(opts.TestCloneTTL))
		} else {
			e.logf(b.ID, "INFO", "Restore completed — isolated clone %q is up with the restored data (no published ports; on the %q network). The original container was not touched.", opts.AsName, "dockback-restore")
		}
		return nil
	}

	// F4: post-restore health gate. A restore that leaves the container
	// crash-looping must not be reported as success — when a pre-restore safety
	// snapshot exists we automatically roll back to it, otherwise we report the
	// unhealthy outcome. An internal rollback restore skips the gate so returning
	// to a previously-unhealthy state doesn't recurse into another rollback.
	if opts.rollback {
		return nil
	}
	if gerr := e.gateRestoreHealth(ctx, cli, b, man, opts, snapID); gerr != nil {
		return gerr
	}
	// F113/F114: only once the restore is actually green. The app's own CLI needs
	// it running, and guidance on a failed restore is noise on top of a problem.
	e.appAddressFollowUp(ctx, cli, b, man, opts)
	// F175: and the same question for an application DockBack has no profile
	// for. Any container that records where it lives keeps recording the previous
	// machine after a move, whether or not this tool knows the app.
	e.reportCarriedAddresses(ctx, cli, b, man, opts)
	// F161: and prove the application can still reach what it depends on, using
	// the credentials that came back. Advisory — a dependency being unreachable
	// is not a defect in the backup.
	e.probeUpstream(ctx, cli, b, man, opts)
	return nil
}

// Post-restore health-gate timeout (F4/F30): how long the gate waits for the
// container to become healthy before deciding the restore didn't come up and
// rolling back. Configurable globally and per container so a heavy app (large DB
// migration on first boot) isn't force-rolled-back mid-migration.
const (
	defaultRestoreHealthSeconds = 300  // shipped default (5 minutes)
	minRestoreHealthSeconds     = 30   // smallest sane wait
	maxRestoreHealthSeconds     = 3600 // 1h ceiling to keep a stored value sane
	restoreHealthKey            = "restore.health_timeout_seconds"
)

// restoreHealthKeyFor is the optional per-container override key (F30).
func restoreHealthKeyFor(nodeID, name string) string {
	return restoreHealthKey + "." + nodeID + "." + name
}

// settingPosInt reads a positive integer setting, returning def when it's unset,
// malformed, or non-positive.
func (e *Engine) settingPosInt(key string, def int) int {
	v, _ := e.Store.GetSetting(key, "")
	if strings.TrimSpace(v) == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// RestoreHealthTimeoutGlobal is the effective global post-restore health timeout in
// SECONDS: the stored setting when present and valid, else the shipped default (F30).
func (e *Engine) RestoreHealthTimeoutGlobal() int {
	return e.settingPosInt(restoreHealthKey, defaultRestoreHealthSeconds)
}

// RestoreHealthTimeoutOverride returns a container's per-container override
// (seconds) and whether one is set (F30).
func (e *Engine) RestoreHealthTimeoutOverride(nodeID, name string) (int, bool) {
	v, _ := e.Store.GetSetting(restoreHealthKeyFor(nodeID, name), "")
	if strings.TrimSpace(v) == "" {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// RestoreHealthTimeoutEffective is the timeout (seconds) that actually applies to a
// container: its per-container override if set, else the global value (F30).
func (e *Engine) RestoreHealthTimeoutEffective(nodeID, name string) int {
	if n, ok := e.RestoreHealthTimeoutOverride(nodeID, name); ok {
		return n
	}
	return e.RestoreHealthTimeoutGlobal()
}

// SetRestoreHealthTimeoutOverride stores (or, when seconds <= 0, clears) a
// container's per-container health-timeout override, clamping a set value (F30).
func (e *Engine) SetRestoreHealthTimeoutOverride(nodeID, name string, seconds int) error {
	key := restoreHealthKeyFor(nodeID, name)
	if seconds <= 0 {
		return e.Store.SetSetting(key, "")
	}
	if seconds < minRestoreHealthSeconds {
		seconds = minRestoreHealthSeconds
	}
	if seconds > maxRestoreHealthSeconds {
		seconds = maxRestoreHealthSeconds
	}
	return e.Store.SetSetting(key, strconv.Itoa(seconds))
}

// restoreHealthTimeout returns the effective post-restore health-gate duration for
// a container — the per-container override, else the global setting, else the
// shipped 5-minute default (F30).
func (e *Engine) restoreHealthTimeout(nodeID, name string) time.Duration {
	return time.Duration(e.RestoreHealthTimeoutEffective(nodeID, name)) * time.Second
}

// restoreVerdict is the outcome of the post-restore health gate (F4).
type restoreVerdict int

const (
	restoreHealthy             restoreVerdict = iota // came up healthy → success
	restoreRollback                                  // unhealthy + a safety snapshot exists → roll back to it
	restoreUnhealthyNoSnapshot                       // unhealthy + no snapshot → report the failure, nothing to roll back to
)

// decideRestoreVerdict is the pure decision behind the health gate, split out so
// it can be unit-tested without Docker.
func decideRestoreVerdict(healthy, hasSnapshot bool) restoreVerdict {
	if healthy {
		return restoreHealthy
	}
	if hasSnapshot {
		return restoreRollback
	}
	return restoreUnhealthyNoSnapshot
}

// restoreForensics pulls the restored container's own last output when the health
// gate did not pass, writes the FULL excerpt to the run log, and returns a short
// REDACTED one for the alert.
//
// The split is deliberate. The run log stays on the operator's own instance and
// is the surface they diagnose from, so it gets everything — redacting it could
// remove the very line that explains the failure. The alert leaves the machine to
// a Gotify server, an SMTP relay or an arbitrary webhook, so its copy is masked
// and kept short.
//
// Best-effort throughout: this only ever runs on a path that has already failed,
// and a logs-read problem must never become the reported cause of that failure.
func (e *Engine) restoreForensics(ctx context.Context, cli *client.Client, b *store.Backup, targetID, name string) string {
	if targetID == "" {
		return ""
	}
	// A fresh context: the gate may have failed BECAUSE ctx was canceled, and the
	// evidence is most wanted in exactly that case.
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 25*time.Second)
	defer cancel()

	tail, err := dockercli.ContainerLogTail(lctx, cli, targetID, restoreLogTailLines)
	if err != nil {
		e.logf(b.ID, "WARN", "Could not read %s's logs for diagnosis (%v) — check them on the host with: docker logs %s", name, err, name)
		return ""
	}
	if strings.TrimSpace(tail) == "" {
		e.logf(b.ID, "WARN", "%s produced no log output — nothing to diagnose from; check the image's entrypoint", name)
		return ""
	}
	e.logf(b.ID, "ERR", "Last %d log line(s) from %s:\n%s", restoreLogTailLines, name, tail)
	return dockercli.RedactLogSecrets(dockercli.LastLines(tail, restoreAlertTailLines))
}

// alertTail formats a log excerpt for an alert body, or nothing when there was none.
func alertTail(excerpt string) string {
	if excerpt == "" {
		return ""
	}
	return "\n\nLast lines from the container (credential-looking values masked):\n" + excerpt
}

const (
	// restoreLogTailLines is what goes to the run log — enough to see a stack
	// trace, not so much that it buries the restore's own messages.
	restoreLogTailLines = 40
	// restoreAlertTailLines is what leaves the machine in a notification.
	restoreAlertTailLines = 15
)

// gateRestoreHealth waits for the restored container to become healthy and, if it
// doesn't, either rolls back to the pre-restore safety snapshot (when one was
// taken) or reports the unhealthy outcome — never silent success (F4). It returns
// a non-nil error in every non-healthy case so the caller (and the UI) sees the
// restore as failed.
func (e *Engine) gateRestoreHealth(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions, snapID string) error {
	e.logf(b.ID, "INFO", "Verifying the restored container comes up healthy…")
	// #35: if the probe about to be trusted cannot fail, say so before its
	// verdict is reported as proof. The gate itself is unchanged — rewriting an
	// application's probe would be introducing configuration it does not have.
	if hinsp, herr := cli.ContainerInspect(ctx, opts.TargetID); herr == nil {
		e.warnIfGateTrustsWeakProbe(b, hinsp)
	}
	healthy := dockercli.WaitForHealthy(ctx, cli, opts.TargetID, e.restoreHealthTimeout(opts.NodeID, b.TargetName))

	// F112: for an app that migrates its schema at startup and ships no
	// healthcheck, "running" is not yet a verdict — see migrationSettle.
	if healthy {
		healthy = e.migrationSettle(ctx, cli, b, opts)
	}

	// Operator canceled (or the run's deadline elapsed) while we were waiting.
	// The data restore itself already finished, so DON'T auto-roll-back — that
	// would silently throw away the restore the operator asked for. Stop, and say
	// exactly what state things are in (naming the snapshot for a manual undo).
	if ctx.Err() != nil {
		e.restoreForensics(ctx, cli, b, opts.TargetID, b.TargetName)
		if snapID != "" {
			e.logf(b.ID, "WARN", "Canceled while waiting for %s to become healthy — the restored data is IN PLACE and the container was left running. To undo, restore the pre-restore snapshot %s.", b.TargetName, short(snapID))
			return fmt.Errorf("%w while waiting for the container to become healthy — restored data is in place; restore the safety snapshot %s to undo", ErrRestoreCanceled, short(snapID))
		}
		e.logf(b.ID, "WARN", "Canceled while waiting for %s to become healthy — the restored data is IN PLACE and the container was left as-is (no safety snapshot was taken).", b.TargetName)
		return fmt.Errorf("%w while waiting for the container to become healthy — restored data is in place", ErrRestoreCanceled)
	}

	switch decideRestoreVerdict(healthy, snapID != "") {
	case restoreHealthy:
		e.logf(b.ID, "INFO", "Restored container is healthy")
		// #10/#33: healthy is not the same as usable. Ask the two questions a
		// health check cannot — where it sends a visitor, and whether anyone can
		// actually write to it. Findings only: the container is up, and both
		// answers are configuration the operator owns.
		e.verifyRestoredHTTP(ctx, cli, b, man, opts)
		return nil

	case restoreRollback:
		// Capture BEFORE the rollback: it replaces the container's state, taking
		// the evidence of why the restore failed with it.
		excerpt := e.restoreForensics(ctx, cli, b, opts.TargetID, b.TargetName)
		e.logf(b.ID, "WARN", "Restored data did not become healthy — rolling back to the pre-restore snapshot (%s)", short(snapID))
		rbErr := e.Restore(ctx, RestoreOptions{
			BackupID: snapID, NodeID: opts.NodeID, TargetID: opts.TargetID,
			Volumes: true, Database: true, Source: "local", rollback: true,
		})
		if rbErr != nil {
			// Worst case: the container is unhealthy AND the rollback failed, so the
			// data may be in an intermediate state. Surface it as loudly as possible.
			e.logf(b.ID, "WARN", "Rollback to the safety snapshot FAILED: %v", rbErr)
			e.notify(notify.KindRestoreFailed, "Restore FAILED and rollback FAILED: "+b.TargetName,
				fmt.Sprintf("Restoring %s produced an unhealthy container AND the automatic rollback to the pre-restore snapshot failed (%v). The container may be in an intermediate state — restore the safety snapshot %s by hand.%s", b.TargetName, rbErr, short(snapID), alertTail(excerpt)))
			return fmt.Errorf("restore did not become healthy and rollback failed (%v) — restore the safety snapshot %s by hand", rbErr, short(snapID))
		}
		e.logf(b.ID, "INFO", "Rolled back to the pre-restore snapshot — the container is back in its prior state")
		e.notify(notify.KindRestoreRolledBack, "Restore rolled back: "+b.TargetName,
			fmt.Sprintf("Restoring %s left the container unhealthy, so DockBack automatically rolled back to the pre-restore safety snapshot. The container is back in its prior state; investigate the backup before retrying.%s", b.TargetName, alertTail(excerpt)))
		return fmt.Errorf("restored data did not come up healthy — rolled back to the pre-restore snapshot")

	default: // restoreUnhealthyNoSnapshot
		excerpt := e.restoreForensics(ctx, cli, b, opts.TargetID, b.TargetName)
		e.logf(b.ID, "WARN", "Restored container did not become healthy and no safety snapshot was taken — leaving it as-is; investigate before relying on it")
		e.notify(notify.KindRestoreFailed, "Restore did not come up healthy: "+b.TargetName,
			fmt.Sprintf("Restoring %s produced a container that did not become healthy, and no pre-restore safety snapshot was available to roll back to. Investigate the container state before relying on it.%s", b.TargetName, alertTail(excerpt)))
		return fmt.Errorf("%w (no safety snapshot to roll back to)", ErrRestoreUnhealthy)
	}
}

// Startup-migration settle window (F112).
//
// WaitForHealthy accepts "running" for a container that declares no healthcheck,
// and it accepts it on the FIRST inspect. For an application that runs its
// database migrations at startup — BookStack's entrypoint runs
// `php artisan migrate --force` on every start — that verdict is reached a
// second or two in, while the migration is still going. If the migration then
// fails and the container exits, the restore has already been reported healthy
// and the failure is never noticed.
//
// So for that specific combination (migrates on start + no healthcheck) the gate
// watches a little longer: if the container EXITS during the window the restore
// failed and the normal rollback applies. The window is short on purpose — this
// catches the fast failure, which is the one that would otherwise pass silently;
// a migration still running at the end of it is reported honestly as still
// running rather than declared good.
//
// The right long-term fix is a healthcheck on the image, which makes the ordinary
// gate meaningful and skips this path entirely. The log says so.
const migrationSettleWindow = 45 * time.Second

// migrationSettlePoll is how often the container's state is re-read while
// settling. Cheap: one inspect per tick against one container.
const migrationSettlePoll = 3 * time.Second

// migrationSettle returns whether the container is still up after its startup
// migration has had a moment to run, and records the startup output — which
// contains the migration log — either way.
//
// Returns true unchanged for every container this does not apply to, so the
// ordinary restore path is byte-for-byte what it was.
func (e *Engine) migrationSettle(ctx context.Context, cli *client.Client, b *store.Backup, opts RestoreOptions) bool {
	p := ProfileFor(manifestImageOf(b))
	if p == nil || p.OneWayMigration == "" || !p.NoHealthcheck {
		return true
	}
	// An image that has since gained a healthcheck makes the ordinary gate
	// meaningful again — trust it rather than second-guessing from the profile.
	if insp, err := cli.ContainerInspect(ctx, opts.TargetID); err == nil &&
		insp.State != nil && insp.State.Health != nil {
		return true
	}

	window := migrationSettleWindow
	if ht := e.restoreHealthTimeout(opts.NodeID, b.TargetName); ht < window {
		window = ht // never wait longer than the operator allowed for health
	}
	e.logf(b.ID, "INFO", "%s runs its database migrations at startup and the image declares no healthcheck, so \"running\" alone is not proof the restore came up — watching for %s to confirm it stays up. (Adding a healthcheck to this service makes this check unnecessary.)", p.Name, window.Round(time.Second))

	deadline := time.Now().Add(window)
	exited := false
	for {
		if ctx.Err() != nil {
			break // canceled: gateRestoreHealth handles this case explicitly
		}
		insp, err := cli.ContainerInspect(ctx, opts.TargetID)
		if err == nil && insp.State != nil && !insp.State.Running {
			exited = true
			break
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(migrationSettlePoll):
		}
	}

	// The startup output IS the migration log. Record it whichever way this went:
	// on failure it is the diagnosis, and on success it is the evidence that the
	// migration ran and what it did.
	e.recordStartupLog(ctx, cli, b, opts.TargetID)

	if exited {
		e.logf(b.ID, "ERR", "%s stopped within %s of starting — its startup migration did not complete. The restore is NOT healthy; see the container output above.", b.TargetName, window.Round(time.Second))
		return false
	}
	e.logf(b.ID, "INFO", "%s is still running after %s — startup migration did not crash it. A long migration may still be in progress; the output above shows how far it got.", b.TargetName, window.Round(time.Second))
	return true
}

// recordStartupLog copies the container's own startup output into the run log.
//
// Best-effort: this is evidence-gathering, and failing to read a log must never
// become the reported outcome of a restore. It complements the F91 forensics
// path, which captures the same thing only when the gate FAILS — here it is
// captured on the success path too, because a migration's output is worth having
// on record even when nothing went wrong.
func (e *Engine) recordStartupLog(ctx context.Context, cli *client.Client, b *store.Backup, targetID string) {
	lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tail, err := dockercli.ContainerLogTail(lctx, cli, targetID, restoreLogTailLines)
	if err != nil || strings.TrimSpace(tail) == "" {
		return
	}
	e.logf(b.ID, "INFO", "Startup output from %s (includes the database migration):\n%s", b.TargetName, tail)
}

// shapeAddress writes the part of a supplied address that a given variable holds
// (F175).
//
// An application does not always record its address in one piece: Nextcloud
// keeps the host, the full URL and the scheme in three separate variables, and
// writing the same string into all three gives a host with a scheme in it and a
// protocol that is a URL. A list defaults to the host, because a set of accepted
// hosts is what every such variable holds.
func shapeAddress(bind AddressBinding, newAddr string) string {
	shape := bind.Shape
	if shape == EnvURL && bind.List {
		shape = EnvHost
	}
	switch shape {
	case EnvHost:
		return AddressHost(newAddr)
	case EnvOrigin:
		scheme := "https"
		if strings.HasPrefix(newAddr, "http://") {
			scheme = "http"
		}
		return scheme + "://" + AddressHost(newAddr)
	case EnvScheme:
		if strings.HasPrefix(newAddr, "http://") {
			return "http"
		}
		return "https"
	default:
		// F186: a URL, and it must carry a scheme.
		//
		// This used to write the address exactly as supplied, which is fine until
		// somebody types "docs.example.uk" into the address field — a perfectly
		// reasonable thing to type, and accepted by the validator, because for
		// most bindings a bare host is exactly right.
		//
		// It is not right here, and the cost is not a cosmetic one. Paperless
		// parses PAPERLESS_URL as a URL and appends it to CSRF_TRUSTED_ORIGINS;
		// Django then refuses to start at all — "the values in the
		// CSRF_TRUSTED_ORIGINS setting must start with a scheme". The restore
		// completes, the container never comes up, and the reason is three layers
		// down in the application's own boot log. BookStack's APP_URL, Mealie's
		// BASE_URL and Nextcloud's OVERWRITECLIURL are the same shape and the same
		// hazard.
		if !strings.Contains(newAddr, "://") {
			return "https://" + newAddr
		}
		return newAddr
	}
}

// noteUnwrittenOriginals says that this backup carries the genuine compose file
// and the stack's .env, and that this restore is not putting them on the host.
//
// Both halves of that are ordinary and neither is a failure — writing to a host
// filesystem is opt-in, and a restore that only brings the container back is a
// perfectly reasonable thing to ask for. What is not reasonable is silence: the
// operator captured those files deliberately, the backup advertises them, and
// arriving at an empty stack folder afterwards reads as data that was lost
// rather than a box that was not ticked.
func (e *Engine) noteUnwrittenOriginals(logID string, man *Manifest, reconstructHost bool) {
	if reconstructHost || man == nil {
		return
	}
	if !man.HasOriginalCompose && !strings.Contains(man.Format.Layout, originalComposeArchivePrefix) {
		return
	}
	e.logf(logID, "INFO", "This backup also holds the stack's genuine compose file and .env, and they were NOT written to this host — %q was not enabled for this restore. The container and its data are restored either way. To put them on disk, restore again with that option ticked, or browse this backup's files and take them from %s",
		"Reconstruct stack folder on host", originalComposeArchivePrefix)
}

// bindSourceAction is what a restore has decided to do about one bind source the
// target host does not have. Create is false when creating it would be a guess.
type bindSourceAction struct {
	Source      string // host path on THIS machine, after the path remap
	Destination string // container path it feeds
	Kind        string // dockercli.MountKindDir | MountKindFile, "" when unrecorded
	Owner       string // "uid:gid" recorded at capture
	Mode        string // octal bits recorded at capture
	Action      string // one of the BindAction* values
	Create      bool
	Note        string // what will happen, or why it will not
}

// What a restore will do about a bind source the target does not have. Named so
// the pre-restore dialog and the run log describe the same five outcomes in the
// same words — an operator who reads "create-empty" before confirming should see
// exactly that afterwards.
const (
	BindActionFill        = "fill"         // create the directory; the archive fills it
	BindActionCreateEmpty = "create-empty" // create it; this backup holds no contents
	BindActionWriteFile   = "write-file"   // the archive holds this file's bytes
	BindActionPlaceholder = "placeholder"  // create an empty file; contents must come from the source host
	BindActionManual      = "manual"       // DockBack will not create this one
)

// BindSourcePlan is one row of "what will happen to the paths this host does not
// have", as the pre-restore dialog shows it.
type BindSourcePlan struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Kind        string `json:"kind,omitempty"`
	Action      string `json:"action"`
	Note        string `json:"note"`
}

// RecordedBindSources returns every host bind this backup's container needs,
// with the restore's path remap already applied — the paths a target would have
// to have.
//
// Read from the manifest, never from the archive. The dialog that shows this
// re-runs on every keystroke in the remap fields, and opening an encrypted
// archive per service per keystroke would be slow and would also write a
// "Restoring from …" line into each backup's run log for something that is only
// a preview.
func RecordedBindSources(man *Manifest, fromPath, toPath string) []dockercli.BindMount {
	if man == nil {
		return nil
	}
	out := make([]dockercli.BindMount, 0, len(man.MountedBinds))
	for _, v := range man.MountedBinds {
		if v.Source == "" || v.Destination == "" {
			continue
		}
		out = append(out, dockercli.BindMount{
			Source:      dockercli.RemapHostPath(v.Source, fromPath, toPath),
			Destination: v.Destination,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}

// PlanBindSources is the exported view of the bucketing the materialiser uses,
// so the dialog and the restore cannot disagree about what is going to happen.
func PlanBindSources(man *Manifest, missing []dockercli.BindMount) []BindSourcePlan {
	actions := planBindSources(missing, bindRootsByDestination(man), capturedBindDestinations(man))
	out := make([]BindSourcePlan, 0, len(actions))
	for _, a := range actions {
		out = append(out, BindSourcePlan{
			Source: a.Source, Destination: a.Destination,
			Kind: a.Kind, Action: a.Action, Note: a.Note,
		})
	}
	return out
}

// Notes attached to each outcome. They are the whole operator-facing value of
// this feature: a path that appears out of nowhere is only reassuring if the log
// says what it is and whether anything is going to fill it.
const (
	bindNoteWillBeFilled      = "will be filled from the backup"
	bindNoteEmptyDir          = "created empty — this backup does not hold its contents"
	bindNoteUnknownKind       = "this backup does not record whether it is a file or a directory, and creating the wrong one is worse than creating nothing — create it yourself and restore again"
	bindNoteWrittenFromBackup = "written from the backup before the container is created"
)

// planBindSources decides what to do about every bind source the target is
// missing, from what the backup recorded about each one.
//
// Pure, and the reason this is a function rather than a loop inside the
// materialiser: the rules decide whether DockBack writes to a host filesystem,
// so every outcome is worth pinning in a test that needs no daemon.
//
// A captured directory is created bare, with no ownership applied. The volume
// archive carries the mount root as its own entry, so extracting it sets the
// owner and mode the source had — stamping them here first would be a guess that
// the untar immediately overwrites anyway.
func planBindSources(missing []dockercli.BindMount, roots map[string]VolumeRef, captured map[string]bool) []bindSourceAction {
	out := make([]bindSourceAction, 0, len(missing))
	for _, bind := range missing {
		root := roots[bind.Destination]
		action := bindSourceAction{
			Source: bind.Source, Destination: bind.Destination,
			Kind: root.Kind, Owner: root.Owner, Mode: root.Mode,
		}
		// One question, asked of the same validator the write will ask, so a path
		// this refuses is never a path something else quietly accepts.
		if err := dockercli.ValidHostTarget(bind.Source); err != nil {
			action.Action, action.Note = BindActionManual, err.Error()
			out = append(out, action)
			continue
		}
		switch {
		case root.Kind == dockercli.MountKindFile && root.Archive != "":
			// The archive holds this file's bytes, and restoreBindFiles writes them
			// before the container exists. Deliberately NOT created here: if the
			// path is still absent by now that write failed, and standing an empty
			// file in its place would hide a real problem behind a working-looking
			// container. The re-probe reports it instead.
			action.Action = BindActionWriteFile
			action.Note = bindNoteWrittenFromBackup
		case root.Kind == dockercli.MountKindFile:
			action.Create = true
			// No contents in the archive — an older backup, or a file over the
			// capture cap. An empty placeholder lets the container start with only
			// the feature that reads the file degraded, which beats a daemon-made
			// directory in a place the application will try to read as a file.
			action.Action = BindActionPlaceholder
			action.Note = "placeholder — copy the real file from the source host: " + placeholderSourceHint(root, bind)
		case root.Kind == dockercli.MountKindDir && captured[bind.Destination]:
			action.Create = true
			action.Owner, action.Mode = "", "" // the untar sets both
			action.Action, action.Note = BindActionFill, bindNoteWillBeFilled
		case root.Kind == dockercli.MountKindDir:
			action.Create = true
			action.Action, action.Note = BindActionCreateEmpty, bindNoteEmptyDir
		default:
			action.Action, action.Note = BindActionManual, bindNoteUnknownKind
		}
		out = append(out, action)
	}
	return out
}

// placeholderSourceHint names the path to go and fetch the file from. The
// manifest's own Source is the SOURCE machine's path, which is the one an
// operator needs; it falls back to the remapped path when a backup predates that
// record.
func placeholderSourceHint(root VolumeRef, bind dockercli.BindMount) string {
	if strings.TrimSpace(root.Source) != "" {
		return root.Source
	}
	return bind.Source
}

// materializeBindSources creates the bind mount sources this host does not have,
// so a restore onto a fresh machine does not dead-end on paths DockBack already
// knows everything about.
//
// It replaced a refusal. The refusal existed for a good reason — Docker's own
// handling of an absent bind source is to refuse the create naming only the
// FIRST one, or, for a container whose spec uses legacy Binds, to quietly invent
// an empty DIRECTORY at every missing path including where the container expects
// a FILE. But telling an operator to go and make seven directories by hand, when
// the backup records what each of them is, who owned it and what mode it carried,
// is answering the wrong question. So the paths it can describe, it now makes;
// the paths it cannot, it still refuses individually, and the rest are created
// regardless.
//
// The re-probe at the end is the verdict, not the creation step: a path that did
// not appear is reported as missing whatever the sidecar thought it did.
func (e *Engine) materializeBindSources(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, inspectBytes []byte) error {
	binds := dockercli.ContainerBindMounts(inspectBytes)
	if len(binds) == 0 {
		return nil
	}
	paths := make([]string, 0, len(binds))
	for _, bind := range binds {
		paths = append(paths, bind.Source)
	}
	kinds, err := dockercli.ProbeHostPaths(ctx, cli, paths)
	if err != nil {
		// A probe that could not run is not grounds to refuse a restore that may
		// well succeed. The daemon still gets its say.
		e.logf(b.ID, "WARN", "Could not check bind mount sources on this host (%v) — continuing to the create", err)
		return nil
	}
	var missing []dockercli.BindMount
	for _, bind := range binds {
		if kinds[bind.Source] == dockercli.HostPathMissing {
			missing = append(missing, bind)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	actions := planBindSources(missing, bindRootsByDestination(man), capturedBindDestinations(man))
	specs := make([]dockercli.HostPathSpec, 0, len(actions))
	for _, a := range actions {
		if a.Create {
			specs = append(specs, dockercli.HostPathSpec{Path: a.Source, Kind: a.Kind, Owner: a.Owner, Mode: a.Mode})
		}
	}
	e.logf(b.ID, "INFO", "%d of this container's %d bind mount sources do not exist on this host — creating the %d this backup describes", len(missing), len(binds), len(specs))
	if len(specs) > 0 {
		if cerr := dockercli.EnsureHostPaths(ctx, cli, specs); cerr != nil {
			e.logf(b.ID, "WARN", "Could not create the missing bind mount sources (%v) — checking what landed anyway", cerr)
		}
	}

	after, perr := dockercli.ProbeHostPaths(ctx, cli, paths)
	if perr != nil {
		e.logf(b.ID, "WARN", "Could not re-check the bind mount sources after creating them (%v) — continuing to the create", perr)
		return nil
	}
	return e.reportBindSources(b, actions, after)
}

// reportBindSources says what actually exists now, and fails the restore only
// for what still does not.
func (e *Engine) reportBindSources(b *store.Backup, actions []bindSourceAction, after map[string]dockercli.HostPathKind) error {
	var stillMissing []bindSourceAction
	for _, a := range actions {
		if after[a.Source] != dockercli.HostPathMissing {
			e.logf(b.ID, "INFO", "  created %s → %s (%s)", a.Source, a.Destination, a.Note)
			continue
		}
		stillMissing = append(stillMissing, a)
	}
	if len(stillMissing) == 0 {
		return nil
	}
	e.logf(b.ID, "ERROR", "%d bind mount source(s) still do not exist on this host, so the container was not created:", len(stillMissing))
	for _, a := range stillMissing {
		e.logf(b.ID, "ERROR", "  %s → %s: %s", a.Source, a.Destination, a.Note)
	}
	e.logf(b.ID, "ERROR", "Create these and restore again. If this host's layout differs from the source machine's, set the host path remap in the restore dialog instead of reproducing the source machine's paths here.")
	return fmt.Errorf("%d bind mount source(s) could not be created on this host, starting with %s — see the run log for the full list", len(stillMissing), stillMissing[0].Source)
}

// bindRootsByDestination indexes what the backup recorded about each bind's
// root, keyed by the container path — the one string that is the same on both
// machines, since the remap rewrites host sources by design.
//
// MountedBinds is the complete set, captured or not, and is therefore the
// authority. A backup taken before it existed still described the binds it
// captured, so those fill any gaps rather than being treated as unrecorded.
func bindRootsByDestination(man *Manifest) map[string]VolumeRef {
	out := map[string]VolumeRef{}
	if man == nil {
		return out
	}
	for _, v := range man.MountedBinds {
		if v.Destination != "" {
			out[v.Destination] = v
		}
	}
	for _, v := range man.Volumes {
		if v.Type != "bind" || v.Destination == "" {
			continue
		}
		root, known := out[v.Destination]
		if !known {
			out[v.Destination] = v
			continue
		}
		// The two records describe different halves and neither has both:
		// MountedBinds knows what the mount ROOT is, the captured ref knows where
		// its contents live in the archive. A file bind needs the second to be
		// told apart from one whose contents were never captured.
		if root.Archive == "" && v.Archive != "" {
			root.Archive = v.Archive
			out[v.Destination] = root
		}
	}
	return out
}

// capturedBindDestinations reports which of a container's bind mounts this
// backup actually holds data for, keyed by the CONTAINER-side path.
//
// Keyed by destination and not by source on purpose: the host path remap
// rewrites sources, so the source recorded in the manifest and the one the
// restore is about to use are different strings by design. The destination is
// the same on both machines.
func capturedBindDestinations(man *Manifest) map[string]bool {
	captured := map[string]bool{}
	if man == nil {
		return captured
	}
	for _, v := range man.Volumes {
		if v.Type == "bind" && v.Destination != "" {
			captured[v.Destination] = true
		}
	}
	return captured
}

// applyRunAsIDs points the ways a container declares which user it runs as at
// the ids pinned for it (F189, #24).
//
// Two of the three UID models are writable from here and both are tried, because
// a container can use either and R4 found one stack using both at once: the
// environment pair an image declares (PUID/PGID, USERMAP_UID/USERMAP_GID), and a
// numeric `user:` override from the stack file. The third — a user baked into
// the image — is not writable and does not need to be: that uid is the same
// number on every host, and aligning the files is the whole job.
//
// Only ever on an explicit pin, and only ever what the container ALREADY
// declares. Adding PUID to an image that never heard of it changes nothing and
// reads, to whoever finds it later, like a setting that should be doing
// something; adding a `user:` where there was none overrides the image's own
// user, which was already correct everywhere.
func (e *Engine) applyRunAsIDs(b *store.Backup, man *Manifest, inspectBytes []byte, opts RestoreOptions) []byte {
	uid, gid, pinned := e.RestoreOwnership(opts.NodeID, b.TargetName)
	if !pinned {
		// #31: reproduce it, and say so when the machine has changed underneath it.
		// Both writable UID models get their own line — the `user:` override and
		// the environment pair — because a container can use either and only one
		// of them was ever reported.
		e.reportCarriedComposeUser(b, man, inspectBytes, opts)
		e.reportCarriedRunAsEnv(b, man, inspectBytes, opts)
		return inspectBytes
	}
	// #24 first, so the message below can tell the truth about what happened: a
	// container with a `user:` override and no environment pair — redis, tika and
	// gotenberg all are — used to fall through to "nothing was changed".
	inspectBytes, userChanged := e.applyRunAsUser(b, inspectBytes, uid, gid)
	inspectBytes, envChanged := e.applyRunAsEnvPair(b, inspectBytes, uid, gid)
	if !userChanged && !envChanged {
		e.logf(b.ID, "INFO", "Restored data will be owned by %d:%d as set for this container. It declares no user-mapping variable and no `user:` override, so nothing in the container was changed — it keeps the user built into the image.", uid, gid)
	}
	return inspectBytes
}

// applyRunAsEnvPair rewrites the environment pair an image declares to say which
// user it drops to (F189).
func (e *Engine) applyRunAsEnvPair(b *store.Backup, inspectBytes []byte, uid, gid int) ([]byte, bool) {
	uidKey, gidKey, ok := dockercli.RunAsEnvPair(dockercli.ContainerEnv(inspectBytes))
	if !ok {
		return inspectBytes, false
	}
	changedAny := false
	for _, kv := range []struct {
		key string
		val int
	}{{uidKey, uid}, {gidKey, gid}} {
		out, changed, err := dockercli.SetContainerEnv(inspectBytes, kv.key, strconv.Itoa(kv.val))
		if err != nil {
			e.logf(b.ID, "WARN", "Could not set %s on the recreated container (%v) — set it by hand, or the application will run as the machine this backup came from", kv.key, err)
			continue
		}
		if changed {
			inspectBytes = out
			changedAny = true
		}
	}
	if changedAny {
		e.logf(b.ID, "INFO", "Set %s/%s to %d:%d on the recreated container, matching the ownership set for it — the application and its restored files agree on this host", uidKey, gidKey, uid, gid)
	}
	return inspectBytes, changedAny
}

// applyEnvAddressBindings rewrites the environment variables an app uses to
// record its own address, in the inspect JSON the recreate is about to be built
// from (F114).
//
// Two shapes, and the difference matters. A SINGLE-valued key (BookStack's
// APP_URL) is set to the new address. A LIST key (Homepage's
// HOMEPAGE_ALLOWED_HOSTS) has the new address ADDED to the set — overwriting the
// set would lock the app out at every address it currently answers on, turning a
// fix for one address into an outage at all the others.
//
// Never fails the restore: an env rewrite that could not be applied leaves a
// recoverable situation the operator is told about, whereas refusing the restore
// leaves them with nothing.
func (e *Engine) applyEnvAddressBindings(b *store.Backup, man *Manifest, inspectBytes []byte, newAddr string) []byte {
	p := ProfileFor(manifestImage(man, b))
	if p == nil {
		return inspectBytes
	}
	for _, bind := range p.Address {
		if bind.Kind != BindEnv {
			continue
		}
		for _, key := range bind.Keys {
			current := dockercli.ContainerEnvValue(inspectBytes, key)
			// F175: only touch a variable the container actually sets. Adding one
			// it never had changes behaviour beyond the address — an application
			// that reads OVERWRITEHOST only when present would start being told
			// where it lives by a backup tool.
			if current == "" {
				continue
			}
			want := shapeAddress(bind, newAddr)
			if bind.List {
				// A trust list holds HOSTS, not URLs — an entry carrying a scheme
				// silently never matches, which presents as "the fix did nothing".
				added, changed := dockercli.AddToEnvListSep(current, want, bind.ListSep)
				if !changed {
					e.logf(b.ID, "INFO", "%s already accepts %s — %s left unchanged", p.Name, want, key)
					continue
				}
				want = added
			} else if current == want {
				continue
			}
			out, changed, err := dockercli.SetContainerEnv(inspectBytes, key, want)
			if err != nil {
				e.logf(b.ID, "WARN", "Could not set %s on the recreated container (%v) — set it by hand, or %s", key, err, p.Name+" will not answer at the new address")
				continue
			}
			if changed {
				inspectBytes = out
				if bind.List {
					e.logf(b.ID, "INFO", "Added %s to %s on the recreated container (existing entries kept, so it still answers at its current addresses)", AddressHost(newAddr), key)
				} else {
					e.logf(b.ID, "INFO", "Set %s to %s on the recreated container", key, want)
				}
			}
		}
	}
	return inspectBytes
}

// appAddressFollowUp performs, and prints, the address work that could only
// happen once the application is up (F114).
//
// Split from the env half above because the ordering is forced: the app's own
// CLI cannot be invoked until the app is running and healthy, and there is no
// point printing a content-rewrite command for a restore that failed.
func (e *Engine) appAddressFollowUp(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions) {
	p := ProfileFor(manifestImage(man, b))
	if p == nil || len(p.Address) == 0 {
		return
	}
	ictx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	insp, err := cli.ContainerInspect(ictx, opts.TargetID)
	if err != nil || insp.Config == nil {
		return
	}
	name := strings.TrimPrefix(insp.Name, "/")
	// Read ONLY the declared address key. A container environment routinely
	// holds passwords and API tokens, and the run log is a surface an operator
	// shares when asking for help — so this narrows to the one value it needs
	// rather than scanning the environment.
	current := envMap(insp.Config.Env)[p.currentAddressEnvKey()]

	for _, bind := range p.Address {
		switch bind.Kind {
		case BindAppCommand:
			if opts.NewSiteAddress == "" {
				// F175: nothing was requested, so nothing is changed — but if the
				// application has just landed on a DIFFERENT machine, the address
				// it still records is almost certainly the previous one, and
				// saying nothing is how a green restore becomes a site that
				// redirects to a host that has moved. Report it; change nothing.
				if crossHostRestore(b, man, opts) {
					e.reportRecordedAddress(ctx, cli, b, p, bind, opts)
				}
				continue
			}
			e.applyAddressBinding(ctx, cli, b, p, bind, opts, crossHostRestore(b, man, opts))
		case BindContentRewrite:
			steps := p.AddressCommandSteps(name, current, opts.NewSiteAddress)
			if len(steps) == 0 {
				continue
			}
			lead := "If you are moving " + p.Name + " to a DIFFERENT address, run these — otherwise do nothing:"
			if opts.NewSiteAddress != "" {
				lead = p.Name + " also stores absolute URLs inside its content, which DockBack will NOT rewrite for you. Run these to finish the address change:"
			}
			e.logf(b.ID, "INFO", "%s\n%s\n%s", lead, strings.Join(steps, "\n"), bind.Note)
		case BindManual:
			if opts.NewSiteAddress == "" {
				continue
			}
			e.logf(b.ID, "INFO", "%s: one step left, which has no command — %s Otherwise %s.", p.Name, bind.Note, bind.Symptom)
		}
	}
}

// crossHostRestore reports whether this restore landed the container on a
// different node than the backup came from (F175).
//
// Conservative in the one direction that matters: an unknown source or target
// node returns false, so an ambiguous case produces silence rather than a
// warning about a move that may not have happened.
func crossHostRestore(b *store.Backup, man *Manifest, opts RestoreOptions) bool {
	src := ""
	if man != nil {
		src = man.NodeID
	}
	if src == "" && b != nil {
		src = b.NodeID
	}
	return src != "" && opts.NodeID != "" && src != opts.NodeID
}

// reportRecordedAddress states the address an application still carries after a
// move, for a binding whose procedure DockBack knows how to read (F175).
//
// Read-only by construction: it dispatches on the same named procedure as
// applyAddressBinding, but only to the reporting half. An application with no
// reader falls back to the profile's own description of the symptom, which is
// still better than silence.
func (e *Engine) reportRecordedAddress(ctx context.Context, cli *client.Client, b *store.Backup, p *AppProfile, bind AddressBinding, opts RestoreOptions) {
	switch bind.Apply {
	case applyNextcloudTrustedDomain:
		e.reportNextcloudRecordedAddress(ctx, cli, b, opts)
	default:
		e.logf(b.ID, "WARN", "%s was restored onto a different machine and no new address was given, so it still records the previous one. %s",
			p.Name, bind.Symptom)
	}
}

// reportCarriedAddresses names the environment variables that still record the
// machine this container came FROM, after a restore onto a different one (F175).
//
// Read-only, and it never fails a restore: an address that needs changing is a
// line in a compose file, and the data is already correct. It exists because the
// alternative is silence — the operator sees a green restore, a redirect to a
// host that has moved, and nothing anywhere connecting the two.
//
// Only on a cross-host restore. On a same-host restore the recorded address is
// almost always still right, and a warning printed every time is a warning
// nobody reads by the time it matters.
func (e *Engine) reportCarriedAddresses(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions) {
	if !crossHostRestore(b, man, opts) {
		return
	}
	ictx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	insp, err := cli.ContainerInspect(ictx, opts.TargetID)
	if err != nil || insp.Config == nil {
		return
	}
	// #22 / PLAYBOOK §9.3: proxy trust is a THIRD role, with a different reason
	// and a different consequence from an address that merely moved — so it is
	// stated separately, and before the early return below, which is about
	// addresses only.
	e.reportProxyTrustEnv(b, insp.Config.Env)

	found := addressLikeEnv(insp.Config.Env, declaredAddressKeys(ProfileFor(manifestImage(man, b))))
	if len(found) == 0 {
		return
	}
	lead := "This container was restored onto a different machine and its environment still records where it used to live"
	if opts.NewSiteAddress != "" {
		lead = "The new address was applied to the settings DockBack knows this application by. These other variables record an address too and were NOT changed, because rewriting a variable whose meaning is a guess is how a good restore becomes a broken app"
	}
	e.logf(b.ID, "WARN", "%s. Check %s:\n  %s\nThe data is correct — anything wrong here is a line in the compose file, not a problem with the backup.",
		lead, plural(len(found), "this variable", "these variables"), strings.Join(found, "\n  "))
}

// applyAddressBinding runs the app's own supported tool for an address setting
// it owns (F114). Dispatches on the binding's named procedure.
func (e *Engine) applyAddressBinding(ctx context.Context, cli *client.Client, b *store.Backup, p *AppProfile, bind AddressBinding, opts RestoreOptions, crossHost bool) {
	switch bind.Apply {
	case applyNextcloudTrustedDomain:
		e.applyNextcloudAddress(ctx, cli, b, opts, crossHost)
	default:
		e.logf(b.ID, "WARN", "%s declares an address setting DockBack has no procedure for — set it by hand", p.Name)
	}
}

// manifestImage prefers the already-parsed manifest and falls back to the
// catalog row, so a caller that has one doesn't re-parse JSON it already holds.
func manifestImage(man *Manifest, b *store.Backup) string {
	if man != nil && man.Image != "" {
		return man.Image
	}
	return manifestImageOf(b)
}

// manifestImageOf reads the image a backup recorded, for the profile lookup.
// Empty when the row carries no readable manifest, which simply means no profile
// applies.
func manifestImageOf(b *store.Backup) string {
	if b == nil || b.ManifestJSON == "" {
		return ""
	}
	var m struct {
		Image string `json:"image"`
	}
	if json.Unmarshal([]byte(b.ManifestJSON), &m) != nil {
		return ""
	}
	return m.Image
}

// assertDockerSocketNotWidened proves the recreated container was not given more
// Docker authority than the backup recorded (F128).
//
// The recreate replays the captured configuration verbatim and a clone strips
// every bind, so this cannot currently fail — which is exactly why it is worth
// asserting. "Correct because of how the code happens to be written" is not a
// security property; "checked, every time, and it says so in the log" is. A
// future change to the recreate path that widened this would otherwise be
// invisible.
//
// Reports only. It never alters a mount — a check that could itself change
// authority would defeat its own purpose.
func (e *Engine) assertDockerSocketNotWidened(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions) {
	recorded := ""
	if man != nil && man.Requires != nil {
		recorded = man.Requires.DockerSocket
	}
	ictx, cancel := context.WithTimeout(ctx, 15*time.Second)
	insp, err := cli.ContainerInspect(ictx, opts.TargetID)
	cancel()
	if err != nil {
		return // unreadable: claim nothing rather than guess
	}
	now := dockerSocketMode(insp)
	if DockerSocketWidened(recorded, now) {
		was := recorded
		if was == "" {
			was = "no Docker socket at all"
		}
		e.logf(b.ID, "ERR", "SECURITY: the restored %s has %s access to the Docker socket but the backup recorded %s. The restore must never widen this — review the container's configuration before relying on it.", b.TargetName, now, was)
		return
	}
	if recorded != "" {
		e.logf(b.ID, "INFO", "Docker socket reproduced as recorded (%s) — not widened", recorded)
	}
}

// embeddedDumpFor returns the embedded-database description for a backup's app,
// or nil when the image bundles no database server of its own (F126).
func embeddedDumpFor(man *Manifest) *EmbeddedDump {
	if man == nil {
		return nil
	}
	p := ProfileFor(man.Image)
	if p == nil {
		return nil
	}
	return p.EmbeddedDump
}

// restoreEmbeddedDBApp restores an application that bundles its own database
// server (F126) — Guacamole's all-in-one image, and anything shaped like it.
//
// The ordering is the whole feature, and it is the opposite of a database
// container's. There, the engine is stopped, its data directory wiped, and the
// dump imported into a freshly-initialised server. Doing that here would wipe an
// application's config directory, because for these images the database lives
// INSIDE it.
//
// So: restore the files first (which also starts the container), let the
// application initialise its own database as it normally would, wait for that
// database to accept connections, and only then import — over the top, into the
// running server. The dump is recorded as a per-database subset precisely so it
// is self-cleaning and can do that.
func (e *Engine) restoreEmbeddedDBApp(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions) error {
	eng := man.Databases[0].Engine
	d := embeddedDumpFor(man)
	e.logf(b.ID, "INFO", "%s runs its own %s server — restoring its files first, then importing the database into it", b.TargetName, eng)

	// 0) Clear any cluster already sitting in the bundled server's data directory.
	//
	// That directory is deliberately NOT in the archive — the dump supersedes it
	// (Options.embeddedDataDir) — so it is the one thing a file restore leaves
	// untouched, and a STALE one is worse than none at all: the application's
	// entrypoint finds it, skips its initdb, and the app then authenticates
	// against a cluster initialised with a DIFFERENT host's password. That is
	// precisely the cross-host failure the dump exists to end, and leaving the
	// directory in place reintroduces it at the last step.
	//
	// Wiping first is what the standalone database restore and the Redis restore
	// already do. This path was the one that did not, so the operator had to
	// delete the directory by hand before every cross-host restore.
	if d != nil && strings.TrimSpace(d.DataDir) != "" {
		e.logf(b.ID, "INFO", "Clearing the bundled %s data directory %s so it re-initialises fresh for the dump", eng, d.DataDir)
		if serr := stopBeforeOverwrite(ctx, cli, opts.TargetID, b.TargetName); serr != nil {
			return serr
		}
		if werr := dockercli.WipeDir(ctx, cli, opts.TargetID, d.DataDir); werr != nil {
			return fmt.Errorf("clearing the bundled %s data directory so it could re-initialise: %w", eng, werr)
		}
	}

	// 1) Files and configuration. This also starts the container, which is what
	//    brings the embedded server up — on the now-empty data directory, so the
	//    entrypoint initialises a cluster the dump can import into cleanly.
	if err := e.restoreFiles(ctx, cli, b, man, opts); err != nil {
		return err
	}

	// 2) The server needs to finish initialising before it will accept an import.
	//
	// F166: for an application that can run more than one way, check FIRST that
	// this deployment is running the bundled server at all. Without it the wait
	// below simply times out, and the operator is left reading "the database did
	// not come up" when the truth is that this deployment does not have one.
	if d != nil && len(d.When) > 0 {
		if _, perr := dockercli.ExecHook(ctx, cli, opts.TargetID, d.When, "", ""); perr != nil {
			return fmt.Errorf("this backup holds a dump of %s's bundled %s database, but the container being restored into is not configured to run one — "+
				"restoring the data would leave the application running on an empty database of a different kind. "+
				"Recreate it from this backup's own configuration (which sets it up the way it was), or restore into a deployment configured the same way",
				b.TargetName, eng)
		}
	}
	e.logf(b.ID, "INFO", "Waiting for the embedded %s to accept connections…", eng)
	if err := dockercli.WaitForDB(ctx, cli, opts.TargetID, eng); err != nil {
		return fmt.Errorf("the embedded %s did not come up, so its data could not be imported: %w", eng, err)
	}

	// #39/#32: the cluster has just re-initialised itself, so this is the one
	// moment its identity can be set to the source's. After the import it is too
	// late for the settings that are stamped into the volume at first boot.
	if eng == "postgres" {
		e.replayPGClusterConfig(ctx, cli, b, man, opts, d)
	}

	// 3) Import. Postgres needs its own sessions cleared first, or the dump's
	//    DROP DATABASE cannot proceed — the app has already connected to itself.
	if eng == "postgres" {
		if subset := cleanDBNames(man.Databases[0].Databases); len(subset) > 0 {
			_, _ = dockercli.ExecCapture(ctx, cli, opts.TargetID, pgTerminateConnectionsCmd(subset, d))
		}
	}
	e.logf(b.ID, "INFO", "Importing the %s dump into the running application…", eng)
	imported := false
	err := e.streamArchive(ctx, b, opts.Source, func(tr *tar.Reader, hdr *tar.Header) (bool, error) {
		if !strings.HasPrefix(hdr.Name, "db/") {
			return true, nil
		}
		if ierr := e.importDatabase(ctx, cli, b.ID, opts.TargetID, dbEngineForEntry(man, hdr.Name), tr, dbDumpForEntry(man, hdr.Name), d); ierr != nil {
			return false, fmt.Errorf("import %s: %w", hdr.Name, ierr)
		}
		imported = true
		return true, nil
	})
	if err != nil {
		return err
	}
	if !imported {
		// The manifest promised a dump and the archive has none. Loud, because the
		// application is now running on whatever empty database it just created
		// for itself, which looks completely healthy.
		return fmt.Errorf("this backup records a %s dump but the archive contains none — the application is running on an EMPTY database; do not rely on this restore", eng)
	}

	// F168: hold the import to the ROWS, not only to the schema. The generic
	// contract for this engine counts tables, which catches a truncated dump and
	// says nothing about an import that created every table and filled none.
	if verr := e.verifyAppTableCounts(ctx, cli, opts.TargetID, d, &man.Databases[0], b.ID); verr != nil {
		return verr
	}
	// #18/#26: and hold it to the ROWS THEMSELVES. Before the restart below,
	// because once the application reconnects every difference could be its own
	// writing — this is the only clean comparison point there is.
	volatile, classified := e.stackVolatileTables(ctx, cli, manifestImage(man, b), b.Stack)
	if !e.verifyTableHashes(ctx, cli, b, opts.TargetID, eng, d, man.Databases[0].TableHashes, volatile, classified) {
		return fmt.Errorf("the restored database does not match this backup's content baseline — see the run log for the tables that differ")
	}

	// The application connected to its database before the import replaced it, so
	// its pooled connections and any cached schema are stale. A restart is the
	// supported way back to a consistent view.
	e.logf(b.ID, "INFO", "Restarting %s so it picks up the imported database", b.TargetName)
	rctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if rerr := cli.ContainerRestart(rctx, opts.TargetID, container.StopOptions{}); rerr != nil {
		e.logf(b.ID, "WARN", "Could not restart %s after the import (%v) — restart it by hand if the application shows stale or missing data", b.TargetName, rerr)
	}
	return nil
}

// shortHash trims a digest for a log line — enough to compare by eye, not enough
// to fill the screen.
func shortHash(h string) string {
	if len(h) > 16 {
		return h[:16]
	}
	return h
}

// maxReportedShortTables bounds how many tables a failure message names — enough
// to diagnose, not a wall of text when a restore went wholesale wrong.
const maxReportedShortTables = 6

// shortTables compares the per-table counts recorded at capture against what came
// back, and describes every table that lost rows (F124).
//
// Asymmetric for the same reason the aggregate check is: MORE rows is legitimate
// (a live container can gain them between the overlay and the read-back), FEWER
// means data is missing. A table absent from the read-back is skipped rather
// than reported as zero — that is "not measured", and the caller must never turn
// an unmeasured thing into a failure.
//
// Returns nil when either side has no per-table data, so a pre-F124 backup or an
// unscannable database falls through to the aggregate check unchanged.
func shortTables(captured, restored map[string]int64) []string {
	if len(captured) == 0 || len(restored) == 0 {
		return nil
	}
	names := make([]string, 0, len(captured))
	for name := range captured {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic message, run to run

	var short []string
	hidden := 0
	for _, name := range names {
		want := captured[name]
		got, measured := restored[name]
		if want <= 0 || !measured || got >= want {
			continue
		}
		if len(short) >= maxReportedShortTables {
			hidden++
			continue
		}
		short = append(short, fmt.Sprintf("%s has %d of %d row(s)", name, got, want))
	}
	if hidden > 0 {
		short = append(short, fmt.Sprintf("and %d more table(s)", hidden))
	}
	return short
}

// alignRestoredOwnership re-owns the restored data to the ids the target
// container will actually run its application as (F117).
//
// The numeric restore preserves the SOURCE machine's uid:gid, which is right
// when the target runs the same ids and wrong when it does not. A Synology host
// whose user is 1026 restoring files owned 1000 gets an application that starts
// perfectly and then cannot write its own database — SQLite calls that "attempt
// to write a readonly database" or "database is locked", which reads like
// corruption and is nothing of the kind. It is the commonest broken restore
// there is, and it is entirely mechanical to prevent.
//
// Runs BEFORE the container starts, so the app never sees the wrong ownership.
// Never fails the restore: the data is present and correct either way, and an
// ownership problem is one command to fix — whereas refusing here would leave
// the operator with nothing. Every failure path says exactly what to run.
// serviceAccountUIDCeiling separates a service account baked into an image from
// a user account belonging to a host.
//
// F117 exists to reconcile ids that differ because two HOSTS number their users
// differently, and every mainstream distribution starts host accounts at 1000
// (Debian, Ubuntu and RHEL all set UID_MIN=1000; Synology DSM allocates from
// 1024). An id below that is not a host account — it is postgres, redis, or
// whoever else the image ships, and those ids are identical on every machine, so
// they never need realigning. Root is excluded: root-owned application data is
// the ordinary case F117 is for.
const serviceAccountUIDCeiling = 1000

// serviceAccountOwner reports whether "uid:gid" names an account that belongs to
// the image rather than to a host. Unparseable input is not one — an owner this
// cannot read is left to the caller's other rules rather than being excluded on
// a guess.
func serviceAccountOwner(owner string) bool {
	uid, _, found := strings.Cut(strings.TrimSpace(owner), ":")
	if !found {
		return false
	}
	n, err := strconv.Atoi(uid)
	if err != nil {
		return false
	}
	return n > 0 && n < serviceAccountUIDCeiling
}

// alignablePaths splits the restored destinations into the ones F117 may re-own
// and the ones it must not touch.
//
// The distinction did not exist before, and its absence was a loaded gun. F117
// re-owns every restored path to the ids the target's application runs as, which
// is right for an ordinary container and wrong for one that bundles its own
// services: a PostgreSQL data directory owned by uid 101 mode 700 that is
// chowned to the app's uid does not start, and reports it as a permissions error
// on a directory the operator never touched.
//
// Three rules, in the order they can be trusted:
//
//  1. The image's own declared bundled-database directory is never the
//     application's, whatever else is known. This is the only rule that protects
//     a backup taken before ownership was recorded at all.
//  2. When the backup recorded what the application ran as, a path owned by
//     anyone else was not its data. Exact, and the common case for the images
//     that declare PUID/PGID.
//  3. Otherwise, an owner below the host-account range belongs to the image.
//     Reached only when an image declares no such pair — which is precisely the
//     all-in-one case where bundled service accounts are the hazard.
//
// A path with no recorded owner and no other signal is aligned, exactly as
// before, so an older archive behaves as it always did.
func alignablePaths(volumes []VolumeRef, runAs *RunAs, embedded *EmbeddedDump) (align, leave []string) {
	embeddedDir := ""
	if embedded != nil {
		embeddedDir = strings.TrimSpace(embedded.DataDir)
	}
	appOwner := ""
	if runAs != nil {
		appOwner = fmt.Sprintf("%d:%d", runAs.UID, runAs.GID)
	}
	for _, v := range volumes {
		if v.Destination == "" {
			continue
		}
		owner := strings.TrimSpace(v.Owner)
		switch {
		case embeddedDir != "" && v.Destination == embeddedDir:
			leave = append(leave, v.Destination)
		case owner == "":
			align = append(align, v.Destination)
		case appOwner != "" && owner != appOwner:
			leave = append(leave, v.Destination)
		case appOwner == "" && serviceAccountOwner(owner):
			leave = append(leave, v.Destination)
		default:
			align = append(align, v.Destination)
		}
	}
	return align, leave
}

func (e *Engine) alignRestoredOwnership(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions) ownershipIntent {
	if man == nil || len(man.Volumes) == 0 {
		return ownershipIntent{}
	}
	// What will the TARGET run as? Read from the container that now exists —
	// after a recreate that is the recorded env, but on a cross-machine move the
	// operator has often edited it, and that edit is the whole point.
	ictx, cancel := context.WithTimeout(ctx, 15*time.Second)
	insp, err := cli.ContainerInspect(ictx, opts.TargetID)
	cancel()
	if err != nil || insp.Config == nil {
		return ownershipIntent{}
	}
	targetUID, targetGID, key, ok := dockercli.RunAsIDs(insp.Config.Env)
	// F184: an operator's explicit pin beats detection, and is the only thing
	// that applies at all when the image declares nothing. An image running as a
	// baked-in user announces no ids, so there is nothing to detect — and that is
	// precisely the case where data from a NAS that numbered its users
	// differently needs saying outright.
	pinned := false
	if u, g, set := e.RestoreOwnership(opts.NodeID, b.TargetName); set {
		targetUID, targetGID, ok, pinned = u, g, true, true
	}
	if !ok {
		return ownershipIntent{} // the image declares no such ids; there is nothing to align to
	}
	// Nothing recorded from the source (a pre-F117 backup) means we cannot tell
	// whether the restored files already match. Aligning anyway is the safe
	// choice: the target's own declared ids are, by definition, the ids its app
	// needs to own its data.
	if man.RunAsIDs != nil && man.RunAsIDs.UID == targetUID && man.RunAsIDs.GID == targetGID {
		return ownershipIntent{} // already owned by the right ids — the common case, silent
	}

	paths, leave := alignablePaths(man.Volumes, man.RunAsIDs, embeddedDumpFor(man))
	paths = e.holdDuplicateTargetConfigs(ctx, cli, b, man, opts.TargetID, paths)
	// Said before the alignment, because it is the half an operator will not
	// otherwise notice: a path deliberately NOT touched looks identical to one
	// nobody thought about.
	if len(leave) > 0 {
		e.logf(b.ID, "INFO", "Leaving %d restored path(s) owned by other service accounts untouched: %s. Those ids belong to services inside the image, not to %s — re-owning them is how a bundled database stops starting.",
			len(leave), strings.Join(leave, ", "), b.TargetName)
	}
	if len(paths) == 0 {
		return ownershipIntent{}
	}
	from := "the backup's ownership"
	if man.RunAsIDs != nil {
		from = fmt.Sprintf("%d:%d", man.RunAsIDs.UID, man.RunAsIDs.GID)
	}
	// Where the ids came from, said plainly — an operator reading this later
	// needs to know whether DockBack read them or was told them.
	source := fmt.Sprintf("runs as %s=%d:%d", key, targetUID, targetGID)
	if pinned {
		source = fmt.Sprintf("is set to restore its data owned by %d:%d", targetUID, targetGID)
	}
	e.logf(b.ID, "INFO", "%s %s, but the restored data is owned by %s — aligning ownership of %d restored path(s) so the app can write its own files",
		b.TargetName, source, from, len(paths))

	cctx, ccancel := context.WithTimeout(ctx, 15*time.Minute)
	defer ccancel()
	n, cerr := dockercli.ChownVolumePaths(cctx, cli, opts.TargetID, paths, targetUID, targetGID)
	// #15: the intent is returned either way. On success it tells the sweep below
	// that these paths were MEANT to change, so it stays quiet about them; on
	// failure it is what lets the sweep name the paths this chown left behind,
	// which the message above cannot.
	intent := ownershipIntent{UID: targetUID, GID: targetGID, Paths: paths, Declared: true}
	if cerr != nil {
		e.logf(b.ID, "WARN", "Could not change ownership of the restored data (%v) — the data is restored, but %s may not be able to write to it. Fix it on the host with: chown -R %d:%d <the restored directories>",
			cerr, b.TargetName, targetUID, targetGID)
		return intent
	}
	e.logf(b.ID, "INFO", "Ownership of %d restored path(s) set to %d:%d — the application can write its own data", n, targetUID, targetGID)
	return intent
}

// ErrRestoreUnhealthy marks the health-gate outcome where the service WAS fully
// restored (recreated + data) but hasn't come up healthy — distinguishable so a
// STACK restore can keep going: on a fresh host a service frequently turns
// healthy only after its dependencies are restored moments later.
var ErrRestoreUnhealthy = errors.New("restore did not become healthy")

// ErrRestoreCanceled marks a restore stopped by the operator (or by the run's
// deadline). It is NOT a failure of the backup: callers report it as "canceled"
// rather than "failed", and a stack restore stops instead of moving on to the
// next service.
var ErrRestoreCanceled = errors.New("restore canceled")

// stackEnvFromArchive returns the .env captured from the source host (F231),
// already put through the remaps, or nil when the backup carries none.
//
// Logged in terms of what it CHANGED, because "nothing to change" and "nothing
// to change it in" look identical from the outside and mean opposite things.
func (e *Engine) stackEnvFromArchive(ctx context.Context, b *store.Backup, source string, logID string,
	fromIP, toIP, fromDomain, toDomain, fromPath, toPath string) []byte {
	raw, err := e.extractEntry(ctx, b, source, "config/original-compose/"+stackEnvArchiveName)
	if err != nil || len(raw) == 0 {
		return nil
	}
	out, ips, domains, paths := RemapEnvFile(raw, fromIP, toIP, fromDomain, toDomain, fromPath, toPath)
	e.logf(logID, "INFO", "Restoring the stack's own .env captured from the source host (%d bytes)", len(raw))
	if ips > 0 {
		e.logf(logID, "INFO", "Remapped host IP %s → %s in %d place(s) of the .env", fromIP, toIP, ips)
	}
	if domains > 0 {
		e.logf(logID, "INFO", "Remapped domain %s → %s in %d place(s) of the .env", fromDomain, toDomain, domains)
	}
	if paths > 0 {
		e.logf(logID, "INFO", "Remapped host path %s → %s in %d place(s) of the .env", fromPath, toPath, paths)
	}
	return out
}

// originalsForHost prepares the captured host compose file(s) for writing beside
// the reconstruction (F57): read from the archive, put through the SAME three
// remaps the reconstruction and the .env get, and reported per file.
//
// Shared by the single-container and stack paths so the two can never disagree
// about what a restored original contains.
func (e *Engine) originalsForHost(ctx context.Context, b *store.Backup, source, logID string,
	fromIP, toIP, fromDomain, toDomain, fromPath, toPath string) []dockercli.NamedFile {
	found := e.originalComposeFromArchive(ctx, b, source)
	if len(found) == 0 {
		return nil
	}
	names := make([]string, 0, len(found))
	for n := range found {
		names = append(names, n)
	}
	sort.Strings(names) // stable order, so the log reads the same way twice
	out := make([]dockercli.NamedFile, 0, len(names))
	for _, n := range names {
		body, ips, domains, paths := RemapEnvFile(found[n], fromIP, toIP, fromDomain, toDomain, fromPath, toPath)
		if ips+domains+paths > 0 {
			e.logf(logID, "INFO", "Remapped the captured %s: %d host IP, %d domain, %d host path substitution(s)", n, ips, domains, paths)
		}
		out = append(out, dockercli.NamedFile{Name: n, Content: body})
	}
	return out
}

// logRestoredOriginals says what was put on the host and, plainly, why it is not
// the file `docker compose` will run (F57).
//
// The distinction is the whole point: the reconstruction is guaranteed to match
// the containers that were just restored, and the original — hand-tuned,
// commented, with the env_file and profiles a reconstruction cannot reproduce —
// is the one worth reading before adopting.
func (e *Engine) logRestoredOriginals(logID string, originals []string) {
	if len(originals) == 0 {
		return
	}
	e.logf(logID, "INFO", "Also restored the genuine compose file(s) from the source host as %s. "+
		"They are NOT what `docker compose` runs here: they may reference env_file targets, relative paths or profiles that do not exist on this machine. "+
		"The canonical file beside them matches what is actually running — read these before adopting them.",
		strings.Join(originals, ", "))
}

// reconstructHostStack rebuilds the on-host compose project directory and writes
// the reconstructed compose file into it after a DR recreate (opt-in via
// RestoreOptions.ReconstructHost). It resolves the target directory from the
// backup's recorded compose working_dir — falling back to the container's own
// compose labels in inspect.json for older backups, or, for a NON-compose
// container, to HostBaseDir + the container name. Everything here is best-effort
// and logged: the container has already been recreated, so failing to lay down
// the folder never fails the restore.
func (e *Engine) reconstructHostStack(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions, inspectBytes []byte) {
	workingDir, composeFile := man.StackWorkingDir, man.ComposeFile
	name := man.TargetName
	if name == "" {
		name = b.TargetName
	}
	// Older backups predate the manifest fields — recover the layout from the
	// container's own compose labels in inspect.json.
	if workingDir == "" || composeFile == "" {
		var probe struct {
			Name   string `json:"Name"`
			Config struct {
				Labels map[string]string `json:"Labels"`
			} `json:"Config"`
		}
		if json.Unmarshal(inspectBytes, &probe) == nil {
			if workingDir == "" {
				workingDir = strings.TrimSpace(probe.Config.Labels["com.docker.compose.project.working_dir"])
			}
			if composeFile == "" {
				if cf := strings.TrimSpace(probe.Config.Labels["com.docker.compose.project.config_files"]); cf != "" {
					composeFile = filepath.Base(strings.TrimSpace(strings.Split(cf, ",")[0]))
				}
			}
			if name == "" {
				name = strings.TrimPrefix(probe.Name, "/")
			}
		}
	}

	// Shared with the F82 plan preview (ResolveStackDir), so what the dialog
	// shows is exactly where the restore writes.
	stackDir := ResolveStackDir(workingDir, name, opts.HostBaseDir, "", "")
	if stackDir == "" {
		e.logf(b.ID, "INFO", "Skipping host stack reconstruction — %q has no recorded compose directory and no base directory was provided", name)
		return
	}
	// F81: relocate the stack folder itself under the new base, so the compose
	// file and the data land together on the target machine's layout.
	if remapped := ResolveStackDir(workingDir, name, opts.HostBaseDir, opts.RemapFromPath, opts.RemapToPath); remapped != stackDir {
		e.logf(b.ID, "INFO", "Remapped stack folder %s → %s", stackDir, remapped)
		stackDir = remapped
	}

	// F193: the source of the written file is the AS-RESTORED configuration —
	// the same inspectBytes the container was just created from, carrying every
	// change the operator asked for in the dialog: the IP remap, the path remap,
	// the new address, the rewritten user-mapping ids. The archived document
	// describes the source machine and is only the fallback when those bytes
	// cannot be parsed, with the text remaps kept for exactly that fallback.
	var composeBytes []byte
	var liveInsp types.ContainerJSON
	if json.Unmarshal(inspectBytes, &liveInsp) == nil && liveInsp.Config != nil {
		if doc, derr := composeFromInspect(liveInsp, man.Image, man.Networks, nil, dockercli.ImageEnv(ctx, cli, liveInsp.Image)); derr == nil {
			composeBytes = doc
		}
	}
	if len(composeBytes) == 0 {
		var err error
		composeBytes, err = e.extractEntry(ctx, b, opts.Source, "config/docker-compose.yml")
		if err != nil || len(composeBytes) == 0 {
			e.logf(b.ID, "WARN", "Cannot reconstruct host stack — no reconstructed compose file in this backup (%v)", err)
			return
		}
		// Keep the written compose consistent with the remapped container config.
		// One helper applies all three remaps (RemapEnvFile is generic text remap —
		// the same pass the .env gets), so this fallback can never drift from the
		// stack path again (it had drifted: the domain remap was missing here).
		// F81: the written compose must point at the new base too, or it wouldn't
		// start the stack it sits beside.
		var ips, domains, paths int
		composeBytes, ips, domains, paths = RemapEnvFile(composeBytes,
			opts.RemapFromIP, opts.RemapToIP,
			opts.RemapFromDomain, opts.RemapToDomain,
			opts.RemapFromPath, opts.RemapToPath)
		if ips > 0 {
			e.logf(b.ID, "INFO", "Remapped host IP %s → %s in %d place(s) of the reconstructed compose file", opts.RemapFromIP, opts.RemapToIP, ips)
		}
		if domains > 0 {
			e.logf(b.ID, "INFO", "Remapped domain %s → %s in %d place(s) of the reconstructed compose file", opts.RemapFromDomain, opts.RemapToDomain, domains)
		}
		if paths > 0 {
			e.logf(b.ID, "INFO", "Remapped host path %s → %s in %d place(s) of the reconstructed compose file", opts.RemapFromPath, opts.RemapToPath, paths)
		}
	}

	// F192: never a deployment tool's numeric id as the folder name when the
	// project's own name and a target base are both known.
	if fixed, changed := PreferProjectLeaf(stackDir, b.Stack, firstNonEmpty(opts.RemapToPath, opts.HostBaseDir)); changed {
		e.logf(b.ID, "INFO", "The recorded compose folder %s looks like a deployment tool's internal path — writing to %s instead, named after the project", stackDir, fixed)
		stackDir = fixed
	}

	// F194: secrets move to a .env beside the file, referenced as ${VAR}.
	composeBytes, envFile, movedSecrets := splitComposeSecrets(composeBytes)
	// F231: and the operator's OWN .env, remapped, goes underneath — theirs wins
	// on any key both define. Without it the stack came back beside a file
	// holding only DockBack's extracted secrets, and every ${VAR} their compose
	// depended on was simply gone.
	if orig := e.stackEnvFromArchive(ctx, b, opts.Source, b.ID,
		opts.RemapFromIP, opts.RemapToIP, opts.RemapFromDomain, opts.RemapToDomain,
		opts.RemapFromPath, opts.RemapToPath); len(orig) > 0 {
		envFile = MergeEnvFiles(orig, envFile)
	}

	e.logf(b.ID, "INFO", "Reconstructing on-host stack layout at %s", stackDir)
	// F230: who the folder belongs to when DockBack just created its parent.
	uid, gid, pinned := e.RestoreOwnership(opts.NodeID, b.TargetName)
	// F57: the operator's OWN compose file(s), restored beside the reconstruction
	// under a non-canonical name. The reconstruction keeps the name
	// `docker compose` reads because it describes the containers as they now are;
	// the original describes the SOURCE machine and may reference env_file targets
	// or paths that do not exist here.
	originals := e.originalsForHost(ctx, b, opts.Source, b.ID,
		opts.RemapFromIP, opts.RemapToIP, opts.RemapFromDomain, opts.RemapToDomain,
		opts.RemapFromPath, opts.RemapToPath)
	res, rerr := dockercli.ReconstructStackDirWithEnv(ctx, cli, stackDir, composeFile, composeBytes, envFile,
		HostFileOwner(uid, gid, pinned, dockercli.ContainerEnv(inspectBytes)), originals...)
	if rerr != nil {
		e.logf(b.ID, "WARN", "Host stack reconstruction skipped: %v", rerr)
		return
	}
	switch {
	case res.Unchanged:
		e.logf(b.ID, "INFO", "The compose file at %s already matches this container — left exactly as it is", res.Path)
	case res.Displaced != "":
		// F176: the reconstruction takes the canonical name, so `docker compose`
		// in that folder acts on what is actually running. The previous file is
		// renamed, never deleted.
		e.logf(b.ID, "INFO", "Wrote the reconstructed compose file to %s. The file that was there is kept as %s — nothing was deleted.", res.Path, res.Displaced)
		e.logf(b.ID, "WARN", "That reconstruction is built from the container's AS-RESTORED configuration — env_file, profiles and comments from your own compose are not carried over. Compare it against %s before you rely on it.", res.Displaced)
	default:
		e.logf(b.ID, "INFO", "Wrote reconstructed compose file to %s", res.Path)
	}
	if res.EnvPath != "" {
		e.logf(b.ID, "INFO", "Wrote %d secret value(s) to %s (mode 600) — the compose file references them as ${VAR} and carries no secrets itself. Keep the .env out of version control.", movedSecrets, res.EnvPath)
	}
	if res.EnvDisplaced != "" {
		e.logf(b.ID, "INFO", "The .env that was there is kept as %s — nothing was deleted.", res.EnvDisplaced)
	}
	e.logRestoredOriginals(b.ID, res.Originals)
	if res.Owner != "" && res.Owner != "0:0" {
		e.logf(b.ID, "INFO", "Reconstructed folder ownership set to %s (matched to its parent directory)", res.Owner)
	} else {
		e.logf(b.ID, "INFO", "Reconstructed folder is root-owned (its parent was absent or root-owned) — chown it to your user if needed")
	}
}

// ResolveStackDir resolves where a service's stack folder lands on the host:
// the recorded compose working dir, else <hostBaseDir>/<name> for a non-compose
// container ("" when neither is available), with F81's base-prefix remap
// applied when both bases are set. ONE function shared by the real
// reconstruction (reconstructHostStack) and the F82 plan preview, so the
// preview can never diverge from where files are actually written. Pure.
func ResolveStackDir(workingDir, name, hostBaseDir, fromPath, toPath string) string {
	dir := strings.TrimSpace(workingDir)
	if dir == "" {
		base := strings.TrimSpace(hostBaseDir)
		if base == "" || name == "" {
			return ""
		}
		dir = strings.TrimRight(base, "/") + "/" + name
	}
	if fromPath != "" && toPath != "" && fromPath != toPath {
		if out, n := dockercli.RemapTextHostPath([]byte(dir), fromPath, toPath); n > 0 {
			dir = string(out)
		}
	}
	return dir
}

// restoreAppExport rebuilds an application from its portable export using the
// app's own importer: start the (recreated) container, wait until it's ready,
// drop the export files into place, then run the import command.
func (e *Engine) restoreAppExport(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions) error {
	ae := man.AppExport
	e.logf(b.ID, "INFO", "App-native restore via the %s importer", ae.Tool)

	if cur, err := cli.ContainerInspect(ctx, opts.TargetID); err == nil && (cur.State == nil || !cur.State.Running) {
		e.logf(b.ID, "INFO", "Starting container")
		if err := cli.ContainerStart(ctx, opts.TargetID, container.StartOptions{}); err != nil {
			return fmt.Errorf("starting container: %w", err)
		}
	}
	e.logf(b.ID, "INFO", "Waiting for the application to become ready…")
	_ = dockercli.WaitForHealthy(ctx, cli, opts.TargetID, 5*time.Minute)

	// Ensure the export directory exists, then drop the export files in.
	_, _ = dockercli.ExecHook(ctx, cli, opts.TargetID, []string{"/bin/sh", "-c", "mkdir -p '" + ae.Dir + "'"}, "", "")
	e.logf(b.ID, "INFO", "Restoring export files into %s", ae.Dir)
	found := false
	if err := e.streamArchive(ctx, b, opts.Source, func(tr *tar.Reader, hdr *tar.Header) (bool, error) {
		if hdr.Name == "appexport.tar" {
			if ierr := dockercli.ExecStdin(ctx, cli, opts.TargetID, []string{"tar", "-xf", "-", "-C", ae.Dir}, tr); ierr != nil {
				return false, fmt.Errorf("unpacking export: %w", ierr)
			}
			found = true
			return false, nil
		}
		return true, nil
	}); err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("export archive not found in backup")
	}

	e.logf(b.ID, "INFO", "Importing via %s: %s", ae.Tool, strings.Join(ae.ImportCmd, " "))
	iout, ierr := dockercli.ExecHook(ctx, cli, opts.TargetID, ae.ImportCmd, "", "")
	if ierr != nil {
		return fmt.Errorf("app import: %w", ierr)
	}
	// F153: a zero exit code is not proof that anything was imported — the lesson
	// the database path learned from a dump that restored 27 of 72 primary keys
	// and reported success. So the importer's OUTPUT is read as well.
	//
	// It WARNS rather than fails, deliberately. Paperless's importer was measured
	// doing the right thing — it exits 1 when it refuses an instance that already
	// holds the documents — so there is no demonstrated case here of a crash
	// hiding behind a clean exit. Turning a heuristic read of another program's
	// output into a restore failure without one would only ever break recoveries
	// that currently work. Saying what was seen costs nothing and is the half
	// that is actually justified.
	if crash := ClassifyAppImportOutput(string(iout)); len(crash) > 0 {
		e.logf(b.ID, "WARN", "The %s importer exited successfully, but its output looks like something crashed part-way through: %s. "+
			"The restore is continuing — check the application's contents before relying on it",
			ae.Tool, strings.Join(crash, " | "))
	}
	// F152: the application's own verdict on what was just imported.
	//
	// This is the one restore path with no completeness contract behind it —
	// there is no dump to tally and no volume checksum, only that the importer
	// exited 0. Where the application ships a checker that exits non-zero on
	// inconsistent data, that turns the importer's word into the application's.
	// A failure here fails the restore, so the health gate's rollback applies.
	if len(ae.VerifyCmd) > 0 {
		e.logf(b.ID, "INFO", "Verifying the imported data with the application's own check: %s", strings.Join(ae.VerifyCmd, " "))
		if out, verr := dockercli.ExecHook(ctx, cli, opts.TargetID, ae.VerifyCmd, "", ""); verr != nil {
			e.logf(b.ID, "ERR", "The application rejected its own imported data: %v — %s", verr, short(strings.TrimSpace(string(out))))
			return fmt.Errorf("post-import verification failed: %w", verr)
		}
		e.logf(b.ID, "INFO", "The application confirmed the imported data is consistent")
	}
	e.logf(b.ID, "INFO", "App-native restore complete — %s data imported", ae.Tool)
	return nil
}

// resolveRestoreChain returns the ordered list of backups a restore must apply to
// reproduce backup b's volume state (F61): a single-element list for a
// self-contained full, or the full baseline followed by each delta up to b for an
// incremental backup. It gathers every successful backup of the same (node,target)
// and their manifests, then walks the parent chain with cryptographic pin checks
// (resolveChain), failing closed if any link is missing, unverified, or tampered.
func (e *Engine) resolveRestoreChain(b *store.Backup, man *Manifest) ([]*store.Backup, error) {
	if man == nil || (!man.Incremental && man.Parent == "") {
		return []*store.Backup{b}, nil // self-contained full — no chain to walk
	}
	list, err := e.Store.ListBackupsForTarget(b.NodeID, b.TargetName, 100000)
	if err != nil {
		return nil, err
	}
	byID := map[string]*Manifest{}
	backupByID := map[string]*store.Backup{}
	for _, x := range list {
		if x.Status != "success" || x.ManifestJSON == "" {
			continue
		}
		m := &Manifest{}
		if unmarshal(x.ManifestJSON, m) == nil {
			byID[x.ID] = m
			backupByID[x.ID] = x
		}
	}
	byID[b.ID], backupByID[b.ID] = man, b // ensure the chosen backup is present
	chainMans, err := resolveChain(b.ID, byID)
	if err != nil {
		return nil, err
	}
	out := make([]*store.Backup, 0, len(chainMans))
	for _, m := range chainMans {
		bk := backupByID[m.BackupID]
		if bk == nil {
			return nil, fmt.Errorf("backup chain is broken: %s is missing or unverified", short(m.BackupID))
		}
		out = append(out, bk)
	}
	return out, nil
}

// stopBeforeOverwrite stops the container whose data is about to be replaced.
//
// Every caller follows this with a wipe or an untar over the application's own
// data directory. The error used to be discarded and the restore carried on
// regardless, so a container the daemon could not stop had its files replaced
// while it was still writing to them — which corrupts the restored data and the
// application at the same time, and leaves neither recoverable from the other.
//
// A container that is already gone, or already stopped, is success: there is
// nothing left to stop. The daemon answers 304 for an already-stopped container
// and the SDK reports that as no error at all; the text check is there for a
// proxy that turns it into one.
func stopBeforeOverwrite(ctx context.Context, cli *client.Client, containerID, name string) error {
	err := cli.ContainerStop(ctx, containerID, container.StopOptions{})
	if err == nil || client.IsErrNotFound(err) || alreadyStopped(err) {
		return nil
	}
	return fmt.Errorf("could not stop %s before restoring its data: %w", name, err)
}

// alreadyStopped reports whether an error means the container was not running.
func alreadyStopped(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "is not running") || strings.Contains(msg, "already stopped")
}

// restoreFiles restores volume/bind data into an app container: quiesce for a
// consistent write, untar the data, then bring it back up. For an incremental
// backup (F61) it first resolves the parent chain and applies the full baseline
// then each delta in order — extracting changed files over, removing paths deleted
// since the parent, and laying each generation's consistent SQLite snapshot back —
// so the container ends at the exact state of the chosen generation.
func (e *Engine) restoreFiles(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions) error {
	insp, ierr := cli.ContainerInspect(ctx, opts.TargetID)
	wasRunning := ierr == nil && insp.State != nil && insp.State.Running
	if opts.Volumes && wasRunning {
		e.logf(b.ID, "INFO", "Stopping container for a consistent restore")
		// Nothing has been written yet, so refusing here changes nothing on disk.
		if serr := stopBeforeOverwrite(ctx, cli, opts.TargetID, b.TargetName); serr != nil {
			return serr
		}
	}
	if opts.Volumes {
		chain, cerr := e.resolveRestoreChain(b, man)
		if cerr != nil {
			return fmt.Errorf("restore: %w", cerr)
		}
		if len(chain) > 1 {
			e.logf(b.ID, "INFO", "Incremental restore: applying %d generations (full baseline → deltas)", len(chain))
		}
		for i, gen := range chain {
			gman := &Manifest{}
			_ = unmarshal(gen.ManifestJSON, gman)
			// Only the CHOSEN backup honors opts.Source; ancestors are opened from any
			// integrity-checked copy they have.
			src := ""
			if gen.ID == b.ID {
				src = opts.Source
			}
			member := "volumes.tar"
			if gman.Incremental {
				member = volumeDeltaMember
			}
			if rerr := e.streamArchive(ctx, gen, src, func(tr *tar.Reader, hdr *tar.Header) (bool, error) {
				if hdr.Name != member {
					return true, nil
				}
				if len(chain) > 1 {
					e.logf(b.ID, "INFO", "Applying generation %d/%d (%s)", i+1, len(chain), member)
				} else {
					e.logf(b.ID, "INFO", "Restoring volume/bind data via sidecar")
				}
				if err := dockercli.UntarToVolumes(ctx, cli, opts.TargetID, tr); err != nil {
					return false, fmt.Errorf("volume restore: %w", err)
				}
				return false, nil // stop after the payload
			}); rerr != nil {
				return rerr
			}
			// Remove files deleted since the parent (deltas only), validated.
			if del := sanitizeVolPaths(gman.Deleted); len(del) > 0 {
				e.logf(b.ID, "INFO", "Removing %d path(s) deleted since the previous generation", len(del))
				if derr := dockercli.DeleteVolumePaths(ctx, cli, opts.TargetID, del); derr != nil {
					return fmt.Errorf("applying deletions: %w", derr)
				}
			}
			// F22: lay this generation's consistent SQLite snapshot(s) over the raw
			// file(s) it just restored. Applied per generation in chain order so the
			// newest generation that touched a database wins.
			if oerr := e.overlaySQLite(ctx, cli, gen, gman, opts); oerr != nil {
				return fmt.Errorf("sqlite restore: %w", oerr)
			}
		}
		e.logf(b.ID, "INFO", "Data restored")
	}
	// F117: align ownership to the ids this host's container actually runs as,
	// BEFORE it starts — so the app never sees data it cannot write. What it
	// intended comes back, so the sweep below can tell a change DockBack made
	// from one nobody asked for (#15).
	ownership := e.alignRestoredOwnership(ctx, cli, b, man, opts)
	// F128: prove the recreate did not hand this container more Docker authority
	// than the backup recorded.
	e.assertDockerSocketNotWidened(ctx, cli, b, man, opts)
	// F143: prove the TLS certificates this backup recorded are actually back —
	// before the container starts, because a proxy told to serve a certificate
	// that is not there does not start at all, and "cannot load certificate" in a
	// container log is a much worse way to learn it.
	if cerr := e.assertCertificatesRestored(ctx, cli, b, man, opts); cerr != nil {
		return cerr
	}
	// F147: say what the sensitive files came back as. Never fails a restore —
	// the permissions are the data's, restoring them faithfully is correct, and a
	// recovery must not be refused over a mode bit. Reported here because a new
	// machine is the best chance this finding will ever get of being acted on.
	e.reportRestoredSecretModes(ctx, cli, b, man, opts)
	// F155: a copy of an application that registers itself with a cloud service
	// must not start carrying the original's identity. Runs ONLY for a clone,
	// against the clone's own throwaway data, and only here — before the start
	// below, because after it has announced itself is too late.
	if opts.AsName != "" {
		if nerr := e.neutralizeClone(ctx, cli, b, man, opts); nerr != nil {
			return nerr
		}
	}
	// #1: prove the data actually arrived before anything runs against it. Last
	// of the pre-start assertions on purpose — every step above can still change
	// what is on disk, and this one is about the final state.
	if derr := e.assertDataBindsFilled(ctx, cli, b, man, opts); derr != nil {
		return derr
	}
	// #34: and prove the application can WRITE to what it just got back, as its
	// own uid. After the ownership alignment above, because that is what this
	// checks the result of.
	e.verifyWritable(ctx, cli, b, man, opts)
	// #15: "verify by stat, never trust tar" — generally, not only for the
	// profile secrets F147 already stats. Reads what is actually on disk now and
	// holds it against what the backup recorded, allowing exactly the changes the
	// alignment above says it made.
	e.verifyRestoredOwnership(ctx, cli, b, man, opts, ownership)
	// #41/#26: and prove the FILES came back byte-for-byte, not merely the same
	// size. Beside the empty-data guard and for the same reason: this is the last
	// moment before anything is running that could write more.
	if ferr := e.verifyRestoredFiles(ctx, cli, b, man, opts, restoredScanRoots(man)); ferr != nil {
		return ferr
	}
	// F160: point the application at a dependency that has moved. Before the
	// start, so it comes up already looking in the right place rather than
	// failing to reach the old address first. Only when one was supplied.
	e.applyUpstreamAddress(ctx, cli, b, man, opts)

	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if cur, err := cli.ContainerInspect(sctx, opts.TargetID); err == nil && (cur.State == nil || !cur.State.Running) {
		e.logf(b.ID, "INFO", "Starting container")
		if err := cli.ContainerStart(sctx, opts.TargetID, container.StartOptions{}); err != nil {
			return fmt.Errorf("restore succeeded but failed to start container: %w", err)
		}
	}
	e.logf(b.ID, "INFO", "Container started with the restored data")
	return nil
}

// overlaySQLite lays each consistent SQLite snapshot (F22) back over its raw file
// and removes the stale WAL/shm side-files. It extracts the `sqlite/<n>.dbk`
// members to local scratch, builds a small overlay tar keyed by each database's
// source path, and hands it to a read-write sidecar. No-op when the backup carries
// no snapshots. The raw file remains as-is if any snapshot member is missing, so a
// partial archive never corrupts the restore.
func (e *Engine) overlaySQLite(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions) error {
	if man == nil || len(man.SQLiteDumps) == 0 {
		return nil
	}
	want := map[string]string{} // archive member -> absolute source db path
	for _, s := range man.SQLiteDumps {
		if s.Archive != "" && s.Source != "" {
			want[s.Archive] = s.Source
		}
	}
	if len(want) == 0 {
		return nil
	}
	tmp, err := os.MkdirTemp(e.WorkDir, "sqlrestore-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	// Extract each snapshot member to a local file named by its source (so the
	// overlay tar can be built with the correct in-volume path).
	local := map[string]string{} // source -> local file
	n := 0
	perr := e.streamArchive(ctx, b, opts.Source, func(tr *tar.Reader, hdr *tar.Header) (bool, error) {
		src, ok := want[hdr.Name]
		if !ok {
			return true, nil
		}
		n++
		lf := filepath.Join(tmp, fmt.Sprintf("%d.dbk", n))
		out, cerr := os.Create(lf)
		if cerr != nil {
			return true, nil // skip this one; raw file stays
		}
		if _, cerr := io.Copy(out, tr); cerr != nil {
			out.Close()
			return true, nil
		}
		out.Close()
		local[src] = lf
		return true, nil
	})
	if perr != nil {
		return perr
	}
	if len(local) == 0 {
		e.logf(b.ID, "WARN", "SQLite snapshots recorded but their data was not found in the archive — restored the raw file(s) instead")
		return nil
	}

	e.logf(b.ID, "INFO", "Applying %d consistent SQLite snapshot(s) over the restored file(s)", len(local))

	// Build the overlay tar (members named by the leading-slash-stripped source
	// path) and stream it to a read-write sidecar that also prunes the stale WAL/shm.
	var sources []string
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		var werr error
		for src, lf := range local {
			fi, serr := os.Stat(lf)
			if serr != nil {
				continue
			}
			f, oerr := os.Open(lf)
			if oerr != nil {
				continue
			}
			hdr := &tar.Header{Name: strings.TrimPrefix(src, "/"), Mode: 0o600, Size: fi.Size(), Typeflag: tar.TypeReg}
			if werr = tw.WriteHeader(hdr); werr != nil {
				f.Close()
				break
			}
			if _, werr = io.Copy(tw, f); werr != nil {
				f.Close()
				break
			}
			f.Close()
		}
		if werr == nil {
			werr = tw.Close()
		}
		pw.CloseWithError(werr)
	}()
	for src := range local {
		sources = append(sources, src)
	}
	checks, oerr := dockercli.OverlaySQLiteRestore(ctx, cli, opts.TargetID, pr, sources)
	if oerr != nil {
		return oerr
	}
	return e.assertSQLiteRestored(b.ID, man, checks)
}

// assertSQLiteRestored compares what the overlay sidecar read back off disk
// against what the manifest recorded at capture (F109), and FAILS THE RESTORE
// when the restored database is corrupt or demonstrably short.
//
// This is the restore-side half of the contract. Everything before it proves the
// archive was intact; only this proves the bytes that landed on the target are a
// working database with the content that was backed up. The failure mode it
// exists for is the quiet one — an app that starts happily on a database missing
// most of its rows, which looks like a successful restore until someone opens it.
//
// Deliberately asymmetric about what it will fail on:
//
//   - integrity_check returning anything but "ok" — a measured fact, always fatal.
//   - FEWER tables or FEWER rows than capture recorded — data is missing.
//   - MORE of either — not an error. A restore into a live container can legally
//     gain rows between the overlay and the read-back.
//   - anything unread (no sqlite3, an unreadable file, a pre-F109 backup with no
//     contract) — logged, never failed. A check that could not run must not
//     masquerade as a check that passed, in either direction.
func (e *Engine) assertSQLiteRestored(backupID string, man *Manifest, checks []dockercli.SQLiteRestoreCheck) error {
	if len(checks) == 0 {
		if len(man.SQLiteDumps) > 0 {
			e.logf(backupID, "WARN", "Restored %d SQLite database(s) but could not read them back to verify (the volume sidecar has no sqlite3) — the snapshots were applied, but their contents were not confirmed on the target", len(man.SQLiteDumps))
		}
		return nil
	}
	want := map[string]SQLiteRef{}
	for _, s := range man.SQLiteDumps {
		want[s.Source] = s
	}
	// F142: tables whose emptiness means this application did not come back — it
	// came up factory-fresh. Judged separately below, because it is a different
	// claim from "short by some rows" and deserves different words.
	critical := ProfileFor(man.Image).criticalTables()
	var failures []string
	for _, c := range checks {
		ref, known := want[c.Path]

		if c.Integrity == "" {
			e.logf(backupID, "WARN", "Restored %s but could not run an integrity check on it — applied, not confirmed", c.Path)
			continue
		}
		if c.Integrity != "ok" {
			failures = append(failures, fmt.Sprintf("%s failed its integrity check after restore (%s)", c.Path, c.Integrity))
			continue
		}

		// F142: the factory-reset guard. Judged before the checksum shortcut
		// below, because an operator who backed up an application that was
		// ALREADY in its factory state deserves to be told so — a byte-perfect
		// restore of nothing is still a restore of nothing.
		fail, warn := criticalTableVerdict(critical, c, ref, known)
		for _, w := range warn {
			e.logf(backupID, "WARN", "%s", w)
		}
		if len(fail) > 0 {
			failures = append(failures, fail...)
			continue
		}

		// F134: byte-identical is the strongest claim available, and it is the one
		// worth making first. A count can only say "the right NUMBER of rows came
		// back"; this says every row, every column value, every byte is the one
		// that was captured — which is the only way to prove something like
		// Karakeep's AI-vs-human tag attribution survived, since that lives in a
		// column value no count can see.
		if known && ref.SHA256 != "" && c.SHA256 != "" {
			if c.SHA256 != ref.SHA256 {
				failures = append(failures, fmt.Sprintf("%s is not the database that was captured — its checksum differs (expected %s…, got %s…)",
					c.Path, shortHash(ref.SHA256), shortHash(c.SHA256)))
				continue
			}
			e.logf(backupID, "INFO", "Verified %s: byte-identical to the captured database (%s…) — every row and value is exactly as backed up", c.Path, shortHash(ref.SHA256))
			continue
		}

		// No recorded contract (a pre-F109 backup): integrity passing is all we
		// can honestly claim, so claim exactly that.
		if !known || (ref.Tables == 0 && ref.Rows == 0) {
			e.logf(backupID, "INFO", "Restored %s: integrity ok (%d table(s)) — this backup predates content recording, so counts could not be compared", c.Path, c.Tables)
			continue
		}
		if ref.Tables > 0 && c.Tables >= 0 && c.Tables < ref.Tables {
			failures = append(failures, fmt.Sprintf("%s came back with %d table(s) but %d were captured", c.Path, c.Tables, ref.Tables))
			continue
		}
		// F124: per-table first — it names WHICH data is missing, and it catches
		// the loss an aggregate hides. Losing every row of a small but critical
		// table is a rounding error against a large one, and vanishes entirely if
		// another table grew in the meantime.
		if short := shortTables(ref.TableRows, c.TableRows); len(short) > 0 {
			failures = append(failures, fmt.Sprintf("%s came back short: %s", c.Path, strings.Join(short, ", ")))
			continue
		}
		if ref.Rows > 0 && c.RowsKnown && c.Rows < ref.Rows {
			failures = append(failures, fmt.Sprintf("%s came back with %d row(s) but %d were captured", c.Path, c.Rows, ref.Rows))
			continue
		}
		switch {
		case ref.Rows > 0 && c.RowsKnown:
			e.logf(backupID, "INFO", "Verified %s: integrity ok, %d table(s), %d row(s) — matches the backup", c.Path, c.Tables, c.Rows)
		default:
			e.logf(backupID, "INFO", "Verified %s: integrity ok, %d table(s) — matches the backup", c.Path, c.Tables)
		}
	}
	if len(failures) > 0 {
		// Loud and specific: the operator has a container running on data that is
		// not what they restored, and needs to know precisely which database.
		for _, f := range failures {
			e.logf(backupID, "ERROR", "SQLite restore verification FAILED: %s", f)
		}
		return fmt.Errorf("restored database did not match the backup: %s", strings.Join(failures, "; "))
	}
	return nil
}

// criticalTableVerdict judges the tables whose emptiness means the application
// came back factory-fresh rather than restored (F142).
//
// The generic contract already catches a shortfall wherever capture recorded
// one. What it cannot do is say what the shortfall MEANS, and for this class of
// table the meaning is the whole point: an empty `user` table in a reverse proxy
// is not "some rows are missing", it is "starting this container offers its
// setup wizard to whoever reaches it first". So the check fires before the
// container is started, and the message says that instead of naming a count.
//
// It is careful about what it will call a failure:
//
//   - restored zero, capture recorded more than zero → FAILURE. Two measured
//     numbers and a contradiction between them.
//   - restored zero, capture also recorded zero → warning. The backup is a
//     faithful copy of an application that was already empty, which is worth
//     knowing and is not a restore defect.
//   - restored zero, capture recorded nothing (an older backup) → warning. There
//     is no second number, so there is no contradiction to assert.
//   - the table could not be counted at all → nothing. A check that could not
//     run must not masquerade as one that did.
func criticalTableVerdict(critical []CriticalTable, c dockercli.SQLiteRestoreCheck, ref SQLiteRef, known bool) (failures, warnings []string) {
	for _, t := range critical {
		if t.DB != "" && t.DB != c.Path {
			continue
		}
		restored, counted := c.TableRows[t.Table]
		if !counted || restored > 0 {
			continue
		}
		capturedN, hadContract := int64(0), false
		if known {
			capturedN, hadContract = ref.TableRows[t.Table]
		}
		switch {
		case hadContract && capturedN > 0:
			failures = append(failures, fmt.Sprintf(
				"%s came back with an empty %q table, but %d row(s) were captured — %s. The restore was stopped before the container was started",
				c.Path, t.Table, capturedN, t.Means))
		case hadContract:
			warnings = append(warnings, fmt.Sprintf(
				"The %q table is empty in %s, and it was empty when this backup was taken too — %s. The restore is faithful; the backup simply has nothing in it to restore",
				t.Table, c.Path, t.Means))
		default:
			warnings = append(warnings, fmt.Sprintf(
				"The %q table is empty in the restored %s — %s. This backup records no count for it, so there is nothing to compare against; check the application before exposing it",
				t.Table, c.Path, t.Means))
		}
	}
	return failures, warnings
}

// ensureRecordedVolumes creates each named volume with the driver options
// recorded at backup time, returning a warning for any that could not be made
// faithfully.
//
// It never fails the restore: a container that comes back with one volume on the
// wrong backing store is recoverable once the operator is TOLD, whereas a
// restore that refuses outright leaves them with nothing. The warnings are
// deliberately loud for exactly that reason.
//
// A clone is never given these — an isolated clone gets fresh anonymous volumes
// and must not touch the original's storage.
func (e *Engine) ensureRecordedVolumes(ctx context.Context, cli *client.Client, man *Manifest) []string {
	var warnings []string
	seen := map[string]bool{}
	for _, v := range volumesToEnsure(man) {
		if v.Type != "volume" || v.Name == "" || seen[v.Name] {
			continue
		}
		seen[v.Name] = true
		// Nothing recorded at all (a pre-F90 backup with no labels either) — leave
		// Docker's own behaviour alone rather than creating a volume from nothing.
		//
		// F226: LABELS count as something recorded. They did not before, so a
		// plain local volume — no driver options, driver "local" or blank — fell
		// through to Docker, which recreated it without the com.docker.compose.*
		// labels its project expects. That is the whole failure: the data is fine
		// and `docker compose up` will not touch the volume afterwards.
		if len(v.Options) == 0 && len(v.Labels) == 0 && v.Driver == "" {
			continue
		}
		if err := dockercli.EnsureVolume(ctx, cli, v.Name, v.Driver, v.Options, v.Labels); err != nil {
			warnings = append(warnings, err.Error())
		}
	}
	return warnings
}

// volumesToEnsure is every named volume the restore must CREATE before the
// container can reference it.
//
// MountedVolumes (F226) is the complete set — every volume the container mounts,
// whatever became of its contents. Volumes is the captured set, and is unioned
// in so a backup taken before MountedVolumes existed still gets the behaviour it
// always had. Pure, so the rule is testable without a daemon.
func volumesToEnsure(man *Manifest) []VolumeRef {
	if man == nil {
		return nil
	}
	out := make([]VolumeRef, 0, len(man.MountedVolumes)+len(man.Volumes))
	out = append(out, man.MountedVolumes...)
	out = append(out, man.Volumes...)
	return out
}

// volumeSnapshotPlan is the pre-restore safety-snapshot decision for a
// standalone volume (F208), separated out so the rule is testable without a
// daemon — the same shape as every other decision in this package.
type volumeSnapshotPlan int

const (
	// volumeSnapshotSkipUnwanted: the operator unticked the safety snapshot, or
	// this is the engine's own internal rollback restore.
	volumeSnapshotSkipUnwanted volumeSnapshotPlan = iota
	// volumeSnapshotSkipAbsent: the target volume does not exist, so there is
	// nothing to overwrite and nothing to save.
	volumeSnapshotSkipAbsent
	// volumeSnapshotTake: a volume with contents is about to be written over.
	volumeSnapshotTake
)

// planVolumeSnapshot decides whether to snapshot before overwriting a volume.
//
// Deliberately NOT a copy of the container path's guard at the top of Restore.
// There, `!clone` skips the snapshot because a clone creates a NEW container
// with fresh isolated volumes and therefore overwrites nothing. Here, an
// alternate target name is not a clone at all — it names the volume that gets
// written INTO, which may hold data of its own. Carrying the container rule
// across would leave the one thing actually at risk unprotected, so the subject
// of this decision is always the volume being written, whatever it is called.
func planVolumeSnapshot(wantSnapshot, isRollback, volumeExists bool) volumeSnapshotPlan {
	if !wantSnapshot || isRollback {
		return volumeSnapshotSkipUnwanted
	}
	if !volumeExists {
		return volumeSnapshotSkipAbsent
	}
	return volumeSnapshotTake
}

// restoreVolumeOnly recreates a standalone named volume and untars its backed-up
// contents into it (F23). The target volume defaults to the original name; an
// explicit AsName restores into a differently-named volume (leaving the original
// untouched). VolumeCreate is idempotent, so an existing volume is reused.
func (e *Engine) restoreVolumeOnly(ctx context.Context, b *store.Backup, opts RestoreOptions) error {
	vol := strings.TrimPrefix(b.TargetName, volumeTargetPrefix)
	if opts.AsName != "" {
		vol = opts.AsName
	}
	if vol == "" {
		return fmt.Errorf("this backup has no target volume name")
	}
	cli, err := e.Reg.Get(opts.NodeID)
	if err != nil {
		return err
	}
	e.logf(b.ID, "INFO", "Restoring standalone volume %q from backup %s", vol, short(b.ID))

	// F208: a rollback point before the overwrite — the same protection a
	// container restore has taken since PLAN §3.7, and the one thing this path was
	// missing. A volume restore is destructive in place: UntarToNamedVolume
	// extracts over whatever is already there, so every path the archive contains
	// replaces the live file at that path, irreversibly.
	//
	// Taken here, before streamArchive below, because that call IS the overwrite —
	// its callback untars as the bytes arrive, so there is no later moment at which
	// the current contents still exist. A corrupt or unreachable source archive
	// therefore costs one local snapshot and destroys nothing, which is the right
	// way round.
	if err := e.snapshotVolumeBeforeOverwrite(ctx, cli, b, vol, opts); err != nil {
		return err
	}

	// F226: create the volume with its RECORDED identity — driver, options and
	// labels — before anything is written into it. UntarToNamedVolume creates a
	// bare local volume if one is missing, which put an NFS/CIFS volume's data on
	// local disk and, for a volume that belonged to a compose project, produced
	// one without the com.docker.compose.* labels that project expects. Creating
	// it here first makes that create a no-op.
	//
	// Warnings only: a refusal here (a redacted credential, a mismatched existing
	// volume) is already reported by EnsureVolume's own error, and a volume
	// restore that cannot honour the recorded backing store should say so rather
	// than silently write somewhere else.
	man := &Manifest{}
	if b.ManifestJSON != "" {
		_ = json.Unmarshal([]byte(b.ManifestJSON), man)
	}
	for _, w := range e.ensureRecordedVolumes(ctx, cli, man) {
		e.logf(b.ID, "WARN", "%s", w)
	}

	found := false
	rerr := e.streamArchive(ctx, b, opts.Source, func(tr *tar.Reader, hdr *tar.Header) (bool, error) {
		if hdr.Name != "volumes.tar" {
			return true, nil
		}
		// volumes.tar for a volume backup is a contents-relative tar of the volume;
		// (re)create the named volume and extract it back in.
		if err := dockercli.UntarToNamedVolume(ctx, cli, vol, tr); err != nil {
			return false, fmt.Errorf("volume restore: %w", err)
		}
		found = true
		return false, nil // stop after the volume payload
	})
	if rerr != nil {
		return rerr
	}
	if !found {
		return fmt.Errorf("archive is missing volumes.tar")
	}
	e.logf(b.ID, "INFO", "Volume %q restored", vol)
	return nil
}

// snapshotVolumeBeforeOverwrite captures the target volume's CURRENT contents as
// a local standalone-volume backup, so a wrong volume restore is reversible
// (F208). Returns an error only when the restore must be abandoned.
//
// It mirrors the container path's safety snapshot (see Restore) in every way that
// matters — local-only, self-contained, abort-the-restore-if-it-fails — and
// differs in the two places the subject genuinely differs:
//
//   - EXISTENCE IS CHECKED, AND NOT-FOUND IS NOT AN ERROR. The primary use of a
//     standalone-volume backup is restoring data whose volume is long gone, and
//     there is nothing to protect in that case. The check has to be
//     not-found-aware rather than the usual bare err == nil: a daemon that cannot
//     be reached must abort, not be read as "nothing to lose". Without any check
//     at all this would be worse than useless — a read-only bind to a missing
//     volume makes Docker CREATE it, so the snapshot would silently succeed with
//     an empty archive, pass verification, and stand as a rollback point that
//     restores nothing.
//
//   - IT IS LABELLED "auto: pre-restore". Retention gives labels beginning with
//     "auto:" their own budget (F48) precisely so machine-made protective
//     snapshots cannot crowd out the generations an operator relies on. For a
//     "volume:" target that budget is otherwise empty — event-triggered snapshots
//     only ever target containers — so the label costs nothing and stops each
//     restore from permanently spending one of that volume's scheduled
//     generations. The container path is deliberately NOT relabelled: there the
//     auto budget is already contested by the pre-change snapshot that a restore's
//     own stop/die fires, so moving it in would SHORTEN the life of a rollback
//     point that the restore's own failure messages tell the operator to use.
//
// LANDMINE, for whoever plumbs a private key into this path. Restore holds
// e.privMu for its whole duration when opts.PrivateKey is set (see the top of
// Restore), and the backup this function starts ends in Verify, which re-locks
// e.privMu through restorePrivFor() whenever the NEW backup is itself write-only
// encrypted. A Go sync.Mutex is not reentrant, so that is a permanent hang, not
// an error — and it holds the restore lock while it hangs. It is unreachable
// from here today only because the standalone-volume path supplies no private
// key at all; adding one without first fixing the privMu protocol would turn
// every write-only volume restore into a stuck stack.
func (e *Engine) snapshotVolumeBeforeOverwrite(ctx context.Context, cli *client.Client, b *store.Backup, vol string, opts RestoreOptions) error {
	if !opts.Snapshot || opts.rollback {
		return nil
	}
	exists, xerr := dockercli.VolumeExists(ctx, cli, vol)
	if xerr != nil {
		// Not "no volume" — "no answer". Refusing here keeps the promise the
		// container path makes: never destroy data without a rollback point.
		return fmt.Errorf("could not check whether volume %q already holds data, so the safety snapshot could not be taken; aborting rather than overwrite it blind: %w", vol, xerr)
	}
	switch planVolumeSnapshot(opts.Snapshot, opts.rollback, exists) {
	case volumeSnapshotSkipAbsent:
		e.logf(b.ID, "INFO", "No existing volume %q — nothing to overwrite, so no safety snapshot is needed", vol)
		return nil
	case volumeSnapshotTake:
	default:
		return nil
	}

	snapNode := opts.NodeID
	if n, nerr := e.Store.GetNode(opts.NodeID); nerr == nil && n.Name != "" {
		snapNode = n.Name // the node NAME is the top-level storage folder, not the id
	}
	e.logf(b.ID, "INFO", "Safety snapshot: backing up current volume state before overwrite")
	sid, serr := e.Run(ctx, snapNode, preRestoreVolumeSnapshotOptions(opts.NodeID, vol))
	if serr != nil {
		return fmt.Errorf("pre-restore safety snapshot failed; aborting to avoid an unreversible overwrite: %w", serr)
	}
	// Named so the operator can find it: this path has no health gate and no
	// automatic rollback, so restoring this snapshot by hand IS the undo.
	e.logf(b.ID, "INFO", "Safety snapshot created (%s) — restore it to put this volume back as it was", short(sid))
	return nil
}

// preRestoreVolumeSnapshotOptions is what a standalone-volume safety snapshot is
// captured with (F208). Every field is load-bearing, so they are named in one
// place a test can assert on rather than inline at the call site.
func preRestoreVolumeSnapshotOptions(nodeID, vol string) Options {
	return Options{
		NodeID:               nodeID,
		VolumeOnly:           vol,
		Compression:          "balanced",
		DestinationsExplicit: true, // local-only — an immediate rollback point, not an offsite copy
		ForceFull:            true, // self-contained, never a delta on an in-flight chain (F61)
		Label:                autoPreRestoreLabel,
		SkipRetention:        true, // never let this snapshot's sweep prune the archive being restored
	}
}

// autoPreRestoreLabel marks a standalone-volume pre-restore snapshot. The "auto:"
// prefix is what puts it in retention's separate budget for machine-made
// protective snapshots (F48), so it never spends a scheduled generation.
const autoPreRestoreLabel = "auto: pre-restore"

// restoreDatabaseFromDump rebuilds a database container's data from the logical
// dump: stop → wipe the data dir → start (fresh init) → wait ready → import.
// This yields a guaranteed-consistent database.
func (e *Engine) restoreDatabaseFromDump(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions) error {
	eng := man.Databases[0].Engine
	// Redis loads its snapshot from an on-disk RDB at boot (not via a stdin
	// import like the SQL engines), so it takes a dedicated restore path.
	if eng == "redis" {
		return e.restoreRedisFromDump(ctx, cli, b, man, opts)
	}
	// Per-database subset restore (F8): the dump contains only the selected
	// databases, each self-cleaning (DROP+CREATE). Import into the RUNNING engine
	// without wiping the data directory, so the OTHER databases on a shared server
	// are preserved — a full-cluster wipe here would destroy them.
	if subset := cleanDBNames(man.Databases[0].Databases); len(subset) > 0 {
		return e.restoreDatabaseSubset(ctx, cli, b, man, opts, eng, subset)
	}
	dataDir := dockercli.DBDataDir(eng)
	e.logf(b.ID, "INFO", "Database container — restoring from a consistent %s dump (not raw files)", eng)

	// F174: this restore is about to EMPTY the data directory so the engine
	// re-initialises. A database image refuses to initialise an empty directory
	// without a root password and exits immediately — and a container whose
	// directory was initialised long ago runs happily for years without one, so
	// the variable can be missing and nothing ever goes wrong until this moment.
	//
	// Checked BEFORE the wipe, because after it the container is dead and its
	// data is gone, and this path returns before the health gate, so nothing
	// rolls it back.
	if dataDir != "" {
		if insp, ierr := cli.ContainerInspect(ctx, opts.TargetID); ierr == nil && insp.Config != nil {
			if ok, missing := dbInitPossible(eng, insp.Config.Env); !ok {
				return fmt.Errorf("this restore would empty the database's data directory so the engine can re-initialise, "+
					"and %s cannot initialise an empty directory without %s in its environment — it would exit immediately, "+
					"after the data had already been deleted. Nothing has been changed. "+
					"Add that variable to the container (any value; it only sets the new root password) and restore again. "+
					"The container has been running without it because its data directory was initialised before, which is exactly why this only shows up now",
					eng, missing)
			}
		}
	}

	e.logf(b.ID, "INFO", "Stopping database for a clean restore")
	if serr := stopBeforeOverwrite(ctx, cli, opts.TargetID, b.TargetName); serr != nil {
		return serr
	}

	if dataDir != "" {
		e.logf(b.ID, "INFO", "Clearing data directory %s so the engine re-initializes fresh", dataDir)
		if err := dockercli.WipeDir(ctx, cli, opts.TargetID, dataDir); err != nil {
			return fmt.Errorf("clearing data dir: %w", err)
		}
	}

	e.logf(b.ID, "INFO", "Starting database (fresh initialization)")
	if err := cli.ContainerStart(ctx, opts.TargetID, container.StartOptions{}); err != nil {
		return fmt.Errorf("starting database: %w", err)
	}
	e.logf(b.ID, "INFO", "Waiting for the database to accept connections…")
	if err := dockercli.WaitForDB(ctx, cli, opts.TargetID, eng); err != nil {
		return err
	}

	// #39/#32: same moment, same reason — the data directory was wiped above and
	// the engine has just built a fresh cluster from THIS machine's defaults.
	if eng == "postgres" {
		e.replayPGClusterConfig(ctx, cli, b, man, opts, nil)
	}

	// #13: a dump taken by an application identity restores its schemas completely
	// and contains none of the server's accounts. Said HERE, where a fresh server
	// is about to be built from it, because that is the moment the gap matters.
	e.noteDumpScope(b, man)
	e.logf(b.ID, "INFO", "Importing %s dump…", eng)
	imported := false
	err := e.streamArchive(ctx, b, opts.Source, func(tr *tar.Reader, hdr *tar.Header) (bool, error) {
		if strings.HasPrefix(hdr.Name, "db/") {
			if ierr := e.importDatabase(ctx, cli, b.ID, opts.TargetID, dbEngineForEntry(man, hdr.Name), tr, dbDumpForEntry(man, hdr.Name), nil); ierr != nil {
				return false, fmt.Errorf("import %s: %w", hdr.Name, ierr)
			}
			imported = true
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	if !imported {
		return fmt.Errorf("no database dump found in archive")
	}
	// #18/#26: prove the rows came back, not just that the import ran. Before
	// declaring success, at the one moment the data is the restore's alone.
	if len(man.Databases) > 0 {
		volatile, classified := e.stackVolatileTables(ctx, cli, manifestImage(man, b), b.Stack)
		if !e.verifyTableHashes(ctx, cli, b, opts.TargetID, eng, nil, man.Databases[0].TableHashes, volatile, classified) {
			return fmt.Errorf("the restored database does not match this backup's content baseline — see the run log for the tables that differ")
		}
	}
	e.logf(b.ID, "INFO", "Database restored from consistent dump — %s is running with healthy data", eng)
	return nil
}

// restoreDatabaseSubset restores a PER-DATABASE subset dump (F8) into the RUNNING
// engine WITHOUT wiping the data directory, so only the selected databases are
// replaced and the rest of a shared cluster is left intact. The dump is
// self-cleaning (Postgres pg_dump --create --clean --if-exists; MySQL
// --add-drop-database), so importing it drops+recreates exactly the selected
// databases. For Postgres, active sessions on those databases are terminated
// first (best-effort) so DROP DATABASE can succeed.
func (e *Engine) restoreDatabaseSubset(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions, eng string, subset []string) error {
	e.logf(b.ID, "INFO", "Per-database restore of %s (%s) — importing into the running engine WITHOUT wiping the cluster; the other databases are preserved", eng, strings.Join(subset, ", "))

	// Ensure the engine is up and accepting connections (do NOT wipe / re-init).
	if cur, err := cli.ContainerInspect(ctx, opts.TargetID); err == nil && (cur.State == nil || !cur.State.Running) {
		e.logf(b.ID, "INFO", "Starting database")
		if err := cli.ContainerStart(ctx, opts.TargetID, container.StartOptions{}); err != nil {
			return fmt.Errorf("starting database: %w", err)
		}
	}
	e.logf(b.ID, "INFO", "Waiting for the database to accept connections…")
	if err := dockercli.WaitForDB(ctx, cli, opts.TargetID, eng); err != nil {
		return err
	}

	// Postgres: free the target databases of active sessions so the dump's
	// DROP DATABASE can succeed. Best-effort — ignore errors.
	if eng == "postgres" {
		e.logf(b.ID, "INFO", "Closing active sessions on the target database(s) so they can be replaced")
		_, _ = dockercli.ExecCapture(ctx, cli, opts.TargetID, pgTerminateConnectionsCmd(subset, nil))
	}

	e.logf(b.ID, "INFO", "Importing the selected database(s)…")
	imported := false
	err := e.streamArchive(ctx, b, opts.Source, func(tr *tar.Reader, hdr *tar.Header) (bool, error) {
		if strings.HasPrefix(hdr.Name, "db/") {
			if ierr := e.importDatabase(ctx, cli, b.ID, opts.TargetID, dbEngineForEntry(man, hdr.Name), tr, dbDumpForEntry(man, hdr.Name), nil); ierr != nil {
				return false, fmt.Errorf("import %s: %w", hdr.Name, ierr)
			}
			imported = true
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	if !imported {
		return fmt.Errorf("no database dump found in archive")
	}
	e.logf(b.ID, "INFO", "Per-database restore complete — %s re-imported into %s (sibling databases untouched)", strings.Join(subset, ", "), eng)
	return nil
}

// pgTerminateConnectionsCmd builds an in-container psql command that terminates
// active sessions on the given databases (except its own), so a subsequent
// DROP DATABASE during a per-database restore isn't blocked by "database is being
// accessed by other users". Names are SQL-escaped (doubled single quotes) for the
// literal, then shell-escaped for the single-quoted -c argument (SEC-8).
func pgTerminateConnectionsCmd(dbs []string, d *EmbeddedDump) []string {
	lits := make([]string, 0, len(dbs))
	for _, d := range dbs {
		lits = append(lits, "'"+strings.ReplaceAll(d, "'", "''")+"'")
	}
	sql := "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname IN (" +
		strings.Join(lits, ",") + ") AND pid <> pg_backend_pid();"
	inner := `psql ` + pgClientOpts(d) + ` -d postgres -c '` + shellEscape(sql) + `'`
	return []string{"/bin/sh", "-c", inner}
}

// restoreRedisFromDump restores a Redis/Valkey/KeyDB container from its captured
// RDB snapshot: stop → wipe the data dir (removing any stale RDB/AOF so the
// restored snapshot is what loads) → write dump.rdb into the data dir → start,
// letting the engine load the snapshot on boot. This mirrors how the SQL engines
// re-initialize from a consistent dump (PLAN §4.1), adapted to Redis's boot-time
// RDB load.
func (e *Engine) restoreRedisFromDump(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions) error {
	dataDir := dockercli.DBDataDir("redis") // /data
	e.logf(b.ID, "INFO", "Redis container — restoring from a consistent RDB snapshot (not raw files)")

	e.logf(b.ID, "INFO", "Stopping Redis for a clean restore")
	if serr := stopBeforeOverwrite(ctx, cli, opts.TargetID, b.TargetName); serr != nil {
		return serr
	}

	if dataDir != "" {
		e.logf(b.ID, "INFO", "Clearing data directory %s so the restored snapshot is what loads", dataDir)
		if err := dockercli.WipeDir(ctx, cli, opts.TargetID, dataDir); err != nil {
			return fmt.Errorf("clearing data dir: %w", err)
		}
	}

	wrote := false
	err := e.streamArchive(ctx, b, opts.Source, func(tr *tar.Reader, hdr *tar.Header) (bool, error) {
		if strings.HasPrefix(hdr.Name, "db/") {
			if werr := dockercli.WriteFileToVolume(ctx, cli, opts.TargetID, dataDir+"/dump.rdb", tr); werr != nil {
				return false, fmt.Errorf("writing rdb: %w", werr)
			}
			wrote = true
			return false, nil // stop after the snapshot
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	if !wrote {
		return fmt.Errorf("no redis snapshot found in archive")
	}

	e.logf(b.ID, "INFO", "Starting Redis (loading the restored snapshot)")
	if err := cli.ContainerStart(ctx, opts.TargetID, container.StartOptions{}); err != nil {
		return fmt.Errorf("starting redis: %w", err)
	}
	_ = dockercli.WaitForHealthy(ctx, cli, opts.TargetID, time.Minute)
	// F99: Redis does not import through a client, so nothing here can be
	// classified — a snapshot that failed to load leaves an EMPTY, healthy Redis
	// and a restore reported as successful. Ask it how many keys it has and hold
	// it to what capture recorded.
	if verr := e.verifyImportCompleteness(ctx, cli, b.ID, opts.TargetID, "redis",
		expectedFor(redisDumpRecord(man), DumpExpect{})); verr != nil {
		return verr
	}
	e.logf(b.ID, "INFO", "Redis restored from snapshot — container started with the restored data")
	return nil
}

// redisDumpRecord finds this backup's Redis dump record, so the restore can be
// held to the key count taken when the snapshot was made. nil on a pre-F99
// backup, which simply has nothing to check against.
func redisDumpRecord(man *Manifest) *DBDump {
	if man == nil {
		return nil
	}
	for i := range man.Databases {
		if man.Databases[i].Engine == "redis" {
			return &man.Databases[i]
		}
	}
	return nil
}

// DecryptTo streams a backup's decrypted, decompressed tar archive to w (for
// manual/granular restore and "restore by hand" — PLAN §9.3/§9.14).
func (e *Engine) DecryptTo(ctx context.Context, backupID string, w io.Writer) error {
	b, err := e.Store.GetBackup(backupID)
	if err != nil {
		return err
	}
	rc, err := e.openVerified(ctx, b, "")
	if err != nil {
		return err
	}
	defer rc.Close()

	ak, algo, err := e.archiveCodec(b)
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(crypto.Decrypt(pw, rc, ak)) }()
	zr, err := newDecompressReader(pr, algo)
	if err != nil {
		return err
	}
	defer zr.Close()
	_, err = io.Copy(w, zr)
	return err
}

// loadImageTar streams the bundled image.tar member straight into the daemon
// (`docker load`) so an air-gapped restore needs no registry (PLAN §0.3 / §8.4).
// The large member is streamed, never buffered in memory.
func (e *Engine) loadImageTar(ctx context.Context, cli *client.Client, b *store.Backup, source string) error {
	found := false
	err := e.streamArchive(ctx, b, source, func(tr *tar.Reader, hdr *tar.Header) (bool, error) {
		if hdr.Name != "image.tar" {
			return true, nil
		}
		resp, lerr := cli.ImageLoad(ctx, tr, true)
		if lerr != nil {
			return false, lerr
		}
		if resp.Body != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		found = true
		return false, nil // stop after the image member
	})
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("image.tar not found in archive")
	}
	return nil
}

// ArchiveEntry is one browsable file inside a backup's volume payload (F21):
// a path relative to the captured volume tree, its size, and whether it's a dir.
type ArchiveEntry struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Dir  bool   `json:"dir"`
}

// ErrEntryNotFound is returned by ExtractOne when the requested path is not a file
// in the backup, so the API can answer a clean 400 rather than stream garbage.
var ErrEntryNotFound = errors.New("file not found in backup")

// maxBrowseEntries caps the STREAMING-fallback listing (legacy archives with no
// stored index), so walking an enormous tar can't build an unbounded response.
// Indexed backups (F61/F70) are never truncated — their listing is a compact
// stored member. The caller is told (via a log line) when a list was truncated.
const maxBrowseEntries = 20000

// ListEntries returns the files inside a backup's volume payload (F21) so the UI
// can browse them without downloading the whole archive. It reuses the same
// decrypt+decompress+tar-walk used by verification, descends into the nested
// `volumes.tar`, and returns each member's volume-relative path + size. Non-volume
// control entries (manifest.json, config/, db/, …) are skipped — this browses the
// user's data, not DockBack's metadata.
func (e *Engine) ListEntries(ctx context.Context, b *store.Backup, source string) ([]ArchiveEntry, error) {
	// Indexed backup (F61 incrementals; F70 universal index on fulls too): serve
	// the COMPLETE post-backup file set from the stored index — one small member
	// read instead of decrypting the whole payload, and NEVER truncated (the
	// index is a compact listing; the 20k cap exists only to bound a full
	// tar-walk of a legacy archive).
	man := &Manifest{}
	if b.ManifestJSON != "" {
		_ = unmarshal(b.ManifestJSON, man)
	}
	if man.VolIndex != "" {
		idx, err := e.loadVolIndex(ctx, b)
		if err != nil {
			return nil, fmt.Errorf("read file index: %w", err)
		}
		out := make([]ArchiveEntry, 0, len(idx.Entries))
		for _, fe := range idx.Entries {
			if name := cleanEntryName(fe.Path); name != "" {
				out = append(out, ArchiveEntry{Name: name, Size: fe.Size})
			}
		}
		return out, nil
	}
	out := []ArchiveEntry{}
	truncated := false
	err := e.streamArchive(ctx, b, source, func(tr *tar.Reader, hdr *tar.Header) (bool, error) {
		if hdr.Name != "volumes.tar" {
			return true, nil // keep scanning for the volume payload
		}
		vtr := tar.NewReader(tr)
		for {
			vh, verr := vtr.Next()
			if verr == io.EOF {
				break
			}
			if verr != nil {
				return false, verr
			}
			name := cleanEntryName(vh.Name)
			if name == "" {
				continue
			}
			out = append(out, ArchiveEntry{Name: name, Size: vh.Size, Dir: vh.FileInfo().IsDir()})
			if len(out) >= maxBrowseEntries {
				truncated = true
				break
			}
		}
		return false, nil // stop after the volume payload
	})
	if err != nil {
		return nil, err
	}
	if truncated {
		e.logf(b.ID, "WARN", "Browse list truncated at %d entries — this backup has more files than the browser shows; use a full restore/download to get them all", maxBrowseEntries)
	}
	return out, nil
}

// ExtractOne streams the single volume file at `name` (a path as returned by
// ListEntries) from the backup to w, decrypted, without buffering the whole file
// (F21). It matches an exact tar-member name inside the nested `volumes.tar` and
// only ever writes that member's bytes to w — it never touches the filesystem by
// path, so a traversal-looking name simply matches nothing. Returns ErrEntryNotFound
// when no such file exists.
func (e *Engine) ExtractOne(ctx context.Context, b *store.Backup, source, name string, w io.Writer) error {
	want := cleanEntryName(name)
	if want == "" {
		return ErrEntryNotFound
	}
	man := &Manifest{}
	if b.ManifestJSON != "" {
		_ = unmarshal(b.ManifestJSON, man)
	}
	// Incremental (F61): the newest version of the file lives in the newest chain
	// generation that touched it. Walk the resolved chain newest→oldest and return
	// the first copy found (browse only offers files present as of this generation,
	// so a hit always exists unless the chain is broken).
	if man.Incremental && man.Parent != "" {
		chain, cerr := e.resolveRestoreChain(b, man)
		if cerr != nil {
			return cerr
		}
		for i := len(chain) - 1; i >= 0; i-- {
			gen := chain[i]
			gman := &Manifest{}
			_ = unmarshal(gen.ManifestJSON, gman)
			member := "volumes.tar"
			src := ""
			if gman.Incremental {
				member = volumeDeltaMember
			}
			if gen.ID == b.ID {
				src = source
			}
			ok, eerr := e.extractFromMember(ctx, gen, src, member, want, w)
			if eerr != nil {
				return eerr
			}
			if ok {
				return nil
			}
		}
		return ErrEntryNotFound
	}
	ok, err := e.extractFromMember(ctx, b, source, "volumes.tar", want, w)
	if err != nil {
		return err
	}
	if !ok {
		return ErrEntryNotFound
	}
	return nil
}

// extractFromMember streams the single volume file `want` out of a specific nested
// payload member (`volumes.tar` or `volumes-delta.tar`) of one backup to w. It
// only ever writes the matched member's bytes — a traversal-looking name simply
// matches nothing. Returns whether the file was found.
func (e *Engine) extractFromMember(ctx context.Context, b *store.Backup, source, member, want string, w io.Writer) (bool, error) {
	return e.walkToEntry(ctx, b, source, member, want, func(_ *tar.Header, r io.Reader) error {
		_, err := io.Copy(w, r)
		return err
	})
}

// walkToEntry locates the single volume file `want` inside a specific nested
// payload member of one backup and hands its header and body to fn.
//
// The header matters for a write-back (F96): mode, ownership and the recorded
// size come from the archive, not from a guess. Extraction (F21) ignores it.
// Only the MATCHED member is ever passed on — a traversal-looking name simply
// matches nothing, which is what makes both callers safe by construction.
func (e *Engine) walkToEntry(ctx context.Context, b *store.Backup, source, member, want string, fn func(*tar.Header, io.Reader) error) (bool, error) {
	found := false
	err := e.streamArchive(ctx, b, source, func(tr *tar.Reader, hdr *tar.Header) (bool, error) {
		if hdr.Name != member {
			return true, nil
		}
		vtr := tar.NewReader(tr)
		for {
			vh, verr := vtr.Next()
			if verr == io.EOF {
				break
			}
			if verr != nil {
				return false, verr
			}
			if cleanEntryName(vh.Name) != want || vh.FileInfo().IsDir() {
				continue
			}
			if cerr := fn(vh, vtr); cerr != nil {
				return false, cerr
			}
			found = true
			return false, nil // stop after the matched file
		}
		return false, nil
	})
	return found, err
}

// cleanEntryName normalizes a tar member name to the volume-relative path the UI
// shows and matches on: leading "./" and "/" stripped, back-slashes normalized.
func cleanEntryName(name string) string {
	n := strings.ReplaceAll(name, "\\", "/")
	n = strings.TrimPrefix(n, "./")
	n = strings.TrimPrefix(n, "/")
	n = strings.TrimSuffix(n, "/") // directory entries
	if n == "." || n == ".." {
		return ""
	}
	return n
}

// extractEntry returns the bytes of a single named file from the backup archive
// (used to read config/inspect.json for recreation).
func (e *Engine) extractEntry(ctx context.Context, b *store.Backup, source, name string) ([]byte, error) {
	var out []byte
	// The caller's ctx, not a background one. Finding even a small member means
	// streaming and decrypting the archive up to it, which on a multi-GB backup
	// is minutes — and "Cancel restore" promises to stop at the next safe point.
	err := e.streamArchive(ctx, b, source, func(tr *tar.Reader, hdr *tar.Header) (bool, error) {
		if hdr.Name == name {
			data, err := io.ReadAll(tr)
			if err != nil {
				return false, err
			}
			out = data
			return false, nil // stop early
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, fmt.Errorf("%s not found in archive", name)
	}
	return out, nil
}

// originalComposeArchivePrefix is where captureOriginalCompose's files live
// inside the archive (layoutPath maps work/original-compose/* to it).
const originalComposeArchivePrefix = "config/original-compose/"

// Where a captured file bind's contents live: bindFileWorkDir under the backup's
// work directory, which layoutPath maps to bindFileArchivePrefix in the archive.
// Members are numbered rather than named after their path — the manifest's
// VolumeRef.Archive says which is which, and a number cannot carry a traversal.
const (
	bindFileWorkDir       = "bind-files"
	bindFileArchivePrefix = "config/bind-files/"
)

// restoreBindFiles writes every captured file-bind onto the target host, before
// the container that mounts them is created (F81).
//
// This is the other half of splitFileBinds. A file bind cannot be restored the
// way every other mount is — the volume archive is extracted in a sidecar where
// each bind is a live mount, and replacing a bind-mounted file needs an unlink
// the kernel refuses — so its bytes travel as their own archive member and land
// here, on the host, while nothing has them open.
//
// Never fatal. A file that cannot be written leaves its path absent, and the
// bind-source preflight immediately after names every absent path at once with
// what to do about it — which is a better report than failing here with the
// first one. What must not happen is a restore that reports success having
// silently skipped an application's key, so every failure is logged.
func (e *Engine) restoreBindFiles(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions) {
	if man == nil {
		return
	}
	written := 0
	for _, v := range man.Volumes {
		if v.Kind != dockercli.MountKindFile || v.Archive == "" || v.Source == "" {
			continue
		}
		data, err := e.extractEntry(ctx, b, opts.Source, v.Archive)
		if err != nil {
			e.logf(b.ID, "WARN", "This backup records the file mounted at %s but the archive does not hold it (%v) — the application will start without it", v.Destination, err)
			continue
		}
		host := dockercli.RemapHostPath(v.Source, opts.RemapFromPath, opts.RemapToPath)
		if werr := dockercli.WriteHostFile(ctx, cli, host, data, v.Owner, v.Mode); werr != nil {
			e.logf(b.ID, "WARN", "Could not write the file mounted at %s to %s (%v) — put it there yourself before starting the container, or it will come up without it", v.Destination, host, werr)
			continue
		}
		written++
		e.logf(b.ID, "INFO", "Wrote file bind %s (%s, %s) — mounted at %s", host, recordedOwnerLabel(v.Owner), recordedModeLabel(v.Mode), v.Destination)
	}
	if written > 0 {
		e.logf(b.ID, "INFO", "Wrote %d file bind(s) to this host before recreating the container", written)
	}
}

// recordedOwnerLabel and recordedModeLabel say what actually happened to a
// restored file's ownership and permissions, including when the backup recorded
// neither — silence there would read as "reproduced exactly", which is the one
// thing it does not mean.
func recordedOwnerLabel(owner string) string {
	if strings.TrimSpace(owner) == "" {
		return "owner not recorded, left as root"
	}
	return owner
}

func recordedModeLabel(mode string) string {
	if strings.TrimSpace(mode) == "" {
		return "mode not recorded, left owner-only"
	}
	return mode
}

// originalComposeEntry decides whether one archive member is a captured host
// compose file, returning the basename to key it by. Pure, so the rule that
// governs which archive entries can reach a host filesystem is unit-testable.
//
// Two exclusions carry weight. The prefix match keeps every other archive member
// (volumes.tar, the manifest, db dumps) out of a path that WRITES TO THE HOST.
// And the captured .env is skipped because it has its own slot and its own
// restore path — routing it through here would write it a second time under a
// compose-shaped name.
func originalComposeEntry(archiveName string) (base string, ok bool) {
	name := strings.TrimPrefix(archiveName, "./")
	if !strings.HasPrefix(name, originalComposeArchivePrefix) {
		return "", false
	}
	// The remainder must be a plain filename directly in the directory. The
	// DIRECTORY entry itself ("config/original-compose/") has an empty remainder
	// — and filepath.Base would happily turn it into "original-compose", a
	// plausible-looking name that would then be written to the host. A nested
	// path is not something capture ever produces, so it is refused too rather
	// than flattened into a basename.
	rest := strings.TrimPrefix(name, originalComposeArchivePrefix)
	if rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	if rest == stackEnvArchiveName || rest == "." || rest == ".." {
		return "", false
	}
	return rest, true
}

// originalComposeFromArchive returns the GENUINE host compose file(s) captured
// at backup time (F57), keyed by their archive basename.
//
// Listed rather than named: the archive may hold prefixed duplicates
// (archiveComposeName) and whatever filenames the project actually used, so
// there is no fixed set to ask for. The captured .env is deliberately excluded —
// it has its own slot and its own restore path (stackEnvFromArchive).
//
// One streaming pass, and a miss is not an error: most backups carry none.
func (e *Engine) originalComposeFromArchive(ctx context.Context, b *store.Backup, source string) map[string][]byte {
	out := map[string][]byte{}
	// The caller's ctx: this is a full pass over the archive, so a cancelled
	// restore must not keep decrypting through it.
	_ = e.streamArchive(ctx, b, source, func(tr *tar.Reader, hdr *tar.Header) (bool, error) {
		base, ok := originalComposeEntry(hdr.Name)
		if !ok {
			return true, nil
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return false, err
		}
		if len(data) > 0 {
			out[base] = data
		}
		return true, nil
	})
	return out
}

// streamArchive decrypts+decompresses a backup and invokes fn for each tar
// entry. fn returns (keepGoing, error).
func (e *Engine) streamArchive(ctx context.Context, b *store.Backup, source string, fn func(*tar.Reader, *tar.Header) (bool, error)) error {
	// openVerified picks an integrity-checked copy (the chosen source first, else
	// local → offsite), so the destructive restore only begins once a good copy
	// is confirmed.
	rc, err := e.openVerified(ctx, b, source)
	if err != nil {
		return err
	}
	defer rc.Close()

	ak, algo, err := e.archiveCodec(b)
	if err != nil {
		return err
	}
	// Handing ctx to the backend is not enough on its own: a local copy is an
	// os.File, which does not watch a context, so a cancelled restore would keep
	// decrypting the whole archive to the end. ctxReader stops the read itself,
	// and the check below stops the walk between members — so a cancel lands
	// within one buffer either way, which is what "stops at the next safe point"
	// has to mean for the longest step in a restore.
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(crypto.Decrypt(pw, ctxReader(ctx, rc), ak)) }()
	zr, err := newDecompressReader(pr, algo)
	if err != nil {
		return err
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	for {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		cont, err := fn(tr, hdr)
		if err != nil {
			return err
		}
		if !cont {
			return nil
		}
	}
}

// importDatabase pipes a dump back into the target container's native client,
// then VERIFIES what the client actually reported: a zero exit code is not
// proof the dump applied (psql/mysql continue past failed statements), so the
// output is classified and any real failure fails the restore — a database
// restored with missing constraints must never be reported as success.
func (e *Engine) importDatabase(ctx context.Context, cli *client.Client, logID, targetID, engine string, r io.Reader, recorded *DBDump, embedded *EmbeddedDump) error {
	cmd, err := importCommand(engine, embedded)
	if err != nil {
		return err
	}
	// Tally what the dump DECLARES as it streams past (no extra pass), so the
	// import can be checked against it afterwards.
	scan := newDumpScannerFor(engine, r)
	out, execErr := dockercli.ExecStdinCapture(ctx, cli, targetID, cmd, scan)
	outcome := ClassifyImportOutput(engine, out)
	if execErr != nil {
		// Non-zero exit: report it, enriched with the classified detail when the
		// client managed to say something useful before dying.
		if !outcome.OK() {
			return fmt.Errorf("%w — failing statements: %s", execErr, outcome.Summary())
		}
		return execErr
	}
	if !outcome.OK() {
		e.logf(logID, "ERR", "Database import reported %d failing statement(s) despite a clean exit — the restored database is INCOMPLETE: %s", len(outcome.Fatal), outcome.Summary())
		return fmt.Errorf("the %s dump did not fully apply (%d failing statement(s)): %s", engine, len(outcome.Fatal), outcome.Summary())
	}
	if outcome.Benign > 0 {
		e.logf(logID, "INFO", "Database import applied cleanly (%d harmless \"already exists\" notice(s) from the roles/globals preamble)", outcome.Benign)
	}
	// A clean exit and no error output still does NOT prove the dump applied —
	// a stream that ends early produces neither. Ask the restored cluster what
	// it actually has and compare with what the dump declared.
	// F87: prefer what the dump DECLARED AT CAPTURE over what we just re-derived.
	// The recorded numbers were taken while the source database was still there,
	// so they describe the dump as it should be — if the stored dump has since
	// been truncated in storage, the streamed tally would agree with the damaged
	// copy and quietly lower the bar it is checked against.
	return e.verifyImportCompleteness(ctx, cli, logID, targetID, engine, expectedFor(recorded, scan.Expect()))
}

// expectedFor picks the contract a restore is held to: the manifest's recorded
// fingerprint when this backup has one (F87), otherwise the tally taken from the
// stream (the only option for backups written before it existed).
//
// The recorded SHA-256 is deliberately NOT enforced here — a per-database subset
// restore legitimately feeds only part of the stored dump to the client, so a
// mismatch would be normal. Storage integrity is checked where it belongs, in
// the verification walk over the whole stored entry.
func expectedFor(recorded *DBDump, streamed DumpExpect) DumpExpect {
	if recorded == nil || !recorded.hasDumpContract() {
		return streamed
	}
	return DumpExpect{
		PrimaryKeys: recorded.DumpPrimaryKeys,
		ForeignKeys: recorded.DumpForeignKeys,
		// F99: the other engines' recorded expectations. MySQL's tables come from
		// the dump text (so the streamed value would agree with a damaged copy —
		// the recorded one is the honest bar); MongoDB's and Redis's could only
		// ever have come from capture, since their dumps declare nothing.
		Tables:      recorded.DumpTables,
		Rows:        recorded.DumpRows,
		Collections: recorded.DumpCollections,
		Keys:        recorded.DumpKeys,
		// F150: how many of those keys could expire on their own.
		VolatileKeys: recorded.DumpVolatileKeys,
		// A backup that recorded constraints but no completion marker cannot exist
		// after F87 (capture refuses to store one), so a recorded dump is complete
		// by construction. Carry the streamed observation anyway: if THIS read was
		// cut short, that is a real problem worth surfacing.
		Complete:   recorded.DumpComplete && streamed.Complete,
		HeaderSeen: streamed.HeaderSeen,
		Bytes:      streamed.Bytes,
	}
}

// dbDumpForEntry finds the manifest record for an archive entry, so a restore is
// checked against what capture wrote down. nil for an entry with no record.
func dbDumpForEntry(man *Manifest, entry string) *DBDump {
	if man == nil {
		return nil
	}
	want := strings.TrimPrefix(entry, "db/")
	for i := range man.Databases {
		if strings.TrimPrefix(man.Databases[i].Path, "db/") == want {
			return &man.Databases[i]
		}
	}
	return nil
}

// verifyImportCompleteness confirms the restored database really carries what
// the dump declared. A shortfall fails the restore.
//
// Every engine is now covered (F99), each by the cheapest invariant its own dump
// format honestly offers: Postgres by primary/foreign key count, MySQL by table
// count, MongoDB by collection count, Redis by key count. Before this, a
// truncated MySQL dump imported "successfully" in exactly the way the Postgres
// one did in the incident that motivated the original check.
func (e *Engine) verifyImportCompleteness(ctx context.Context, cli *client.Client, logID, targetID, engine string, exp DumpExpect) error {
	if engine == "postgres" {
		return e.verifyPostgresImport(ctx, cli, logID, targetID, exp)
	}
	want, checkable := expectationFor(engine, exp)
	if !checkable {
		return nil // an empty database, or a backup taken before this was recorded
	}
	cmd := dbCountCmd(engine)
	if cmd == nil {
		return nil
	}
	out, err := dockercli.ExecCapture(ctx, cli, targetID, cmd)
	if err != nil {
		// Never fail a good restore because the CHECK couldn't run — say so
		// instead, so the gap is visible rather than silently assumed fine.
		e.logf(logID, "WARN", "Could not verify the restored %s is complete (%v) — the import reported success; check its contents by hand if this backup matters", engine, err)
		return nil
	}
	actual, ok := parseNameCount(string(out), want.metric)
	if !ok {
		e.logf(logID, "WARN", "Could not read a %s count from the restored %s — the import reported success; check its contents by hand if this backup matters", want.noun, engine)
		return nil
	}
	// F150: Redis is judged differently, because a shortfall there is usually the
	// mechanism working rather than a fault — it drops keys whose time-to-live has
	// passed as it loads a snapshot. Holding it to the ordinary rule made a
	// day-old backup of a cache or task broker unrestorable.
	if engine == "redis" {
		fail, note := redisImportVerdict(exp, actual)
		if fail != nil {
			e.logf(logID, "ERR", "Database restore verification FAILED: %v", fail)
			return fail
		}
		if note != "" {
			e.logf(logID, "WARN", "%s", note)
		} else {
			e.logf(logID, "INFO", "Restored redis verified complete: %d key(s) — matches the snapshot", actual)
		}
		return nil
	}
	if verr := VerifyImportCounts(engine, exp, actual); verr != nil {
		e.logf(logID, "ERR", "Database restore verification FAILED: %v", verr)
		return verr
	}
	e.logf(logID, "INFO", "Restored %s verified complete: %d %s — matches the dump", engine, actual, want.noun)
	return nil
}

// verifyPostgresImport is the original constraint-count check, unchanged.
func (e *Engine) verifyPostgresImport(ctx context.Context, cli *client.Client, logID, targetID string, exp DumpExpect) error {
	if exp.PrimaryKeys == 0 && exp.ForeignKeys == 0 {
		return nil
	}
	out, err := dockercli.ExecCapture(ctx, cli, targetID, pgConstraintCountCmd())
	if err != nil {
		e.logf(logID, "WARN", "Could not verify the restored schema is complete (%v) — the import reported success; check the database's constraints by hand if this backup matters", err)
		return nil
	}
	pk, fk := parsePGConstraintCounts(string(out))
	if verr := VerifyImportCompleteness(exp, pk, fk); verr != nil {
		e.logf(logID, "ERR", "Database restore verification FAILED: %v", verr)
		return verr
	}
	e.logf(logID, "INFO", "Restored schema verified complete: %d primary key(s), %d foreign key(s) — matches the dump", pk, fk)
	return nil
}

// importCommand returns the native client invocation that ingests a dump for an
// engine, on stdin.
// pgClientOpts renders the -h/-U a psql invocation needs to reach an
// application's BUNDLED PostgreSQL (F126).
//
// The default is the official image's shape: a `postgres` superuser over TCP,
// with POSTGRES_USER supplied by the environment. A bundled server frequently
// has neither — one real application carries no POSTGRES_* variables and no
// `postgres` role, so the default authenticates as a role that does not exist
// and the import fails on a healthy application. What such an image does have is
// the application's own role and a socket that trusts local connections: the
// same pair the dump was taken with.
//
// TCP stays the default deliberately. During first-init the official image runs
// a TRANSIENT server on a socket, and connecting there would land the import on
// the wrong instance; a profile that declares a socket is opting out of that
// concern for its own image, where the socket is the real server's.
func pgClientOpts(d *EmbeddedDump) string {
	host := "-h 127.0.0.1"
	if dir := pgSocketDir(d); dir != "" {
		host = "-h '" + shellEscape(dir) + "'"
	}
	user := `-U "${POSTGRES_USER:-postgres}"`
	if d != nil {
		if u := strings.TrimSpace(d.User); u != "" {
			user = "-U '" + shellEscape(u) + "'"
		}
	}
	return host + " " + user
}

func importCommand(engine string, d *EmbeddedDump) ([]string, error) {
	var cmd []string
	// Connect over TCP (127.0.0.1) so the dump lands on the real server, not the
	// transient first-init instance — unless an embedded profile named its own
	// socket, in which case that IS the real server (see pgClientOpts).
	switch engine {
	case "postgres":
		cmd = []string{"/bin/sh", "-c", "psql " + pgClientOpts(d) + " -d postgres"}
	case "mysql":
		// Import into the real server (not the first-init socket instance), trying,
		// in order: root over TCP with the password (official), root over TCP with
		// NO password (linuxserver/mariadb, which ignores the root password), the
		// application user over TCP, root over the LOCAL SOCKET with the password,
		// then root over the socket via unix_socket (no password). Mirrors WaitForDB
		// so any DB that WaitForDB reports ready can also be imported. The password
		// is passed via MYSQL_PWD, never on the argv.
		cmd = []string{"/bin/sh", "-c", "" +
			"CLI=mysql; command -v mariadb >/dev/null 2>&1 && CLI=mariadb; " +
			"RP=\"${MYSQL_ROOT_PASSWORD:-$MARIADB_ROOT_PASSWORD}\"; " +
			"AU=\"${MYSQL_USER:-$MARIADB_USER}\"; AP=\"${MYSQL_PASSWORD:-$MARIADB_PASSWORD}\"; " +
			"if [ -n \"$RP\" ] && MYSQL_PWD=\"$RP\" \"$CLI\" -h127.0.0.1 -P3306 -uroot -e 'SELECT 1' >/dev/null 2>&1; then " +
			"exec env MYSQL_PWD=\"$RP\" \"$CLI\" --force -h127.0.0.1 -P3306 -uroot; " +
			"elif \"$CLI\" -h127.0.0.1 -P3306 -uroot -e 'SELECT 1' >/dev/null 2>&1; then " +
			"exec \"$CLI\" --force -h127.0.0.1 -P3306 -uroot; " +
			"elif [ -n \"$AU\" ] && MYSQL_PWD=\"$AP\" \"$CLI\" -h127.0.0.1 -P3306 -u\"$AU\" -e 'SELECT 1' >/dev/null 2>&1; then " +
			"exec env MYSQL_PWD=\"$AP\" \"$CLI\" --force -h127.0.0.1 -P3306 -u\"$AU\"; " +
			"elif [ -n \"$RP\" ] && MYSQL_PWD=\"$RP\" \"$CLI\" -uroot -e 'SELECT 1' >/dev/null 2>&1; then " +
			"exec env MYSQL_PWD=\"$RP\" \"$CLI\" --force -uroot; " +
			"elif \"$CLI\" -uroot -e 'SELECT 1' >/dev/null 2>&1; then exec \"$CLI\" --force -uroot; " +
			"else exec env MYSQL_PWD=\"$RP\" \"$CLI\" --force -h127.0.0.1 -P3306 -uroot; fi"}
	case "mongodb":
		// Auth parity with the other engines (F9): a fresh-initialized mongo has
		// auth enabled iff a root user was configured, so restore with those root
		// creds; otherwise connect without auth. Credentials are read from the
		// container's OWN environment in-shell (never hardcoded), mirroring the
		// MySQL restore. --drop replaces existing data with the dump's.
		cmd = []string{"/bin/sh", "-c", "" +
			`RU="${MONGO_INITDB_ROOT_USERNAME:-$MONGODB_ROOT_USER}"; ` +
			`RP="${MONGO_INITDB_ROOT_PASSWORD:-$MONGODB_ROOT_PASSWORD}"; ` +
			`if [ -z "$RU" ] && [ -n "$RP" ]; then RU=root; fi; ` +
			`if [ -n "$RU" ]; then exec mongorestore --host 127.0.0.1 --archive --drop --username="$RU" --password="$RP" --authenticationDatabase=admin; fi; ` +
			`exec mongorestore --host 127.0.0.1 --archive --drop`}
	default:
		return nil, fmt.Errorf("unknown db engine %q", engine)
	}
	return cmd, nil
}

func dbEngineForEntry(man *Manifest, entry string) string {
	for _, d := range man.Databases {
		if "db/"+strings.TrimPrefix(d.Path, "db/") == entry {
			return d.Engine
		}
	}
	// Fall back to extension.
	switch {
	case strings.HasSuffix(entry, ".archive"):
		return "mongodb"
	case strings.HasSuffix(entry, ".rdb"):
		return "redis"
	default:
		return "postgres"
	}
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// The #14/#21 resolution, encoded (#21 restore half).
//
// Report #2 asked for permission normalisation on restore. R3 §4 then proved by
// controlled experiment why that must never happen: a my.cnf holding
// skip_grant_tables=ON was inert ONLY because mode 0777 made MariaDB reject it as
// world-writable. At mode 644 the same file is honoured and a user that does not
// exist, supplying no password, executes queries. Two of the three obvious
// "fixes" — chmod 644 and adding :ro — open that hole.
//
// DockBack's answer is its standing never-normalise policy: modes are recorded
// and reproduced, never adjusted (F147), and mount flags are never added. That
// leaves exactly one mutation a restore performs on restored files — aligning
// OWNERSHIP so the application can write its own data (F117/F184) — and this is
// where that one mutation learns to refuse.
//
// Ownership is not the mode, so a chown cannot activate the world-writable case
// above. It can activate the ownership-shaped analogue: a configuration file the
// application could not read, and therefore ignored, becomes readable. Same
// class, same consequence, and the same answer — leave it and say so.

// configFileGlobs are the names that make a directory worth refusing to touch.
// Deliberately the extensions a server SCANS a directory for, not every file that
// might be configuration: this decides whether to withhold a fix, so a broad
// pattern would withhold it from data directories that need it.
var configFileGlobs = []string{"*.cnf", "*.conf", "*.ini"}

// configScanDepth bounds the probe. A scanned configuration directory holds its
// files at the top; the extra level is slack for a conf.d inside a data dir.
const configScanDepth = 2

// holdDuplicateTargetConfigs removes from the alignment list any path whose host
// directory this container mounts more than once AND which actually holds
// configuration files.
//
// Both halves matter. Without the duplicate check it would refuse to fix
// ownership on ordinary configuration, which is most of what needs fixing. Without
// the configuration check it would refuse on any doubly-mounted data directory,
// leaving applications unable to write.
//
// When it does refuse, it refuses EVERY destination of that source, not just the
// one that looks like configuration: they are one directory, so chowning it
// through the data path changes exactly the same inodes as chowning it through the
// config path. Holding one and not the other would be a guard in name only.
func (e *Engine) holdDuplicateTargetConfigs(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, targetID string, paths []string) []string {
	if len(paths) == 0 {
		return paths
	}
	dupes := duplicateMountTargets(man.MountedBinds)
	if len(dupes) == 0 {
		return paths
	}

	suspect := map[string]bool{}
	for _, dests := range dupes {
		for _, d := range dests {
			suspect[d] = true
		}
	}
	candidates := make([]string, 0, len(paths))
	for _, p := range paths {
		if suspect[normalizeMountDest(p)] {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return paths
	}

	hasConfig, err := e.configBearingPaths(ctx, cli, targetID, candidates)
	if err != nil {
		// Could not look. The safe reading of "unknown" here is the cautious one:
		// these paths are doubly mounted, which is the shape the experiment
		// showed is dangerous, and not aligning them costs write permission the
		// operator can grant — where guessing wrong the other way costs an
		// authentication bypass.
		e.logf(b.ID, "WARN", "Could not check whether the doubly-mounted path(s) hold configuration (%v) — leaving them untouched rather than risk making an ignored configuration file take effect", err)
		hasConfig = map[string]bool{}
		for _, c := range candidates {
			hasConfig[normalizeMountDest(c)] = true
		}
	}

	keep, held := splitHeldConfigDuplicates(paths, dupes, hasConfig)
	for _, p := range held {
		e.logf(b.ID, "WARN", "left %s untouched — it is mounted at multiple destinations and ownership changes can make previously-ignored configuration take effect", p)
	}
	return keep
}

// splitHeldConfigDuplicates is the decision, separated from the looking so it can
// be tested without a daemon.
//
// hasConfig is keyed by destination and answers only "does this path hold
// configuration files". Promotion to the whole source's destination set happens
// here, where the mount topology is known.
func splitHeldConfigDuplicates(paths []string, dupes map[string][]string, hasConfig map[string]bool) (keep, held []string) {
	hold := map[string]bool{}
	for _, dests := range dupes {
		bearing := false
		for _, d := range dests {
			if hasConfig[d] {
				bearing = true
				break
			}
		}
		if !bearing {
			continue
		}
		for _, d := range dests {
			hold[d] = true
		}
	}
	for _, p := range paths {
		if hold[normalizeMountDest(p)] {
			held = append(held, p)
			continue
		}
		keep = append(keep, p)
	}
	return keep, held
}

// configBearingPaths asks the restored container which of these directories
// actually contain configuration files.
//
// Asked of the container rather than read from the manifest because it is the
// state AFTER extraction that a server will scan, and that is what the decision
// is about.
func (e *Engine) configBearingPaths(ctx context.Context, cli *client.Client, targetID string, paths []string) (map[string]bool, error) {
	var script strings.Builder
	script.WriteString("for d in")
	for _, p := range paths {
		script.WriteString(" '" + shellEscape(p) + "'")
	}
	script.WriteString(`; do [ -d "$d" ] || continue; if find "$d" -maxdepth `)
	fmt.Fprintf(&script, "%d", configScanDepth)
	script.WriteString(` -type f \( `)
	for i, g := range configFileGlobs {
		if i > 0 {
			script.WriteString(" -o ")
		}
		script.WriteString("-name '" + shellEscape(g) + "'")
	}
	script.WriteString(` \) 2>/dev/null | head -n 1 | grep -q .; then printf 'CONFIG|%s\n' "$d"; fi; done`)

	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := dockercli.ExecCapture(cctx, cli, targetID, []string{"/bin/sh", "-c", script.String()})
	if err != nil {
		return nil, err
	}
	found := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "CONFIG|")
		if !ok || rest == "" {
			continue
		}
		found[normalizeMountDest(rest)] = true
	}
	return found, nil
}
