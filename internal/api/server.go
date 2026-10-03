// Package api is DockBack's HTTP layer: single-admin auth, the REST API for
// nodes/backups/restore/settings, live log streaming (SSE), health and
// Prometheus metrics, and serving the embedded React UI (PLAN §6.9, §9.11-§9.12).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dockback/internal/backup"
	"dockback/internal/config"
	"dockback/internal/crypto"
	"dockback/internal/dockercli"
	"dockback/internal/egress"
	"dockback/internal/notify"
	"dockback/internal/store"
)

// Server wires together all dependencies and exposes an http.Handler.
type Server struct {
	cfg    *config.Config
	store  *store.Store
	reg    *dockercli.Registry
	engine *backup.Engine
	bcast  *broadcaster

	// recoveryTool is the embedded dockback-recover.py served for the "restore
	// without DockBack" recovery kit (F35). recoveryToolSHA is its hex SHA-256 so
	// the recovery sheet can fingerprint the exact script this build ships — a real,
	// verifiable value rather than a hardcoded one.
	recoveryToolName string
	recoveryTool     []byte
	recoveryToolSHA  string

	// machines caches per-node hardware probes (F105). Each probe spawns a
	// short-lived container on the node, so it is never run implicitly — only
	// while an operator has that node's Machine page open, and at most once per
	// machineTTL, with concurrent callers collapsed onto one probe.
	machines *machineCache

	// fsUsage caches free-space readings for the restore dialog's capacity panel
	// (#37), so a debounced refetch does not mean a sidecar per keystroke.
	fsUsage *fsUsageCache

	// guard throttles/locks out abusive logins and bounds argon2 cost (§10.1).
	guard *loginGuard

	// decoyHash is a fixed argon2id hash verified against when a login names a
	// non-existent account, so timing never reveals valid usernames (§10.1).
	decoyHash string

	// backupSem caps concurrent backups across the whole fleet; nodeSems caps
	// them PER node so one busy node can't starve the others (PLAN §4.13/§9.9).
	// Both are resizable-at-runtime counting semaphores so the caps can be tuned
	// in-app without a restart (F29).
	backupSem  *dynSem
	perNodeCap int
	nodeSemMu  sync.Mutex
	nodeSems   map[string]*dynSem

	// jobs tracks queued + in-flight backups so they can be canceled by the user.
	jobMu sync.Mutex
	jobs  map[string]*backupJob

	// createMu serializes the manual-backup dedup check with its enqueue, so a
	// double-click can't slip two identical jobs past activeBackup (the queue is
	// otherwise only consulted, not reserved, between check and enqueue).
	createMu sync.Mutex

	// mirrorInFlight guards on-demand "mirror existing backup" runs so the same
	// backup can't be mirrored twice concurrently (a wasteful re-upload race).
	mirrorInFlight sync.Map // backupID -> struct{}

	// backfill tracks a per-destination "backfill existing history" sweep (F51):
	// one at a time per destination, with live progress for the GET status endpoint.
	backfillMu sync.Mutex
	backfills  map[string]*backfillState // destID -> progress (lazily created)

	// F77: layout-migration job progress (one fleet-wide job at a time).
	migrateMu     sync.Mutex
	migrateLayout migrateLayoutState

	// In-flight cancelable restores, keyed by the run id the UI streams their
	// log on (backup id / "stack:<project>" / "node:<id>").
	restoreMu sync.Mutex
	restores  map[string]*restoreRun

	// runLogTick counts persisted run-log lines so we trim (bound growth) only
	// every so often rather than on every line (F8).
	runLogTick atomic.Uint64

	// Fleet-scale job queue (PLAN §4.13): a priority queue drained by a single
	// dispatcher into the global+per-node concurrency caps, so interactive backups
	// outrank scheduled ones and a nightly window is staggered instead of spawning
	// thousands of goroutines at once.
	queueMu  sync.Mutex
	queue    []*queuedJob
	queueSig chan struct{} // buffered(1) wake for the dispatcher (enqueue/completion)

	// rotating holds the dispatcher while the master key is being rotated (F16),
	// so no backup can be written with the outgoing key after the re-wrap has
	// already passed it by.
	rotating atomic.Bool
	queueSeq int64 // FIFO tiebreak

	// Background-refreshed node inventory cache so the dashboard reads an
	// instant snapshot and never blocks on Docker latency (PLAN §4.13). The
	// snapshot is event-driven (a per-node Docker event-stream watcher triggers a
	// debounced refresh) plus a slow periodic reconcile, and is persisted in
	// SQLite so it is instant on startup and survives restarts.
	statMu sync.RWMutex
	stats  map[string]*nodeStat

	// watchers holds the cancel func for each node's event-stream watcher
	// goroutine, so a removed node's watcher is stopped (PLAN §4.13).
	watchMu  sync.Mutex
	watchers map[string]context.CancelFunc

	metrics metricsState

	// restartCh signals main to shut down cleanly so the container restarts
	// (restart: unless-stopped) — used to apply a staged app-config restore.
	restartCh chan struct{}

	// notifier delivers backup/verification notifications (PLAN §4.10).
	notifier *notify.Dispatcher

	// critLast records the last time a low-RPO backup was enqueued per critical
	// database, so the tick doesn't re-queue before the prior dump materializes
	// into a backup row (PLAN §9.7).
	critMu   sync.Mutex
	critLast map[string]time.Time
	// critPaused tracks which critical DBs have their low-RPO auto-backups paused
	// by the circuit-breaker (repeated verification failures), so pause/resume is
	// logged once per transition (PLAN §9.7). Guarded by critMu.
	critPaused map[string]bool

	// autosnapLast records the last time an event-triggered "backup before change"
	// snapshot was enqueued per container, so a burst of destructive events
	// (kill+die+destroy of one recreate) coalesces into a single snapshot (F7).
	autosnapMu   sync.Mutex
	autosnapLast map[string]time.Time

	// recentOOM records the last time a container emitted an "oom" event, so the
	// die (exit 137) that Docker fires immediately after is recognised as the same
	// kill and raises a single OOM notification instead of OOM + crash (F24).
	crashMu   sync.Mutex
	recentOOM map[string]time.Time

	// locks enforces stack-exclusive operations: a backup never overlaps a restore
	// of the same stack, and the same container is never backed up twice at once
	// (PLAN §9.10).
	locks *opLocks
	// egressAudit records hosts the outbound allow-list WOULD refuse, while audit
	// mode observes instead of enforcing (F207).
	egressAudit *egressAuditRecorder
}

// RestartRequested returns a channel that fires when the app should restart
// (e.g. to apply a staged application-config restore). main selects on it.
func (s *Server) RestartRequested() <-chan struct{} { return s.restartCh }

// requestRestart asks for a graceful restart (non-blocking; coalesced).
func (s *Server) requestRestart() {
	select {
	case s.restartCh <- struct{}{}:
	default:
	}
}

// nodeStat is one node's cached inventory snapshot (PLAN §4.13). It holds the
// dashboard Summary plus the full container/stack list so the container-list
// page reads the cache instead of re-listing Docker on every load.
type nodeStat struct {
	Summary    *dockercli.NodeSummary
	Containers []*dockercli.Container
	Stacks     []*dockercli.Stack
	Reachable  bool
	Error      string
	UpdatedAt  int64
	// HostKeyChanged (F67): the last connect was REFUSED on a pin mismatch —
	// shown as its own state, never as a generic "offline". Cleared by a
	// successful connect or a pin reset.
	HostKeyChanged bool
	// Streaming (#40): a bulk transfer holds this node, so the inventory below is
	// the last one taken BEFORE it started rather than a fresh read. Distinct
	// from Reachable=false on purpose — the node is fine, it is busy, and showing
	// it as offline is exactly the wrong thing to tell an operator mid-backup.
	Streaming bool
}

// nodeInvBlob is the JSON shape persisted in SQLite (node_inventory.payload).
type nodeInvBlob struct {
	Summary    *dockercli.NodeSummary `json:"summary"`
	Containers []*dockercli.Container `json:"containers"`
	Stacks     []*dockercli.Stack     `json:"stacks"`
}

// New constructs the API server.
func New(cfg *config.Config, st *store.Store, reg *dockercli.Registry) *Server {
	s := &Server{
		cfg:          cfg,
		store:        st,
		reg:          reg,
		bcast:        newBroadcaster(500),
		guard:        newLoginGuard(st),
		machines:     newMachineCache(), // F105 host probe cache
		fsUsage:      newFSUsageCache(), // #37 restore capacity panel
		backupSem:    newDynSem(cfg.MaxConcurrentBackups),
		perNodeCap:   cfg.MaxConcurrentPerNode,
		nodeSems:     map[string]*dynSem{},
		stats:        map[string]*nodeStat{},
		watchers:     map[string]context.CancelFunc{},
		jobs:         map[string]*backupJob{},
		queueSig:     make(chan struct{}, 1),
		restartCh:    make(chan struct{}, 1),
		critLast:     map[string]time.Time{},
		critPaused:   map[string]bool{},
		autosnapLast: map[string]time.Time{},
		recentOOM:    map[string]time.Time{},
		locks:        newOpLocks(),
	}
	// SSH host-key pinning (F1): give the registry store-backed load/save closures
	// so an SSH node's host key is pinned on first connect and verified thereafter.
	// A throwaway "test-…" node id (Test Connection) is never persisted — a test is
	// not a commitment; real pinning happens on a managed node's first connect.
	// F88: the same trust-on-first-use posture for the volume sidecar IMAGE. A
	// throwaway "test-…" node never pins, for the same reason it never pins a host
	// key — a connection test is not a commitment.
	reg.LoadSidecarPin = func(nodeID string) (string, bool) { return st.GetSidecarPin(nodeID) }
	reg.SaveSidecarPin = func(nodeID, pin string) {
		if strings.HasPrefix(nodeID, "test-") {
			return
		}
		if err := st.SetSidecarPin(nodeID, pin); err != nil {
			return
		}
		log.Printf("pinned the volume sidecar image on node %s (%s)", nodeID, pin)
	}

	reg.LoadHostKey = func(nodeID string) ([]byte, bool) {
		hk, ok := st.GetHostKey(nodeID)
		if !ok {
			return nil, false
		}
		return dockercli.AuthorizedKeyBytes(hk.KeyType, hk.KeyB64), true
	}
	reg.SaveHostKey = func(nodeID string, pub []byte) {
		if strings.HasPrefix(nodeID, "test-") {
			return // a Test Connection must not pin
		}
		keyType, keyB64, fp, ok := dockercli.ParseHostKey(pub)
		if !ok {
			return
		}
		hostport := ""
		if n, err := st.GetNode(nodeID); err == nil {
			hostport = dockercli.SSHHostPort(n.Address)
		}
		_ = st.SetHostKey(store.HostKey{NodeID: nodeID, HostPort: hostport, KeyType: keyType, KeyB64: keyB64, Fingerprint: fp})
	}

	s.engine = backup.New(st, reg, mustStorage(cfg), cfg.EncryptionKey, cfg.WorkDir, s.logSink)
	// F68: tamper-evident audit trail — install the HMAC chainer (keyed by the
	// master key) before anything in the server's lifetime writes an audit row,
	// and record the chain-start anchor on the first run with the feature.
	s.installAuditChain()
	// Shared aggregate offsite upload cap so a fleet-wide window can't saturate the
	// uplink (PLAN §4.13/§9.9). Always-present limiter, tuned live from settings
	// (env value as default) so a UI change survives a restart (F15).
	s.engine.UpLimiter = backup.NewSharedUploadLimiter()
	s.engine.SetUploadLimit(s.uploadMbps())
	// Re-apply the persisted DB-ready timeout override so a value set in the UI
	// takes effect on boot, not only after the next save (F15).
	dockercli.SetDBReadyTimeout(s.dbReadyTimeoutSeconds())
	// Seed the volume-sidecar image from the persisted setting, else the env default
	// (cfg.SidecarImage), so an air-gapped mirror set in the UI or env applies from
	// boot — not only after the next save (F25).
	sidecar := cfg.SidecarImage
	if v, _ := st.GetSetting("backup.sidecar_image", ""); strings.TrimSpace(v) != "" {
		sidecar = strings.TrimSpace(v)
	}
	dockercli.SetSidecarImage(sidecar)
	// Seed the concurrency caps from settings (env value as default) so a value set
	// in the UI survives a restart (F29). nodeSems are created lazily with the
	// current perNodeCap, so setting it before any node runs is enough at boot.
	s.backupSem.setLimit(s.maxConcurrent())
	s.perNodeCap = s.maxConcurrentPerNode()
	// Re-apply the persisted drill boot-wait so a UI value takes effect on boot, not
	// only after the next save (F30). The restore health-gate reads its setting
	// fresh per restore, so it needs no boot seeding.
	dockercli.SetDrillBootWait(s.settingInt("drill.boot_wait_seconds", 45))
	// Re-apply the persisted egress allow-list over the env default so a UI value
	// takes effect on boot, not only after the next save (F39). Empty setting leaves
	// the env-configured policy (installed in config.Load) untouched.
	if v, _ := st.GetSetting("security.egress_allow", ""); strings.TrimSpace(v) != "" {
		egress.Configure(splitCleanList(v))
	}
	// F207: install the would-be-denied recorder and restore the persisted audit
	// mode BEFORE anything can dial. An audit whose mode did not survive a restart
	// would silently begin enforcing a list the operator was still testing.
	s.installEgressAudit()
	// Re-apply the persisted session idle window over the env default, same reason
	// (F203). The absolute lifetime and the password minimum need no boot seeding:
	// both are resolved from settings at the moment they are used, so they are
	// already correct on the first request. The idle window is the exception
	// because the store holds it as process state rather than reading it per
	// request — a restart would otherwise silently revert a tightened window.
	store.SetSessionIdleTTL(time.Duration(s.settingInt("security.session_idle_minutes", int(store.SessionIdleTTL.Minutes()))) * time.Minute)
	// Pluggable notifications (PLAN §4.10): the dispatcher loads the (decrypted)
	// config on each send; the engine fires asynchronously so a slow notifier
	// never blocks or fails a backup.
	s.notifier = notify.New(s.loadNotifyConfig, func(level, msg string) { s.logSink("notify", level, msg) })
	s.engine.Notify = s.engineNotify // severity-routed + throttled operational alerts (PLAN §9.15)
	// Precompute a decoy password hash (same argon2 params as real hashes) so
	// logins for non-existent accounts still pay an identical verify cost —
	// constant-time login, no username-enumeration oracle (§10.1).
	if h, err := crypto.HashPassword(randToken()); err == nil {
		s.decoyHash = h
	} else {
		log.Printf("warning: could not precompute login decoy hash: %v", err)
	}
	// Discard backups orphaned by a previous restart/rebuild mid-run (and any
	// older 'interrupted by restart' rows): they never finished and only confuse
	// the list — the next run re-backs-up cleanly.
	if n, err := st.PurgeInterruptedBackups(); err == nil && n > 0 {
		log.Printf("startup: discarded %d interrupted backup(s)", n)
	}
	// Seal any legacy plaintext node secrets at rest, one-time (F2 / PLAN §3.8).
	s.migrateNodeSecrets()
	s.migrateSchedules()
	return s
}

// migrateNodeSecrets seals any node whose stored secret is still legacy plaintext
// (no NSE1 magic) with the master key, one-time at startup (F2). Idempotent:
// already-sealed and empty secrets are skipped.
func (s *Server) migrateNodeSecrets() {
	nodes, err := s.store.ListNodes()
	if err != nil {
		return
	}
	sealed := 0
	for _, n := range nodes {
		if len(n.SecretEnc) == 0 || isSealedNodeSecret(n.SecretEnc) {
			continue
		}
		n.SecretEnc = SealNodeSecret(n.SecretEnc, s.cfg.EncryptionKey)
		if err := s.store.UpsertNode(n); err == nil {
			sealed++
		}
	}
	if sealed > 0 {
		log.Printf("startup: re-encrypted %d node secret(s) at rest", sealed)
	}
}

// Engine exposes the backup engine (used by main for bootstrap tasks).
func (s *Server) Engine() *backup.Engine { return s.engine }

// Inventory cache tuning (PLAN §4.13). Freshness is driven by the per-node
// Docker event-stream watchers; the periodic full reconcile is a slow safety net
// (and the cadence at which CPU/mem stats are re-sampled), not the primary path.
const (
	reconcileInterval = 20 * time.Second
	eventDebounce     = 1500 * time.Millisecond // coalesce event bursts (e.g. compose up)
	watchBackoffBase  = 3 * time.Second
	watchBackoffMax   = 60 * time.Second
	watchMinUp        = 5 * time.Second // a stream shorter than this counts as a failed connect
)

// StartBackground launches the event-driven node inventory subsystem (PLAN
// §4.13): it loads the persisted cache so the dashboard is instant, starts a
// per-node Docker event-stream watcher (refresh on change, not constant
// polling), and runs a slow periodic reconcile that re-samples stats and catches
// any missed events.
func (s *Server) StartBackground() {
	s.loadInventoryCache() // instant dashboard + survives restart
	go func() {
		s.refreshAll()
		s.manageWatchers()
		t := time.NewTicker(reconcileInterval)
		defer t.Stop()
		for range t.C {
			s.refreshAll()
			s.manageWatchers() // start/stop watchers as nodes are added/removed
		}
	}()
	s.resumeQueuedJobs()       // re-enqueue jobs persisted before a restart (PLAN §9.10)
	go s.dispatchLoop()        // drain the fleet-scale job queue into the caps (PLAN §4.13)
	s.startScheduler()         // global auto-backup schedule (PLAN §4.7)
	s.startRetentionPrune()    // optional scheduled fleet-wide retention sweep (F17)
	s.startNodePurge()         // hard-delete forgotten nodes once their undo window elapses
	s.startAppBackupSchedule() // automatic application (control-plane) backups (F4)
	s.startScrub()             // periodic re-verification of stored backups (PLAN §9.4)
	s.startDrills()            // periodic per-container restore drills into a sandbox (PLAN §9.4)
	s.startStandby()           // periodic cross-node standby rehearsals (F62)
	s.startTestCloneReaper()   // remove expired one-click test clones + their volumes (F219)
	s.startCriticalLoop()      // low-RPO dumps for critical databases (PLAN §9.7)
	s.startAlertMonitor()      // state-based operational alerts (dest full, key not backed up) (PLAN §9.15)
	s.startDigest()            // optional daily summary digest of the last 24h (F15)
	s.startAuditBeacon()       // publish the audit chain head off-host on a cadence (F200)
	// Periodic expired/idle-session sweep so dead rows + their csrf entries don't
	// accumulate (they're also pruned lazily on access). (PLAN §10.2)
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		var lastAuditCheck time.Time // F68: opportunistic daily chain verification
		for range t.C {
			_, _ = s.store.SweepExpiredSessions()
			// F199: expired export tickets. Harmless if they linger (the deadline
			// is checked on redemption), but an abandoned download would otherwise
			// leave a settings row for good.
			s.pruneExportTickets()
			// Prune connection-health transitions older than 90 days (F40) so the
			// table stays small even for a chronically-flapping node.
			_ = s.store.PruneNodeHealth(time.Now().AddDate(0, 0, -90).Unix())
			// Keep the persistent alert inbox bounded (F46) — newest 2000 kept.
			_ = s.store.PruneAlerts(2000)
			// Both of these grow with the OUTSIDE world rather than with the
			// fleet: one lockout row per address that has ever failed a sign-in,
			// one throttle stamp per alert kind per scope. Neither had anything
			// removing it, so on an internet-adjacent deployment the settings
			// table grew forever — and every row of it goes into every
			// application backup.
			s.pruneLockouts()
			s.pruneAlertStamps()
			// F68: verify the audit trail's hash chain once a day; a broken chain
			// raises an integrity-failure alert.
			if time.Since(lastAuditCheck) >= 24*time.Hour {
				lastAuditCheck = time.Now()
				s.auditVerifySweep()
			}
		}
	}()
	// Heartbeat keep-alive tick (PLAN §9.11): sends an interval heartbeat between
	// backups when interval mode is configured (no-op otherwise).
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for range t.C {
			s.notifier.HeartbeatTick()
		}
	}()
}

// nodePurgeGrace is how long a forgotten node's tombstone is retained so the
// client-side undo (~5s) can restore it, with comfortable margin for a slow
// click or clock skew. Only after this does the node's data get hard-deleted.
const nodePurgeGrace = 2 * time.Minute

// startNodePurge periodically finalizes forgotten nodes: any node soft-deleted
// longer ago than nodePurgeGrace is hard-purged along with ALL its associated
// data (backups, credentials, settings, schedule targets).
func (s *Server) startNodePurge() {
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for range t.C {
			s.purgeExpiredNodes()
		}
	}()
}

// purgeExpiredNodes hard-deletes every node whose forget-undo window has elapsed.
// Synchronous per node: DeleteNode is the last step, so a node can't be re-listed
// and double-purged on the next tick.
func (s *Server) purgeExpiredNodes() {
	ids, err := s.store.ListDeletedNodesBefore(time.Now().Add(-nodePurgeGrace).Unix())
	if err != nil {
		return
	}
	for _, id := range ids {
		s.purgeNode(id)
	}
}

// purgeNode permanently removes a forgotten node and EVERYTHING associated with
// it: its backups (from every location — local + offsite), run logs and drill
// results, per-node/per-container settings and policy overrides, schedule
// targets, cached inventory/events, the pinned SSH host key, and finally the node
// row itself (which holds the sealed connection credentials). Never touches the
// remote host (PLAN §5.4) — it only forgets state DockBack stored.
func (s *Server) purgeNode(id string) {
	// Stop using the connection (idempotent — the soft-delete already did this).
	s.reg.Remove(id)
	s.stopWatcher(id)
	s.dropNodeCache(id)

	// Delete every backup of this node from all locations, plus its run log and
	// drill result (DeleteBackup cascades those). Bounded so a hung offsite delete
	// can't block the purge sweep forever.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	// Page the catalog rather than materialising it. Each row carries the manifest,
	// verification and locations JSON blobs, and DeleteArtifacts needs the storage
	// key and locations of the row it is deleting — so the fields cannot be trimmed,
	// only the number held at once. Each pass re-reads from the top because the
	// previous pass deleted its rows.
	deletedBackups := 0
	const purgeBatch = 200
	for {
		backups, err := s.store.ListBackups(id, purgeBatch)
		if err != nil || len(backups) == 0 {
			break
		}
		progressed := false
		for _, b := range backups {
			s.engine.DeleteArtifacts(ctx, b)
			if derr := s.store.DeleteBackup(b.ID); derr == nil {
				deletedBackups++
				progressed = true
			}
		}
		if !progressed {
			// Every row in this batch failed to delete: another pass would fetch the
			// same rows forever. Stop rather than spin.
			break
		}
		if ctx.Err() != nil {
			break
		}
	}

	// Drop this node's targets from every saved schedule.
	s.removeNodeFromSchedules(id)

	// Per-node/per-container settings + policy overrides, then node-level state.
	_ = s.store.PurgeNodeData(id)
	_ = s.store.DeleteNodeInventory(id)
	_ = s.store.DeleteNodeEvents(id)
	_ = s.store.DeleteHostKey(id)
	s.machineForget(id)        // F105: never let a recycled id inherit another machine's hardware
	_ = s.store.DeleteNode(id) // removes the row + its sealed credentials last

	s.logSink("node", "INFO", fmt.Sprintf("Purged forgotten node %s and its data (%d backup(s) removed)", id, deletedBackups))
	_ = s.store.Audit("system", "node.purge", id, fmt.Sprintf("backups_deleted=%d", deletedBackups))
}

// removeNodeFromSchedules drops every target belonging to a node from all saved
// schedules, so a purged node leaves no dangling scheduled targets.
func (s *Server) removeNodeFromSchedules(nodeID string) {
	rows, err := s.store.ListSchedules()
	if err != nil {
		return
	}
	for _, row := range rows {
		sc := scheduleFromRow(row)
		kept := make([]ScheduleTarget, 0, len(sc.Targets))
		for _, t := range sc.Targets {
			if t.NodeID != nodeID {
				kept = append(kept, t)
			}
		}
		if len(kept) == len(sc.Targets) {
			continue // nothing for this node
		}
		sc.Targets = kept
		if newRow, err := sc.toRow(); err == nil {
			newRow.LastRun = row.LastRun // preserve the run baseline
			_ = s.store.UpsertSchedule(newRow)
		}
	}
}

func (s *Server) refreshAll() {
	nodes, err := s.store.ListNodes()
	if err != nil {
		return
	}
	sem := make(chan struct{}, 8) // bounded fan-out across the fleet
	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s.refreshNode(id)
		}(n.ID)
	}
	wg.Wait()
}

// refreshNode runs a FULL reconcile of one node (containers + counts + stats +
// events). A slow or dead node only affects its own entry (PLAN §4.13).
func (s *Server) refreshNode(id string) { s.refreshNodeImpl(id, true) }

// refreshNodeLite runs an event-triggered refresh: it re-lists containers and
// recomputes counts but skips the expensive stats/event sampling, carrying the
// last sampled stats forward so stats stay interval-sampled (PLAN §4.13).
func (s *Server) refreshNodeLite(id string) { s.refreshNodeImpl(id, false) }

// refreshNodeImpl recomputes one node's cached inventory and persists it. When a
// node is unreachable the last-known snapshot is preserved (only flipped to
// not-reachable with the error) so the UI keeps showing the last good list.
func (s *Server) refreshNodeImpl(id string, withStats bool) {
	prev := s.getStat(id)

	// #40: a bulk transfer is running against this node, so do not ask it
	// anything. R5 §6 measured the cost of asking: with a 58 GB stream running
	// through the socket proxy "a plain `docker inspect` call timed out after 120
	// seconds. The proxy itself — a single haproxy in front of the socket — is
	// saturated by the transfer, so all API access to the source is effectively
	// lost for the duration."
	//
	// The poll would therefore not merely be slow: it would fail, mark a healthy
	// node offline, and alert an operator about the machine their backup is
	// currently succeeding on. The last-known inventory is kept and flagged, and
	// a single refresh runs when the transfer ends.
	if s.nodeStreaming(id) {
		s.setStat(id, withStreaming(prev))
		return
	}

	// A node that failed recently is in connection back-off: skip the expensive
	// probe and cheaply keep the last-known snapshot marked offline, so a dead/
	// slow node never starves the bounded fan-out that live nodes share (§4.13).
	if backed, lastErr := s.reg.Backoff(id); backed {
		s.setStat(id, withOffline(prev, lastErr))
		return
	}

	cli, err := s.reg.Get(id)
	if err != nil {
		s.reg.RecordHealth(id, err)
		_ = s.store.SetNodeStatus(id, "offline")
		off := withOffline(prev, err.Error())
		// F67: a pin mismatch is refused deep in the transport — surface it as a
		// DEDICATED critical alert (possible reinstall or man-in-the-middle),
		// deduped per (node, new key) so it fires once per change, not per tick,
		// and stamp the stat so the UI shows "host key changed", not "offline".
		off.HostKeyChanged = false
		var hk *dockercli.HostKeyChangedError
		if errors.As(err, &hk) {
			off.HostKeyChanged = true
			s.alertHostKeyChanged(id, hk)
		}
		s.setStatPersist(id, off)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	cs, stacks, sum, ierr := dockercli.InventorySnapshot(ctx, cli, withStats)
	cancel()
	if ierr != nil {
		s.reg.Drop(id)
		s.reg.RecordHealth(id, ierr)
		_ = s.store.SetNodeStatus(id, "offline")
		s.setStatPersist(id, withOffline(prev, ierr.Error()))
		return
	}

	// Event-driven lite refresh skips stats; carry the last sampled values
	// forward so the dashboard CPU/mem/events don't flicker to zero (§4.13).
	if !withStats && prev != nil && prev.Summary != nil {
		carryStats(sum, prev.Summary)
	}
	s.applyEventCounts(id, sum) // real persisted counts (PLAN §5.4)
	s.reg.RecordHealth(id, nil)
	_ = s.store.SetNodeStatus(id, "healthy")
	s.setStatPersist(id, &nodeStat{
		Summary:    sum,
		Containers: cs,
		Stacks:     stacks,
		Reachable:  true,
		UpdatedAt:  time.Now().Unix(),
	})
	// F19: reconcile declarative dockback.* labels into policy/schedule state.
	// Idempotent (writes only on a real change), so it's cheap on every refresh.
	s.applyLabelPolicies(id, cs)
}

// withOffline returns a stat that keeps prev's last-known inventory but marks the
// node not-reachable with errMsg (so flapping offline doesn't blank the card).
// The host-key-changed flag (F67) is carried forward: back-off ticks replay the
// same refusal, and the flag only clears on a successful connect or a pin reset.
func withOffline(prev *nodeStat, errMsg string) *nodeStat {
	ns := &nodeStat{Reachable: false, Error: errMsg, UpdatedAt: time.Now().Unix()}
	if prev != nil {
		ns.Summary, ns.Containers, ns.Stacks = prev.Summary, prev.Containers, prev.Stacks
		ns.HostKeyChanged = prev.HostKeyChanged
	}
	return ns
}

// withStreaming keeps prev's inventory and marks the node busy rather than
// re-reading it (#40).
//
// Reachable stays TRUE, deliberately. The node is not down — it is answering a
// large request — and the one thing this must never do is turn a running backup
// into an offline alert. A node with no snapshot yet stays unflagged: there is
// nothing to preserve, and its first refresh will happen when the transfer ends.
func withStreaming(prev *nodeStat) *nodeStat {
	if prev == nil {
		return &nodeStat{Streaming: true, UpdatedAt: time.Now().Unix()}
	}
	ns := *prev
	ns.Streaming = true
	return &ns
}

// nodeStreaming reports whether a bulk transfer currently holds this node.
//
// RUNNING work only. A queued backup has reserved nothing and is streaming
// nothing — treating it as busy would pause the dashboard for a job that has not
// started, possibly for the whole length of a nightly window.
func (s *Server) nodeStreaming(nodeID string) bool {
	if nodeID == "" {
		return false
	}
	s.jobMu.Lock()
	for _, j := range s.jobs {
		// cancel is nil until the dispatcher actually starts the job.
		if j != nil && j.nodeID == nodeID && j.cancel != nil {
			s.jobMu.Unlock()
			return true
		}
	}
	s.jobMu.Unlock()

	// A restore streams a whole archive INTO the same proxy, which saturates it
	// exactly as a backup does.
	s.restoreMu.Lock()
	defer s.restoreMu.Unlock()
	for _, r := range s.restores {
		if r != nil && r.nodeID == nodeID {
			return true
		}
	}
	return false
}

// refreshAfterStreaming runs the one refresh a paused node is owed, once the
// transfer that paused it is over (#40).
//
// Checked rather than assumed: a stack backup runs several services against one
// node, and the second one finishing while the third still streams must not
// re-open the polling the third is being protected from.
//
// Asynchronous, and that is not a detail. Both callers are DEFERRED teardowns —
// a backup job unwinding its slots and locks, and a restore deregistering — and
// the refresh they trigger reaches over a network with a 25-second timeout.
// Running it inline would hold a backup slot and a container lock for the length
// of a poll that nothing is waiting on.
func (s *Server) refreshAfterStreaming(nodeID string) {
	if nodeID == "" {
		return
	}
	go func() {
		// A background poll must never be able to take the daemon down with it:
		// this goroutine is started from a deferred teardown, where a panic has no
		// caller left to catch it.
		defer guardPanic("post-transfer refresh", nodeID, nil)
		if s.nodeStreaming(nodeID) {
			return
		}
		s.refreshNodeLite(nodeID)
	}()
}

// alertSidecarChanged raises the F88 critical alert when a node's volume-sidecar
// image no longer matches its pin.
//
// Deduped per (node, presented digest): a NEWLY-changed image alerts once, and a
// nightly schedule replaying the same refusal does not re-alert every run. The
// image runs with read access to the data being backed up, so an unexplained
// change is treated as a possible supply-chain event, not a warning.
func (s *Server) alertSidecarChanged(nodeName, nodeID string, e *dockercli.SidecarDigestChangedError) {
	s.notifyThrottled(notify.KindSidecarChanged, nodeID+"\x00"+e.Got,
		"Volume sidecar image changed on "+nodeName,
		e.Error()+" The backup was refused — the image was NOT run. If you did not change it, treat this as a possible supply-chain compromise and check the image before re-pinning.",
		24*time.Hour)
}

// alertHostKeyChanged raises the F67 critical alert for a refused pin mismatch,
// deduped per (node, presented key) — a NEW key alerts immediately; the same
// mismatch re-alerts only after the long cooldown or once the operator acks.
func (s *Server) alertHostKeyChanged(nodeID string, hk *dockercli.HostKeyChangedError) {
	name := nodeID
	if n, err := s.store.GetNode(nodeID); err == nil && n.Name != "" {
		name = n.Name
	}
	s.notifyThrottled(notify.KindHostKeyChanged, nodeID+"\x00"+hk.NewFP,
		"Host key changed: "+name,
		fmt.Sprintf("The SSH host key presented by %q no longer matches the pinned key (was %s, now %s). DockBack is refusing to connect. If you re-installed or re-keyed this host, reset its pinned key on the node page; otherwise treat this as a possible man-in-the-middle and investigate before trusting the connection.",
			name, hk.OldFP, hk.NewFP),
		24*time.Hour)
}

// carryStats copies interval-sampled fields from the previous summary onto a
// freshly-listed lite summary (PLAN §4.13: stats sampled on an interval).
func carryStats(dst, src *dockercli.NodeSummary) {
	dst.CPUPercent = src.CPUPercent
	dst.MemUsed, dst.MemTotal, dst.MemPercent = src.MemUsed, src.MemTotal, src.MemPercent
	dst.NetRx, dst.NetTx = src.NetRx, src.NetTx
	// Event counts come from the persisted per-node counter (applyEventCounts),
	// not carried/sampled — so they are accurate and uncapped (PLAN §5.4).
}

// todayKey is the local date the per-day event count belongs to.
func todayKey() string { return time.Now().Format("2006-01-02") }

// recordNodeEvent bumps a node's persisted event counter (called by the event
// watcher for each inventory-relevant Docker event). Best-effort.
func (s *Server) recordNodeEvent(id string) {
	_ = s.store.IncrNodeEvents(id, todayKey())
}

// applyEventCounts overlays the real persisted counts onto a node summary so the
// dashboard shows a true running total + today (replacing the daemon's capped
// 256-event log, PLAN §5.4).
func (s *Server) applyEventCounts(id string, sum *dockercli.NodeSummary) {
	if sum == nil {
		return
	}
	if total, today, err := s.store.GetNodeEvents(id, todayKey()); err == nil {
		sum.EventsTotal, sum.EventsToday = total, today
	}
}

func (s *Server) getStat(id string) *nodeStat {
	s.statMu.RLock()
	defer s.statMu.RUnlock()
	return s.stats[id]
}

func (s *Server) setStat(id string, ns *nodeStat) {
	s.statMu.Lock()
	if s.stats == nil {
		s.stats = map[string]*nodeStat{}
	}
	s.stats[id] = ns
	s.statMu.Unlock()
	// Persist a reachability TRANSITION for the connection-health history (F40).
	// Transition-only, so this is a cheap indexed lookup per refresh and never one
	// row per tick. Best-effort — a health-log write must never affect the refresh.
	_ = s.store.RecordNodeHealth(id, ns.Reachable, ns.Error)
	// Push a node.summary delta so the dashboard updates without a full-reload
	// poll (A7). Same data the REST list returns — nothing new is exposed.
	if s.bcast != nil {
		s.bcast.publishEvent("node.summary", nodeSummaryEvent{NodeID: id, Reachable: ns.Reachable, Error: ns.Error, Summary: ns.Summary})
	}
}

// nodeSummaryEvent is the SSE payload for a node's live inventory/health delta.
type nodeSummaryEvent struct {
	NodeID    string                 `json:"node_id"`
	Reachable bool                   `json:"reachable"`
	Error     string                 `json:"error,omitempty"`
	Summary   *dockercli.NodeSummary `json:"summary,omitempty"`
}

// backupStatusEvent is the SSE payload for a backup lifecycle transition.
type backupStatusEvent struct {
	BackupID string `json:"backup_id"`
	NodeID   string `json:"node_id,omitempty"`
	Status   string `json:"status"`
}

// runDoneEvent is the structured "this run is over" signal (#N10).
//
// Every console used to learn a run's outcome by matching the TEXT of its log
// lines, and that produced three separate defects in one investigation: a
// pattern ending with `restored$` closed the stream after `[1/5] Service "db"
// restored`, a pattern reading `stack .* restored` reported a partial failure as
// success, and 27 lines logged at "ERROR" reached nothing at all. Every one of
// them was a message this codebase can reword at any time.
//
// So the outcome is now said once, structurally, on the channel that already
// carries named deltas. The prose stays exactly as it is — an operator still
// reads it — but nothing DECIDES on it any more.
type runDoneEvent struct {
	// RunID is what the console is following: a backup id, "stack:<project>" or
	// "node:<id>".
	RunID string `json:"run_id"`
	// Outcome is "ok", "failed" or "canceled". Canceled is its own answer, not a
	// failure: the operator chose it, and a destructive run stopped part-way
	// needs saying differently from one that broke.
	Outcome string `json:"outcome"`
	// Message is the same sentence the run log carries, so a console can show it
	// without re-deriving one.
	Message string `json:"message"`
}

// Run outcomes. Named so a caller cannot invent a fourth spelling.
const (
	runOutcomeOK       = "ok"
	runOutcomeFailed   = "failed"
	runOutcomeCanceled = "canceled"
)

// publishRunDone announces that a followed run has ended.
//
// Unreplayed, like every named delta — a console that connects after the fact
// still falls back to the log lines it already reads. But not DROPPED on the
// same terms as a log line: this is the one event a console cannot reconstruct,
// and the fallback it lands in is the text matching the code comments elsewhere
// describe as unreliable. A momentarily-full subscriber gets a grace period.
func (s *Server) publishRunDone(runID, outcome, message string) {
	if s.bcast == nil || runID == "" {
		return
	}
	s.bcast.fanoutImportant(sseMsg{
		event: "run.done",
		data:  mustJSON(runDoneEvent{RunID: runID, Outcome: outcome, Message: message}),
	})
}

// pushBackupStatus streams a backup.status delta so the Backups/Node views reflect
// a start/finish within a second instead of on the next poll (A7).
func (s *Server) pushBackupStatus(backupID, nodeID, status string) {
	if s.bcast != nil {
		s.bcast.publishEvent("backup.status", backupStatusEvent{BackupID: backupID, NodeID: nodeID, Status: status})
	}
}

// setStatPersist updates the in-memory cache and writes it through to SQLite so
// the snapshot survives a restart (PLAN §4.13). Persistence is best-effort.
func (s *Server) setStatPersist(id string, ns *nodeStat) {
	s.setStat(id, ns)
	payload, err := json.Marshal(nodeInvBlob{Summary: ns.Summary, Containers: ns.Containers, Stacks: ns.Stacks})
	if err != nil {
		return
	}
	if err := s.store.SaveNodeInventory(id, payload, ns.Reachable, ns.Error, ns.UpdatedAt); err != nil {
		log.Printf("inventory cache: persist node %s: %v", id, err)
	}
}

// loadInventoryCache primes the in-memory cache from SQLite at startup so the
// dashboard and container lists are instant and survive restarts (PLAN §4.13).
func (s *Server) loadInventoryCache() {
	rows, err := s.store.ListNodeInventories()
	if err != nil {
		return
	}
	s.statMu.Lock()
	defer s.statMu.Unlock()
	for _, r := range rows {
		var blob nodeInvBlob
		if err := json.Unmarshal(r.Payload, &blob); err != nil {
			continue
		}
		s.stats[r.NodeID] = &nodeStat{
			Summary:    blob.Summary,
			Containers: blob.Containers,
			Stacks:     blob.Stacks,
			Reachable:  r.Reachable,
			Error:      r.Error,
			UpdatedAt:  r.UpdatedAt,
		}
	}
}

// manageWatchers reconciles the set of running event-stream watchers with the
// current node list: it starts a watcher for any new node and stops the watcher
// for any removed node (PLAN §4.13).
func (s *Server) manageWatchers() {
	nodes, err := s.store.ListNodes()
	if err != nil {
		return
	}
	live := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		live[n.ID] = struct{}{}
	}
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	for id := range live {
		if _, ok := s.watchers[id]; !ok {
			ctx, cancel := context.WithCancel(context.Background())
			s.watchers[id] = cancel
			go s.watchNode(ctx, id)
		}
	}
	for id, cancel := range s.watchers {
		if _, ok := live[id]; !ok {
			cancel()
			delete(s.watchers, id)
		}
	}
}

// stopWatcher cancels and forgets a node's event watcher (on node removal).
func (s *Server) stopWatcher(id string) {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	if cancel, ok := s.watchers[id]; ok {
		cancel()
		delete(s.watchers, id)
	}
}

// watchNode subscribes to a node's Docker event stream and triggers a debounced
// lite refresh on every inventory-changing event, reconnecting with back-off if
// the stream drops (PLAN §4.13: refresh on events, not constant polling). One
// debouncer goroutine coalesces bursts (e.g. a compose up) into a single refresh.
func (s *Server) watchNode(ctx context.Context, id string) {
	sig := make(chan struct{}, 1)
	trigger := func() {
		select {
		case sig <- struct{}{}:
		default: // a refresh is already pending; coalesce
		}
	}

	// Debouncer: after a signal, wait for eventDebounce of quiet (resetting on
	// each further signal) before running one lite refresh.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sig:
				t := time.NewTimer(eventDebounce)
			quiet:
				for {
					select {
					case <-ctx.Done():
						t.Stop()
						return
					case <-sig:
						if !t.Stop() {
							<-t.C
						}
						t.Reset(eventDebounce)
					case <-t.C:
						break quiet
					}
				}
				s.refreshNodeLite(id)
			}
		}
	}()

	backoff := watchBackoffBase
	for {
		if ctx.Err() != nil {
			return
		}
		cli, err := s.reg.Get(id)
		if err == nil {
			start := time.Now()
			// Per relevant event: bump the real persisted counter (PLAN §5.4),
			// trigger a debounced inventory refresh, and — for a protected container
			// facing a destructive change — fire a pre-change protective snapshot (F7).
			_ = dockercli.StreamRelevantEvents(ctx, cli, func(ev dockercli.ChangeEvent) {
				s.recordNodeEvent(id)
				trigger()
				s.maybeAutosnap(id, ev)
				s.maybeCrashAlert(id, ev)
			})
			if ctx.Err() != nil {
				return
			}
			s.reg.Drop(id) // stream ended; force a fresh client on reconnect
			if time.Since(start) >= watchMinUp {
				backoff = watchBackoffBase // it was genuinely connected
			} else {
				backoff = minDur(backoff*2, watchBackoffMax) // failed fast
			}
		} else {
			backoff = minDur(backoff*2, watchBackoffMax)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// dropNodeCache removes a node from the cache (on delete).
func (s *Server) dropNodeCache(id string) {
	s.statMu.Lock()
	delete(s.stats, id)
	s.statMu.Unlock()
}

// logSink fans engine log lines out to SSE subscribers and the audit-free ring
// buffer, and also to stderr for container logs (PLAN §4.11).
func (s *Server) logSink(backupID, level, msg string) {
	nodeID, nodeName, containerID := s.logSourceFor(backupID)
	line := LogLine{Time: time.Now().UTC().Format(time.RFC3339), BackupID: backupID, NodeID: nodeID, NodeName: nodeName, ContainerID: containerID, Level: level, Msg: msg}
	s.bcast.publish(line)
	log.Printf("[%s] %s %s", level, backupID, msg)
	// Persist per-run logs so a finished run's log survives reload/reconnect (F8).
	// Only real run ids (a backup/stack id) are stored — not the fleet-wide
	// sources like schedule/notify/queue that aren't tied to one artifact.
	if isRunLogID(backupID) {
		if err := s.store.AppendRunLog(backupID, level, msg); err == nil {
			// Trim occasionally (not every line) to bound growth for chatty runs.
			if s.runLogTick.Add(1)%256 == 0 {
				_ = s.store.TrimRunLog(backupID, maxRunLogLines)
			}
		}
	} else if reservedLogSources[backupID] {
		// F46: persist fleet-wide operational lines (schedule/queue/critical/notify)
		// into a bounded activity log — reusing run_logs under an "ops:" id, no new
		// table — so "what happened last night" survives a restart and the SSE ring wrap.
		if err := s.store.AppendRunLog("ops:"+backupID, level, msg); err == nil {
			if s.runLogTick.Add(1)%256 == 0 {
				_ = s.store.TrimRunLog("ops:"+backupID, maxRunLogLines)
			}
		}
	}
}

// maxRunLogLines bounds a single run's persisted log (newest kept).
const maxRunLogLines = 2000

// reservedLogSources are logSink sources that are fleet-wide, not a single
// backup/restore run, so their lines aren't persisted per-run (F8).
var reservedLogSources = map[string]bool{
	"schedule": true, "notify": true, "queue": true, "critical": true,
	"migrate": true, // F77: layout-migration activity log
}

// isRunLogID reports whether a logSink source id is a real per-run id worth
// persisting (a backup or stack id), i.e. non-empty and not a reserved source.
func isRunLogID(backupID string) bool {
	return backupID != "" && !reservedLogSources[backupID]
}

// logSourceFor resolves the source node and container for a backup's log lines
// from the job registry (empty for non-backup logs like schedule/notify/queue),
// so the Logs page can key by node and a per-container console can show only its
// own run. PLAN §4.13.
func (s *Server) logSourceFor(backupID string) (nodeID, nodeName, containerID string) {
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	if j, ok := s.jobs[backupID]; ok {
		return j.nodeID, j.nodeName, j.containerID
	}
	return "", "", ""
}

// Handler builds the routed http.Handler with middleware applied.
func (s *Server) Handler(uiFS fs.FS) http.Handler {
	mux := http.NewServeMux()

	// --- Auth (no session required) ---
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /api/version", s.handleVersion)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	// The ONE unauthenticated dynamic read surface (F59): a signed, expiring,
	// revocable, REDACTED runbook view. Verified by HMAC + expiry + revocation,
	// rate-limited per IP, never cached or indexed.
	mux.HandleFunc("GET /share/runbook/{token}", s.handleSharedRunbook)

	// --- Authenticated API ---
	mux.Handle("GET /api/me", s.auth(http.HandlerFunc(s.handleMe)))
	mux.Handle("POST /api/session/extend", s.auth(s.csrf(http.HandlerFunc(s.handleExtendSession))))
	mux.Handle("POST /api/session/activity", s.auth(s.csrf(http.HandlerFunc(s.handleActivity))))
	mux.Handle("POST /api/session/revoke-others", s.auth(s.csrf(http.HandlerFunc(s.handleRevokeOtherSessions))))
	// F202: see the signed-in devices before deciding, and end one on its own.
	// The session id travels in the POST body, never a path — a session token is
	// a credential and must not be written into request logs.
	mux.Handle("GET /api/session/list", s.auth(http.HandlerFunc(s.handleListSessions)))
	mux.Handle("POST /api/session/revoke", s.auth(s.csrf(http.HandlerFunc(s.handleRevokeSession))))
	mux.Handle("POST /api/account/password", s.auth(s.csrf(http.HandlerFunc(s.handleChangePassword))))
	// Application backup & restore — the app's own state (PLAN §6.5/§9.3).
	mux.Handle("GET /api/app-backup/info", s.auth(http.HandlerFunc(s.handleAppBackupInfo)))
	mux.Handle("GET /api/app-backup/list", s.auth(http.HandlerFunc(s.handleAppBackupList)))
	mux.Handle("POST /api/app-backup/create", s.auth(s.csrf(http.HandlerFunc(s.handleAppBackupCreate))))
	// F58: prove the newest app-backup restores (integrity drill), on demand.
	mux.Handle("POST /api/app-backup/drill", s.auth(s.csrf(http.HandlerFunc(s.handleAppBackupDrill))))
	mux.Handle("GET /api/app-backup/schedule", s.auth(http.HandlerFunc(s.handleGetAppBackupSchedule)))
	mux.Handle("PUT /api/app-backup/schedule", s.auth(s.csrf(http.HandlerFunc(s.handleSetAppBackupSchedule))))
	mux.Handle("POST /api/app-backup/export-grant", s.auth(s.csrf(http.HandlerFunc(s.handleAppBackupExportGrant)))) // F199
	mux.Handle("GET /api/app-backup/download", s.auth(http.HandlerFunc(s.handleAppBackupDownload)))
	mux.Handle("POST /api/app-backup/restore", s.auth(s.csrf(http.HandlerFunc(s.handleAppBackupRestore))))
	mux.Handle("POST /api/app-backup/restore-local", s.auth(s.csrf(http.HandlerFunc(s.handleAppBackupRestoreLocal))))
	mux.Handle("DELETE /api/app-backup/{file}", s.auth(s.csrf(http.HandlerFunc(s.handleAppBackupDelete))))
	// App-backup external destinations (separate from container destinations).
	mux.Handle("GET /api/app-backup/destinations", s.auth(http.HandlerFunc(s.handleAppDestList)))
	mux.Handle("POST /api/app-backup/destinations", s.auth(s.csrf(http.HandlerFunc(s.handleAppDestAdd))))
	mux.Handle("PUT /api/app-backup/destinations/{id}", s.auth(s.csrf(http.HandlerFunc(s.handleAppDestToggle))))
	mux.Handle("DELETE /api/app-backup/destinations/{id}", s.auth(s.csrf(http.HandlerFunc(s.handleAppDestDelete))))
	mux.Handle("POST /api/app-backup/external", s.auth(s.csrf(http.HandlerFunc(s.handleAppExternalBackup))))
	mux.Handle("GET /api/app-backup/destinations/{id}/backups", s.auth(http.HandlerFunc(s.handleAppExternalList)))
	mux.Handle("POST /api/app-backup/destinations/{id}/restore", s.auth(s.csrf(http.HandlerFunc(s.handleAppExternalRestore))))
	mux.Handle("GET /api/account/2fa", s.auth(http.HandlerFunc(s.handleTOTPStatus)))
	mux.Handle("POST /api/account/2fa/begin", s.auth(s.csrf(http.HandlerFunc(s.handleTOTPBegin))))
	mux.Handle("POST /api/account/2fa/enable", s.auth(s.csrf(http.HandlerFunc(s.handleTOTPEnable))))
	mux.Handle("POST /api/account/2fa/disable", s.auth(s.csrf(http.HandlerFunc(s.handleTOTPDisable))))
	// F104 clusters: grouping + the cluster tier of the policy chain. Deleting a
	// cluster removes a grouping only — never a node, container or backup.
	mux.Handle("GET /api/clusters", s.auth(http.HandlerFunc(s.handleListClusters)))
	mux.Handle("POST /api/clusters", s.auth(s.csrf(http.HandlerFunc(s.handleCreateCluster))))
	mux.Handle("PATCH /api/clusters/{name}", s.auth(s.csrf(http.HandlerFunc(s.handleUpdateCluster))))
	mux.Handle("DELETE /api/clusters/{name}", s.auth(s.csrf(http.HandlerFunc(s.handleDeleteCluster))))
	mux.Handle("GET /api/clusters/{name}/members", s.auth(http.HandlerFunc(s.handleClusterMembers)))
	mux.Handle("POST /api/clusters/{name}/members", s.auth(s.csrf(http.HandlerFunc(s.handleAssignClusterMembers))))
	mux.Handle("GET /api/clusters/{name}/policy", s.auth(http.HandlerFunc(s.handleGetClusterPolicy)))
	mux.Handle("PUT /api/clusters/{name}/policy", s.auth(s.csrf(http.HandlerFunc(s.handleSetClusterPolicy))))

	mux.Handle("GET /api/nodes", s.auth(http.HandlerFunc(s.handleListNodes)))
	mux.Handle("POST /api/nodes", s.auth(s.csrf(http.HandlerFunc(s.handleAddNode))))
	mux.Handle("POST /api/nodes/test", s.auth(s.csrf(http.HandlerFunc(s.handleTestNode))))
	mux.Handle("PUT /api/nodes/{id}", s.auth(s.csrf(http.HandlerFunc(s.handleUpdateNode))))
	mux.Handle("DELETE /api/nodes/{id}", s.auth(s.csrf(http.HandlerFunc(s.handleDeleteNode))))
	mux.Handle("POST /api/nodes/{id}/restore", s.auth(s.csrf(http.HandlerFunc(s.handleRestoreNode))))
	// F221: ad-hoc whole-node backup — every eligible container, each with its own
	// remembered options, shared host folders captured once.
	mux.Handle("POST /api/nodes/{id}/backup-all", s.auth(s.csrf(http.HandlerFunc(s.handleBackupNodeAll))))
	mux.Handle("POST /api/nodes/{id}/restore-all", s.auth(s.csrf(http.HandlerFunc(s.handleRestoreNodeAll))))
	// F211: the same plan the whole-node restore will execute, read-only, so a
	// service it cannot handle is visible BEFORE the run rather than discovered
	// at its turn in the sequence.
	mux.Handle("GET /api/nodes/{id}/restore-all/plan", s.auth(http.HandlerFunc(s.handleRestoreNodeAllPlan)))
	mux.Handle("DELETE /api/nodes/{id}/hostkey", s.auth(s.csrf(http.HandlerFunc(s.handleResetHostKey))))
	mux.Handle("POST /api/nodes/{id}/sidecar/repin", s.auth(s.csrf(http.HandlerFunc(s.handleRepinSidecar)))) // F88
	mux.Handle("GET /api/nodes/{id}", s.auth(http.HandlerFunc(s.handleGetNode)))
	mux.Handle("GET /api/nodes/{id}/policy", s.auth(http.HandlerFunc(s.handleGetNodePolicy)))
	mux.Handle("PUT /api/nodes/{id}/policy", s.auth(s.csrf(http.HandlerFunc(s.handleSetNodePolicy))))
	mux.Handle("GET /api/nodes/{id}/containers", s.auth(http.HandlerFunc(s.handleListContainers)))
	mux.Handle("GET /api/nodes/{id}/stacks", s.auth(http.HandlerFunc(s.handleListStacks)))
	// Connection-health history + uptime % (F40).
	mux.Handle("GET /api/nodes/{id}/health", s.auth(http.HandlerFunc(s.handleNodeHealth)))
	// F105: hardware + live host utilisation. Cached and never run implicitly —
	// each probe spawns a short-lived read-only container on the node.
	mux.Handle("GET /api/nodes/{id}/machine", s.auth(http.HandlerFunc(s.handleNodeMachine)))
	// Orphaned named volumes — data with no container (F23).
	mux.Handle("GET /api/nodes/{id}/orphan-volumes", s.auth(http.HandlerFunc(s.handleListOrphanVolumes)))
	mux.Handle("POST /api/nodes/{id}/orphan-volumes/{name}/backup", s.auth(s.csrf(http.HandlerFunc(s.handleBackupOrphanVolume))))
	mux.Handle("POST /api/nodes/{id}/stacks/{project}/backup", s.auth(s.csrf(http.HandlerFunc(s.handleBackupStack))))
	// F220: one-click protect for a whole compose project — ONE app-consistent
	// schedule target instead of one per service, plus a first backup.
	mux.Handle("POST /api/nodes/{id}/stacks/{project}/protect", s.auth(s.csrf(http.HandlerFunc(s.handleProtectStack))))
	mux.Handle("POST /api/nodes/{id}/stacks/{project}/restore", s.auth(s.csrf(http.HandlerFunc(s.handleRestoreStack))))
	// App-consistent snapshot groups available to restore this stack from (F43) — read-only.
	mux.Handle("GET /api/nodes/{id}/stacks/{project}/groups", s.auth(http.HandlerFunc(s.handleStackGroups)))
	mux.Handle("GET /api/nodes/{id}/stacks/{project}/restore-plan", s.auth(http.HandlerFunc(s.handleStackRestorePlan))) // F82 read-only pre-restore plan
	// F215: the cross-host choices this route needed last time, so the dialog can
	// offer them instead of asking for the same domain and paths again.
	mux.Handle("GET /api/nodes/{id}/stacks/{project}/restore-defaults", s.auth(http.HandlerFunc(s.handleStackRestoreDefaults)))
	mux.Handle("GET /api/nodes/{id}/stacks/{project}/options", s.auth(http.HandlerFunc(s.handleStackOptions))) // F80 per-service backup options
	mux.Handle("GET /api/nodes/{id}/containers/{cid}", s.auth(http.HandlerFunc(s.handleContainerDetail)))
	mux.Handle("GET /api/nodes/{id}/containers/{cid}/mounts", s.auth(http.HandlerFunc(s.handleContainerMounts)))
	mux.Handle("GET /api/nodes/{id}/containers/{cid}/databases", s.auth(http.HandlerFunc(s.handleListDatabases)))
	mux.Handle("PUT /api/nodes/{id}/containers/{cid}/hooks", s.auth(s.csrf(http.HandlerFunc(s.handleSetContainerHooks))))
	mux.Handle("PUT /api/nodes/{id}/containers/{cid}/pause-mode", s.auth(s.csrf(http.HandlerFunc(s.handleSetPauseMode))))
	mux.Handle("PUT /api/nodes/{id}/containers/{cid}/backup-options", s.auth(s.csrf(http.HandlerFunc(s.handleSetBackupOptions))))   // F80 stack panel inline edits
	mux.Handle("PUT /api/nodes/{id}/containers/{cid}/mount-selection", s.auth(s.csrf(http.HandlerFunc(s.handleSetMountSelection)))) // F115 stack panel mount picker
	mux.Handle("POST /api/nodes/{id}/containers/{cid}/protect", s.auth(s.csrf(http.HandlerFunc(s.handleProtectContainer))))
	mux.Handle("PUT /api/nodes/{id}/containers/{cid}/export-profile", s.auth(s.csrf(http.HandlerFunc(s.handleSetExportProfile))))
	// Share/import an app-native export profile between containers (F32).
	// F101: the fleet-level preset library behind the per-container profile.
	mux.Handle("GET /api/export-presets", s.auth(http.HandlerFunc(s.handleListExportPresets)))
	mux.Handle("POST /api/export-presets", s.auth(s.csrf(http.HandlerFunc(s.handleSaveExportPresets))))
	mux.Handle("DELETE /api/export-presets/{id}", s.auth(s.csrf(http.HandlerFunc(s.handleDeleteExportPreset))))
	mux.Handle("GET /api/nodes/{id}/containers/{cid}/export-profile", s.auth(http.HandlerFunc(s.handleExportProfileExport)))
	mux.Handle("POST /api/nodes/{id}/containers/{cid}/export-profile", s.auth(s.csrf(http.HandlerFunc(s.handleExportProfileImport))))
	mux.Handle("GET /api/nodes/{id}/containers/{cid}/critical", s.auth(http.HandlerFunc(s.handleGetCritical)))
	mux.Handle("PUT /api/nodes/{id}/containers/{cid}/critical", s.auth(s.csrf(http.HandlerFunc(s.handleSetCritical))))
	mux.Handle("PUT /api/nodes/{id}/containers/{cid}/autosnap", s.auth(s.csrf(http.HandlerFunc(s.handleSetAutosnap))))
	mux.Handle("PUT /api/nodes/{id}/containers/{cid}/bind-threshold", s.auth(s.csrf(http.HandlerFunc(s.handleSetBindThreshold))))
	mux.Handle("PUT /api/nodes/{id}/containers/{cid}/regenerable", s.auth(s.csrf(http.HandlerFunc(s.handleSetExcludeRegenerable))))      // F132 regenerable-path exclusion
	mux.Handle("PUT /api/nodes/{id}/containers/{cid}/restore-ownership", s.auth(s.csrf(http.HandlerFunc(s.handleSetRestoreOwnership))))  // F184 per-container restore uid:gid
	mux.Handle("PUT /api/nodes/{id}/containers/{cid}/export-cleanup", s.auth(s.csrf(http.HandlerFunc(s.handleSetCleanupExport))))        // F151 empty the export dir after capture
	mux.Handle("PUT /api/nodes/{id}/containers/{cid}/require-write-only", s.auth(s.csrf(http.HandlerFunc(s.handleSetRequireWriteOnly)))) // F163 refuse a backup that is not write-only
	// F205: the Redis password for a broker that set it at runtime. Write-only —
	// sealed at rest, and there is deliberately no endpoint that reads it back.
	mux.Handle("PUT /api/nodes/{id}/containers/{cid}/redis-auth", s.auth(s.csrf(http.HandlerFunc(s.handleSetRedisAuth))))
	// F206: whether overwriting this container in place demands fresh proof of the
	// password. Not step-up gated itself — a password to ARM a safety catch would
	// stop people arming it — but every change is audited.
	mux.Handle("PUT /api/nodes/{id}/containers/{cid}/restore-step-up", s.auth(s.csrf(http.HandlerFunc(s.handleSetRestoreStepUp))))
	mux.Handle("PUT /api/nodes/{id}/containers/{cid}/restore-timeout", s.auth(s.csrf(http.HandlerFunc(s.handleSetRestoreTimeout))))
	mux.Handle("GET /api/nodes/{id}/containers/{cid}/sizes", s.auth(http.HandlerFunc(s.handleContainerSizes)))
	mux.Handle("GET /api/nodes/{id}/containers/{cid}/policy", s.auth(http.HandlerFunc(s.handleGetContainerPolicy)))
	mux.Handle("PUT /api/nodes/{id}/containers/{cid}/policy", s.auth(s.csrf(http.HandlerFunc(s.handleSetContainerPolicy))))
	// Pilot-light standby rehearsals (F62): prove a container fails over to another node.
	mux.Handle("GET /api/nodes/{id}/containers/{cid}/standby", s.auth(http.HandlerFunc(s.handleGetStandby)))
	mux.Handle("PUT /api/nodes/{id}/containers/{cid}/standby", s.auth(s.csrf(http.HandlerFunc(s.handleSetStandby))))
	mux.Handle("DELETE /api/nodes/{id}/containers/{cid}/standby", s.auth(s.csrf(http.HandlerFunc(s.handleDeleteStandby))))
	mux.Handle("POST /api/nodes/{id}/containers/{cid}/standby/run", s.auth(s.csrf(http.HandlerFunc(s.handleRunStandby))))
	// Ransomware tripwire (F69): clear a container's retention hold after review.
	mux.Handle("POST /api/nodes/{id}/containers/{cid}/tripwire/clear", s.auth(s.csrf(http.HandlerFunc(s.handleClearTripwire))))
	// Config drift (F73): field-by-field breakdown vs the newest backup, on demand.
	mux.Handle("GET /api/nodes/{id}/containers/{cid}/drift", s.auth(http.HandlerFunc(s.handleContainerDrift)))

	mux.Handle("GET /api/backups", s.auth(http.HandlerFunc(s.handleListBackups)))
	mux.Handle("POST /api/backups", s.auth(s.csrf(http.HandlerFunc(s.handleCreateBackup))))
	mux.Handle("GET /api/backups/{id}", s.auth(http.HandlerFunc(s.handleGetBackup)))
	mux.Handle("GET /api/backups/{id}/restore-readiness", s.auth(http.HandlerFunc(s.handleRestoreReadiness)))
	mux.Handle("DELETE /api/backups/{id}", s.auth(s.csrf(http.HandlerFunc(s.handleDeleteBackup))))
	mux.Handle("POST /api/backups/delete", s.auth(s.csrf(http.HandlerFunc(s.handleBulkDeleteBackup))))
	mux.Handle("POST /api/backups/{id}/cancel", s.auth(s.csrf(http.HandlerFunc(s.handleCancelBackup))))
	mux.Handle("POST /api/backups/{id}/verify", s.auth(s.csrf(http.HandlerFunc(s.handleVerifyBackup))))
	mux.Handle("PUT /api/backups/{id}/pin", s.auth(s.csrf(http.HandlerFunc(s.handleSetBackupPin))))
	mux.Handle("PUT /api/backups/{id}/label", s.auth(s.csrf(http.HandlerFunc(s.handleSetBackupLabel))))
	mux.Handle("POST /api/backups/{id}/mirror", s.auth(s.csrf(http.HandlerFunc(s.handleMirrorBackup))))
	mux.Handle("POST /api/backups/{id}/drill", s.auth(s.csrf(http.HandlerFunc(s.handleDrillBackup))))
	mux.Handle("POST /api/backups/{id}/restore", s.auth(s.csrf(http.HandlerFunc(s.handleRestore))))
	// Cancel any in-flight restore (single / stack / whole node) by its run id.
	mux.Handle("GET /api/restores", s.auth(http.HandlerFunc(s.handleListRestores)))
	// F219: one-click test clones — what is currently up on a node, and remove one
	// now rather than waiting for its expiry.
	mux.Handle("GET /api/nodes/{id}/test-clones", s.auth(http.HandlerFunc(s.handleListTestClones)))
	mux.Handle("POST /api/test-clones/remove", s.auth(s.csrf(http.HandlerFunc(s.handleDeleteTestClone))))
	mux.Handle("POST /api/restores/{id}/cancel", s.auth(s.csrf(http.HandlerFunc(s.handleCancelRestore))))
	// F199: exports are step-up gated. The GET carries a one-shot ticket because
	// a browser navigation has no body to put a password in; the POST below is
	// where the password actually goes.
	mux.Handle("POST /api/backups/{id}/export-grant", s.auth(s.csrf(http.HandlerFunc(s.handleExportGrant))))
	// F204: prove a saved offline recovery key actually opens a write-only backup,
	// without restoring it. Step-up gated inside the handler (it decrypts with an
	// operator-supplied key), and the key never leaves the request.
	mux.Handle("POST /api/backups/{id}/verify-key", s.auth(s.csrf(http.HandlerFunc(s.handleVerifyRecoveryKey))))
	mux.Handle("POST /api/security/write-only/identify-key", s.auth(s.csrf(http.HandlerFunc(s.handleWriteOnlyKeyFingerprint))))
	mux.Handle("GET /api/backups/{id}/download", s.auth(http.HandlerFunc(s.handleDownload)))
	// Browse a backup's files + download one (F21).
	// F70 universal file index: cross-backup file search + generation diff
	// (literal segments — more specific than /api/backups/{id}, so no conflict).
	mux.Handle("GET /api/backups/search-file", s.auth(http.HandlerFunc(s.handleFileSearch)))
	mux.Handle("GET /api/backups/diff", s.auth(http.HandlerFunc(s.handleBackupDiff)))
	mux.Handle("GET /api/backups/{id}/entries", s.auth(http.HandlerFunc(s.handleBackupEntries)))
	mux.Handle("GET /api/backups/{id}/extract", s.auth(http.HandlerFunc(s.handleBackupExtract)))
	// F96: put one recovered file BACK into the running container. Destructive, so
	// CSRF + the container-scoped restore lock (see handleRestoreFile).
	mux.Handle("POST /api/backups/{id}/restore-file", s.auth(s.csrf(http.HandlerFunc(s.handleRestoreFile))))
	mux.Handle("GET /api/backups/{id}/log", s.auth(http.HandlerFunc(s.handleRunLog)))
	mux.Handle("GET /api/backups/{id}/log/download", s.auth(http.HandlerFunc(s.handleRunLogDownload)))

	mux.Handle("GET /api/stats", s.auth(http.HandlerFunc(s.handleStats)))
	mux.Handle("GET /api/coverage", s.auth(http.HandlerFunc(s.handleCoverage)))
	mux.Handle("GET /api/insights", s.auth(http.HandlerFunc(s.handleInsights)))
	mux.Handle("GET /api/runbook", s.auth(http.HandlerFunc(s.handleRunbook)))
	// Signed, expiring runbook share links (F59) — mint / list / revoke.
	mux.Handle("POST /api/runbook/share", s.auth(s.csrf(http.HandlerFunc(s.handleShareRunbook))))
	mux.Handle("GET /api/runbook/shares", s.auth(http.HandlerFunc(s.handleShareRunbookList)))
	mux.Handle("POST /api/runbook/shares/{id}/revoke", s.auth(s.csrf(http.HandlerFunc(s.handleShareRunbookRevoke))))
	mux.Handle("GET /api/policy", s.auth(http.HandlerFunc(s.handleGetPolicy)))
	mux.Handle("PUT /api/policy", s.auth(s.csrf(http.HandlerFunc(s.handleSetPolicy))))
	mux.Handle("POST /api/policy/run", s.auth(s.csrf(http.HandlerFunc(s.handleRunSchedule))))
	// Named backup schedules (F6).
	mux.Handle("GET /api/schedules", s.auth(http.HandlerFunc(s.handleListSchedules)))
	mux.Handle("POST /api/schedules", s.auth(s.csrf(http.HandlerFunc(s.handleCreateSchedule))))
	mux.Handle("PUT /api/schedules/{id}", s.auth(s.csrf(http.HandlerFunc(s.handleUpdateSchedule))))
	mux.Handle("DELETE /api/schedules/{id}", s.auth(s.csrf(http.HandlerFunc(s.handleDeleteSchedule))))
	mux.Handle("POST /api/schedules/{id}/run", s.auth(s.csrf(http.HandlerFunc(s.handleRunScheduleNow))))
	mux.Handle("GET /api/retention/preview", s.auth(http.HandlerFunc(s.handleRetentionPreview)))
	mux.Handle("POST /api/retention/prune", s.auth(s.csrf(http.HandlerFunc(s.handleRetentionPrune))))
	// F77: migrate legacy flat-layout archives into the per-stack folder layout.
	mux.Handle("POST /api/maintenance/migrate-layout", s.auth(s.csrf(http.HandlerFunc(s.handleMigrateLayout))))
	mux.Handle("GET /api/maintenance/migrate-layout", s.auth(http.HandlerFunc(s.handleMigrateLayoutStatus)))
	mux.Handle("GET /api/destinations", s.auth(http.HandlerFunc(s.handleListDestinations)))
	mux.Handle("POST /api/destinations", s.auth(s.csrf(http.HandlerFunc(s.handleAddDestination))))
	mux.Handle("POST /api/destinations/test", s.auth(s.csrf(http.HandlerFunc(s.handleTestDestination))))
	mux.Handle("GET /api/destinations/{id}/config", s.auth(http.HandlerFunc(s.handleGetDestinationConfig)))
	mux.Handle("PUT /api/destinations/{id}", s.auth(s.csrf(http.HandlerFunc(s.handleUpdateDestination))))
	// F66: clear + re-pin an SFTP destination's SSH host key after a legitimate change.
	mux.Handle("POST /api/destinations/{id}/reset-hostkey", s.auth(s.csrf(http.HandlerFunc(s.handleResetDestHostKey))))
	mux.Handle("POST /api/destinations/{id}/test", s.auth(s.csrf(http.HandlerFunc(s.handleTestDestinationByID))))
	// Scan a destination for orphaned archives and re-import them into the catalog (F20).
	mux.Handle("POST /api/destinations/{id}/adopt", s.auth(s.csrf(http.HandlerFunc(s.handleAdoptDestination))))
	// Backfill existing history to a destination (F51) — start (mutation) + poll progress.
	mux.Handle("POST /api/destinations/{id}/backfill", s.auth(s.csrf(http.HandlerFunc(s.handleBackfillDestination))))
	mux.Handle("GET /api/destinations/{id}/backfill", s.auth(http.HandlerFunc(s.handleBackfillStatus)))
	mux.Handle("DELETE /api/destinations/{id}", s.auth(s.csrf(http.HandlerFunc(s.handleDeleteDestination))))
	mux.Handle("GET /api/audit", s.auth(http.HandlerFunc(s.handleAudit)))
	mux.Handle("GET /api/audit/export", s.auth(http.HandlerFunc(s.handleAuditExport)))
	// F68: walk the tamper-evident hash chain (read-only — no step-up).
	mux.Handle("GET /api/audit/verify", s.auth(http.HandlerFunc(s.handleAuditVerify)))
	// F200: check the trail against a checkpoint the operator kept OUTSIDE this
	// machine — the only comparison a container compromise cannot arrange to pass.
	mux.Handle("POST /api/audit/verify-beacon", s.auth(s.csrf(http.HandlerFunc(s.handleAuditVerifyBeacon))))
	mux.Handle("GET /api/settings", s.auth(http.HandlerFunc(s.handleGetSettings)))
	mux.Handle("POST /api/settings", s.auth(s.csrf(http.HandlerFunc(s.handleSetSettings))))
	mux.Handle("GET /api/drills", s.auth(http.HandlerFunc(s.handleListDrills)))
	// Encryption-key escrow & recovery (PLAN §9.2).
	mux.Handle("GET /api/security/key-status", s.auth(http.HandlerFunc(s.handleKeyStatus)))
	mux.Handle("POST /api/security/key-reveal", s.auth(s.csrf(http.HandlerFunc(s.handleKeyReveal))))
	mux.Handle("POST /api/security/key-acknowledge", s.auth(s.csrf(http.HandlerFunc(s.handleKeyAcknowledge))))
	mux.Handle("POST /api/security/key-keyfile", s.auth(s.csrf(http.HandlerFunc(s.handleKeyKeyfile))))
	// F86 write-only backups. Enable/disable are step-up gated inside the handlers
	// (arming this changes what a compromise of this instance is worth); status
	// returns public material only.
	mux.Handle("GET /api/security/write-only", s.auth(http.HandlerFunc(s.handleWriteOnlyStatus)))
	mux.Handle("POST /api/security/write-only/enable", s.auth(s.csrf(http.HandlerFunc(s.handleWriteOnlyEnable))))
	mux.Handle("POST /api/security/write-only/disable", s.auth(s.csrf(http.HandlerFunc(s.handleWriteOnlyDisable))))
	mux.Handle("POST /api/security/key-rotate", s.auth(s.csrf(http.HandlerFunc(s.handleKeyRotate))))
	// The offline recovery tool + its fingerprint, for the "restore without DockBack"
	// recovery kit (F35). Auth-gated: it's not secret, but there's no reason to
	// expose it unauthenticated.
	mux.Handle("GET /api/security/recovery-tool", s.auth(http.HandlerFunc(s.handleRecoveryTool)))
	mux.Handle("GET /api/security/recovery-tool/download", s.auth(http.HandlerFunc(s.handleRecoveryToolDownload)))
	// Test a host against the live egress allow-list (F39).
	mux.Handle("POST /api/security/egress-test", s.auth(s.csrf(http.HandlerFunc(s.handleEgressTest))))
	// Suggest allow-list hosts from what's already configured (F54) — read-only.
	mux.Handle("GET /api/security/egress-suggestions", s.auth(http.HandlerFunc(s.handleEgressSuggestions)))
	// F207: audit mode — what the allow-list WOULD refuse, before it does.
	mux.Handle("GET /api/security/egress/audit", s.auth(http.HandlerFunc(s.handleEgressAudit)))
	mux.Handle("POST /api/security/egress/audit/clear", s.auth(s.csrf(http.HandlerFunc(s.handleEgressAuditClear))))
	// Scoped API tokens for automation (F45) — session-only management (tokens can't
	// manage tokens; enforced in tokenAllows).
	mux.Handle("GET /api/security/tokens", s.auth(http.HandlerFunc(s.handleListTokens)))
	mux.Handle("POST /api/security/tokens", s.auth(s.csrf(http.HandlerFunc(s.handleCreateToken))))
	mux.Handle("DELETE /api/security/tokens/{id}", s.auth(s.csrf(http.HandlerFunc(s.handleDeleteToken))))
	mux.Handle("GET /api/notifications", s.auth(http.HandlerFunc(s.handleGetNotify)))
	mux.Handle("PUT /api/notifications", s.auth(s.csrf(http.HandlerFunc(s.handleSetNotify))))
	mux.Handle("POST /api/notifications/test", s.auth(s.csrf(http.HandlerFunc(s.handleTestNotify))))
	mux.Handle("GET /api/notifications/hint", s.auth(http.HandlerFunc(s.handleNotifyHint)))
	mux.Handle("POST /api/notifications/hint/dismiss", s.auth(s.csrf(http.HandlerFunc(s.handleDismissNotifyHint))))
	// F222: favorite backup targets — a UI preference stored server-side so the
	// list roams between browsers. Names and node ids only; no secrets, no policy.
	// F223: the first-run / post-disaster recovery wizard — state and guidance
	// only; every step links to the screen that already does the work.
	mux.Handle("GET /api/recovery/state", s.auth(http.HandlerFunc(s.handleRecoveryState)))
	mux.Handle("PUT /api/recovery/state", s.auth(s.csrf(http.HandlerFunc(s.handleSetRecoveryState))))
	mux.Handle("GET /api/ui/favorites", s.auth(http.HandlerFunc(s.handleGetFavorites)))
	mux.Handle("PUT /api/ui/favorites", s.auth(s.csrf(http.HandlerFunc(s.handleSetFavorites))))
	// Write-only on purpose: GET /api/nodes already answers in the saved order.
	mux.Handle("PUT /api/ui/node-order", s.auth(s.csrf(http.HandlerFunc(s.handleSetNodeOrder))))
	mux.Handle("GET /api/logs/stream", s.auth(http.HandlerFunc(s.handleLogStream)))
	// Persistent alert inbox + activity feed (F46).
	mux.Handle("GET /api/alerts", s.auth(http.HandlerFunc(s.handleListAlerts)))
	mux.Handle("GET /api/alerts/count", s.auth(http.HandlerFunc(s.handleAlertsCount)))
	mux.Handle("POST /api/alerts/{id}/ack", s.auth(s.csrf(http.HandlerFunc(s.handleAckAlert))))
	mux.Handle("POST /api/alerts/ack-all", s.auth(s.csrf(http.HandlerFunc(s.handleAckAllAlerts))))
	mux.Handle("GET /api/ops-log/{source}", s.auth(http.HandlerFunc(s.handleOpsLog)))

	// --- Static UI (SPA fallback) ---
	mux.Handle("/", spaHandler(uiFS))

	// gzip sits INSIDE securityHeaders (headers land on the shared map before
	// the body writer is wrapped) and outside the mux so assets + API JSON both
	// compress; the SSE/download/share skip list lives in the middleware.
	return s.recoverer(s.securityHeaders(gzipMiddleware(mux)))
}

// backupJob is an in-flight backup the user can cancel.
type backupJob struct {
	cancel      context.CancelFunc
	canceled    bool
	nodeID      string // source node, so log lines for this backup can be node-keyed
	nodeName    string
	containerID string // source container, so a per-container console shows only its own run
}

// cancelBackup signals a queued or running backup to stop. Returns false if no
// such job exists. A queued job is removed from the queue so it never starts; a
// running job is canceled via its context (PLAN §4.13).
func (s *Server) cancelBackup(id string) bool {
	s.jobMu.Lock()
	j, ok := s.jobs[id]
	queued := false
	if ok {
		j.canceled = true
		if j.cancel != nil { // nil while still queued
			j.cancel()
		} else {
			queued = true // never dispatched: runQueued's cleanup will never run
		}
	}
	s.jobMu.Unlock()
	if !ok {
		return false
	}
	s.removeFromQueue(id) // no-op if already dispatched
	if queued {
		// A job cancelled BEFORE dispatch never enters runQueued, so the deferred
		// delete(s.jobs, id) there never fires. Drop it here, or the map grows by
		// one permanent entry per cancelled click — and logSourceFor walks it on
		// every persisted log line.
		s.jobMu.Lock()
		if jb, still := s.jobs[id]; still && jb == j && jb.cancel == nil {
			delete(s.jobs, id)
		}
		s.jobMu.Unlock()
	}
	return true
}

func (s *Server) wasCanceled(id string) bool {
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	j, ok := s.jobs[id]
	return ok && j.canceled
}

// runBackupAsync launches a backup in a worker, respecting the concurrency cap.
// nodeSem returns the per-node concurrency semaphore (lazily created, sized to
// the per-node cap), so backups for one node never exceed it (PLAN §4.13).
func (s *Server) nodeSem(nodeID string) *dynSem {
	s.nodeSemMu.Lock()
	defer s.nodeSemMu.Unlock()
	cap := s.perNodeCap
	if cap < 1 {
		cap = 1
	}
	if s.nodeSems == nil {
		s.nodeSems = map[string]*dynSem{}
	}
	sem, ok := s.nodeSems[nodeID]
	if !ok {
		sem = newDynSem(cap)
		s.nodeSems[nodeID] = sem
	}
	return sem
}

// setMaxConcurrent resizes the fleet-wide backup concurrency cap live (F29):
// running backups are never preempted; a lower cap simply serializes NEW work as
// slots free, a higher cap lets more start immediately.
func (s *Server) setMaxConcurrent(n int) {
	if s.backupSem != nil {
		s.backupSem.setLimit(n) // dynSem clamps >=1
	}
}

// setMaxConcurrentPerNode updates the per-node cap and resizes every existing
// per-node semaphore live (F29), so a change applies to nodes already seen too.
func (s *Server) setMaxConcurrentPerNode(n int) {
	if n < 1 {
		n = 1
	}
	s.nodeSemMu.Lock()
	s.perNodeCap = n
	for _, sem := range s.nodeSems {
		sem.setLimit(n)
	}
	s.nodeSemMu.Unlock()
}

// maxConcurrent / maxConcurrentPerNode resolve the effective caps: the in-app
// setting when present, else the env/config default (F29 — mirrors F15).
func (s *Server) maxConcurrent() int {
	return clampInt(s.settingInt("backup.max_concurrent", s.cfg.MaxConcurrentBackups), 1, 64)
}
func (s *Server) maxConcurrentPerNode() int {
	return clampInt(s.settingInt("backup.max_concurrent_per_node", s.cfg.MaxConcurrentPerNode), 1, 64)
}

// runBackupAsync enqueues an INTERACTIVE backup (manual / stack action). It
// outranks scheduled work and is never jittered, so a user's "back up now" jumps
// ahead of a running nightly window (PLAN §4.13). Returns the backup id so the
// caller can hand it to the UI for precise progress/completion tracking.
func (s *Server) runBackupAsync(nodeName string, opts backup.Options) string {
	return s.enqueueBackup(nodeName, opts, prioInteractive)
}

// --- log broadcaster (SSE) ---

// LogLine is one streamed log entry (PLAN §4.11 live log streaming).
type LogLine struct {
	Time        string `json:"time"`
	BackupID    string `json:"backup_id"`
	NodeID      string `json:"node_id,omitempty"`      // source node (PLAN §4.13 node-keyed logs)
	NodeName    string `json:"node_name,omitempty"`    // friendly node name for the Logs filter
	ContainerID string `json:"container_id,omitempty"` // source container, so a per-container console shows only its own run
	Level       string `json:"level"`
	Msg         string `json:"msg"`
}

// sseMsg is one Server-Sent Event. event=="" is the default channel (a live log
// line, PLAN §4.11); a non-empty event is a named delta (node.summary /
// backup.status, A7) that lets the UI update without full-reload polling.
type sseMsg struct {
	event string
	data  string // pre-serialized JSON payload
}

// runDoneGrace is how long fanoutImportant will wait for one slow subscriber.
//
// Long enough for a browser that is mid-render to drain a buffered message,
// short enough that a dead connection cannot hold up the run that is finishing.
const runDoneGrace = 250 * time.Millisecond

type broadcaster struct {
	mu     sync.Mutex
	subs   map[chan sseMsg]struct{}
	ring   []sseMsg // replay buffer — LOG lines only (named events aren't replayed)
	ringSz int
	// dropped counts terminal events that could not be delivered even after the
	// grace period. A non-zero value here is the measurable form of "a console
	// fell back to matching log text", which is the thing run.done exists to
	// replace — so it is worth a metric rather than silence.
	dropped atomic.Int64
}

func newBroadcaster(ringSize int) *broadcaster {
	return &broadcaster{subs: map[chan sseMsg]struct{}{}, ringSz: ringSize}
}

// fanout delivers m to every subscriber (dropping for slow ones so the engine
// never blocks) and, when keep is set, appends it to the replay ring.
func (b *broadcaster) fanout(m sseMsg, keep bool) {
	if b == nil {
		return // a Server built without a broadcaster still logs; it just can't stream
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if keep {
		b.ring = append(b.ring, m)
		if len(b.ring) > b.ringSz {
			b.ring = b.ring[len(b.ring)-b.ringSz:]
		}
	}
	for ch := range b.subs {
		select {
		case ch <- m:
		default: // drop for slow subscribers; never block the engine
		}
	}
}

// fanoutImportant delivers an event that a console cannot reconstruct if it is
// missed, giving a momentarily-full subscriber a bounded grace period instead of
// dropping on the same terms as a log line.
//
// The subscriber set is snapshotted under the lock and the waiting happens
// OUTSIDE it: a blocking send while holding b.mu would stall every other
// publisher — including the engine — behind one slow browser. That is safe
// because unsubscribe no longer closes the channel; a reader that has gone away
// leaves a buffered channel nothing reads, which is collected.
func (b *broadcaster) fanoutImportant(m sseMsg) {
	b.mu.Lock()
	subs := make([]chan sseMsg, 0, len(b.subs))
	for ch := range b.subs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- m:
			continue // the common case: buffer space was available
		default:
		}
		t := time.NewTimer(runDoneGrace)
		select {
		case ch <- m:
		case <-t.C:
			b.dropped.Add(1)
		}
		t.Stop()
	}
}

// droppedEvents reports terminal events that could not be delivered (metrics).
func (b *broadcaster) droppedEvents() int64 {
	if b == nil {
		return 0
	}
	return b.dropped.Load()
}

// publish streams a log line on the default SSE channel and keeps it for replay.
func (b *broadcaster) publish(l LogLine) { b.fanout(sseMsg{data: mustJSON(l)}, true) }

// publishEvent streams a NAMED delta (node.summary / backup.status). Not replayed
// on connect — clients do an initial REST fetch, then merge these deltas (A7).
func (b *broadcaster) publishEvent(event string, payload any) {
	b.fanout(sseMsg{event: event, data: mustJSON(payload)}, false)
}

func (b *broadcaster) subscribe() (chan sseMsg, []sseMsg) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan sseMsg, 64)
	b.subs[ch] = struct{}{}
	hist := make([]sseMsg, len(b.ring))
	copy(hist, b.ring)
	return ch, hist
}

// unsubscribe removes a reader. The channel is deliberately NOT closed: a
// terminal event may be mid-delivery on it (fanoutImportant waits outside the
// lock), and a send on a closed channel is a panic that would take the whole
// process down. The handler's own loop already exits on its request context, and
// an abandoned buffered channel is collected.
func (b *broadcaster) unsubscribe(ch chan sseMsg) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subs, ch)
}
