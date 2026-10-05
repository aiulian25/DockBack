// Thin typed client for the DockBack REST API. Mutations attach the CSRF
// token from the cookie set at login (double-submit).

// qstr builds a "?a=1&b=2" query string from a params object, skipping empty/
// undefined values so a blank search/filter doesn't constrain the result.
function qstr(p: Record<string, unknown>): string {
  const u = new URLSearchParams();
  for (const [k, v] of Object.entries(p)) {
    if (v !== undefined && v !== "" && v !== null) u.set(k, String(v));
  }
  const s = u.toString();
  return s ? `?${s}` : "";
}

export interface NodeSummary {
  running: number; stopped: number; paused: number; restarting: number;
  total: number; images: number; volumes: number; networks: number; stacks: number;
  stacks_running: number; stacks_stopped: number; stacks_errored: number;
  cpu_percent: number; mem_used: number; mem_total: number; mem_percent: number;
  net_rx: number; net_tx: number; events_today: number; events_total: number;
}
export interface FleetStats { backup_success_rate: number; backups_30d: number; backups_verified: number; }
// Backup coverage / "unprotected containers" (B2): running containers with no
// successful backup and not covered by the schedule.
export interface CoverageContainer { container_id: string; name: string; stack?: string; image?: string; last_backup_at?: number; scheduled?: boolean; }
// unprotected: running, never backed up. stale: running, newest backup older
// than twice its schedule's interval (8 days with no schedule).
export interface CoverageNode { node_id: string; node_name: string; cluster: string; reachable: boolean; running: number; protected: number; last_backup_at: number; unprotected: CoverageContainer[]; stale: CoverageContainer[]; stopped_at_risk: CoverageContainer[]; }
// off_machine_destinations: enabled destinations that leave this machine (a
// "local" destination is a folder on it). Zero means every copy is here.
export interface Coverage { off_machine_destinations: number; app_off_machine_destinations: number; unprotected_total: number; stale_total: number; running_total: number; stopped_at_risk_total: number; nodes: CoverageNode[]; }
// One-click "Protect this container" (B5): what the smart-default flow decided.
export interface ProtectResult {
  container: string; is_database: boolean; engine?: string; pause_mode: string;
  destinations: string[]; scheduled: boolean; schedule_enabled: boolean;
  schedule_id: string; // F38: the schedule Protect chose, so the UI can enable it in one click
  schedule_added: boolean; backup_started: boolean; summary: string;
}
// F220: the stack counterpart. The three fields the UI acts on — summary,
// schedule_enabled, schedule_id — carry the same names as ProtectResult, so one
// toast handles both; the rest describes a project rather than a container.
export interface ProtectStackResult {
  stack: string; services: number; databases?: string[]; consistent: boolean;
  destinations: string[]; scheduled: boolean; schedule_enabled: boolean;
  schedule_id: string; schedule_added: boolean;
  replaced_targets?: number; // per-container targets the one stack target subsumed
  backup_started: boolean; summary: string;
}
// F221: the result of a whole-node run. count is what STARTED; skipped is what
// was already queued or running and was coalesced rather than duplicated — the
// two are separate so "nothing happened" and "it was already happening" never
// look the same.
export interface NodeBackupAllResult {
  status: string; count: number; skipped: number; names: string[];
}
// F223: whether this instance looks brand-new, and how far the recovery wizard
// has got. Guidance only — nothing here starts, restores or deletes anything.
export interface RecoveryState {
  fresh_install: boolean;
  step: "" | "app-restored" | "dests-verified" | "done" | "dismissed";
  nodes: number;   // nodes the OPERATOR added (the auto-registered local one is not counted)
  backups: number;
  has_app_destinations: boolean;
}

// A favorite backup target: a whole stack or a single container, on a node (F222).
// ref is the stack PROJECT name, or the container NAME — never an id, which dies
// on every recreate. Defined here rather than in the component because the list
// now round-trips through the API.
export interface Favorite {
  kind: "stack" | "container";
  nodeId: string;
  nodeName: string; // display cache, so the menu paints without a fetch
  ref: string;
  name: string;
}
export interface HostInfo {
  name: string; os: string; os_version: string; kernel: string; arch: string;
  docker_version: string; ncpu: number; mem_total: number;
}
// F88: the volume-sidecar image this node is pinned to. `pinned` false means it
// hasn't run a sidecar yet — the pin is taken on first use.
export interface SidecarPin { image: string; digest: string; pinned: boolean; }
export interface NodeDetail {
  node: Node; reachable?: boolean; error?: string; summary?: NodeSummary;
  host?: HostInfo; backups: FleetStats; sidecar?: SidecarPin;
}
export interface ContainerInfo {
  id: string; name: string; image: string; image_digest: string; state: string;
  uptime: string; started_at: string; ip: string; health: string; description: string;
  command: string; stack: string; service: string; ports: string; created_at: number;
  cpu_percent: number; mem_used: number; mem_limit: number; volume_count: number; is_database: boolean;
}
// F140: post_restore runs after a RESTORE puts the data back and starts the
// container, before the health gate. Optional so older saved hooks are unchanged.
export interface SavedHooks { pre: string[]; post: string[]; user: string; post_restore?: string[]; }
// verify_cmd (F152): the app's own check, run after a successful import; a
// non-zero exit fails the restore. Empty for apps with no checker that can fail.
export interface AppExportProfile { available: boolean; tool: string; dir: string; export_cmd: string; import_cmd: string; verify_cmd?: string; user: string; }
// Remembered manual backup options reused by scheduled/whole-node runs (F3).
export interface SavedBackupOptions { compression: string; app_export: boolean; save_image: boolean; incremental?: boolean; incremental_full_every?: number; }
export interface ContainerDetailResp {
  container: ContainerInfo; backups: Backup[]; backup_count: number; total_bytes: number;
  sqlite_files?: number; // F22: SQLite DBs the last backup captured consistently
  label_managed?: boolean; label_policy?: LabelPolicy; // F19: dockback.* label-driven policy
  storage: string; node_id: string; node_name: string;
  hooks: SavedHooks; auto_hooks: string[]; app_export: AppExportProfile; pause_mode?: string;
  // F145: the quiesce mode this application declares for itself, and why. Empty
  // for the images that declare none, which is nearly all of them — those keep
  // the shipped "pause during copy" default.
  pause_default?: string; pause_default_why?: string;
  backup_options?: SavedBackupOptions; autosnap?: boolean;
  tripwire_hold?: string; // F69: reason of an active mass-change retention hold ("" = none)
  drift?: { changed: boolean } | null; // F73: live config vs newest backup fingerprint (null = undecidable/legacy)
  bind_skip_gib?: number; bind_skip_gib_override?: number; // F12: effective large-bind cutoff + per-container override (0 = none)
  // F132: directories this image rebuilds by itself, which the operator may
  // trade away for archive size. Absent for an image that declares none.
  regenerable?: RegenerablePath[]; exclude_regenerable?: boolean;
  // F151: whether this container's app-native export directory is emptied after
  // a successful capture.
  cleanup_export?: boolean;
  // F206: whether an in-place restore of this container demands fresh proof of
  // the password, what the derived default would be (critical / require-write-
  // only), and whether an operator has recorded an explicit choice.
  restore_step_up?: boolean;
  restore_step_up_default?: boolean;
  restore_step_up_set?: boolean;
  // F163: whether this container refuses to be backed up unless write-only
  // encryption is on, and — when it does — the reason its next run would be
  // refused right now. An empty reason means the next run proceeds.
  require_write_only?: boolean;
  write_only_blocked_reason?: string;
  // F205: the detected database engine ("redis", "postgres", …), so the Redis
  // password field is offered only where it applies, and WHETHER a password is
  // recorded. The value itself is never returned by any endpoint.
  db_engine?: string;
  redis_auth_set?: boolean;
  restore_health_timeout_seconds?: number; restore_health_timeout_override?: number; // F30: effective restore health-gate timeout + per-container override (0 = none)
  // F184: uid:gid this container's restored data is pinned to ("" = follow what
  // the image declares), and what the image declares, so the field can show what
  // an override would be replacing.
  restore_ownership?: string; run_as?: string;
}
export interface Node {
  id: string; name: string; cluster: string; transport: string; address: string;
  status: string; last_seen: number; created_at: number;
  summary?: NodeSummary; reachable: boolean; error?: string;
  host_key_changed?: boolean; // F67: connection refused on an SSH pin mismatch — possible reinstall or MITM
  streaming?: boolean; // #40: a bulk transfer holds this node, so its figures are paused, not stale
  ssh_auth?: string; // "key" | "password" — which SSH credential this node uses (edit-form hint; never the secret)
}
// Credential fields accepted by add/update/test-node. auth_method selects the SSH
// credential ("key" | "password"); a blank field on edit keeps the stored value.
type NodeCreds = { secret?: string; passphrase?: string; password?: string; auth_method?: string };
export interface Mount { type: string; name: string; source: string; destination: string; rw: boolean; }
export interface Container {
  id: string; name: string; image: string; image_id: string; state: string;
  status: string; ports: string; stack: string; service: string; created_at: number; mounts: Mount[];
}
export interface Stack { name: string; working_dir: string; config_files: string; containers: Container[]; running: number; total: number; }
export interface StackInfo { name: string; services: number; running: number; backed_up: number; }
// A container or stack the operator told DockBack to leave alone on one server:
// no never-backed-up or stale warnings, no alerts, skipped by whole-server
// backups. `present` turns false once it is gone; it stays listed so the choice
// can be undone. `detail` is a container's image or a stack's service count.
export interface IgnoredItem { kind: "container" | "stack"; name: string; present: boolean; container_id?: string; detail?: string; }
export interface OrphanVolume { name: string; driver: string; bytes: number; } // F23: named volume with no container
// F40: node connection-health history.
export interface NodeHealthRow { ts: number; reachable: boolean; error?: string; }
// F105: a node's hardware + live host utilisation, read by a read-only probe.
// Every field is optional by design — a VM, a NAS or a locked-down firmware
// simply reports less, and absence renders as "not reported", never a zero.
export interface MachineIdentity {
  vendor?: string; product?: string; version?: string;
  board_vendor?: string; board?: string;
  bios_version?: string; bios_date?: string; chassis_type?: string; virtualized?: boolean;
}
export interface MachineCPU {
  model?: string; cores: number; threads: number; cache_kb?: number;
  mhz_now: number; mhz_min: number; mhz_max: number;
  usage_pct: number; load1: number; load5: number; load15: number; virtual?: boolean;
}
export interface MachineMemory {
  total_bytes: number; available_bytes: number; used_bytes: number;
  cached_bytes: number; swap_total_bytes: number; swap_used_bytes: number;
}
// `name` is the resolved model where the host could tell us (the NVIDIA driver's
// own /proc entry, or the host's pci.ids). Empty means show vendor + device id.
export interface MachineGPU {
  card?: string; // DRM node (card0/card1) — the identity key for dual identical GPUs
  name?: string; vendor: string; vendor_id: string; device_id: string; driver?: string;
  // Live telemetry from the driver's hwmon node. All optional: a driver that
  // doesn't publish one has no value, and the UI omits the line rather than
  // showing a zero. usage/VRAM are amdgpu-only — the proprietary NVIDIA driver
  // publishes neither outside nvidia-smi.
  temp_c?: number; fan_rpm?: number; fan_pct?: number; power_w?: number;
  usage_pct?: number; vram_total_bytes?: number; vram_used_bytes?: number;
  pci_addr?: string;
  telemetry?: string; // "hwmon" | "nvidia-smi" — where the live numbers came from
}
export interface MachineFS {
  device: string; mount: string; total_bytes: number; used_bytes: number; free_bytes: number;
}
export interface MachineDisk {
  name: string; model?: string; size_bytes: number; rotational: boolean;
  read_bps: number; write_bps: number;
  // Filesystems mounted from this disk. Absent when nothing is mounted from it —
  // a spare or unformatted drive, which is a real answer rather than a gap.
  filesystems?: MachineFS[];
  fs_total_bytes?: number; fs_used_bytes?: number; fs_free_bytes?: number;
}
export interface MachineNet {
  name: string; speed_mbps: number; state?: string; rx_bps: number; tx_bps: number;
}
export interface MachineSensor { label: string; celsius: number; }
export interface MachinePlatform {
  os_name?: string; kernel?: string; arch?: string;
  uptime_seconds?: number; battery_pct?: number; on_ac?: boolean;
}
export interface MachineInfo {
  identity: MachineIdentity; cpu: MachineCPU; memory: MachineMemory;
  gpus: MachineGPU[]; disks: MachineDisk[]; nets: MachineNet[]; sensors: MachineSensor[];
  filesystems: MachineFS[]; // every mounted block-backed filesystem on the host
  platform: MachinePlatform; probed_at: number;
  complete?: boolean; // the probe ran to its end marker; false = cut short
  partial?: boolean; warnings: string[];
}
export interface MachineResp {
  node_id: string; node_name: string;
  host?: HostInfo;
  machine?: MachineInfo;
  docker: { version?: string; containers: number; running: number; images: number; volumes: number };
  error?: string;   // the probe failed; Docker-side facts are still present
  // How old the shown reading is, and whether a probe is running right now. The
  // endpoint is stale-while-revalidate: it answers from cache and refreshes in
  // the background, so the page never waits on a container starting on a host.
  age_seconds: number;
  refreshing: boolean;
  // A FIRST reading is being taken and there is nothing to show yet. Distinct
  // from an error: a scan in progress is not a scan that failed.
  scanning: boolean;
  ttl_seconds: number;
}

export interface NodeHealthResp { rows: NodeHealthRow[]; uptime_pct_7d: number; uptime_pct_30d: number; days: number; }
// F19: parsed dockback.* label policy for a container (present only when label_managed).
export interface LabelPolicy {
  enable?: boolean;
  schedule?: string;
  pause_mode?: string;
  exclude_mounts?: string[];
  retention?: { keep_daily: number; keep_weekly: number; keep_monthly: number; keep_yearly: number; generations: number; autoprune: boolean };
}
export interface ContainerCounts { total: number; running: number; stopped: number; paused: number; restarting: number; }
export interface ContainerPage {
  containers: Container[]; stacks: Stack[]; total: number; page: number; page_size: number;
  counts?: ContainerCounts; cached_at?: number; reachable?: boolean;
}
export interface BackupPage { items: Backup[]; total: number; page: number; page_size: number; }
export interface ContainerQuery { q?: string; state?: string; page?: number; page_size?: number; }
export interface BackupQuery { node_id?: string; q?: string; status?: string; verified?: string; page?: number; page_size?: number; }
// BackupSummary (perf Fix 8): the compact digest LIST rows carry instead of the
// three raw JSON blobs. When `summary` is present the blob fields are empty —
// fetch GET /api/backups/{id} for the full row (the detail drawer does).
export interface BackupCopy { name: string; type?: string; status?: string; detail?: string; immutable?: boolean; }
export interface BackupSummary {
  partial: boolean; covered_skips?: number; covered_by?: string;
  incremental?: boolean; chain_depth?: number; image_bundled?: boolean;
  write_only?: boolean; // F86: sealed to an offline key — restoring needs the private key
  db_fallback?: boolean; // F103/F154: a database in this backup was copied as raw files rather than snapshotted consistently
  db_count?: number; image?: string; image_digest?: string; copies?: BackupCopy[];
}
export interface Backup {
  id: string; node_id: string; stack: string; target_name: string; status: string;
  verified: string; size_bytes: number; cipher_sha256: string; storage_key: string;
  manifest_json: string; verification_json: string; locations: string; error: string; created_at: number; completed_at: number;
  summary?: BackupSummary; // present on slim list rows (blobs empty)
  chain_dependents?: number; // F63: live deltas depending on this backup (full-row endpoint)
  last_verified_at?: number; // last successful scrub/re-verify (PLAN §9.4)
  key_mismatch?: boolean; // encrypted with a master key that isn't the current one (PLAN §3.3)
  confidence?: { grade: string; reasons: string[] }; // F50: restore-confidence grade A–F + what lowered it
  pinned?: boolean; // F2: "keep forever" — exempt from all pruning
  label?: string; // F2: user note, searchable
  duration_ms?: number; // F28: wall-clock run time, for run-to-run drift
  suspect?: string; // F69: reason this delta looked like a mass-change/ransomware event ("" / absent = clean)
}
// One persisted line of a run's log (F8).
export interface RunLogLine { seq: number; ts: number; level: string; msg: string; }
export interface RegenerablePath { path: string; label: string; cost: string; }
export interface MountInfo {
  destination: string; source: string; type: string; name?: string; driver?: string;
  rw: boolean; size_bytes: number; size_known: boolean; selected: boolean; reason?: string;
  unreadable?: boolean; // bind reader hit permission denied (UID/GID mismatch)
  fs_type?: string; // backing filesystem (zfs/btrfs/ext4/…), capability detect (§9.6)
  snapshotable?: boolean; // backing FS supports atomic snapshots (zfs/btrfs)
  shared_with?: string[]; // F83: other containers on the node mounting the same host Source
  covered_by?: string; // F83: container whose recent backups already capture this source (unselected binds)
}
// Critical-database low-RPO protection status (PLAN §9.7).
export interface CriticalStatus {
  enabled: boolean;
  rpo_seconds: number;
  is_database: boolean;
  engine?: string;
  measured_rpo_seconds: number; // age of newest VERIFIED backup, -1 if none
  target_met: boolean;
  paused: boolean;           // low-RPO auto-backups paused by the circuit-breaker
  verify_failures: number;   // consecutive verification failures
  // F34: Postgres point-in-time-recovery readiness, from the read-only probe
  // this endpoint runs on demand. pitr_checked is false for anything that is not
  // a running Postgres, and the panel renders nothing.
  pitr_checked: boolean;
  wal_level?: string;
  archive_mode?: string;
  max_wal_senders?: number;
  pitr_ready: boolean;
  pitr_detail?: string;
}
export interface AuditEntry { ts: number; actor: string; action: string; target: string; detail: string; }
export interface AuditPage { items: AuditEntry[]; total: number; page: number; page_size: number; }
export interface AuditQuery { q?: string; from?: number; to?: number; page?: number; page_size?: number; }
export interface Destination {
  id: string; name: string; type: string; enabled: boolean; status: string;
  created_at: number; free_bytes: number; total_bytes: number; used_bytes: number; reachable: boolean;
  // Capacity forecast (PLAN §9.13): trend + projected fill-up. days_to_full = -1
  // when not projected (no quota, shrinking, or too little history).
  growth_bytes_per_month: number; fill_date: number; days_to_full: number; history_points: number;
}
// Test-connection result, incl. the Object-Lock (WORM) preflight (PLAN §9.1).
export interface TestDestResult {
  ok: boolean; error?: string; free_bytes?: number; total_bytes?: number;
  object_lock_requested?: boolean;  // the config asked for immutability
  object_lock_enforced?: boolean;   // the bucket actually enforces Object Lock
  object_lock_checked?: boolean;    // we could determine it (false = e.g. append-only key)
  object_lock_detail?: string;
  object_lock_warning?: string;     // set when requested but NOT enforced
  host_key_fp?: string;             // F66 SFTP: the pinned (or to-be-pinned) SSH host key fingerprint
}
export interface AppBackup {
  file: string; created_at: number; app_version: string; key_fingerprint: string;
  nodes: number; backups: number; destinations: number; size_bytes: number;
}
export interface AppDest { id: string; name: string; type: string; enabled: boolean; reachable: boolean; }
// Automatic application (control-plane) backup schedule (F4).
export interface AppBackupSchedule {
  enabled: boolean; kind: string; time: string; weekday: number; monthday: number;
  keep: number; push_external: boolean;
}
export interface ScheduleTarget { node_id: string; container_id?: string; container_name?: string; stack?: string; consistent?: boolean; }
export interface Schedule {
  enabled: boolean; kind: string; time: string; weekday: number; monthday: number;
  cron: string; targets: ScheduleTarget[]; include_stopped?: boolean;
  // F27: per-schedule offsite destination override. When destinations_explicit is
  // true, runs mirror only to `destinations` (empty = local-only); otherwise they
  // use the effective policy destinations.
  destinations_explicit?: boolean; destinations?: string[];
}
// A named backup schedule (F6): a Schedule with an id/name plus server-computed
// run times. id is "" for a draft not yet saved.
export interface NamedSchedule extends Schedule {
  id: string; name: string; last_run?: number; next_run?: number;
}
// scheduleBody strips the read-only, server-computed fields (last_run/next_run)
// before writing a schedule back. The API rejects unknown fields, so sending the
// object round-tripped from a GET verbatim would 400 ("invalid request").
function scheduleBody(sc: Schedule): Schedule {
  const { last_run: _lr, next_run: _nr, ...body } = sc as NamedSchedule;
  void _lr; void _nr;
  return body;
}
export interface NotifyConfig {
  gotify: { enabled: boolean; on_success: boolean; on_failure: boolean; min_severity?: string; url: string; token: string; priority: number };
  email: { enabled: boolean; on_success: boolean; on_failure: boolean; min_severity?: string; host: string; port: number; username: string; password: string; from: string; to: string };
  webhook: { enabled: boolean; on_success: boolean; on_failure: boolean; min_severity?: string; url: string };
  heartbeat: { enabled: boolean; url: string; interval_minutes: number };
  // F200: publish the audit trail's chain head off-host on a cadence.
  head_beacon: { enabled: boolean; interval_hours: number };
  digest: { enabled: boolean; time: string; success_mode: string; include_dr?: boolean }; // F15: daily summary; F60: DR confidence
}
export const emptyNotifyConfig: NotifyConfig = {
  gotify: { enabled: false, on_success: false, on_failure: true, min_severity: "", url: "", token: "", priority: 8 },
  email: { enabled: false, on_success: false, on_failure: true, min_severity: "", host: "", port: 587, username: "", password: "", from: "", to: "" },
  webhook: { enabled: false, on_success: false, on_failure: true, min_severity: "", url: "" },
  heartbeat: { enabled: false, url: "", interval_minutes: 0 },
  head_beacon: { enabled: false, interval_hours: 24 },
  digest: { enabled: false, time: "09:00", success_mode: "per_backup" },
};
export interface Policy {
  destinations: string[]; generations: number; autoprune: boolean; schedule: Schedule;
  prune_schedule?: Schedule; // F17: fleet-wide retention sweep on its own cadence (no targets)
  keep_daily?: number; keep_weekly?: number; keep_monthly?: number; keep_yearly?: number;
  autoclean_missing_days?: number; // F13: drop a scheduled target after its container is missing this many days (0 = never)
}
// Per-node policy override (PLAN §4.13). Each group of fields is gated by an
// Override* flag; when false those fields inherit the global policy.
export interface PolicyOverride {
  override_destinations: boolean; destinations: string[];
  override_retention: boolean; generations: number;
  keep_daily: number; keep_weekly: number; keep_monthly: number; keep_yearly: number; autoprune: boolean;
  override_frequency?: boolean; min_interval_hours?: number;
}
export interface NodePolicy { global: Policy; override: PolicyOverride; effective: Policy; }
// A registered cluster (F104). Membership still lives on the node's `cluster`
// field (which holds the NAME); this record adds identity — description, colour,
// rename and delete — plus the cluster tier of the policy chain.
export interface Cluster { name: string; description: string; color: string; created_at: number; nodes: number; }
// The cluster tier on its own: the global default, this cluster's override, and
// what its members inherit BEFORE their own node/container overrides apply.
export interface ClusterPolicy { global: Policy; override: PolicyOverride; resolved: Policy; nodes: number; }
// One server as shown on the cluster screen — identity only, never a credential.
export interface ClusterMember { id: string; name: string; address: string; transport: string; cluster: string; reachable: boolean; }
// Per-container override (PLAN §4.2 granular control): the container's own
// override plus the retention it would inherit (node → global) otherwise.
export interface ContainerPolicy {
  name: string;
  override: PolicyOverride;
  inherited: { generations: number; keep_daily: number; keep_weekly: number; keep_monthly: number; keep_yearly: number; autoprune: boolean };
  // Every schedule, on or off, that backs this container up, and how it reaches
  // it: by name, through its compose stack, or with the rest of the server.
  schedules: ContainerSchedule[];
}
export interface ContainerSchedule extends NamedSchedule { covers: "container" | "stack" | "node"; }
// A schedule that backs a compose project up: as the stack itself (consistent =
// every service in one quiesce window), by naming some of its services, or with
// the whole server. `own` marks the stack's own schedule, the one its page edits.
export interface StackSchedule extends NamedSchedule {
  covers: "stack" | "container" | "node";
  consistent?: boolean;
  services?: string[];
  own: boolean;
}
// When a stack's own schedule runs, and whether it snapshots every service at once.
export interface StackScheduleTiming {
  enabled: boolean; kind: string; time: string; weekday: number; monthday: number; cron: string; consistent: boolean;
}
export interface RetentionPreview {
  active: boolean; total_prune: number; total_prune_bytes: number;
  targets: { node_id: string; node_name: string; target: string; keep: number; prune: number; prune_bytes: number;
    items: { id: string; created_at: number; size_bytes: number; verified: string; action: string; pinned?: boolean; label?: string }[] }[];
}

// Insights aggregate (B6): trends/analytics computed from data already collected.
export interface DestSample { ts: number; total_bytes: number; used_bytes: number; }
export interface InsightsDay { day: number; total: number; verified: number; failed: number; }
export interface InsightsDest {
  id: string; name: string; type: string; total_bytes: number; used_bytes: number;
  days_to_full: number; fill_date: number; growth_bytes_per_month: number; history: DestSample[];
}
export interface InsightsDrill { backup_id: string; ok: boolean; ran_at: number; detail: string; target?: string; stack?: string; node_name?: string; }
export interface InsightsRPOItem { node: string; container: string; target_seconds: number; measured_seconds: number; met: boolean; }
// Per-container backup-size trend (F11).
export interface SizePoint { ts: number; bytes: number; }
export interface InsightsGrowth {
  node: string; node_id: string; container: string; stack?: string;
  latest_bytes: number; growth_bytes_per_month: number; points: SizePoint[];
}
export interface ContainerSizes { name: string; points: SizePoint[]; growth_bytes_per_month: number; latest_bytes: number; }
export interface Insights {
  fleet: { backup_success_rate: number; backups_30d: number; backups_verified: number };
  backups_daily: InsightsDay[];
  destinations: InsightsDest[];
  drills: { total: number; passed: number; failed: number; recent: InsightsDrill[] };
  rpo: { total: number; meeting: number; breaching: number; containers: InsightsRPOItem[] };
  top_growth: InsightsGrowth[];
}

// Disaster-Recovery runbook (C3): auto-composed from manifests, drills, key
// status, and app-backup — the restore order + gotchas you need at 2 a.m.
// F16: master-key rotation outcome.
export interface KeyRotateResult {
  status: string; new_fingerprint: string;
  backups: { rewrapped: number; skipped: number; failed: number };
  app_backups: { rewrapped: number; skipped: number; failed: number };
  remote_app_backups?: { rewrapped: number; failed: number }; // F72: archives on app-backup destinations
  nodes_resealed: number; destinations_resealed: number; totp_resealed: number;
  app_dests_resealed?: number; // F72: app-backup destination credentials
  warnings: string[]; message: string;
  fresh_app_backup?: { file: string }; // F53: a fresh app-backup taken on the new key (absent if it failed — see warnings)
}

// F45: a scoped automation token's metadata (never the token value).
// F202: one signed-in session as the owner sees it. There is deliberately no
// token field — see internal/api/sessions.go.
export interface SessionDevice {
  id: string; current: boolean; created_at: number; last_seen: number; expires_at: number;
  ip: string; user_agent: string; device: string;
}
export interface ApiToken { id: string; name: string; scopes: string; created_at: number; last_used: number; expires_at: number; allowed_cidrs?: string; } // expires_at (F65): unix seconds, 0 = never; allowed_cidrs (F201): "" = any source

// F77: layout-migration job progress.
export interface MigrateLayoutStatus { running: boolean; total: number; moved: number; skipped_worm: number; failed: number; remaining: number; started_at?: number; }

// F46: a persisted operational alert.
export interface Alert { id: number; ts: number; kind: string; severity: string; title: string; message: string; dedup: string; acked: boolean; }
export interface OpsLogLine { ts: number; level: string; msg: string; }

export interface RunbookLoc { name: string; type: string; status?: string; detail?: string; immutable?: boolean; }
export interface RunbookSkipped { destination: string; type: string; reason: string; bytes?: number; covered_by?: string; } // covered_by (F83): captured via another container's backups — listed, not PARTIAL
// F82: one row of the stack-restore plan preview — mirrors the backend's exact
// restore selection/order so the dialog shows what WILL happen before confirm.
export interface StackPlanEntry {
  order: number; service: string; backup_id: string; target_name: string;
  created_at: number; verified: string; data_tier: boolean;
  partial: boolean; incremental: boolean; group_id?: string;
  // F94: what the TARGET host cannot honor for this service on a cross-host
  // restore. Advisory — the plan is a preview, never a block.
  portability?: string[];
  // Step 27: devices the target verifiably lacks. These DO stop the restore
  // until the operator confirms them.
  missing_devices?: string[];
  // F174: a problem in the service's OWN configuration that would stop its
  // restore — a database whose environment cannot initialize an empty data
  // directory. Said here because by the time the restore hits it, the services
  // before it have already been restored.
  restore_block?: string;
  // F177: environment variables this service records an address in. Names only
  // — the manifest carries no values — and only on a cross-host restore, where
  // they are what a move invalidates.
  address_vars?: string[];
  // F209: this member is sealed to the offline keypair, so the restore needs the
  // private key. Flagged in the plan so the dialog can ask BEFORE the operator
  // commits, instead of the restore stopping at this service with the ones
  // before it already overwritten.
  write_only?: boolean;
  // F213: a member of an application whose services are only meaningful together
  // (F146). It cannot be deselected — restoring a subset of one produces a
  // healthy-looking deployment that does not work.
  atomic?: boolean;
  // F81: the host paths this service binds that the TARGET does not have, and
  // what the restore will do about each. Absent when the target already has them
  // all, and for a backup taken before bind roots were recorded.
  bind_plan?: BindSourcePlan[];
}
// F211: one row of the whole-node restore plan — what the executor will do, in
// the order it will do it. `blocked` is why a service cannot be restored by a
// whole-node run (a write-only backup it has no key for, a database whose
// environment cannot re-initialise); empty when it can.
export interface NodeRestorePlanEntry {
  order: number; label: string; container: string; stack?: string; service?: string;
  role: string; backup_id: string; created_at: number; verified: string;
  write_only?: boolean; blocked?: string;
  // F146/F227: a member of an application whose services are only meaningful
  // together. It cannot be deselected on its own — the engine refuses such a set,
  // and the selector must not offer what would be refused after the confirm.
  atomic?: boolean;
}
// F214: one copy this stack's members can be read from, with how many of them
// actually hold it — so the picker can say "3 of 5 services" instead of implying
// a destination covers the whole stack.
// F215: the cross-host choices a route (project + target machine) needed last
// time. Hostnames, IPs and filesystem paths only — never the offline key.
export interface CrossRestoreDefaults {
  remap_ip?: boolean; remap_from_ip?: string; remap_to_ip?: string;
  remap_domain?: boolean; remap_from_domain?: string; remap_to_domain?: string;
  remap_path?: boolean; remap_from_path?: string; remap_to_path?: string;
  reconstruct_host?: boolean; host_base_dir?: string;
  new_site_address?: string; new_upstream_address?: string;
  at?: number;
}
export interface StackSourceCopy { id: string; name: string; type: string; services: number; }
// BindSourcePlan is one host path the target is missing, and the restore's
// intention for it. The five actions are the same words the run log uses, so
// what an operator reads before confirming is what they read afterwards.
export interface BindSourcePlan {
  source: string;      // host path on the TARGET, after the path remap
  destination: string; // container path it feeds
  kind?: string;       // "dir" | "file"
  action: "fill" | "create-empty" | "write-file" | "placeholder" | "manual";
  note: string;
}
// Finding is one thing DockBack discovered about the SOURCE while backing it up
// — a config file that is present but inert, one host path bound at two
// destinations, a healthcheck whose success condition is satisfied by the
// failure state. Reported, never silently reproduced or fixed.
//
// Carried inside the backup's manifest_json, so it needs no endpoint of its own.
export interface Finding {
  code: string;                              // stable slug, e.g. "duplicate-mount-target"
  severity: "info" | "warn" | "danger";
  message: string;                           // operator-facing sentence, names the fix
  subject?: string;                          // path / env KEY (never a value) / image
}

// #37: what a restore will write, and what the destination filesystem has left.
// Per FILESYSTEM, never per host — R5's machine had 74 GB on root and 2.2 TB one
// mount point away, and a host aggregate would have called it roomy.
export interface RestoreCapacity {
  path: string;
  filesystem?: string;
  mount_point?: string;
  payload_bytes: number;
  free_bytes: number;
  total_bytes?: number;
  // What would be left. The number an operator actually decides on.
  after_bytes: number;
  margin_bytes: number;
  // Set when the payload was derived from the stored archive rather than
  // measured at capture, so the UI never presents a guess as a measurement.
  estimated?: boolean;
  refuse: boolean;
}

export interface StackRestorePlan {
  services: StackPlanEntry[]; target_dirs: Record<string, string>;
  // #37: the capacity report, shown before the operator commits. Absent when
  // there is nothing honest to show — no measurable payload, or a filesystem
  // that could not be read.
  capacity?: RestoreCapacity;
  // The copies the planned members actually hold. Only these are offered.
  source_copies?: StackSourceCopy[];
  // F216: members of this stack that have NO backup, so a restore cannot bring
  // them back. Computed server-side from the cached inventory, which is what
  // makes it survive a source node that is down — the case it exists for.
  missing_members?: string[];
  // F146: set when this selection would be REFUSED — the application's services
  // are only meaningful together and the chosen backups do not form one complete
  // snapshot. Shown before the operator commits, because the fix is to pick a
  // different snapshot and this dialog is the only place that choice exists.
  atomic_block?: string;
  // F149: the anchor application's own preconditions — the things that decide
  // whether a restored stack actually works (addresses, ports, agents), which
  // were previously only ever shown in the single-backup drawer.
  app_preconditions?: AppPreconditions;
}
// F80: one row of the stack-backup panel — a service's effective per-container
// backup settings (the same values its own page reads/writes).
export interface StackServiceOptions {
  container_id: string; name: string; service?: string; state: string;
  is_database: boolean; engine?: string; pause_mode: string;
  backup_options: SavedBackupOptions;
  mounts_selected: number; // -1 = size-based default selection
  mounts_total: number;
  export_available: boolean;
  covered_binds?: number; // F83: shared binds captured by another container
  version_skew?: string; // #23: shares a directory with a container on a different build of the same image
}
export interface RunbookService {
  order: number; container: string; stack?: string; service?: string; role: string;
  engine?: string; image?: string; image_digest?: string; image_bundled?: boolean; extensions?: string[];
  volumes: number; databases: number; skipped_mounts?: RunbookSkipped[];
  locations: RunbookLoc[]; last_backup_at: number; last_drill_at?: number;
  last_drill_ok?: boolean; partial: boolean; notes?: string[];
  standby_node?: string; standby_ok?: boolean; standby_at?: number; // F62: cross-node standby readiness
  config_drift?: boolean; // F73: live config no longer matches this backup's captured config
}
// F62: a container's pilot-light standby rehearsal config + last result.
export interface Standby {
  node_id: string; target: string; standby_node: string; interval_days: number;
  last_run: number; last_ok: boolean; last_detail: string; boot_ms: number;
}
// F11: pre-restore image-availability verdict for a single backup.
// F207: one host the outbound allow-list would have refused while audit mode is
// on. `configured` is the safety-relevant field: false means nothing DockBack is
// configured to reach uses this host, so it arrived some other way (a redirect, a
// DNS rebind) and must not be bulk-added to the allow-list.
export interface EgressAuditEntry {
  host: string; first_seen: number; last_seen: number; count: number;
  source?: string; configured: boolean; allowed_now: boolean;
}
export interface EgressAudit {
  audit_mode: boolean; entries: EgressAuditEntry[]; unconfigured: number; truncated: boolean;
}
export interface RestoreReadiness {
  image_ok: boolean; has_image_tar: boolean; image?: string; image_digest?: string; detail: string;
  // F94: what the TARGET host cannot honor for a cross-host restore. Advisory —
  // it never blocks; the operator may know something DockBack cannot.
  portability?: string[];
  // F95: what the image about to run declares that this backup's configuration
  // does not provide. Empty when the exact recorded image is still available.
  image_drift?: string[];
  // F174: a problem in the container's OWN configuration that would stop this
  // restore — a database whose environment cannot initialize an empty data
  // directory, which restoring from a dump requires it to do.
  restore_block?: string;
  // F177: environment variables this container records an address in, names
  // only, on a cross-host restore.
  address_vars?: string[];
  // F110: what the APPLICATION needs from wherever it lands. Portability asks
  // whether the host can run the container; this asks whether the app's data
  // will still mean anything once it does — the mount destinations its database
  // has baked into its own rows, and any path that must be on local disk.
  app_preconditions?: AppPreconditions;
  // F206: an in-place restore of this container will ask for the password.
  // Reported before the operator commits, so it is a stated condition rather
  // than a surprise when the request comes back. Never applies to a copy.
  step_up_required?: boolean;
  // F119: other containers on the target node that use the same host
  // directories this restore writes into.
  shared_data?: string[];
  // F144: host ports this container publishes that something on the target
  // already holds. Advisory — the holder may be what this restore replaces.
  port_conflicts?: string[];
  // F143: the TLS certificates travelling inside this backup, so an expired one
  // is visible before the restore rather than from a browser afterwards.
  certificates?: CertRef[];
}
// F143: one TLS certificate captured inside a backup. Public half only — the
// covered hostnames are counted, never listed, and the private key's presence is
// recorded without its contents ever being read.
export interface CertRef {
  path: string;
  issuer?: string;
  not_before?: string;
  not_after?: string;
  names?: number;
  has_key?: boolean;
}
// F110: application-level restore preconditions, present only for images
// DockBack carries a profile for.
export interface AppPreconditions {
  app: string;
  data_paths?: string[];
  notes?: string[];
  // F114: true when an unmet precondition makes the app UNREACHABLE rather than
  // merely imperfect — styled as a warning rather than an informational note.
  blocking?: boolean;
  // F114: label for the optional new-address field. Absent for an app that
  // records its address nowhere, in which case the field is not shown.
  address_prompt?: string;
  // F160: label for the optional field naming a new address for a service this
  // application DEPENDS ON. Distinct from address_prompt — that one is where the
  // app itself is reached, this one is where the thing it talks to lives, and
  // they break at opposite moments.
  upstream_prompt?: string;
}
export interface ArchiveEntry { name: string; size: number; dir: boolean; } // F21: one browsable file in a backup
// F102: one archive a Scan & adopt did NOT take, and why. `key_fingerprint` names
// the master key the archive belongs to when that could be read — the whole point
// being that the operator can tell a foreign-key skip from a harmless duplicate.
export interface AdoptSkip { key: string; reason: string; key_fingerprint?: string; }
// F101: a named, reusable app-native export recipe. `match` is a lowercase
// substring tested against the image ref; empty means library-only (applied by
// hand, never claiming an image on its own). `builtin` entries ship with
// DockBack and are read-only.
export interface ExportPreset {
  id: string; name: string; match: string;
  dir: string; export_cmd: string; import_cmd: string; user: string;
  // F152: optional post-import check; a non-zero exit fails the restore.
  verify_cmd?: string;
  builtin?: boolean;
}
// F100: one in-flight restore. `id` is the run id the log stream and the cancel
// endpoint both key on: the backup id, "stack:<project>" or "node:<nodeId>".
export interface RunningRestore { id: string; label: string; started_at: number; }
// F70 universal file index: cross-backup file search + generation diff.
export interface FileSearchHit { backup_id: string; created_at: number; target: string; path: string; size: number; mtime: number; }
export interface FileSearchResp { results: FileSearchHit[]; searched: number; skipped: number; truncated?: boolean; }
export interface BackupDiffEntry { path: string; size: number; prev_size?: number; }
export interface BackupDiffResp {
  a: { id: string; created_at: number }; b: { id: string; created_at: number };
  added: BackupDiffEntry[]; changed: BackupDiffEntry[]; deleted: string[];
  counts: { added: number; changed: number; deleted: number }; truncated?: boolean;
}
export interface RunbookNode { node_id: string; node_name: string; reachable: boolean; services: RunbookService[]; }
export interface Runbook {
  generated_at: number; app_version: string;
  key: { fingerprint: string; acknowledged: boolean; ephemeral: boolean };
  app_backup: { exists: boolean; newest_at: number; count: number };
  destinations: { name: string; type: string; enabled: boolean }[];
  nodes: RunbookNode[];
  summary: { services: number; databases: number; partial_backups: number; undrilled: number };
}

// RestoreCompatError is thrown by api.restore on a 409 from the restore-time
// engine/extension compatibility gate (F10): the target image's DB engine or
// vector-extension family differs from the backup's dump. The caller shows the
// warning and, if the user accepts, retries with confirm_incompatible.
export class RestoreCompatError extends Error {
  compat_warning: string;
  extensions: string[];
  constructor(warning: string, extensions: string[]) {
    super(warning);
    this.name = "RestoreCompatError";
    this.compat_warning = warning;
    this.extensions = extensions;
  }
}

// TestClone (F219) is an isolated copy brought up to prove a backup, carrying its
// own expiry — the reaper removes it and the volumes Docker made for it.
export interface TestClone {
  id: string; name: string; node_id: string; state: string;
  expires_at: number; // unix seconds; the container itself is the record
}

// RestoreVerifyFailedError is thrown by api.restore on a 409 from the known-bad
// gate (F218): this backup's LAST VERIFICATION FAILED, so it did not re-read
// intact. The caller shows the refusal and, if the user accepts the risk,
// retries with confirm_unverified. Distinct from RestoreCompatError because the
// two say different things and only one of them is about the archive itself.
// Step 27: a cross-host restore stopped because the target lacks devices or
// shared networks the containers need. The operator may know better; the
// caller confirms and retries with the matching confirm_missing_* flags.
export class RestoreNeedsConfirmError extends Error {
  devices: string[];
  networks: string[];
  constructor(message: string, devices: string[], networks: string[]) {
    super(message);
    this.name = "RestoreNeedsConfirmError";
    this.devices = devices;
    this.networks = networks;
  }
}

export class RestoreVerifyFailedError extends Error {
  verify_failed = true as const;
  constructor(message: string) {
    super(message);
    this.name = "RestoreVerifyFailedError";
  }
}

// Session-gate memo (perf Fix 4): Protected validates the session ONCE per
// page load instead of on every route change. Lives here (not main.tsx) so
// Layout's logout can reset it without an import cycle. An expired session
// needs no reset: the 401 handler below does a full location.href load, which
// re-initializes module state anyway.
export const sessionGate = { checked: false };

// Exported for unit tests (api.test.ts): the CSRF cookie regex is the only thing
// standing between the app and every mutation failing "csrf check failed", so it
// is worth pinning directly. Runtime behaviour is unchanged.
export function csrf(): string {
  // Matches the plain cookie (HTTP) and the __Host- prefixed one (behind TLS).
  const m = document.cookie.match(/(?:^|;\s*)(?:__Host-)?dback_csrf=([^;]+)/);
  return m ? decodeURIComponent(m[1]) : "";
}

// StepUpError (F64): a security-critical endpoint wants the account password
// re-entered ("sudo mode") — the session itself is still valid, so this must
// NOT bounce to /login. The UI catches it, prompts, and retries with credentials.
export class StepUpError extends Error {
  step_up_required = true as const;
  totp_required: boolean;
  constructor(message: string, totpRequired: boolean) {
    super(message);
    this.totp_required = totpRequired;
  }
}

// What an error body may carry. Anything else on it is passed through untouched.
type ResponseBody = {
  error?: string;
  step_up_required?: boolean;
  totp_required?: boolean;
  [key: string]: unknown;
};

/**
 * parseBody reads a response that is SUPPOSED to be JSON but might not be.
 *
 * During an outage the body is whatever sits in front of the app: a reverse
 * proxy's HTML error page, a gateway's plain text, an empty 502. Letting
 * JSON.parse throw replaced the server's status with "Unexpected token '<'" —
 * a message about the parser, shown at exactly the moment the operator needs
 * to know what actually failed.
 */
function parseBody(text: string): ResponseBody | null {
  if (!text) return null;
  try {
    const parsed = JSON.parse(text);
    return parsed && typeof parsed === "object" ? (parsed as ResponseBody) : null;
  } catch {
    return null;
  }
}

/**
 * httpError builds the clearest message available: the app's own error text,
 * else the HTTP reason phrase, else the bare status.
 *
 * The last fallback is not theoretical — HTTP/2 carries no reason phrase, so
 * `statusText` is an empty string for every response behind a modern proxy,
 * which is precisely where a non-JSON error body comes from.
 */
function httpError(res: Response, data: ResponseBody | null): Error {
  const message = data?.error || res.statusText || `HTTP ${res.status}`;
  const devices = Array.isArray(data?.missing_devices) ? (data.missing_devices as string[]) : [];
  const networks = Array.isArray(data?.missing_networks) ? (data.missing_networks as string[]) : [];
  if (res.status === 409 && (devices.length > 0 || networks.length > 0)) return new RestoreNeedsConfirmError(message, devices, networks);
  return new Error(message);
}

async function req<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (method !== "GET") headers["X-CSRF-Token"] = csrf();
  const res = await fetch(path, {
    method, headers, credentials: "same-origin",
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401) {
    // F64: distinguish "re-authenticate for this action" from "session dead".
    const data = parseBody(await res.text());
    if (data?.step_up_required) throw new StepUpError(data.error || "re-authentication required", !!data.totp_required);
    if (!location.pathname.startsWith("/login")) location.href = "/login";
    throw new Error("unauthenticated");
  }
  const data = parseBody(await res.text());
  if (!res.ok) throw httpError(res, data);
  return data as T;
}

// Optional step-up credentials (F64) attached to security-critical calls.
export type StepUpCreds = { password?: string; code?: string };

// A backup's restore-drill outcome (PLAN §9.4): the last sandbox test-restore of
// that specific backup. One row per backup_id; absent means never drilled.
export interface DrillStatus {
  backup_id: string;
  ok: boolean;
  detail: string;
  ran_at: number;
}

export const api = {
  me: () => req<{ username: string; expires_at: number; idle_expires_at: number; extended: boolean }>("GET", "/api/me"),
  extendSession: () => req<{ expires_at: number; idle_expires_at: number; extended: boolean }>("POST", "/api/session/extend", {}),
  // Slides the sliding idle-timeout window; called (throttled) on real user
  // interaction. Background polling deliberately does NOT call this.
  sessionActivity: () => req<{ expires_at: number; idle_expires_at: number; extended: boolean }>("POST", "/api/session/activity", {}),
  // Sign out every other device/session, keeping this browser signed in.
  revokeOtherSessions: () => req<{ revoked: number }>("POST", "/api/session/revoke-others", {}),
  // F202: the signed-in devices, so an unfamiliar one can be ended on its own.
  // `id` is a non-secret handle — the session token itself never leaves the
  // server, and never travels in a URL.
  listSessions: () => req<{ sessions: SessionDevice[] }>("GET", "/api/session/list"),
  revokeSession: (id: string) => req<{ revoked: number }>("POST", "/api/session/revoke", { id }),
  // Login uses its own fetch (not req) so server messages surface on the Login
  // page, and so the "two-factor required" signal can be returned (not thrown).
  login: async (username: string, password: string, code?: string): Promise<{ status: "ok" } | { status: "totp_required" }> => {
    const res = await fetch("/api/login", {
      method: "POST", credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username, password, ...(code ? { code } : {}) }),
    });
    const data = parseBody(await res.text());
    if (res.ok) return { status: "ok" };
    if (res.status === 401 && data?.totp_required) return { status: "totp_required" };
    throw httpError(res, data);
  },
  twofaStatus: () => req<{ enabled: boolean; recovery_remaining: number }>("GET", "/api/account/2fa"),
  twofaBegin: () => req<{ secret: string; otpauth_uri: string }>("POST", "/api/account/2fa/begin", {}),
  twofaEnable: (code: string) => req<{ recovery_codes: string[] }>("POST", "/api/account/2fa/enable", { code }),
  // Both factors: the code being removed has to confirm its own removal.
  twofaDisable: (password: string, code: string) =>
    req<{ status: string }>("POST", "/api/account/2fa/disable", { password, code }),
  logout: () => req<unknown>("POST", "/api/logout", {}),

  // Application (whole-app) backup & restore — distinct from container backups.
  appBackupInfo: () => req<{ key_fingerprint: string; db_bytes: number; last_drill_at: number; last_drill_ok: boolean; last_drill_detail: string; drill_interval_days: number }>("GET", "/api/app-backup/info"),
  // F58: prove the newest app-backup restores (integrity drill). Runs synchronously.
  appBackupDrill: () => req<{ ok: boolean; detail: string; last_drill_at: number }>("POST", "/api/app-backup/drill", {}),
  appBackupList: () => req<AppBackup[]>("GET", "/api/app-backup/list"),
  appBackupCreate: () => req<AppBackup>("POST", "/api/app-backup/create", {}),
  // Automatic application-backup schedule (F4).
  getAppBackupSchedule: () => req<{ schedule: AppBackupSchedule; next_run: number }>("GET", "/api/app-backup/schedule"),
  setAppBackupSchedule: (sch: AppBackupSchedule) => req<{ schedule: AppBackupSchedule; next_run: number }>("PUT", "/api/app-backup/schedule", sch),
  // F199: restoring the control plane is step-up gated like downloading it.
  appBackupRestoreLocal: (file: string, stepUp?: StepUpCreds) =>
    req<{ status: string; backup_created: number }>("POST", "/api/app-backup/restore-local", { file, ...stepUp }),
  appBackupDelete: (file: string) => req<{ status: string }>("DELETE", `/api/app-backup/${encodeURIComponent(file)}`),
  appBackupDownloadUrl: (file: string, ticket: string) =>
    `/api/app-backup/download?file=${encodeURIComponent(file)}&ticket=${encodeURIComponent(ticket)}`,
  // App-backup external destinations (separate from container destinations).
  appDestinations: () => req<AppDest[]>("GET", "/api/app-backup/destinations"),
  addAppDestination: (d: { name: string; type: string; config: Record<string, string> }) => req<{ id: string }>("POST", "/api/app-backup/destinations", d),
  toggleAppDestination: (id: string, enabled: boolean) => req<{ enabled: boolean }>("PUT", `/api/app-backup/destinations/${id}`, { enabled }),
  deleteAppDestination: (id: string) => req<unknown>("DELETE", `/api/app-backup/destinations/${id}`),
  appExternalBackup: () => req<{ file: string; pushed: string[]; failed: Record<string, string> }>("POST", "/api/app-backup/external", {}),
  appExternalList: (id: string) => req<{ key: string; name: string }[]>("GET", `/api/app-backup/destinations/${id}/backups`),
  appExternalRestore: (id: string, file: string, stepUp?: StepUpCreds) =>
    req<{ status: string }>("POST", `/api/app-backup/destinations/${id}/restore`, { file, ...stepUp }),
  appBackupRestore: async (file: File, stepUp?: StepUpCreds): Promise<{ status: string; backup_created: number }> => {
    const fd = new FormData();
    // Credentials first, so the server has them before it reads the archive.
    if (stepUp?.password) fd.append("password", stepUp.password);
    if (stepUp?.code) fd.append("code", stepUp.code);
    fd.append("file", file);
    const res = await fetch("/api/app-backup/restore", {
      method: "POST", credentials: "same-origin",
      headers: { "X-CSRF-Token": csrf() }, // multipart: let the browser set Content-Type
      body: fd,
    });
    const data = parseBody(await res.text());
    // This path builds its own request, so it has to raise the step-up signal
    // itself — req() cannot do it for us.
    if (res.status === 401 && data?.step_up_required) {
      throw new StepUpError(data.error || "re-authentication required", !!data.totp_required);
    }
    if (!res.ok) throw httpError(res, data);
    return (data ?? {}) as { status: string; backup_created: number };
  },
  changePassword: (current: string, newPassword: string) =>
    req<{ status: string; revoked_other_sessions: number }>("POST", "/api/account/password", { current, new: newPassword }),

  nodes: () => req<Node[]>("GET", "/api/nodes"),
  addNode: (n: Partial<Node> & NodeCreds) => req<Node>("POST", "/api/nodes", n),
  updateNode: (id: string, n: Partial<Node> & NodeCreds) => req<Node>("PUT", `/api/nodes/${id}`, n),
  testNode: (n: Partial<Node> & NodeCreds) =>
    req<{ ok: boolean; error?: string; summary?: NodeSummary; host_key_fingerprint?: string }>("POST", "/api/nodes/test", n),
  deleteNode: (id: string) => req<unknown>("DELETE", `/api/nodes/${id}`),
  restoreNode: (id: string) => req<{ status: string }>("POST", `/api/nodes/${id}/restore`),
  // Whole-node disaster-recovery restore: run the runbook's computed order (F6).
  // F210: the widest destructive action in the app. When any container on the
  // node is marked protected it demands fresh proof of the password, exactly as
  // restoring one of them on its own does — credentials in the body, never a URL.
  // F212: a whole-node rebuild can land on a DIFFERENT machine, with the same
  // cross-host grammar the stack dialog uses. Everything is in the body — the
  // offline key and the password have no business in a URL.
  restoreNodeAll: (id: string, opts?: StepUpCreds & {
    skip_blocked?: boolean; target_node?: string; private_key?: string;
    // F227: which services this run covers, by their plan label. Omitted = all.
    services?: string[];
    reconstruct_host?: boolean; host_base_dir?: string;
    remap_ip?: boolean; remap_from_ip?: string; remap_to_ip?: string;
    remap_domain?: boolean; remap_from_domain?: string; remap_to_domain?: string;
    remap_path?: boolean; remap_from_path?: string; remap_to_path?: string;
  }) => req<{ status: string }>("POST", `/api/nodes/${id}/restore-all`, opts ? { ...opts } : {}),
  // F211: the exact plan the whole-node restore will execute, read-only, so a
  // service it cannot handle is visible BEFORE the run instead of aborting it
  // part-way through.
  restoreNodeAllPlan: (id: string) =>
    req<{ services: NodeRestorePlanEntry[]; blocked: number }>("GET", `/api/nodes/${id}/restore-all/plan`),
  // Forget a node's pinned SSH host key so the next connect re-pins it (F1).
  resetHostKey: (id: string) => req<{ status: string }>("DELETE", `/api/nodes/${id}/hostkey`),
  nodeDetail: (id: string) => req<NodeDetail>("GET", `/api/nodes/${id}`),
  // F40: connection-health transitions + uptime %.
  nodeHealth: (id: string, days = 30) => req<NodeHealthResp>("GET", `/api/nodes/${id}/health?days=${days}`),
  containers: (id: string) => req<{ containers: Container[]; stacks: Stack[] }>("GET", `/api/nodes/${id}/containers`),
  // Server-side searched/filtered/paginated container list (PLAN §4.13).
  containersPage: (id: string, p: ContainerQuery = {}) =>
    req<ContainerPage>("GET", `/api/nodes/${id}/containers${qstr(p as Record<string, unknown>)}`),
  stackList: (id: string) => req<StackInfo[]>("GET", `/api/nodes/${id}/stacks`),
  // F23: named volumes with data but no container, and a direct backup of one.
  orphanVolumes: (id: string) => req<{ volumes: OrphanVolume[] }>("GET", `/api/nodes/${id}/orphan-volumes`),
  backupOrphanVolume: (id: string, name: string) => req<{ backup_id: string; status: string }>("POST", `/api/nodes/${id}/orphan-volumes/${encodeURIComponent(name)}/backup`, {}),
  // compression (F79): optional per-run override; omitted = each service's
  // remembered manual choice, exactly like a scheduled run.
  backupStack: (id: string, project: string, destinations?: string[], pauseMode?: string, consistent?: boolean, compression?: string) => req<{ status: string; count?: number; consistent?: boolean }>("POST", `/api/nodes/${id}/stacks/${encodeURIComponent(project)}/backup`, { ...(destinations !== undefined ? { destinations } : {}), ...(pauseMode ? { pause_mode: pauseMode } : {}), ...(consistent ? { consistent: true } : {}), ...(compression ? { compression } : {}) }),
  // opts.recreate = "Revert update": recreate every service from its backup's
  // image digest (rolls a broken upgrade back), snapshotting current state first
  // unless snapshot=false.
  // `id` is the SOURCE node (where the stack's backups are cataloged). Pass
  // opts.target_node to restore onto a DIFFERENT node (cross-host DR); omit it for
  // an in-place restore.
  restoreStack: (id: string, project: string, opts?: { recreate?: boolean; snapshot?: boolean; promote_restart_policy?: boolean; inject_healthchecks?: boolean; reconstruct_host?: boolean; host_base_dir?: string; target_node?: string; remap_ip?: boolean; remap_from_ip?: string; remap_to_ip?: string; remap_domain?: boolean; remap_from_domain?: string; remap_to_domain?: string; remap_path?: boolean; remap_from_path?: string; remap_to_path?: string; group?: string; new_site_address?: string; new_upstream_address?: string; private_key?: string; password?: string; code?: string; services?: string[]; source?: string; confirm_missing?: boolean; confirm_missing_devices?: boolean; confirm_missing_networks?: boolean; allow_different_image?: boolean; files_only?: boolean }) => {
    const qs = new URLSearchParams();
    // F173: the two optional address changes, same as the single-service restore.
    if (opts?.new_site_address) qs.set("new_site_address", opts.new_site_address);
    if (opts?.new_upstream_address) qs.set("new_upstream_address", opts.new_upstream_address);
    if (opts?.recreate) qs.set("recreate", "true");
    // #8: opt-in — the default reproduces whatever the source had.
    if (opts?.promote_restart_policy) qs.set("promote_restart_policy", "true");
    if (opts?.inject_healthchecks) qs.set("inject_healthchecks", "true");
    // The server snapshots unless told not to, so an unticked box must say so
    // whether or not this is a revert.
    if (opts?.snapshot === false) qs.set("snapshot", "false");
    if (opts?.reconstruct_host) { qs.set("reconstruct_host", "true"); if (opts?.host_base_dir) qs.set("host_base_dir", opts.host_base_dir); }
    if (opts?.target_node && opts.target_node !== id) qs.set("target_node", opts.target_node);
    if (opts?.remap_ip) { qs.set("remap_ip", "true"); if (opts?.remap_from_ip) qs.set("remap_from_ip", opts.remap_from_ip); if (opts?.remap_to_ip) qs.set("remap_to_ip", opts.remap_to_ip); }
    // F195: the domain rewrite — both endpoints required, so both are sent together or not at all.
    if (opts?.remap_domain && opts?.remap_from_domain && opts?.remap_to_domain) { qs.set("remap_domain", "true"); qs.set("remap_from_domain", opts.remap_from_domain); qs.set("remap_to_domain", opts.remap_to_domain); }
    // F81: move bind sources + compose + stack folder to the new machine's base.
    if (opts?.remap_path) { qs.set("remap_path", "true"); if (opts?.remap_from_path) qs.set("remap_from_path", opts.remap_from_path); if (opts?.remap_to_path) qs.set("remap_to_path", opts.remap_to_path); }
    if (opts?.group) qs.set("group", opts.group);
    // F213: restore only these services. Repeated `service=` params, like every
    // other selector here. Omitted entirely means the whole stack.
    for (const svc of opts?.services || []) qs.append("service", svc);
    // F214: which copy every member is read from. "" = auto.
    if (opts?.source) qs.set("source", opts.source);
    // F216: the operator saw and accepted that members with no backup stay out.
    if (opts?.confirm_missing) qs.set("confirm_missing", "true");
    // Step 22: only when the operator chose to run a newer image if the backed-up one is gone.
    if (opts?.allow_different_image) qs.set("allow_different_image", "true");
    // Step 23: put back the stack's files only — no service stopped or changed.
    if (opts?.files_only) qs.set("files_only", "true");
    // Step 27: the operator saw which devices the target lacks and went ahead.
    if (opts?.confirm_missing_devices) qs.set("confirm_missing_devices", "true");
    if (opts?.confirm_missing_networks) qs.set("confirm_missing_networks", "true");
    const q = qs.toString();
    // F209: the offline private key goes in the BODY and never the query string —
    // a URL lands in proxy access logs, browser history and Referer headers, and
    // this key unlocks every backup. Everything else stays a query param, so a
    // caller that sends no key is byte-for-byte unchanged.
    const body: Record<string, string> = {};
    if (opts?.private_key) body.private_key = opts.private_key;
    // F210: step-up credentials for a protected member, in the body for the same
    // reason the key is.
    if (opts?.password) { body.password = opts.password; if (opts.code) body.code = opts.code; }
    return req<{ status: string }>("POST", `/api/nodes/${id}/stacks/${encodeURIComponent(project)}/restore${q ? `?${q}` : ""}`, body);
  },
  // F43: app-consistent snapshot groups available to restore a stack from. [] when
  // the stack was never captured with an app-consistent snapshot.
  // F215: what this route needed last time, so the dialog can offer it rather
  // than asking for the same domain and paths again. Empty object when unused.
  stackRestoreDefaults: (id: string, project: string, targetNode?: string) =>
    req<CrossRestoreDefaults>("GET", `/api/nodes/${id}/stacks/${encodeURIComponent(project)}/restore-defaults${qstr({ target_node: targetNode })}`),
  stackGroups: (id: string, project: string) => req<{ id: string; at: number; services: string[]; complete: boolean }[]>("GET", `/api/nodes/${id}/stacks/${encodeURIComponent(project)}/groups`),
  // F80: per-service effective backup options for the stack-backup panel — the
  // same settings the container page reads/writes (one source of truth).
  stackOptions: (id: string, project: string) => req<StackServiceOptions[]>("GET", `/api/nodes/${id}/stacks/${encodeURIComponent(project)}/options`),
  stackSchedules: (id: string, project: string) => req<StackSchedule[]>("GET", `/api/nodes/${id}/stacks/${encodeURIComponent(project)}/schedules`),
  setStackSchedule: (id: string, project: string, timing: StackScheduleTiming) =>
    req<StackSchedule[]>("PUT", `/api/nodes/${id}/stacks/${encodeURIComponent(project)}/schedule`, timing),
  removeStackFromSchedule: (id: string, project: string, scheduleId: string) =>
    req<StackSchedule[]>("DELETE", `/api/nodes/${id}/stacks/${encodeURIComponent(project)}/schedules/${encodeURIComponent(scheduleId)}`),
  // F80: update a container's remembered backup options WITHOUT starting a
  // backup. Partial: omitted fields keep their stored values.
  setBackupOptions: (id: string, cid: string, body: { compression?: string; app_export?: boolean; save_image?: boolean; incremental?: boolean; incremental_full_every?: number }) =>
    req<SavedBackupOptions>("PUT", `/api/nodes/${id}/containers/${cid}/backup-options`, body),
  // F115: store a container's mount selection WITHOUT running a backup, so the
  // stack panel's picker writes the same setting the container page writes.
  // `mounts: null` clears the selection back to the size-based default.
  setMountSelection: (id: string, cid: string, mounts: string[] | null) =>
    req<{ mounts: string[]; total: number }>("PUT", `/api/nodes/${id}/containers/${cid}/mount-selection`, { mounts }),
  // F82: read-only pre-restore plan — the exact per-service backups and order the
  // stack restore would execute, plus resolved target folders when reconstructing.
  // opts.target_node is the node the restore would land on, so the plan can say
  // which bind mount sources THAT host is missing (F81). Omitted for an in-place
  // restore, where the paths are already there by definition.
  // `services` narrows only the CAPACITY verdict — the response still lists every
  // member so the picker can offer them. Omit it to size the whole stack.
  stackRestorePlan: (id: string, project: string, opts?: { group?: string; services?: string[]; reconstruct_host?: boolean; host_base_dir?: string; remap_path?: boolean; remap_from_path?: string; remap_to_path?: string; target_node?: string }) =>
    req<StackRestorePlan>("GET", `/api/nodes/${id}/stacks/${encodeURIComponent(project)}/restore-plan${qstr({
      group: opts?.group, services: opts?.services?.join(",") || "",
      reconstruct_host: opts?.reconstruct_host ? "true" : "", host_base_dir: opts?.host_base_dir,
      remap_path: opts?.remap_path ? "true" : "", remap_from_path: opts?.remap_from_path, remap_to_path: opts?.remap_to_path,
      target_node: opts?.target_node && opts.target_node !== id ? opts.target_node : "",
    })}`),
  containerDetail: (id: string, cid: string) => req<ContainerDetailResp>("GET", `/api/nodes/${id}/containers/${cid}`),
  containerSizes: (id: string, cid: string) => req<ContainerSizes>("GET", `/api/nodes/${id}/containers/${cid}/sizes`),
  setHooks: (id: string, cid: string, h: SavedHooks) => req<{ status: string }>("PUT", `/api/nodes/${id}/containers/${cid}/hooks`, h),
  setExportProfile: (id: string, cid: string, p: { dir: string; export_cmd: string; import_cmd: string; verify_cmd?: string; user: string }) =>
    req<{ status: string }>("PUT", `/api/nodes/${id}/containers/${cid}/export-profile`, p),
  // F151: empty the app-native export directory after a successful capture, so
  // the plaintext copy the exporter writes does not linger beside the encrypted
  // archive. Off by default.
  setCleanupExport: (id: string, cid: string, cleanup: boolean) =>
    req<{ cleanup_export: boolean }>("PUT", `/api/nodes/${id}/containers/${cid}/export-cleanup`, { cleanup }),
  // F163: refuse a backup of this container unless write-only encryption is on.
  // Off by default; the response says whether the NEXT run would be refused.
  setRequireWriteOnly: (id: string, cid: string, require: boolean) =>
    req<{ require_write_only: boolean; write_only_blocked_reason: string }>("PUT", `/api/nodes/${id}/containers/${cid}/require-write-only`, { require }),
  // F205: record (or clear, with "") the Redis password for a broker that set it
  // at runtime. Write-only — sealed at rest, and no endpoint reads it back.
  setRedisAuth: (id: string, cid: string, password: string) =>
    req<{ redis_auth_set: boolean }>("PUT", `/api/nodes/${id}/containers/${cid}/redis-auth`, { password }),
  // F206: require fresh proof of the password to OVERWRITE this container in
  // place. null clears the override, back to the derived default.
  setRestoreStepUp: (id: string, cid: string, require: boolean | null) =>
    req<{ restore_step_up: boolean; restore_step_up_default: boolean; restore_step_up_set: boolean }>(
      "PUT", `/api/nodes/${id}/containers/${cid}/restore-step-up`, { require }),

  // F101: the fleet-level app-native export preset library. Saving one preset
  // upserts it; sending a whole library REPLACES it and needs replace:true, so an
  // import of someone else's commands can never happen by accident.
  exportPresets: () => req<{ presets: ExportPreset[] }>("GET", "/api/export-presets"),
  saveExportPreset: (preset: ExportPreset) => req<{ preset: ExportPreset }>("POST", "/api/export-presets", { preset }),
  importExportPresets: (presets: ExportPreset[]) => req<{ presets: ExportPreset[] }>("POST", "/api/export-presets", { presets, replace: true }),
  deleteExportPreset: (id: string) => req<{ status: string }>("DELETE", `/api/export-presets/${encodeURIComponent(id)}`),

  // slim=1 (perf Fix 8): list rows carry a compact `summary` instead of the raw
  // JSON blobs (~6-10× smaller). The detail drawer fetches the full row by id.
  backups: (nodeId?: string) => req<Backup[]>("GET", `/api/backups?slim=1${nodeId ? `&node_id=${nodeId}` : ""}`),
  // Server-side searched/filtered/paginated backup history (PLAN §4.13).
  backupsPage: (p: BackupQuery) => req<BackupPage>("GET", `/api/backups${qstr({ ...(p as Record<string, unknown>), slim: 1 })}`),
  backup: (id: string) => req<Backup>("GET", `/api/backups/${id}`),
  // status "started" carries the new run's backup_id; "already_running" /
  // "already_queued" mean a duplicate click was coalesced onto the id returned.
  createBackup: (node_id: string, container_id: string, stop_app: boolean, compression: string = "balanced", destinations?: string[], app_export = false, mounts?: string[], save_image = false, pause_mode?: string, databases?: string[], incremental?: { enabled: boolean; full_every: number }) =>
    req<{ status: string; backup_id?: string }>("POST", "/api/backups", { node_id, container_id, stop_app, compression, app_export, save_image, ...(pause_mode !== undefined ? { pause_mode } : {}), ...(destinations !== undefined ? { destinations } : {}), ...(mounts !== undefined ? { mounts } : {}), ...(databases !== undefined ? { databases } : {}), ...(incremental !== undefined ? { incremental: incremental.enabled, incremental_full_every: incremental.full_every } : {}) }),
  // F222: back up by container NAME. An id dies on every recreate; the name is
  // what survives one, and the server resolves it to whatever id it has now.
  createBackupByName: (node_id: string, container_name: string) =>
    req<{ status: string; backup_id?: string }>("POST", "/api/backups", { node_id, container_name, stop_app: false }),
  // F223: the recovery wizard's state.
  recoveryState: () => req<RecoveryState>("GET", "/api/recovery/state"),
  setRecoveryStep: (step: RecoveryState["step"]) => req<{ step: string }>("PUT", "/api/recovery/state", { step }),
  // F222: favorite backup targets, stored server-side so the list is the same on
  // every browser signed in to this instance.
  getFavorites: () => req<{ favorites: Favorite[] }>("GET", "/api/ui/favorites"),
  saveFavorites: (favorites: Favorite[]) => req<{ favorites: Favorite[] }>("PUT", "/api/ui/favorites", { favorites }),
  // The operator's hand-arranged fleet order. Write-only on purpose: GET
  // /api/nodes already hands the list back in this order, so the array the
  // browser is holding IS the order and there is nothing to read back.
  saveNodeOrder: (order: string[]) => req<{ order: string[] }>("PUT", "/api/ui/node-order", { order }),
  // F221: back up every eligible container on one node, server-side — each with
  // its OWN remembered options and shared host folders captured once per stack.
  // Replaces a browser-side loop that hardcoded balanced compression and
  // deduplicated nothing.
  backupNodeAll: (id: string, opts?: { includeStopped?: boolean; destinations?: string[] }) =>
    req<NodeBackupAllResult>("POST", `/api/nodes/${id}/backup-all`, {
      ...(opts?.includeStopped ? { include_stopped: true } : {}),
      ...(opts?.destinations !== undefined ? { destinations: opts.destinations } : {}),
    }),
  // List a DB container's app databases for per-database backup selection (F8).
  listDatabases: (id: string, cid: string) =>
    req<{ engine: string; supported: boolean; running?: boolean; databases: string[] }>("GET", `/api/nodes/${id}/containers/${cid}/databases`),
  setPauseMode: (id: string, cid: string, mode: string) => req<{ status: string }>("PUT", `/api/nodes/${id}/containers/${cid}/pause-mode`, { mode }),
  // A container with NOTHING to mount is a normal answer, not an error — the
  // paperless stack has two such services. Normalized here so no caller has to
  // remember that an empty list can arrive as null (F179).
  containerMounts: (id: string, cid: string) =>
    req<MountInfo[] | null>("GET", `/api/nodes/${id}/containers/${cid}/mounts`).then((r) => r || []),
  getCritical: (id: string, cid: string) => req<CriticalStatus>("GET", `/api/nodes/${id}/containers/${cid}/critical`),
  setCritical: (id: string, cid: string, body: { enabled: boolean; rpo_seconds: number }) =>
    req<{ ok: boolean }>("PUT", `/api/nodes/${id}/containers/${cid}/critical`, body),
  // Event-triggered "back up before changes" toggle (F7).
  setAutosnap: (id: string, cid: string, enabled: boolean) =>
    req<{ enabled: boolean }>("PUT", `/api/nodes/${id}/containers/${cid}/autosnap`, { enabled }),
  // Pilot-light standby rehearsal (F62): prove failover onto another node.
  getStandby: (id: string, cid: string) => req<Standby | null>("GET", `/api/nodes/${id}/containers/${cid}/standby`),
  setStandby: (id: string, cid: string, body: { standby_node: string; interval_days: number }) =>
    req<Standby>("PUT", `/api/nodes/${id}/containers/${cid}/standby`, body),
  deleteStandby: (id: string, cid: string) => req<{ status: string }>("DELETE", `/api/nodes/${id}/containers/${cid}/standby`),
  runStandby: (id: string, cid: string) => req<{ status: string }>("POST", `/api/nodes/${id}/containers/${cid}/standby/run`, {}),
  // F69: clear a container's mass-change retention hold after review.
  clearTripwire: (id: string, cid: string) => req<{ status: string; rows_cleared: number }>("POST", `/api/nodes/${id}/containers/${cid}/tripwire/clear`, {}),
  // F73: field-by-field config-drift breakdown vs the newest successful backup.
  containerDrift: (id: string, cid: string) => req<{ changed: boolean; fields?: string[] }>("GET", `/api/nodes/${id}/containers/${cid}/drift`),
  // Per-container large-bind cutoff override (F12); gib <= 0 clears the override.
  setBindThreshold: (id: string, cid: string, gib: number) =>
    req<{ bind_skip_gib: number; bind_skip_gib_override: number }>("PUT", `/api/nodes/${id}/containers/${cid}/bind-threshold`, { gib }),
  // F184: pin (or clear, with "") the uid:gid restored data is owned by.
  setRestoreOwnership: (id: string, cid: string, ownership: string) =>
    req<{ restore_ownership: string }>("PUT", `/api/nodes/${id}/containers/${cid}/restore-ownership`, { ownership }),
  // F132: include or leave out the app-declared regenerable directories.
  setExcludeRegenerable: (id: string, cid: string, exclude: boolean) =>
    req<{ exclude_regenerable: boolean }>("PUT", `/api/nodes/${id}/containers/${cid}/regenerable`, { exclude }),
  // Per-container post-restore health-timeout override (F30); seconds <= 0 clears it.
  setRestoreTimeout: (id: string, cid: string, seconds: number) =>
    req<{ restore_health_timeout_seconds: number; restore_health_timeout_override: number }>("PUT", `/api/nodes/${id}/containers/${cid}/restore-timeout`, { seconds }),
  // cascade (F63): also delete every incremental descendant, newest-first —
  // offered after a 409 "baseline of N deltas" refusal.
  deleteBackup: (id: string, cascade?: boolean) => req<unknown>("DELETE", `/api/backups/${id}`, cascade ? { cascade: true } : undefined),
  deleteBackups: (ids: string[]) => req<{ deleted: number; failed: { id: string; error: string }[] }>("POST", "/api/backups/delete", { ids }),
  cancelBackup: (id: string) => req<{ status: string }>("POST", `/api/backups/${id}/cancel`),
  // F44: re-verify (scrub) a backup. Pass a source ("local" or a destination ID) to
  // scrub a specific copy; omit to scrub the local copy (or, for an adopted
  // destination-only backup, its offsite copy).
  verifyBackup: (id: string, source?: string) => req<{ status: string }>("POST", `/api/backups/${id}/verify`, source ? { source } : {}),
  pinBackup: (id: string, pinned: boolean) => req<{ status: string; pinned: boolean }>("PUT", `/api/backups/${id}/pin`, { pinned }),
  labelBackup: (id: string, label: string) => req<{ status: string; label: string }>("PUT", `/api/backups/${id}/label`, { label }),
  mirrorBackup: (id: string, destinations?: string[]) => req<{ status: string }>("POST", `/api/backups/${id}/mirror`, destinations && destinations.length ? { destinations } : {}),
  drills: () => req<DrillStatus[]>("GET", "/api/drills"),
  drillBackup: (id: string) => req<{ status: string }>("POST", `/api/backups/${id}/drill`),
  // F11: on-demand check that this backup's image can still be obtained for a
  // restore (present locally, pullable by digest/tag, or bundled). No pull.
  // targetNode asks "can THIS host honor it?" — omit for the origin node.
  restoreReadiness: (id: string, targetNode?: string) =>
    req<RestoreReadiness>("GET", `/api/backups/${id}/restore-readiness${targetNode ? `?target_node=${encodeURIComponent(targetNode)}` : ""}`),
  // Restore uses its own fetch (not req) so the 409 compatibility-gate response
  // (F10) surfaces as a typed RestoreCompatError the drawer can act on, instead of
  // a generic thrown message.
  // Stop an in-flight restore by its run id (backup id / "stack:<project>" /
  // "node:<id>"). The engine stops at the next safe point — it never rolls back
  // behind your back; the log says exactly what state things were left in.
  cancelRestore: (runId: string) => req<{ status: string }>("POST", `/api/restores/${encodeURIComponent(runId)}/cancel`, {}),
  // F100: which restores are in flight, so a page reloaded mid-restore can
  // re-attach to the run and offer Cancel again.
  runningRestores: () => req<{ running: RunningRestore[] }>("GET", "/api/restores"),
  restore: async (id: string, body: { node_id: string; target_id: string; volumes: boolean; database: boolean; confirm: boolean; snapshot?: boolean; recreate?: boolean; promote_restart_policy?: boolean; inject_healthchecks?: boolean; source?: string; confirm_incompatible?: boolean; confirm_unverified?: boolean; confirm_missing_devices?: boolean; confirm_missing_networks?: boolean; allow_different_image?: boolean; files_only?: boolean; test_clone?: boolean; as_name?: string; isolated?: boolean; reconstruct_host?: boolean; host_base_dir?: string; remap_ip?: boolean; remap_from_ip?: string; remap_to_ip?: string; remap_domain?: boolean; remap_from_domain?: string; remap_to_domain?: string; remap_path?: boolean; remap_from_path?: string; remap_to_path?: string; new_site_address?: string; new_upstream_address?: string; private_key?: string; password?: string; code?: string }): Promise<{ status: string }> => {
    const res = await fetch(`/api/backups/${id}/restore`, {
      method: "POST", credentials: "same-origin",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf() },
      body: JSON.stringify(body),
    });
    if (res.status === 401) {
      // F206: an in-place restore of a PROTECTED container asks for the password
      // again. This is the same distinction req<T> already makes — without it a
      // step-up prompt would land here as "session dead" and sign the operator
      // out in the middle of a restore they are entitled to run.
      const se = parseBody(await res.text());
      if (se?.step_up_required) throw new StepUpError(se.error || "re-authentication required", !!se.totp_required);
      if (!location.pathname.startsWith("/login")) location.href = "/login";
      throw new Error("unauthenticated");
    }
    const data = parseBody(await res.text());
    if (res.status === 409 && data?.compat_warning) {
      throw new RestoreCompatError(String(data.compat_warning), (data.extensions as string[]) || []);
    }
    // F218: known-bad archive. Typed separately so the caller can offer the one
    // override that applies, rather than showing a dead-end error.
    if (res.status === 409 && data?.verify_failed) {
      throw new RestoreVerifyFailedError(data.error || "this backup's last verification failed");
    }
    if (!res.ok) throw httpError(res, data);
    return (data ?? {}) as { status: string };
  },
  // F219: one-click test clones — the isolated copies DockBack names and reaps.
  // The list is per node and read from the host itself, so it is true even for a
  // clone this instance did not create.
  testClones: (nodeId: string) => req<{ clones: TestClone[]; ttl_hours: number }>("GET", `/api/nodes/${nodeId}/test-clones`),
  removeTestClone: (nodeId: string, containerId: string) =>
    req<{ status: string }>("POST", "/api/test-clones/remove", { node_id: nodeId, container_id: containerId }),
  // F199: exports carry a one-shot, step-up-issued ticket. The URL is worthless
  // without one, and worthless again after the first use.
  downloadURL: (id: string, ticket: string) => `/api/backups/${id}/download?ticket=${encodeURIComponent(ticket)}`,
  exportGrant: (id: string, purpose: "download" | "extract", password?: string, code?: string) =>
    req<{ ticket: string; expires_in: number }>("POST", `/api/backups/${id}/export-grant`, { purpose, password, code }),
  // Step 25: one download for a whole stack — every service's newest backup, decrypted, in one zip.
  stackExportGrant: (node: string, project: string, password?: string, code?: string) =>
    req<{ ticket: string; expires_in: number }>("POST", `/api/nodes/${encodeURIComponent(node)}/stacks/${encodeURIComponent(project)}/export-grant`, { password, code }),
  stackDownloadURL: (node: string, project: string, ticket: string) =>
    `/api/nodes/${encodeURIComponent(node)}/stacks/${encodeURIComponent(project)}/download?ticket=${encodeURIComponent(ticket)}`,
  // Step 28: a node's frozen evidence — every container's inspect (environment names only), networks, volumes.
  evidenceGrant: (node: string, password?: string, code?: string) =>
    req<{ ticket: string; expires_in: number }>("POST", `/api/nodes/${encodeURIComponent(node)}/evidence-grant`, { password, code }),
  evidenceURL: (node: string, ticket: string) => `/api/nodes/${encodeURIComponent(node)}/evidence?ticket=${encodeURIComponent(ticket)}`,
  appBackupExportGrant: (password?: string, code?: string) =>
    req<{ ticket: string; expires_in: number }>("POST", `/api/app-backup/export-grant`, { password, code }),
  // F21: browse a backup's files, and a per-file download URL.
  backupEntries: (id: string) => req<{ entries: ArchiveEntry[] }>("GET", `/api/backups/${id}/entries`),
  // F70 universal file index: cross-backup file search + generation diff (index-only reads).
  searchFile: (nodeId: string, q: string, target?: string) =>
    req<FileSearchResp>("GET", `/api/backups/search-file${qstr({ node_id: nodeId, q, target })}`),
  diffBackups: (a: string, b: string) => req<BackupDiffResp>("GET", `/api/backups/diff${qstr({ a, b })}`),
  backupExtractURL: (id: string, path: string, ticket: string) =>
    `/api/backups/${id}/extract?path=${encodeURIComponent(path)}&ticket=${encodeURIComponent(ticket)}`,
  // F96: write ONE recovered file back into the running container at its original
  // path. Destructive but narrow — it overwrites that file and nothing else.
  restoreFile: (id: string, body: { path: string; target_id: string; node_id?: string; source?: string; keep_backup?: boolean }) =>
    req<{ status: string; path: string }>("POST", `/api/backups/${id}/restore-file`, body),
  // Persisted per-run log (F8): full log of a finished run, downloadable.
  runLog: (id: string) => req<{ backup_id: string; lines: RunLogLine[] }>("GET", `/api/backups/${id}/log`),
  runLogDownloadURL: (id: string) => `/api/backups/${id}/log/download`,

  version: () => req<{ version: string }>("GET", "/api/version"),
  stats: () => req<FleetStats>("GET", "/api/stats"),
  coverage: () => req<Coverage>("GET", "/api/coverage"),
  insights: () => req<Insights>("GET", "/api/insights"),
  // Normalized so fleet-shaped arrays are never null (a node with no successful
  // backups — or an older backend — serializes them as null, which crashed the
  // Recovery page on `services.length`).
  runbook: () => req<Runbook>("GET", "/api/runbook").then((r) => ({
    ...r,
    destinations: r.destinations ?? [],
    nodes: (r.nodes ?? []).map((n) => ({ ...n, services: n.services ?? [] })),
  })),
  // F59: signed, expiring, revocable runbook share links.
  shareRunbook: (hours: number) => req<{ url: string; id: string; exp: number }>("POST", "/api/runbook/share", { hours }),
  runbookShares: () => req<{ id: string; created: number; exp: number; revoked: boolean; expired: boolean }[]>("GET", "/api/runbook/shares"),
  revokeRunbookShare: (id: string) => req<{ status: string }>("POST", `/api/runbook/shares/${id}/revoke`, {}),
  protectContainer: (id: string, cid: string, backupNow = true) =>
    req<ProtectResult>("POST", `/api/nodes/${id}/containers/${cid}/protect`, backupNow ? {} : { backup_now: false }),
  // F220: protect a whole compose project as ONE app-consistent schedule target.
  protectStack: (id: string, project: string, backupNow = true) =>
    req<ProtectStackResult>("POST", `/api/nodes/${id}/stacks/${encodeURIComponent(project)}/protect`, backupNow ? {} : { backup_now: false }),
  ignored: (id: string) => req<IgnoredItem[]>("GET", `/api/nodes/${id}/ignored`),
  ignore: (id: string, kind: IgnoredItem["kind"], name: string) =>
    req<IgnoredItem[]>("PUT", `/api/nodes/${id}/ignored/${kind}/${encodeURIComponent(name)}`),
  unignore: (id: string, kind: IgnoredItem["kind"], name: string) =>
    req<IgnoredItem[]>("DELETE", `/api/nodes/${id}/ignored/${kind}/${encodeURIComponent(name)}`),
  audit: () => req<AuditEntry[]>("GET", "/api/audit"),
  // Server-side paged/searched/date-ranged audit history (F7).
  auditPage: (p: AuditQuery) => req<AuditPage>("GET", `/api/audit${qstr(p as Record<string, unknown>)}`),
  // Attachment URL for exporting ALL matching audit rows (CSV/JSON).
  auditExportUrl: (p: AuditQuery & { format: "csv" | "json" }) => `/api/audit/export${qstr(p as unknown as Record<string, unknown>)}`,
  // F68: walk the tamper-evident hash chain server-side.
  auditVerify: () => req<{
    ok: boolean; checked: number; first_bad_id?: number;
    // F200: the checkpoint this instance last SENT. Shown so the operator knows
    // which message to check against — it is not itself evidence.
    beacon_sent?: { head_id: number; chain: string; at: number };
  }>("GET", "/api/audit/verify"),
  // F200: check the trail against a checkpoint kept OUTSIDE this machine. The
  // operator pastes the entry number and value from their own copy.
  auditVerifyBeacon: (head_id: number, chain: string) =>
    req<{ ok: boolean; reason: string; head_id: number; current_head_id: number }>(
      "POST", "/api/audit/verify-beacon", { head_id, chain }),

  getNotifications: () => req<{ config: NotifyConfig; gotify_token_set: boolean; email_password_set: boolean }>("GET", "/api/notifications"),
  setNotifications: (c: NotifyConfig) => req<{ config: NotifyConfig; gotify_token_set: boolean; email_password_set: boolean }>("PUT", "/api/notifications", c),
  testNotification: (channel: string, config: NotifyConfig) => req<{ status: string }>("POST", "/api/notifications/test", { channel, config }),
  // Backup-page nudge: show the "set up notifications" hint only when no channel
  // is configured and the user hasn't dismissed it.
  notificationsHint: () => req<{ show: boolean; configured: boolean }>("GET", "/api/notifications/hint"),
  dismissNotificationsHint: () => req<{ ok: boolean }>("POST", "/api/notifications/hint/dismiss"),
  getPolicy: () => req<{ policy: Policy; next_run: number; last_run?: number; last_caught_up?: boolean; prune_next_run?: number }>("GET", "/api/policy"),
  setPolicy: (p: Policy) => req<{ policy: Policy; next_run: number; last_run?: number; last_caught_up?: boolean; prune_next_run?: number }>("PUT", "/api/policy", p),
  // Clusters (F104). Deleting a cluster removes a GROUPING: nodes, containers and
  // backups are never touched. A cluster that still has nodes is refused with 409
  // unless reassignTo names where those nodes should move.
  listClusters: () => req<{ clusters: Cluster[] }>("GET", "/api/clusters"),
  createCluster: (name: string, description = "", color = "") =>
    req<Cluster>("POST", "/api/clusters", { name, description, color }),
  updateCluster: (name: string, patch: { name: string; description: string; color: string }) =>
    req<Cluster>("PATCH", `/api/clusters/${encodeURIComponent(name)}`, patch),
  deleteCluster: (name: string, reassignTo?: string) =>
    req<{ deleted: string; nodes_reassigned: number }>("DELETE",
      `/api/clusters/${encodeURIComponent(name)}${reassignTo ? `?reassign_to=${encodeURIComponent(reassignTo)}` : ""}`),
  // Members: this cluster's servers plus the rest of the fleet, so the cluster
  // screen can list and reassign without cross-referencing the node list itself.
  clusterMembers: (name: string) =>
    req<{ members: ClusterMember[]; available: ClusterMember[] }>("GET", `/api/clusters/${encodeURIComponent(name)}/members`),
  // Moves servers into this cluster. Changes only their cluster — never the
  // transport, address or stored credential.
  assignClusterMembers: (name: string, nodeIds: string[]) =>
    req<{ moved: number }>("POST", `/api/clusters/${encodeURIComponent(name)}/members`, { node_ids: nodeIds }),
  getClusterPolicy: (name: string) => req<ClusterPolicy>("GET", `/api/clusters/${encodeURIComponent(name)}/policy`),
  setClusterPolicy: (name: string, ov: PolicyOverride) =>
    req<ClusterPolicy>("PUT", `/api/clusters/${encodeURIComponent(name)}/policy`, ov),

  // F105: hardware + live host utilisation. Each call may spawn a short-lived
  // read-only probe container on the node, so the server caches it — pass
  // force=true only for an explicit "Re-probe".
  nodeMachine: (id: string, force = false) =>
    req<MachineResp>("GET", `/api/nodes/${id}/machine${force ? "?refresh=1" : ""}`),
  // F88: clear a node's pinned sidecar image so the next use re-pins what is
  // present. Deliberately does not accept a digest — a pin must be something
  // DockBack observed, never a value handed to it.
  repinSidecar: (id: string) =>
    req<{ status: string; previous: string }>("POST", `/api/nodes/${id}/sidecar/repin`, {}),
  getNodePolicy: (id: string) => req<NodePolicy>("GET", `/api/nodes/${id}/policy`),
  setNodePolicy: (id: string, ov: PolicyOverride) => req<NodePolicy>("PUT", `/api/nodes/${id}/policy`, ov),
  getContainerPolicy: (id: string, cid: string) => req<ContainerPolicy>("GET", `/api/nodes/${id}/containers/${cid}/policy`),
  setContainerPolicy: (id: string, cid: string, ov: PolicyOverride) => req<ContainerPolicy>("PUT", `/api/nodes/${id}/containers/${cid}/policy`, ov),
  // Encryption-key escrow & recovery (PLAN §9.2).
  keyStatus: () => req<{ acknowledged: boolean; acknowledged_at: number; ephemeral: boolean; fingerprint: string; min_passphrase_len: number }>("GET", "/api/security/key-status"),
  keyReveal: (stepUp?: StepUpCreds) => req<{ key_hex: string; fingerprint: string; ephemeral: boolean }>("POST", "/api/security/key-reveal", { ...stepUp }),
  keyAcknowledge: () => req<{ status: string }>("POST", "/api/security/key-acknowledge", {}),
  keyKeyfile: (passphrase: string, stepUp?: StepUpCreds) => req<{ keyfile: string }>("POST", "/api/security/key-keyfile", { passphrase, ...stepUp }),
  // F86 write-only backups. `enable` returns the private key EXACTLY ONCE — it is
  // never stored, so it must be saved from that one response or the backups taken
  // while the mode is armed become unrecoverable.
  writeOnlyStatus: () => req<{ enabled: boolean; public_key: string; fingerprint: string }>("GET", "/api/security/write-only"),
  enableWriteOnly: (stepUp?: StepUpCreds) =>
    req<{ public_key: string; private_key: string; fingerprint: string }>("POST", "/api/security/write-only/enable", { ...stepUp }),
  disableWriteOnly: (stepUp?: StepUpCreds) =>
    req<{ disabled: boolean; still_write_only: number; keep_recovery_sheet: boolean }>("POST", "/api/security/write-only/disable", { ...stepUp }),
  // F204: prove a saved offline recovery key actually opens a write-only backup,
  // without restoring it. The key is sent for this one request and is never
  // stored, logged or echoed back — same handling as a restore's private_key.
  verifyRecoveryKey: (id: string, privateKey: string, stepUp?: StepUpCreds) =>
    req<{ ok: boolean; fingerprint: string; message?: string; error?: string }>(
      "POST", `/api/backups/${id}/verify-key`, { private_key: privateKey, ...stepUp }),
  // Which recovery sheet am I holding? Derived from the key's own public half.
  identifyRecoveryKey: (privateKey: string, stepUp?: StepUpCreds) =>
    req<{ fingerprint: string }>("POST", "/api/security/write-only/identify-key", { private_key: privateKey, ...stepUp }),
  // F207: egress audit mode — the hosts the allow-list WOULD refuse, observed
  // instead of blocked, so enforcement can be turned on with confidence.
  egressAudit: () => req<EgressAudit>("GET", "/api/security/egress/audit"),
  clearEgressAudit: () => req<{ cleared: boolean }>("POST", "/api/security/egress/audit/clear", {}),
  // F35: offline recovery tool metadata for the "restore without DockBack" kit.
  recoveryTool: () => req<{ filename: string; sha256: string; size: number; available: boolean; version: string; release_url: string; invocation: string }>("GET", "/api/security/recovery-tool"),
  // F16: rotate the master key — re-wraps every backup DEK + re-seals node/dest/
  // notify/2FA secrets, then switches the running process to the new key.
  rotateKey: (current_hex: string, new_hex: string, password: string, code?: string) =>
    req<KeyRotateResult>("POST", "/api/security/key-rotate", { current_hex, new_hex, password, code }),
  retentionPreview: () => req<RetentionPreview>("GET", "/api/retention/preview"),
  // F77: migrate legacy flat-layout archives into the per-stack folder layout.
  migrateLayout: () => req<{ status: string; candidates: number }>("POST", "/api/maintenance/migrate-layout", {}),
  migrateLayoutStatus: () => req<MigrateLayoutStatus>("GET", "/api/maintenance/migrate-layout"),
  retentionPrune: () => req<{ pruned: number; freed_bytes: number }>("POST", "/api/retention/prune", {}),
  runSchedule: () => req<{ status: string }>("POST", "/api/policy/run", {}),
  // Named backup schedules (F6).
  schedules: () => req<NamedSchedule[]>("GET", "/api/schedules"),
  // F38: enable one schedule by id, losslessly — load the full schedule, flip
  // `enabled`, and PUT it back (the update endpoint is a full-object replace, so a
  // partial body would blank its kind/time/targets).
  enableSchedule: async (id: string): Promise<NamedSchedule> => {
    const scheds = await req<NamedSchedule[]>("GET", "/api/schedules");
    const sc = scheds.find((x) => x.id === id);
    if (!sc) throw new Error("schedule not found");
    return req<NamedSchedule>("PUT", `/api/schedules/${id}`, scheduleBody({ ...sc, enabled: true }));
  },
  addSchedule: (sc: Schedule) => req<NamedSchedule>("POST", "/api/schedules", scheduleBody(sc)),
  updateSchedule: (id: string, sc: Schedule) => req<NamedSchedule>("PUT", `/api/schedules/${id}`, scheduleBody(sc)),
  deleteSchedule: (id: string) => req<{ status: string }>("DELETE", `/api/schedules/${id}`),
  runScheduleById: (id: string) => req<{ status: string }>("POST", `/api/schedules/${id}/run`, {}),

  destinations: () => req<Destination[]>("GET", "/api/destinations"),
  addDestination: (d: { name: string; type: string; config: Record<string, string> }) => req<Destination>("POST", "/api/destinations", d),
  testDestination: (d: { type: string; config: Record<string, string> }) =>
    req<TestDestResult>("POST", "/api/destinations/test", d),
  // Edit an existing destination. Config is fetched WITHOUT secrets; on save a
  // blank secret keeps the stored one (never round-tripped to the browser).
  getDestinationConfig: (id: string) => req<{ id: string; name: string; type: string; config: Record<string, string> }>("GET", `/api/destinations/${id}/config`),
  updateDestination: (id: string, d: { name: string; type: string; config: Record<string, string> }) => req<{ id: string }>("PUT", `/api/destinations/${id}`, d),
  // F66: clear + re-pin an SFTP destination's SSH host key after a legitimate change.
  resetDestHostKey: (id: string) => req<{ status: string; repinned: boolean; host_key_fp?: string }>("POST", `/api/destinations/${id}/reset-hostkey`, {}),
  testDestinationByID: (id: string, d: { config: Record<string, string> }) =>
    req<TestDestResult>("POST", `/api/destinations/${id}/test`, d),
  deleteDestination: (id: string) => req<unknown>("DELETE", `/api/destinations/${id}`),
  // F20: scan a destination for orphaned .dback archives and re-import them into the catalog.
  // F102: the scan reports WHICH archives it skipped and why, so "42 adopted,
  // 7 skipped" stops being unactionable after a key rotation.
  adoptDestination: (id: string) => req<{ adopted: number; skipped: number; skips?: AdoptSkip[] }>("POST", `/api/destinations/${id}/adopt`, {}),
  // F51: backfill existing history to a destination (start + poll progress).
  backfillDestination: (id: string) => req<{ status: string; candidates: number }>("POST", `/api/destinations/${id}/backfill`, {}),
  backfillStatus: (id: string) => req<{ total: number; done: number; failed: number; running: boolean; started_at?: number }>("GET", `/api/destinations/${id}/backfill`),
  getSettings: () => req<Record<string, string>>("GET", "/api/settings"),
  setSettings: (s: Record<string, string>) => req<unknown>("POST", "/api/settings", s),
  // F39: test whether a host would be permitted by the live egress allow-list.
  egressTest: (host: string) => req<{ ok: boolean; error?: string }>("POST", "/api/security/egress-test", { host }),
  // F54: hosts DockBack is already configured to reach (destinations, notifications,
  // nodes) — to compose the allow-list from real endpoints. Hostnames only.
  egressSuggestions: () => req<{ host: string; source: string; allowed_now: boolean }[]>("GET", "/api/security/egress-suggestions"),
  // F46: persistent alert inbox + activity feed.
  alertsCount: () => req<{ unacked: number }>("GET", "/api/alerts/count"),
  listAlerts: (opts?: { unacked?: boolean; page?: number }) => {
    const qs = new URLSearchParams();
    if (opts?.unacked) qs.set("unacked", "1");
    if (opts?.page) qs.set("page", String(opts.page));
    const q = qs.toString();
    return req<{ alerts: Alert[]; page: number }>("GET", `/api/alerts${q ? `?${q}` : ""}`);
  },
  ackAlert: (id: number) => req<{ status: string }>("POST", `/api/alerts/${id}/ack`, {}),
  ackAllAlerts: () => req<{ status: string }>("POST", "/api/alerts/ack-all", {}),
  opsLog: (source: string) => req<{ source: string; lines: OpsLogLine[] }>("GET", `/api/ops-log/${source}`),
  // F45: scoped API tokens for automation. createToken returns the plaintext ONCE.
  listTokens: () => req<ApiToken[]>("GET", "/api/security/tokens"),
  // F201: allowedCidrs pins the token to source addresses; [] = any address.
  createToken: (name: string, scopes: string[], ttlDays = 0, stepUp?: StepUpCreds, allowedCidrs: string[] = []) =>
    req<{ id: string; name: string; scopes: string; token: string; expires_at: number; allowed_cidrs: string }>(
      "POST", "/api/security/tokens", { name, scopes, ttl_days: ttlDays, allowed_cidrs: allowedCidrs, ...stepUp }),
  deleteToken: (id: string) => req<{ status: string }>("DELETE", `/api/security/tokens/${id}`),
};

export function fmtBytes(n: number): string {
  if (!n) return "0 B";
  const u = ["B", "KB", "MB", "GB", "TB", "PB"];
  const i = Math.floor(Math.log(n) / Math.log(1024));
  return `${(n / Math.pow(1024, i)).toFixed(1)} ${u[i]}`;
}
export function fmtAgo(ts: number): string {
  if (!ts) return "never";
  const s = Math.max(0, Math.floor(Date.now() / 1000 - ts));
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}
