// Backing up ONE container, as a page (F225).
//
// This was "Run Manual Backup": roughly 840 lines inside a one-third-width
// column of a 2,093-line page. The thing you came to do sat in the narrowest
// part of the screen, and the mount picker — the control that decides what is
// actually in the backup — was a scrolling list inside it.
//
// It was also three jobs with no seam between them: run a backup now, this
// container's remembered settings, and its protection posture. Only the first is
// something you DO; the other two you set once, and they are what made the first
// one scroll. So the run gets the width and its own tab, and everything
// remembered is grouped behind four more — one route, five tabs, nothing new.
//
// Every control, guard and saved setting is the old card's, moved. Each writes
// the same per-container setting it always did, so a stack backup and a
// scheduled run see a toggle flipped here immediately.
import { useEffect, useRef, useState } from "react";
import StateChip from "../components/StateChip";
import { fmtDuration } from "../lib/format";
import { useParams, useNavigate, Link } from "react-router-dom";
import {
  ChevronRight, CloudUpload, CalendarClock, Pencil,
  CheckCircle2, Loader2, AlertTriangle, Database, ShieldCheck, HardDrive, Wrench, Bell, Gauge, X, History, RotateCcw, Tags, Copy, ClipboardPaste, Layers, ServerCog, Play, Users, Check,
} from "lucide-react";
import { api, Backup, ContainerInfo, CriticalStatus, Destination, ExportPreset, MountInfo, Node, RegenerablePath, PolicyOverride, ContainerPolicy, ContainerSchedule, LabelPolicy, Standby, fmtBytes, fmtAgo } from "../api";
import { Button, Card, Input, Label, Select } from "../components/ui";
import BackupCloudIcon from "../components/BackupCloudIcon";
import ScheduleList from "../components/ScheduleList";
import BackupConsole from "../components/BackupConsole";
import { useToast } from "../components/Toast";
import { usePoll } from "../hooks/usePoll";


// F217: sealed to an OFFLINE key (F86) — restoring it needs the private key
// from its recovery sheet, which the quick-restore card deliberately does not
// ask for. Same test the restore drawer makes, from whichever of the two shapes
// this row arrived in: the paged list carries a computed summary, the
// container-detail payload carries the manifest itself.




// rpoChoices are the selectable target RPO windows (seconds). 5 min is the floor
// enforced server-side so low-RPO can't overload the database (PLAN §9.7/§9.9).
const rpoChoices = [
  { secs: 900, label: "15 minutes" },
  { secs: 3600, label: "1 hour" },
  { secs: 21600, label: "6 hours" },
  { secs: 43200, label: "12 hours" },
];


// The five groups. Flat rather than nested: each is a coherent set, and a tab in
// the URL means a specific one can be linked to — the same ?tab= convention the
// Settings page uses.
const BACKUP_TABS = [
  { id: "run", label: "Run backup" },
  { id: "schedule", label: "Schedule & retention" },
  { id: "protection", label: "Protection" },
  { id: "restore", label: "Restore behaviour" },
  { id: "hooks", label: "Hooks & export" },
] as const;
type BackupTab = typeof BACKUP_TABS[number]["id"];

export default function ContainerBackup() {
  const { id = "", cid = "" } = useParams();
  const navigate = useNavigate();
  const toast = useToast();
  const [c, setC] = useState<ContainerInfo | null>(null);
  const [nodeName, setNodeName] = useState("");
  const [backups, setBackups] = useState<Backup[]>([]);
  const [ready, setReady] = useState(false); // first container-detail load done (so we know backup state)
  const [storage, setStorage] = useState("");
  const [err, setErr] = useState("");
  const [msg, setMsg] = useState("");

  // F19: declarative dockback.* label management. When set, labels win and the
  // controls they govern are read-only (edit the labels where the container is defined).
  const [labelManaged, setLabelManaged] = useState(false);
  const [labelPolicy, setLabelPolicy] = useState<LabelPolicy | null>(null);
  // Manual backup form.
  const [compression, setCompression] = useState("balanced");
  const [pauseMode, setPauseMode] = useState("pause"); // none | pause | stop (remembered per container; default pause, PLAN §4.2)
  const seededPause = useRef(false);
  const seededOpts = useRef(false); // seed compression/export/save-image once (remembered per container, F3)
  const [starting, setStarting] = useState(false);
  // Bridge between "backup accepted" (POST returned) and "backup visible in the
  // polled list": holds the accepted run's id so the button stays disabled with
  // no enabled gap — the gap is what made clicks feel unrecorded and invited
  // repeat clicks (each of which used to queue a whole extra run).
  const [pendingId, setPendingId] = useState<string | null>(null);
  const [dests, setDests] = useState<Destination[]>([]);
  const [selDests, setSelDests] = useState<Set<string>>(new Set());
  // How many SQLite databases the last backup captured consistently (F22).
  const [sqliteFiles, setSqliteFiles] = useState(0);
  // F34: Postgres PITR readiness from the read-only probe (running Postgres only).
  // Which container mounts (volumes/binds) to back up.
  const [mounts, setMounts] = useState<MountInfo[] | null>(null);
  const [selMounts, setSelMounts] = useState<Set<string>>(new Set());
  const [mountsLoading, setMountsLoading] = useState(false);
  // Quiesce hooks.
  const [autoHooks, setAutoHooks] = useState<string[]>([]);
  const [preHooks, setPreHooks] = useState("");
  const [postHooks, setPostHooks] = useState("");
  // F140: commands run after a RESTORE, before the health gate judges it.
  const [postRestoreHooks, setPostRestoreHooks] = useState("");
  const [hookUser, setHookUser] = useState("");
  const [hooksOpen, setHooksOpen] = useState(false);
  const [protecting, setProtecting] = useState(false); // one-click protect (B5)
  // App-native export.
  const [exportAvail, setExportAvail] = useState(false);
  // F152: the application's own post-import check, run after a successful
  // app-native restore. Optional — many apps ship no checker that can fail.
  const [exVerify, setExVerify] = useState("");
  // F151: empty the export directory after a successful capture, so the
  // plaintext copy the exporter writes does not linger beside the encrypted
  // archive. Off by default; the exporter is incremental, so this trades
  // re-export time for not leaving that copy around.
  const [cleanupExport, setCleanupExport] = useState(false);
  const [cleanupSaving, setCleanupSaving] = useState(false);
  // F163: refuse a backup of this container unless write-only encryption is on.
  // Off by default. Worth turning on for anything holding credentials to OTHER
  // systems, where an archive the server can open is protected by a key sitting
  // on a machine the archive grants access to.
  const [requireWO, setRequireWO] = useState(false);
  const [woBlocked, setWoBlocked] = useState("");
  // F205: the Redis password for a broker that set it at runtime with
  // CONFIG SET requirepass — invisible to every place the dump looks, so without
  // it that container falls back to a raw file capture on every single run.
  // Write-only: the server never sends the value back, so the field starts empty
  // and `redisAuthSet` is all we know about what is already stored.
  // F206: whether overwriting this container in place asks for the password
  // first. `stepUpDefault` is what it would be with no explicit choice (critical
  // data, or marked require-write-only), shown so the control can say what it is
  // overriding rather than presenting an unexplained state.
  const [restoreStepUp, setRestoreStepUp] = useState(false);
  const [stepUpDefault, setStepUpDefault] = useState(false);
  const [stepUpExplicit, setStepUpExplicit] = useState(false);
  const [stepUpSaving, setStepUpSaving] = useState(false);
  const [dbEngine, setDbEngine] = useState("");
  const [redisAuthSet, setRedisAuthSet] = useState(false);
  const [redisPw, setRedisPw] = useState("");
  const [redisPwSaving, setRedisPwSaving] = useState(false);
  const [redisPwMsg, setRedisPwMsg] = useState("");
  const [woSaving, setWoSaving] = useState(false);
  const [exportTool, setExportTool] = useState("");
  const [useExport, setUseExport] = useState(false);
  const [saveImage, setSaveImage] = useState(false);
  const [incremental, setIncremental] = useState(false); // F61: changed-files-only volume capture
  const [incFullEvery, setIncFullEvery] = useState(7);    // force a fresh full every N (clamp 2..30)
  // F62: pilot-light standby rehearsal onto a second node.
  const [standby, setStandby] = useState<Standby | null>(null);
  const [allNodes, setAllNodes] = useState<Node[]>([]);
  const [standbyNode, setStandbyNode] = useState("");
  const [standbyInterval, setStandbyInterval] = useState(7);
  const [standbySaving, setStandbySaving] = useState(false);
  const [standbyRunning, setStandbyRunning] = useState(false);
  const [exDir, setExDir] = useState("");
  const [exExport, setExExport] = useState("");
  const [exImport, setExImport] = useState("");
  // F101: the fleet preset library, so a worked-out recipe is one click away
  // instead of being retyped per container. Applying one only FILLS the fields —
  // the operator still reviews them and presses Save, so nothing is configured
  // behind their back.
  const [presets, setPresets] = useState<ExportPreset[]>([]);
  useEffect(() => { api.exportPresets().then((r) => setPresets(r.presets || [])).catch(() => setPresets([])); }, []);
  const applyPreset = (id: string) => {
    const p = presets.find((x) => x.id === id);
    if (!p) return;
    setExDir(p.dir); setExExport(p.export_cmd); setExImport(p.import_cmd); setExVerify(p.verify_cmd || "");
    if (p.user) setHookUser(p.user);
  };
  // Critical-data low-RPO protection (PLAN §9.7).
  const [critical, setCritical] = useState<CriticalStatus | null>(null);
  const [critSaving, setCritSaving] = useState(false);
  // "Back up before changes" — event-triggered protective snapshot (F7).
  const [autosnap, setAutosnap] = useState(false);
  const [autosnapSaving, setAutosnapSaving] = useState(false);
  const seededAutosnap = useRef(false);
  // Large-bind cutoff (F12): the effective GiB threshold + this container's
  // override (0 = follow the global). bindInput is the editable override field.
  const [bindGiB, setBindGiB] = useState(5);
  // F132: directories this app rebuilds by itself (e.g. Jellyfin's trickplay
  // previews) and whether the operator has chosen to leave them out.
  const [regenerable, setRegenerable] = useState<RegenerablePath[]>([]);
  const [excludeRegen, setExcludeRegen] = useState(false);
  const [regenBusy, setRegenBusy] = useState(false);
  const [bindOverride, setBindOverride] = useState(0);
  const [bindInput, setBindInput] = useState("");
  const [bindSaving, setBindSaving] = useState(false);
  const seededBind = useRef(false);
  // Post-restore health-gate timeout (F30): the effective seconds + this
  // container's override (0 = follow the global). rtInput is the editable field.
  const [rtSecs, setRtSecs] = useState(300);
  const [rtOverride, setRtOverride] = useState(0);
  const [rtInput, setRtInput] = useState("");
  const [rtSaving, setRtSaving] = useState(false);
  // F184: the uid:gid restored data is pinned to for this container, what the
  // image itself declares (so the field can say what it is replacing), and the
  // edit buffer.
  const [ownPinned, setOwnPinned] = useState("");
  const [ownDeclared, setOwnDeclared] = useState("");
  const [ownInput, setOwnInput] = useState("");
  const [ownSaving, setOwnSaving] = useState(false);
  const seededRT = useRef(false);
  // Per-database backup selection for shared Postgres/MySQL (F8). dbList=null until
  // loaded; selDbs defaults to all (empty selection sent = full-cluster dump).
  const [dbList, setDbList] = useState<string[] | null>(null);
  const [selDbs, setSelDbs] = useState<Set<string>>(new Set());
  // "Set up notifications" nudge — shown only when no channel is configured and
  // the user hasn't dismissed it (server-tracked).
  const [notifyNudge, setNotifyNudge] = useState(false);
  // F69 ransomware tripwire: reason of an active mass-change retention hold ("" = none).
  // F73 config drift: cheap boolean from the detail payload; the field breakdown
  // is lazy-loaded from the /drift endpoint only while the banner is showing.
  const [drift, setDrift] = useState<{ changed: boolean } | null>(null);
  const [driftFields, setDriftFields] = useState<string[] | null>(null);

  // F73: fetch the drift field breakdown once per shown banner.
  useEffect(() => {
    if (!drift?.changed || driftFields !== null) return;
    let cancelled = false;
    api.containerDrift(id, cid)
      .then((r) => { if (!cancelled) setDriftFields(r.fields || []); })
      .catch(() => { if (!cancelled) setDriftFields([]); });
    return () => { cancelled = true; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [drift?.changed, driftFields, id, cid]);



  const load = async () => {
    try {
      const d = await api.containerDetail(id, cid);
      setC(d.container); setNodeName(d.node_name || ""); setBackups(d.backups || []); setStorage(d.storage);
      setSqliteFiles(d.sqlite_files || 0);
      setDrift(d.drift || null);
      if (!d.drift?.changed) setDriftFields(null); // banner cleared (fresh backup) → reset the breakdown
      setLabelManaged(!!d.label_managed); setLabelPolicy(d.label_policy || null);
      setReady(true);
      // Seed the pause selector once so the 6s poll doesn't clobber an edit.
      if (!seededPause.current) { setPauseMode(d.pause_mode || "pause"); seededPause.current = true; }
      setPauseDefault(d.pause_default || ""); setPauseDefaultWhy(d.pause_default_why || "");
      if (!seededAutosnap.current) { setAutosnap(!!d.autosnap); seededAutosnap.current = true; }
      if (!seededBind.current) {
        setRegenerable(d.regenerable ?? []);
        setExcludeRegen(!!d.exclude_regenerable);
        setOwnPinned(d.restore_ownership ?? "");
        setOwnInput(d.restore_ownership ?? "");
        setOwnDeclared(d.run_as ?? "");
        setBindGiB(d.bind_skip_gib ?? 5);
        setBindOverride(d.bind_skip_gib_override ?? 0);
        setBindInput(d.bind_skip_gib_override ? String(d.bind_skip_gib_override) : "");
        seededBind.current = true;
      }
      if (!seededRT.current) {
        setRtSecs(d.restore_health_timeout_seconds ?? 300);
        setRtOverride(d.restore_health_timeout_override ?? 0);
        setRtInput(d.restore_health_timeout_override ? String(d.restore_health_timeout_override) : "");
        seededRT.current = true;
      }
      setAutoHooks(d.auto_hooks || []);
      // Only seed the hook editor once (don't clobber edits on the 6s poll).
      setPreHooks((cur) => cur || (d.hooks?.pre || []).join("\n"));
      setPostHooks((cur) => cur || (d.hooks?.post || []).join("\n"));
      setPostRestoreHooks((cur) => cur || (d.hooks?.post_restore || []).join("\n"));
      setHookUser((cur) => cur || (d.hooks?.user || ""));
      const ae = d.app_export;
      setExportAvail(!!ae?.available); setExportTool(ae?.tool || "");
      setExDir((cur) => cur || (ae?.dir || ""));
      setExExport((cur) => cur || (ae?.export_cmd || ""));
      setExImport((cur) => cur || (ae?.import_cmd || ""));
      setExVerify((cur) => cur || (ae?.verify_cmd || ""));
      setCleanupExport(!!d.cleanup_export);
      setRequireWO(!!d.require_write_only); setWoBlocked(d.write_only_blocked_reason || "");
      setDbEngine(d.db_engine || ""); setRedisAuthSet(!!d.redis_auth_set);
      setRestoreStepUp(!!d.restore_step_up); setStepUpDefault(!!d.restore_step_up_default); setStepUpExplicit(!!d.restore_step_up_set);
      // Seed the backup-option controls once from the remembered choice (F3), so the
      // form reflects — and doesn't silently reset — what scheduled runs will use.
      if (!seededOpts.current) {
        const bo = d.backup_options;
        if (bo) {
          setCompression(bo.compression || "balanced");
          setSaveImage(!!bo.save_image);
          setUseExport(!!bo.app_export && !!ae?.available);
          setIncremental(!!bo.incremental);
          setIncFullEvery(bo.incremental_full_every || 7);
        }
        seededOpts.current = true;
      }
    } catch (e) { setErr((e as Error).message); }
  };
  useEffect(() => {
    load();
    // Load destinations + this node's EFFECTIVE policy (global with the node's
    // override applied); pre-select its default destinations so a node that
    // overrides where its backups go — e.g. a Synology node that shouldn't copy
    // back to the same Synology box — is honored here too, not just for scheduled
    // runs. When the node explicitly overrides, honor it exactly (empty = Local
    // only); otherwise fall back to all enabled if nothing is configured.
    Promise.all([api.destinations(), api.getNodePolicy(id).catch(() => null)]).then(([d, np]) => {
      setDests(d);
      const eff = (np?.effective?.destinations || []).filter((x) => d.some((y) => y.id === x));
      const overridden = !!np?.override?.override_destinations;
      setSelDests(new Set(overridden ? eff : (eff.length ? eff : d.map((x) => x.id))));
    }).catch(() => {});
    // Whether to show the "set up notifications" nudge (no channel configured yet
    // and not dismissed). Re-checked on each mount, so configuring in Settings and
    // returning hides it.
    api.notificationsHint().then((h) => setNotifyNudge(h.show)).catch(() => {});
    // F62: standby rehearsal config/result + the other nodes it can rehearse onto.
    api.getStandby(id, cid).then((sb) => {
      setStandby(sb);
      if (sb) { setStandbyNode(sb.standby_node); setStandbyInterval(sb.interval_days || 7); }
    }).catch(() => {});
    api.nodes().then((ns) => setAllNodes(ns)).catch(() => {});
  }, [id, cid]);
  usePoll(load, 6000, [id, cid]);

  // F62: standby rehearsal actions.
  const saveStandby = async () => {
    if (!standbyNode) return;
    setStandbySaving(true); setErr("");
    try {
      const sb = await api.setStandby(id, cid, { standby_node: standbyNode, interval_days: standbyInterval });
      setStandby(sb); setMsg("Standby rehearsal saved.");
    } catch (e) { setErr((e as Error).message); }
    finally { setStandbySaving(false); }
  };
  const removeStandby = async () => {
    setStandbySaving(true); setErr("");
    try { await api.deleteStandby(id, cid); setStandby(null); setStandbyNode(""); setMsg("Standby rehearsal removed."); }
    catch (e) { setErr((e as Error).message); }
    finally { setStandbySaving(false); }
  };
  const rehearseNow = async () => {
    setStandbyRunning(true); setErr("");
    try { await api.runStandby(id, cid); setMsg("Standby rehearsal started — the result appears here when it finishes."); }
    catch (e) { setErr((e as Error).message); }
    finally { setStandbyRunning(false); }
  };

  // Restore-drill results feed the recovery timeline's "drill-proven" state.
  // Light, fleet-wide call; refreshed slowly so a drill's outcome appears.

  const dismissNotifyNudge = async () => {
    setNotifyNudge(false);
    await api.dismissNotificationsHint().catch(() => {});
  };

  // True while a backup of THIS container is running (from the polled list).
  const backupInProgress = !!backups?.some((b) => b.status === "running");


  // Load critical-DB (low-RPO) status once we know this is a database container,
  // and keep the measured-RPO figure fresh on the poll.
  useEffect(() => {
    if (!c?.is_database) { setCritical(null); return; }
    api.getCritical(id, cid).then(setCritical).catch(() => {});
  }, [id, cid, c?.is_database]);
  usePoll(() => api.getCritical(id, cid).then(setCritical).catch(() => {}),
    c?.is_database ? 15000 : null, [id, cid]);

  // Load the DB container's databases for per-database selection (F8), once.
  // Defaults to all-selected (which sends an empty selection = full-cluster dump).
  useEffect(() => {
    if (!c?.is_database) { setDbList(null); return; }
    let cancelled = false;
    api.listDatabases(id, cid).then((r) => {
      if (cancelled) return;
      const dbs = r.supported ? (r.databases || []) : [];
      setDbList(dbs);
      setSelDbs(new Set(dbs));
    }).catch(() => { if (!cancelled) setDbList([]); });
    return () => { cancelled = true; };
  }, [id, cid, c?.is_database]);

  const toggleDb = (name: string, on: boolean) => {
    setSelDbs((prev) => { const n = new Set(prev); if (on) n.add(name); else n.delete(name); return n; });
  };

  const saveCritical = async (enabled: boolean, rpoSeconds: number) => {
    setCritSaving(true);
    try {
      await api.setCritical(id, cid, { enabled, rpo_seconds: rpoSeconds });
      const s = await api.getCritical(id, cid);
      setCritical(s);
      setMsg(enabled ? `Low-RPO protection on (target ${fmtDuration(rpoSeconds)})` : "Low-RPO protection off");
    } catch (e) { setErr((e as Error).message); }
    finally { setCritSaving(false); }
  };

  // Reset the (per-container) mount data + pending-run bridge when switching containers.
  useEffect(() => { setMounts(null); setSelMounts(new Set()); setPendingId(null); }, [id, cid]);


  // Load the container's mounts (volumes/binds) with sizes so the user can pick
  // what to back up. The size scan is HEAVY (it walks volume contents), so we
  // only run it once we KNOW no backup is in progress — re-fetching it while a
  // backup runs would compete for resources for nothing (PLAN §4.13/§9.9). It
  // runs automatically once a running backup finishes.
  useEffect(() => {
    if (!ready || backupInProgress || mounts !== null || mountsLoading) return;
    setMountsLoading(true);
    api.containerMounts(id, cid)
      .then((ms) => { setMounts(ms); setSelMounts(new Set(ms.filter((m) => m.selected).map((m) => m.destination))); })
      .catch(() => setMounts([]))
      .finally(() => setMountsLoading(false));
  }, [id, cid, ready, backupInProgress, mounts, mountsLoading]);

  const toggleDest = (did: string, on: boolean) =>
    setSelDests((prev) => { const n = new Set(prev); on ? n.add(did) : n.delete(did); return n; });
  const toggleMount = (dest: string, on: boolean) =>
    setSelMounts((prev) => { const n = new Set(prev); on ? n.add(dest) : n.delete(dest); return n; });



  // One-click protect (B5): smart defaults + schedule coverage + first backup.
  const protectNow = async () => {
    setProtecting(true);
    try {
      const res = await api.protectContainer(id, cid);
      // F38: a disabled schedule means no automatic protection — warn + offer enable.
      if (res.schedule_enabled === false && res.schedule_id) {
        toast.action({
          message: res.summary,
          actionLabel: "Enable the schedule",
          onAction: async () => {
            try {
              await api.enableSchedule(res.schedule_id);
              toast.success("Schedule enabled — this container will now back up automatically.");
              setTimeout(load, 500);
            } catch (e) { toast.error(`Couldn't enable the schedule: ${(e as Error).message}`); }
          },
        });
      } else {
        toast.success(res.summary);
      }
      setTimeout(load, 1500);
    } catch (e) { toast.error(`Couldn't protect: ${(e as Error).message}`); }
    finally { setProtecting(false); }
  };

  const initiate = async () => {
    setStarting(true); setMsg("");
    try {
      // Per-database selection (F8): only send it when the user has narrowed to a
      // strict subset — all-selected (or no list) sends nothing, keeping the
      // full-cluster dump so today's behavior is unchanged.
      const dbSel = c?.is_database && dbList && dbList.length > 0 && selDbs.size < dbList.length
        ? Array.from(selDbs) : undefined;
      const res = await api.createBackup(id, cid, false, compression, Array.from(selDests), useExport, mounts ? Array.from(selMounts) : undefined, saveImage, pauseMode, dbSel, { enabled: incremental, full_every: incFullEvery });
      if (res.status === "already_running" || res.status === "already_queued") {
        // Server coalesced a duplicate click onto the pending run — nothing new
        // started. Track that run so the button reflects it.
        toast.info(`A backup of ${c?.name || "this container"} is already ${res.status === "already_running" ? "running" : "queued"}.`);
        setPendingId(res.backup_id || null);
      } else {
        const where = ["Local", ...dests.filter((d) => selDests.has(d.id)).map((d) => d.name)].join(", ");
        setMsg(`${useExport ? `App-native (${exportTool}) backup` : "Backup"} initiated → ${where}. Watch the console below.`);
        setPendingId(res.backup_id || null);
      }
      // Refresh now (the run usually appears within a second) and again shortly
      // after, so the pending state resolves without waiting for the 6s poll.
      load();
      setTimeout(load, 1500);
    }
    catch (e) { setMsg((e as Error).message); }
    finally { setStarting(false); }
  };

  // Resolve the pending run once it shows up in the polled list. Large
  // containers surface as "running" (the in-progress state takes over); small
  // ones can finish between polls — surface THAT explicitly, so a backup that
  // completed in under a second doesn't look like a click that never happened.
  useEffect(() => {
    if (!pendingId) return;
    const row = backups.find((b) => b.id === pendingId);
    if (!row) return;
    setPendingId(null);
    if (row.status === "success") toast.success(`Backup of ${c?.name || row.target_name} completed.`);
    else if (row.status === "failed") toast.error(`Backup of ${c?.name || row.target_name} failed${row.error ? `: ${row.error}` : ""}.`);
    // "running" needs no toast — the button flips to "Backup in progress…".
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [backups, pendingId]);

  // Failsafe: never leave the button stuck if the accepted run's row can't be
  // observed (e.g. it stays queued behind other work on this node for a long
  // time). Re-enabling is safe — the server now coalesces duplicate requests.
  useEffect(() => {
    if (!pendingId) return;
    const t = setTimeout(() => setPendingId(null), 90000);
    return () => clearTimeout(t);
  }, [pendingId]);

  const saveHooks = async () => {
    const toLines = (s: string) => s.split("\n").map((l) => l.trim()).filter(Boolean);
    try {
      await api.setHooks(id, cid, { pre: toLines(preHooks), post: toLines(postHooks), post_restore: toLines(postRestoreHooks), user: hookUser.trim() });
      toast.success("Backup hooks saved");
    } catch (e) { toast.error(`Couldn't save hooks: ${(e as Error).message}`); }
  };
  const saveExportProfile = async () => {
    try {
      await api.setExportProfile(id, cid, { dir: exDir.trim(), export_cmd: exExport.trim(), import_cmd: exImport.trim(), verify_cmd: exVerify.trim(), user: hookUser.trim() });
      setExportAvail(!!(exDir.trim() && exExport.trim() && exImport.trim()));
      toast.success("Export profile saved");
    } catch (e) { toast.error(`Couldn't save export profile: ${(e as Error).message}`); }
  };
  // F32: share a working export profile between containers via clipboard JSON.
  const copyProfile = async () => {
    const profile = { dir: exDir.trim(), export_cmd: exExport.trim(), import_cmd: exImport.trim(), verify_cmd: exVerify.trim(), user: hookUser.trim() };
    if (!profile.dir && !profile.export_cmd && !profile.import_cmd) { toast.error("Nothing to copy — the export profile is empty"); return; }
    try { await navigator.clipboard.writeText(JSON.stringify(profile, null, 2)); toast.success("Export profile copied to clipboard"); }
    catch { toast.error("Couldn't access the clipboard"); }
  };
  const pasteProfile = async () => {
    let text = "";
    try { text = await navigator.clipboard.readText(); } catch { /* fall back to a prompt */ }
    if (!text.trim()) text = window.prompt("Paste the export profile JSON:") || "";
    if (!text.trim()) return;
    try {
      const p = JSON.parse(text) as { dir?: string; export_cmd?: string; import_cmd?: string; verify_cmd?: string; user?: string };
      if (typeof p !== "object" || p === null) throw new Error("not an object");
      setExDir(p.dir ?? "");
      setExExport(p.export_cmd ?? "");
      setExImport(p.import_cmd ?? "");
      setExVerify(p.verify_cmd ?? "");
      if (typeof p.user === "string") setHookUser(p.user);
      toast.success("Profile pasted — review, then Save export profile");
    } catch { toast.error("That isn't a valid export profile JSON"); }
  };

  // F145: an application may declare its own quiesce default, with a reason.
  // Where it does, "live copy" is the considered choice rather than an omission,
  // so the nudge below must not fire against it — a warning that contradicts the
  // product's own default is how operators learn to ignore warnings.
  const [pauseDefault, setPauseDefault] = useState("");
  const [pauseDefaultWhy, setPauseDefaultWhy] = useState("");
  const recommendStop = !!c && !c.is_database && c.volume_count > 0 && pauseMode === "none" && pauseDefault !== "none";

  // Persist the per-container pause behavior immediately so scheduled/bulk
  // backups honor it too (PLAN §4.2).
  const changeAutosnap = async (enabled: boolean) => {
    setAutosnap(enabled); setAutosnapSaving(true);
    try { await api.setAutosnap(id, cid, enabled); } catch (e) { setAutosnap(!enabled); setMsg((e as Error).message); }
    finally { setAutosnapSaving(false); }
  };

  // F151: saved immediately, for consistency with every other per-container
  // toggle — and reverted in place if the server refuses, so the checkbox never
  // shows a state the backend does not hold.
  // F163: saved immediately, like every other per-container toggle. The response
  // says whether the NEXT run would be refused, so an operator who turns this on
  // while write-only is off finds out here rather than from a failed schedule.
  const changeRequireWriteOnly = async (on: boolean) => {
    setRequireWO(on); setWoSaving(true);
    try { const r = await api.setRequireWriteOnly(id, cid, on); setWoBlocked(r.write_only_blocked_reason || ""); }
    catch (e) { setRequireWO(!on); setMsg((e as Error).message); }
    finally { setWoSaving(false); }
  };

  // F206: saved immediately, like every other per-container toggle, and reverted
  // in place if the server refuses so the switch never shows a state the backend
  // does not hold. Setting it back to the derived default clears the override
  // rather than pinning the same value, so a container marked critical later is
  // protected without anyone coming back here.
  const changeRestoreStepUp = async (on: boolean) => {
    const prev = restoreStepUp;
    setRestoreStepUp(on); setStepUpSaving(true);
    try {
      const r = await api.setRestoreStepUp(id, cid, on === stepUpDefault ? null : on);
      setRestoreStepUp(r.restore_step_up); setStepUpDefault(r.restore_step_up_default); setStepUpExplicit(r.restore_step_up_set);
    } catch (e) { setRestoreStepUp(prev); setMsg((e as Error).message); }
    finally { setStepUpSaving(false); }
  };

  // F205: saved immediately, like every other per-container option. An empty
  // value CLEARS the recorded password rather than storing an empty one.
  const saveRedisAuth = async (clear = false) => {
    setRedisPwSaving(true); setRedisPwMsg("");
    try {
      const r = await api.setRedisAuth(id, cid, clear ? "" : redisPw);
      setRedisAuthSet(r.redis_auth_set);
      setRedisPw("");
      setRedisPwMsg(r.redis_auth_set
        ? "Saved. The next backup of this container will use it for a point-in-time snapshot."
        : "Cleared. Backups will look for the password the usual ways, and fall back to a file capture if none works.");
    } catch (e) {
      setRedisPwMsg((e as Error).message);
    } finally { setRedisPwSaving(false); }
  };

  const changeCleanupExport = async (on: boolean) => {
    setCleanupExport(on); setCleanupSaving(true);
    try { await api.setCleanupExport(id, cid, on); }
    catch (e) { setCleanupExport(!on); setMsg((e as Error).message); }
    finally { setCleanupSaving(false); }
  };

  const changePauseMode = async (mode: string) => {
    setPauseMode(mode);
    try { await api.setPauseMode(id, cid, mode); } catch (e) { setMsg((e as Error).message); }
  };

  // Save (or clear, when blank) this container's large-bind cutoff override, then
  // re-scan mounts so the default selection reflects the new threshold (F12).
  const saveBindThreshold = async () => {
    const raw = bindInput.trim();
    const gib = raw === "" ? 0 : Math.min(1024, Math.max(1, parseInt(raw, 10) || 0));
    setBindSaving(true);
    try {
      const r = await api.setBindThreshold(id, cid, gib);
      setBindGiB(r.bind_skip_gib);
      setBindOverride(r.bind_skip_gib_override);
      setBindInput(r.bind_skip_gib_override ? String(r.bind_skip_gib_override) : "");
      setMounts(null); // re-evaluate the default selection against the new threshold
    } catch (e) { setMsg((e as Error).message); }
    finally { setBindSaving(false); }
  };

  // Save (or clear, when blank) this container's post-restore health-timeout
  // override, so a heavy app isn't force-rolled-back mid-migration (F30).
  const saveRestoreOwnership = async () => {
    setOwnSaving(true);
    try {
      const r = await api.setRestoreOwnership(id!, cid!, ownInput.trim());
      setOwnPinned(r.restore_ownership);
      setOwnInput(r.restore_ownership);
    } catch (e) { setMsg((e as Error).message); }
    finally { setOwnSaving(false); }
  };

  const saveRestoreTimeout = async () => {
    const raw = rtInput.trim();
    const secs = raw === "" ? 0 : Math.min(3600, Math.max(30, parseInt(raw, 10) || 0));
    setRtSaving(true);
    try {
      const r = await api.setRestoreTimeout(id, cid, secs);
      setRtSecs(r.restore_health_timeout_seconds);
      setRtOverride(r.restore_health_timeout_override);
      setRtInput(r.restore_health_timeout_override ? String(r.restore_health_timeout_override) : "");
    } catch (e) { setMsg((e as Error).message); }
    finally { setRtSaving(false); }
  };


  const [tab, setTab] = useState<BackupTab>(() => {
    const q = new URLSearchParams(window.location.search).get("tab");
    return (BACKUP_TABS.some((t) => t.id === q) ? q : "run") as BackupTab;
  });
  const selectTab = (t: BackupTab) => {
    setTab(t);
    // Reflected in the URL so a specific group can be linked to, without a
    // history entry per tab click.
    const u = new URL(window.location.href);
    if (t === "run") u.searchParams.delete("tab"); else u.searchParams.set("tab", t);
    window.history.replaceState(null, "", u.toString());
  };

  return (
    <div>
      <nav className="mb-6 flex flex-wrap items-center gap-2 text-xs uppercase tracking-wider text-on-surface-variant">
        <Link to="/servers" className="hover:text-primary">Servers</Link>
        <ChevronRight size={14} className="shrink-0" />
        <Link to={`/servers/${id}`} className="hover:text-primary">{nodeName || id}</Link>
        <ChevronRight size={14} className="shrink-0" />
        <Link to={`/servers/${id}/containers/${cid}`} className="min-w-0 break-all hover:text-primary">{c?.name || cid.slice(0, 12)}</Link>
        <ChevronRight size={14} className="shrink-0" />
        <span className="font-semibold text-on-surface">Back up</span>
      </nav>

      {err && <div className="mb-4 break-words rounded bg-error/10 px-4 py-3 text-error">{err}</div>}
      {msg && <div className="mb-4 break-words rounded bg-secondary/10 px-4 py-3 text-secondary">{msg}</div>}

      <Card className="mb-5 p-5">
        <div className="flex flex-wrap items-center gap-3">
          <h1 className="min-w-0 break-all text-2xl font-bold">Back up {c?.name || cid.slice(0, 12)}</h1>
          <StateChip state={c?.state} />
          {c?.image && <span className="min-w-0 break-all rounded bg-surface-highest px-2 py-0.5 font-mono text-xs text-on-surface-variant">{c.image}</span>}
          <Button variant="secondary" className="ml-auto" onClick={protectNow} disabled={protecting || !c || labelManaged}
            title="Apply smart defaults (live DB dump / consistent volumes, policy destinations, add to schedule) and start the first backup">
            {protecting ? <Loader2 size={15} className="animate-spin" /> : <ShieldCheck size={15} />} Protect
          </Button>
        </div>
      </Card>

      <div className="mb-5 flex flex-wrap gap-1 border-b border-outline-variant">
        {BACKUP_TABS.map((t) => (
          <button key={t.id} onClick={() => selectTab(t.id)}
            className={`-mb-px rounded-t-lg border border-b-0 px-3.5 py-2 text-xs font-semibold ${tab === t.id ? "border-outline-variant bg-surface-container text-on-surface" : "border-transparent text-on-surface-variant hover:text-on-surface"}`}>
            {t.label}
          </button>
        ))}
      </div>


            {labelManaged && (
              <div className="mb-4 rounded border border-primary/30 bg-primary/10 p-3 text-sm">
                <div className="flex items-center gap-2 font-semibold text-on-surface"><Tags size={15} className="text-primary" /> Managed by labels</div>
                <p className="mt-1 text-xs text-on-surface-variant">This container's protection is defined by <code className="font-mono">dockback.*</code> labels; edit them where the container is defined. The controls they govern are read-only here to avoid drift.</p>
                <ul className="mt-2 space-y-0.5 text-xs text-on-surface-variant">
                  {labelPolicy?.enable !== undefined && <li>· Protection: <span className="font-mono text-on-surface">{labelPolicy.enable ? "enabled" : "disabled"}</span></li>}
                  {labelPolicy?.schedule && <li>· Schedule: <span className="font-mono text-on-surface">{labelPolicy.schedule}</span></li>}
                  {labelPolicy?.pause_mode && <li>· Pause mode: <span className="font-mono text-on-surface">{labelPolicy.pause_mode}</span></li>}
                  {!!labelPolicy?.exclude_mounts?.length && <li>· Excluded paths: <span className="font-mono text-on-surface">{labelPolicy.exclude_mounts.join(", ")}</span></li>}
                  {labelPolicy?.retention && <li>· Retention: <span className="font-mono text-on-surface">{`daily:${labelPolicy.retention.keep_daily} weekly:${labelPolicy.retention.keep_weekly} monthly:${labelPolicy.retention.keep_monthly} yearly:${labelPolicy.retention.keep_yearly}`}</span></li>}
                </ul>
              </div>
            )}


      {/* ---------------- RUN ---------------- */}
      {tab === "run" && (
      <div className="grid grid-cols-1 gap-5 lg:grid-cols-3">
        <div className="space-y-5 lg:col-span-2">
          <Card className="p-5">
            <h2 className="mb-3 text-base font-bold">What gets captured</h2>
            {!useExport && (
              <>
                <Label>Volumes / paths to back up</Label>
                <div className="mb-4 space-y-2">
                  {sqliteFiles > 0 && (
                    <div className="flex items-start gap-2 rounded border border-primary/30 bg-primary/10 px-3 py-2 text-xs text-on-surface">
                      <Database size={13} className="mt-0.5 shrink-0 text-primary" />
                      <span>{sqliteFiles} SQLite database{sqliteFiles === 1 ? "" : "s"} — captured consistently (online snapshot, not a live file copy).</span>
                    </div>
                  )}
                  {backupInProgress && !mounts && (
                    <div className="flex items-center gap-2 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-xs text-on-surface-variant">
                      <BackupCloudIcon size={13} active className="text-secondary" /> A backup is in progress — follow it in the console below. Volume scan paused to save resources.
                    </div>
                  )}
                  {!backupInProgress && mountsLoading && (
                    <div className="flex items-center gap-2 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-xs text-on-surface-variant">
                      <Loader2 size={13} className="animate-spin" /> Scanning volumes & sizes…
                    </div>
                  )}
                  {!mountsLoading && mounts && mounts.length === 0 && (
                    <div className="rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-xs text-on-surface-variant">
                      No named volumes or writable bind mounts — only the container configuration will be captured.
                    </div>
                  )}
                  {!mountsLoading && mounts && mounts.map((m) => (
                    <label key={m.destination} className="flex cursor-pointer items-start gap-3 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm">
                      <input type="checkbox" className="mt-1 disabled:cursor-not-allowed disabled:opacity-60" checked={selMounts.has(m.destination)} disabled={labelManaged && !!labelPolicy?.exclude_mounts?.length} title={labelManaged && labelPolicy?.exclude_mounts?.length ? "Managed by a dockback.mounts.exclude label" : undefined} onChange={(e) => toggleMount(m.destination, e.target.checked)} />
                      <div className="min-w-0 flex-1">
                        <div className="flex items-center gap-2">
                          <span className="truncate font-mono text-xs text-on-surface">{m.destination}</span>
                          <span className="text-[10px] uppercase text-on-surface-variant">{m.type}</span>
                          {m.type === "bind" && !m.rw && <span className="shrink-0 rounded-sm bg-surface-high px-1.5 py-0.5 text-[10px] font-semibold uppercase text-on-surface-variant" title="The container mounts this read-only. DockBack can still read and back it up — the mount's own flag never stopped that — and its contents are often exactly what a restore onto a new machine cannot recreate, such as a key or a licence file.">read-only</span>}
                          {m.unreadable && <span className="shrink-0 rounded-sm bg-warning/15 px-1.5 py-0.5 text-[10px] font-semibold uppercase text-warning" title="The backup reader can't access some files here (likely a UID/GID mismatch / NFS root_squash). Those files would be missing from the backup.">permission</span>}
                          {m.snapshotable && <span className="shrink-0 rounded-sm bg-primary/15 px-1.5 py-0.5 text-[10px] font-semibold uppercase text-primary" title={`This volume sits on ${m.fs_type}, which supports atomic filesystem snapshots. DockBack still captures it consistently via its sidecar; a host-level snapshot would need privileges the hardened app deliberately doesn't have.`}>{m.fs_type}</span>}
                          {m.covered_by && <span className="shrink-0 rounded-sm bg-success/15 px-1.5 py-0.5 text-[10px] font-semibold uppercase text-success" title={`This shared folder is captured in ${m.covered_by}'s backups — leaving it unticked here is not data loss, and stack backups capture it once.`}>covered</span>}
                          <span className="ml-auto shrink-0 text-xs text-on-surface-variant">{m.size_known ? fmtBytes(m.size_bytes) : "size n/a"}</span>
                        </div>
                        {m.covered_by
                          ? <div className="mt-0.5 text-[11px] text-success">backed up via {m.covered_by}</div>
                          : m.reason && <div className="mt-0.5 text-[11px] text-warning">{m.reason}</div>}
                        {m.shared_with && m.shared_with.length > 0 && (
                          <div className="mt-0.5 text-[11px] text-on-surface-variant">also mounted in {m.shared_with.join(", ")}</div>
                        )}
                        {m.unreadable && <div className="mt-0.5 text-[11px] text-warning">Some files aren't readable — fix host ownership to match the container's user, or those files will be missing.</div>}
                      </div>
                    </label>
                  ))}
                  <p className="text-xs text-on-surface-variant">Named volumes are selected by default; bind mounts larger than <b>{bindGiB} GiB</b> (e.g. media libraries) are skipped unless you tick them. Your selection is remembered for scheduled backups.</p>

                  {/* F132: directories this app regenerates on its own. Offered only
                      for images that declare any, and default OFF — fidelity first;
                      the size trade should be an explicit choice, not something that
                      quietly happens to someone's backup. */}
                  {regenerable.length > 0 && (
                    <label className="flex cursor-pointer items-start gap-3 rounded border border-outline-variant/50 bg-surface-lowest px-3 py-2.5 text-xs">
                      <input type="checkbox" className="mt-0.5 disabled:opacity-60" checked={excludeRegen} disabled={regenBusy}
                        onChange={async (e) => {
                          const next = e.target.checked;
                          setRegenBusy(true); setExcludeRegen(next);
                          try { await api.setExcludeRegenerable(id!, cid!, next); }
                          catch { setExcludeRegen(!next); }
                          finally { setRegenBusy(false); }
                        }} />
                      <span className="min-w-0">
                        <span className="font-medium text-on-surface">
                          Leave out {regenerable.map((r) => (r.label || r.path || "").toLowerCase()).filter(Boolean).join(", ")}
                        </span>
                        {regenerable.map((r) => (
                          <span key={r.path} className="mt-0.5 block break-words text-on-surface-variant">
                            <span className="font-mono text-[11px]">{r.path}</span> — {r.cost}
                          </span>
                        ))}
                        <span className="mt-0.5 block text-on-surface-variant">
                          Off by default: everything is captured. Tick this to trade a smaller backup for that regeneration.
                        </span>
                      </span>
                    </label>
                  )}
                  {/* Large-bind cutoff (F12): per-container override of the global threshold. */}
                  <div className="rounded border border-outline-variant/50 bg-surface-lowest px-3 py-2.5 text-xs">
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="text-on-surface-variant">Skip binds larger than</span>
                      <input type="number" min={1} max={1024} value={bindInput} placeholder={String(bindGiB)}
                        onChange={(e) => setBindInput(e.target.value)}
                        aria-label="Large bind-mount threshold in GiB for this container"
                        className="w-24 rounded border border-outline-variant bg-surface-lowest px-2 py-1 text-xs text-on-surface outline-none focus:border-docker-blue focus:ring-2 focus:ring-docker-blue/40" />
                      <span className="text-on-surface-variant">GiB</span>
                      <Button size="sm" variant="secondary" disabled={bindSaving} onClick={saveBindThreshold}>
                        {bindSaving && <Loader2 size={13} className="animate-spin" />} Save
                      </Button>
                      {bindOverride > 0
                        ? <span className="text-[11px] text-primary">Per-container override active ({bindOverride} GiB). Clear the field and Save to follow the global setting.</span>
                        : <span className="text-[11px] text-on-surface-variant">Following the global setting ({bindGiB} GiB). Leave blank to keep following it.</span>}
                    </div>
                    <p className="mt-1 text-on-surface-variant">Set a per-container cutoff, or leave blank to follow the global value (Settings → Performance &amp; tuning). Changing it re-evaluates the default selection above.</p>
                  </div>
                </div>
              </>
            )}

          </Card>
          <Card className="p-5">
            <h2 className="mb-3 text-base font-bold">Included in the archive</h2>
            <Label>Backup Options</Label>
            {c?.is_database ? (
              <>
                <div className="mb-2 flex items-center gap-3 rounded border border-outline-variant bg-surface-lowest/50 px-3 py-2.5 text-sm text-on-surface-variant">
                  Consistency during volume backup
                  <span className="ml-auto text-xs">Database — dumped live (never paused)</span>
                </div>
                {/* Per-database selection for a shared engine (F8). Only shown when the
                    engine hosts more than one app database. */}
                {dbList && dbList.length > 1 && (
                  <div className="mb-2 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm">
                    <div className="mb-1.5 flex items-center gap-2">
                      <Database size={15} className="shrink-0 text-primary" />
                      <span>Databases to include</span>
                      <span className="ml-auto text-xs text-on-surface-variant tnum">{selDbs.size}/{dbList.length}</span>
                    </div>
                    <div className="space-y-1">
                      {dbList.map((db) => (
                        <label key={db} className="flex cursor-pointer items-center gap-2 text-xs">
                          <input type="checkbox" className="shrink-0" checked={selDbs.has(db)} onChange={(e) => toggleDb(db, e.target.checked)} />
                          <span className="min-w-0 truncate font-mono text-on-surface">{db}</span>
                        </label>
                      ))}
                    </div>
                    <p className="mt-1.5 text-[11px] italic text-on-surface-variant">
                      {selDbs.size === dbList.length
                        ? "All databases (whole cluster, incl. roles/globals) — the default."
                        : selDbs.size === 0
                          ? "None selected — the whole cluster is backed up (unselecting all means “all”)."
                          : "Only the selected database(s) are dumped; a restore re-imports just them and leaves the others on this engine untouched."}
                    </p>
                  </div>
                )}
              </>
            ) : (
              <>
                <div className="mb-1 flex items-center gap-3 rounded border border-outline-variant bg-surface-lowest px-3 py-2 text-sm">
                  <span>Consistency during volume backup</span>
                  <select
                    value={pauseMode}
                    onChange={(e) => changePauseMode(e.target.value)}
                    disabled={labelManaged && !!labelPolicy?.pause_mode}
                    title={labelManaged && labelPolicy?.pause_mode ? "Set by a dockback.pause-mode label" : undefined}
                    className="ml-auto rounded border border-outline-variant bg-surface-lowest px-2 py-1 text-xs focus:outline-none focus:ring-1 focus:ring-primary disabled:cursor-not-allowed disabled:opacity-60"
                  >
                    <option value="none">Live copy (no pause){pauseDefault === "none" ? " — recommended for this app" : ""}</option>
                    <option value="pause">Pause during copy{pauseDefault === "" ? " (default, recommended)" : ""}</option>
                    <option value="stop">Stop during copy (full quiesce){pauseDefault === "stop" ? " — recommended for this app" : ""}</option>
                  </select>
                </div>
                <p className="mb-2 mt-0.5 text-xs italic text-on-surface-variant">
                  {pauseMode === "stop"
                    ? "Container is stopped during the volume copy, then restarted — brief downtime, maximum consistency."
                    : pauseMode === "pause"
                      ? "Container is frozen during the volume copy — near-zero downtime, consistent snapshot. Remembered for scheduled backups."
                      : "Files are copied live — fast, but a busy app may produce an inconsistent snapshot."}
                </p>
                {/* F145: why this app's default differs from the shipped one.
                    Said out loud so the choice can be disagreed with — the
                    dropdown above still offers every mode. */}
                {pauseDefaultWhy && (
                  <p className="mb-2 -mt-1 break-words rounded bg-primary/10 px-2 py-1.5 text-xs text-on-surface">
                    DockBack defaults this container to <b>{pauseDefault === "none" ? "a live copy" : pauseDefault === "stop" ? "a full stop" : pauseDefault}</b> because {pauseDefaultWhy}.
                  </p>
                )}
              </>
            )}
            {exportAvail && (
              <label className="mb-2 flex cursor-pointer items-center gap-3 rounded border border-secondary/30 bg-secondary/10 px-3 py-2.5 text-sm">
                <input type="checkbox" checked={useExport} onChange={(e) => setUseExport(e.target.checked)} />
                App-native export ({exportTool}) <span className="ml-auto text-xs text-on-surface-variant">portable</span>
              </label>
            )}
            {/* Incremental volume capture (F61): after a full baseline, back up only
                changed files; force a fresh full every N so chains stay short. */}
            {!useExport && (
              <label className="mb-2 flex cursor-pointer items-start gap-3 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm">
                <input type="checkbox" className="mt-0.5" checked={incremental} onChange={(e) => setIncremental(e.target.checked)} />
                <span className="min-w-0 flex-1">
                  <span className="flex items-center gap-2"><Layers size={15} className="shrink-0 text-primary" /> Incremental volume capture <span className="ml-auto shrink-0 text-xs text-on-surface-variant">changed files only</span></span>
                  <span className="mt-0.5 block text-[11px] italic text-on-surface-variant">Captures only files changed since the previous backup. Databases are always dumped in full. Restore, verify, and drills apply the whole chain automatically.</span>
                  {incremental && (
                    <span className="mt-2 flex flex-wrap items-center gap-2 text-xs not-italic text-on-surface" onClick={(e) => e.preventDefault()}>
                      Full backup every
                      <input type="number" min={2} max={30} value={incFullEvery}
                        onChange={(e) => setIncFullEvery(Math.max(2, Math.min(30, Number(e.target.value) || 7)))}
                        aria-label="Force a full backup every N backups"
                        className="w-16 rounded border border-outline-variant bg-surface-lowest px-2 py-1 text-xs text-on-surface outline-none focus:border-docker-blue focus:ring-2 focus:ring-docker-blue/40" />
                      backups
                    </span>
                  )}
                </span>
              </label>
            )}
          </Card>
        </div>

        <div className="space-y-5 lg:row-span-2">
          <Card className="p-5">
            <h2 className="mb-3 text-base font-bold">Run</h2>
            <Label>Destinations</Label>
            <div className="mb-4 space-y-2">
              <div className="flex items-center gap-3 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm text-on-surface-variant">
                <input type="checkbox" checked disabled />
                <HardDrive size={15} /> Local <span className="ml-auto font-mono text-xs">{storage || "/app/backups"}</span>
              </div>
              {dests.map((d) => (
                <label key={d.id} className="flex cursor-pointer items-center gap-3 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm">
                  <input type="checkbox" checked={selDests.has(d.id)} onChange={(e) => toggleDest(d.id, e.target.checked)} />
                  <CloudUpload size={15} className="text-secondary" /> {d.name}
                  <span className="ml-auto text-xs uppercase text-on-surface-variant">{d.type}</span>
                </label>
              ))}
              {dests.length === 0 && (
                <button onClick={() => navigate("/settings")} className="w-full rounded border border-dashed border-outline-variant px-3 py-2.5 text-left text-xs text-on-surface-variant hover:border-primary hover:text-primary">
                  No external destinations yet — add Synology / Nextcloud / S3 in Settings →
                </button>
              )}
              <p className="text-xs text-on-surface-variant">Copies are written to Local plus every checked destination (3-2-1). Pick a different set to spread copies and avoid a single point of failure.</p>
            </div>
            <Label>Compression</Label>
            <Select value={compression} onChange={(e) => setCompression(e.target.value)}>
              <option value="fast">Fast (zstd)</option>
              <option value="balanced">Balanced (zstd) — recommended</option>
              <option value="max">Max (zstd)</option>
              <option value="max-long">Max + long-range (zstd) — large redundant data</option>
              <option value="gzip">gzip (universal compatibility)</option>
              <option value="xz">xz (maximum ratio, slow)</option>
            </Select>
            <p className="mb-4 mt-1 text-xs italic text-on-surface-variant">zstd gives the best speed/ratio balance (recommended). Max + long-range uses a 32&nbsp;MiB match window to deduplicate repeats far apart in large archives (repeated files, DB dumps, logs) — best for big, redundant payloads at a higher CPU/RAM cost. gzip for universal compatibility; xz for the smallest archive at a high CPU cost.</p>

            <Label>Target Volume / Path</Label>
            <div className="mb-4 flex items-center gap-2">
              <input readOnly value={storage || "—"} className="w-full rounded border border-outline-variant bg-surface-lowest px-3 py-2 font-mono text-xs text-on-surface-variant" />
              <button onClick={() => navigate("/settings")} title="Configure storage" className="rounded border border-outline-variant p-2 hover:bg-surface-highest"><Pencil size={15} /></button>
            </div>

            <Button variant="primary" className="mb-2 w-full py-3" disabled={starting || !c || backupInProgress || !!pendingId} onClick={initiate}>
              <BackupCloudIcon size={18} active={starting || !!pendingId || backupInProgress} /> {runButtonLabel({ backupInProgress, queued: !!pendingId, starting })}
            </Button>
          </Card>
        </div>

        {/* After the Run card in source order, so a single-column layout puts it
            straight under the button; on wide screens it fills the space under
            the capture options. */}
        <BackupConsole containerId={cid} className="lg:col-span-2" />
      </div>
      )}

      {/* ---------------- SCHEDULE & RETENTION ---------------- */}
      {tab === "schedule" && (
        <Card className="p-5">
            <ContainerPolicyPanel id={id} cid={cid} stack={c?.stack} onNavigateSettings={() => navigate("/settings#schedule")} />

            {(recommendStop || c?.is_database) && (
              <div className="mt-4 flex items-start gap-2 rounded border border-warning/20 bg-warning/10 px-3 py-2 text-sm text-warning">
                {c?.is_database ? <Database size={16} className="mt-0.5 shrink-0" /> : <AlertTriangle size={16} className="mt-0.5 shrink-0" />}
                {c?.is_database
                  ? <span><b>{c?.name}</b> is a database — it will be dumped live with native tools (no downtime); it is never paused.</span>
                  : <span><b>{c?.name}</b> has {c?.volume_count} persistent volume(s). Set <b>Pause</b> (or Stop) under "Consistency during volume backup" for a clean snapshot.</span>}
              </div>
            )}

            {/* Event-triggered protective snapshot (F7): back up before a recreate/update/removal. */}
            <label className="mb-2 flex cursor-pointer items-start gap-3 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm">
              <input type="checkbox" className="mt-0.5" checked={autosnap} disabled={autosnapSaving} onChange={(e) => changeAutosnap(e.target.checked)} />
              <span className="min-w-0 flex-1">
                <span className="flex items-center gap-2"><History size={15} className="shrink-0 text-primary" /> Back up before changes <span className="ml-auto shrink-0 text-xs text-on-surface-variant">auto-snapshot</span></span>
                <span className="mt-0.5 block text-[11px] italic text-on-surface-variant">When this container is about to be recreated, updated, or removed (e.g. <span className="font-mono">docker compose up -d</span> or an auto-updater), DockBack first captures a labeled <span className="font-mono">auto: pre-change</span> snapshot, so a bad update always has a fresh rollback point. Rapid events are coalesced into one.</span>
              </span>
            </label>
        </Card>
      )}

      {/* ---------------- PROTECTION ---------------- */}
      {tab === "protection" && (
        <Card className="p-5">
                {/* F34: Postgres point-in-time-recovery readiness from the read-only
                    probe — green when the server can do PITR, amber (with the exact
                    one-line postgresql.conf fix) when recovery is full-dump only.
                    It reports the SERVER's capability: DockBack's own base is a
                    logical pg_dumpall, which restores into a freshly initialised
                    cluster that archived WAL cannot be replayed onto, so its own
                    recovery point is the last dump either way. Labelled "Server"
                    so a green chip on a backup tool's page is not read as a
                    promise about the backups on that page. */}
                {critical?.pitr_checked && (
                  <div className={`mb-2 flex items-start gap-2.5 rounded border px-3 py-2.5 text-sm ${critical.pitr_ready ? "border-success/30 bg-success/10" : "border-warning/40 bg-warning/10"}`}>
                    {critical.pitr_ready
                      ? <ShieldCheck size={16} className="mt-0.5 shrink-0 text-success" />
                      : <AlertTriangle size={16} className="mt-0.5 shrink-0 text-warning" />}
                    <div className="min-w-0">
                      <div className="flex flex-wrap items-center gap-2">
                        <span className="min-w-0 break-words font-medium text-on-surface">Server PITR readiness</span>
                        <span className={`shrink-0 rounded-full px-2 py-0.5 text-[11px] font-medium ${critical.pitr_ready ? "bg-success/20 text-success" : "bg-warning/20 text-warning"}`}>
                          {critical.pitr_ready ? "Server ready" : "Full-dump only"}
                        </span>
                      </div>
                      {critical.pitr_detail && <p className="mt-1 text-xs text-on-surface-variant">{critical.pitr_detail}</p>}
                      <p className="mt-1 break-all font-mono text-[11px] text-on-surface-variant">
                        wal_level={critical.wal_level || "?"} · archive_mode={critical.archive_mode || "?"}
                        {critical.max_wal_senders ? ` · max_wal_senders=${critical.max_wal_senders}` : ""}
                      </p>
                    </div>
                  </div>
                )}
          {c?.is_database && (
            <>
                {/* Critical-data low-RPO protection (PLAN §9.7). */}
                <div className="mb-2 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm">
                  <label className="flex cursor-pointer items-center gap-3">
                    <Gauge size={16} className="shrink-0 text-primary" />
                    <span>Critical data (low-RPO protection)</span>
                    <input
                      type="checkbox"
                      className="ml-auto"
                      disabled={critSaving}
                      checked={!!critical?.enabled}
                      onChange={(e) => saveCritical(e.target.checked, critical?.rpo_seconds || 900)}
                    />
                  </label>
                  {critical?.enabled && (
                    <div className="mt-2 flex items-center gap-2 text-xs">
                      <span className="text-on-surface-variant">Max data loss (RPO)</span>
                      <select
                        value={critical.rpo_seconds}
                        disabled={critSaving}
                        onChange={(e) => saveCritical(true, parseInt(e.target.value, 10))}
                        className="ml-auto rounded border border-outline-variant bg-surface-lowest px-2 py-1 focus:outline-none focus:ring-1 focus:ring-primary"
                      >
                        {rpoChoices.map((o) => <option key={o.secs} value={o.secs}>{o.label}</option>)}
                      </select>
                    </div>
                  )}
                  <p className="mt-1.5 text-[11px] italic text-on-surface-variant">
                    Keeps a fresh, verified dump within the window so worst-case loss is the RPO, not a day. Uses the normal verified backup pipeline — no database restart or reconfiguration.
                  </p>
                  {critical?.enabled && (
                    <div className={`mt-1.5 flex items-center gap-1.5 text-[11px] ${critical.target_met ? "text-success" : "text-warning"}`}>
                      {critical.target_met ? <CheckCircle2 size={13} /> : <AlertTriangle size={13} />}
                      Newest verified recovery point: {fmtDuration(critical.measured_rpo_seconds)}{critical.measured_rpo_seconds >= 0 ? " ago" : ""}
                      {!critical.target_met && " — exceeds target"}
                    </div>
                  )}
                  {critical?.enabled && critical.paused && (
                    <div className="mt-1.5 flex items-start gap-1.5 rounded border border-error/30 bg-error/10 px-2 py-1.5 text-[11px] text-error">
                      <AlertTriangle size={13} className="mt-0.5 shrink-0" />
                      <span>Auto-backups <b>paused</b> — the last {critical.verify_failures} backups failed verification. They'll resume automatically once one verifies. Fix the backup, then run one manually to confirm.</span>
                    </div>
                  )}
                  {critical?.pitr_checked && (
                    <div className="mt-1.5 text-[11px] text-on-surface-variant" title="Read-only check of the server's WAL settings. DockBack does not enable continuous WAL archiving itself, as that requires restarting and reconfiguring your database.">
                      WAL: {critical.wal_level}, archive_mode {critical.archive_mode}, {critical.max_wal_senders} sender(s)
                      {critical.pitr_ready ? " — ready for external WAL-PITR tooling" : ""}
                    </div>
                  )}
                </div>
            </>
          )}
            {/* F163: for a container holding credentials to OTHER systems, an
                archive this server can open is protected by a key sitting on a
                machine the archive grants access to. This refuses to write one. */}
            <label className="mb-2 flex cursor-pointer items-start gap-3 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm">
              <input type="checkbox" className="mt-0.5" checked={requireWO} disabled={woSaving} onChange={(e) => changeRequireWriteOnly(e.target.checked)} />
              <span className="min-w-0">
                Never back this up without write-only encryption
                <span className="ml-2 text-xs text-on-surface-variant">refuses the run instead</span>
              </span>
            </label>
            {/* F206: reading a backup out of DockBack already asks for the
                password. Destroying the live data it protects did not. */}
            <label className="mb-2 flex cursor-pointer items-start gap-3 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm">
              <input type="checkbox" className="mt-0.5" checked={restoreStepUp} disabled={stepUpSaving} onChange={(e) => changeRestoreStepUp(e.target.checked)} />
              <span className="min-w-0 flex-1">
                <span className="flex flex-wrap items-center gap-x-2 gap-y-1">
                  Confirm my password before overwriting this container
                  {stepUpExplicit
                    ? <span className="text-xs text-on-surface-variant">set here</span>
                    : <span className="text-xs text-on-surface-variant">{stepUpDefault ? "on by default for this container" : "following the default"}</span>}
                </span>
                <span className="mt-0.5 block text-[11px] italic text-on-surface-variant">
                  An <b>in-place</b> restore wipes this container&rsquo;s data before writing the backup back, and that cannot be undone. With this on, it asks for your password (and two-factor code) first &mdash; the same as downloading a decrypted backup or revealing the encryption key. <b>Restore as a copy</b> is never affected: it leaves the original untouched.
                  {stepUpDefault && !restoreStepUp && <> This container is <b>on by default</b> because its data is marked critical or it requires write-only encryption; turning it off is recorded in the audit trail.</>}
                </span>
              </span>
            </label>
            {requireWO && (
              <p className={`mb-2 -mt-1 break-words rounded px-2 py-1.5 text-xs ${woBlocked ? "bg-warning/10 text-warning" : "text-on-surface-variant italic"}`}>
                {woBlocked
                  ? woBlocked
                  : "Write-only encryption is on, so backups of this container are sealed to your offline key and this server cannot open them. A run started while it is off will be refused rather than writing a readable archive."}
              </p>
            )}
            {saveImage && (
              <p className="mb-2 -mt-1 text-xs italic text-on-surface-variant">Bundles a <span className="font-mono">docker save</span> of the image so a restore needs no registry. Makes this backup substantially larger.</p>
            )}
            {/* F205: a Redis whose password was set at runtime hides it from every
                place the dump looks, so that broker falls back to a file capture on
                EVERY run. This is where the answer gets recorded once. */}
            {dbEngine === "redis" && (
              <div className="mb-2 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm">
                <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                  <span className="font-medium">Redis password</span>
                  {redisAuthSet
                    ? <span className="rounded-full bg-success/15 px-2 py-0.5 text-[11px] font-medium text-success">recorded</span>
                    : <span className="rounded-full bg-surface-highest px-2 py-0.5 text-[11px] font-medium text-on-surface-variant">not set</span>}
                  <span className="text-xs text-on-surface-variant">only needed if it was set at runtime</span>
                </div>
                <p className="mt-1 text-xs text-on-surface-variant">
                  DockBack already finds the password in the container&rsquo;s environment, a mounted secret file, <span className="font-mono">REDIS_ARGS</span>, a <span className="font-mono">redis.conf</span>, or on the server&rsquo;s own command line. One arrangement defeats all of those: a password set while Redis was running, with <span className="font-mono">CONFIG SET requirepass</span>. Record it here and every backup of this container gets a real point-in-time snapshot instead of copying <span className="font-mono">/data</span> as files.
                </p>
                <div className="mt-2 flex flex-wrap items-center gap-2">
                  <input
                    type="password"
                    value={redisPw}
                    onChange={(e) => { setRedisPw(e.target.value); setRedisPwMsg(""); }}
                    autoComplete="new-password"
                    spellCheck={false}
                    placeholder={redisAuthSet ? "Enter a new password to replace the stored one" : "The password this Redis requires"}
                    aria-label="Redis password"
                    className="min-w-[220px] flex-1 rounded border border-outline-variant bg-surface px-3 py-1.5 font-mono text-sm text-on-surface outline-none focus:border-primary"
                  />
                  <Button variant="secondary" disabled={redisPwSaving || !redisPw.trim()} onClick={() => saveRedisAuth(false)}>
                    {redisPwSaving ? <Loader2 size={15} className="animate-spin" /> : <Check size={15} />} Save
                  </Button>
                  {redisAuthSet && (
                    <Button variant="ghost" disabled={redisPwSaving} onClick={() => saveRedisAuth(true)}>Clear</Button>
                  )}
                </div>
                <p className="mt-1.5 text-[11px] text-outline">
                  Stored encrypted with your master key and never shown again &mdash; there is no way to read it back, so replace it rather than look it up. It reaches <span className="font-mono">redis-cli</span> through an environment variable, never a command line, so it is not visible in the container&rsquo;s process list.
                </p>
                {redisPwMsg && <p className="mt-1.5 break-words text-xs text-on-surface-variant">{redisPwMsg}</p>}
              </div>
            )}
            {/* Pilot-light standby rehearsal (F62): prove this container fails over to another node. */}
            {allNodes.filter((n) => n.id !== id).length > 0 && (
              <div className="mb-2 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="flex items-center gap-2 text-on-surface-variant"><ServerCog size={15} className="shrink-0 text-primary" /> Standby rehearsal</span>
                  <span className="text-on-surface-variant">Rehearse on</span>
                  <Select value={standbyNode} onChange={(e) => setStandbyNode(e.target.value)} className="min-w-[8rem]">
                    <option value="">— choose a node —</option>
                    {allNodes.filter((n) => n.id !== id).map((n) => <option key={n.id} value={n.id}>{n.name}</option>)}
                  </Select>
                  <Select value={String(standbyInterval)} onChange={(e) => setStandbyInterval(Number(e.target.value))} aria-label="Rehearsal cadence">
                    <option value="7">every week</option>
                    <option value="30">every month</option>
                  </Select>
                  <Button size="sm" variant="secondary" disabled={standbySaving || !standbyNode} onClick={saveStandby}>
                    {standbySaving && <Loader2 size={13} className="animate-spin" />} Save
                  </Button>
                  {standby && (
                    <>
                      <Button size="sm" variant="secondary" disabled={standbyRunning} onClick={rehearseNow} title="Run a standby rehearsal now">
                        {standbyRunning ? <Loader2 size={13} className="animate-spin" /> : <Play size={13} />} Rehearse now
                      </Button>
                      <Button size="sm" variant="ghost" disabled={standbySaving} onClick={removeStandby}>Remove</Button>
                    </>
                  )}
                </div>
                {standby && standby.last_run > 0 ? (
                  standby.last_ok ? (
                    <p className="mt-1.5 flex items-start gap-1.5 text-[11px] text-success">
                      <CheckCircle2 size={13} className="mt-0.5 shrink-0" />
                      Proven on {allNodes.find((n) => n.id === standby.standby_node)?.name || standby.standby_node} · {fmtAgo(standby.last_run)} · booted in {Math.round(standby.boot_ms / 1000)}s
                    </p>
                  ) : (
                    <p className="mt-1.5 flex items-start gap-1.5 text-[11px] text-error">
                      <AlertTriangle size={13} className="mt-0.5 shrink-0" />
                      Failed on {allNodes.find((n) => n.id === standby.standby_node)?.name || standby.standby_node} · {fmtAgo(standby.last_run)} — {standby.last_detail}
                    </p>
                  )
                ) : (
                  <p className="mt-1.5 text-[11px] italic text-on-surface-variant">{standby ? "Configured — never rehearsed yet." : "Periodically restores the newest verified backup as an isolated clone on another node and proves it boots — so you know it can fail over there. The clone is torn down afterward."}</p>
                )}
              </div>
            )}
            {notifyNudge && (
              <div className="mb-4 flex items-start gap-3 rounded border border-outline-variant bg-surface-lowest/50 px-3 py-2.5 text-sm text-on-surface-variant">
                <Bell size={15} className="mt-0.5 shrink-0" />
                <div className="min-w-0 flex-1">
                  <div>No notification channel is set up — you won't be alerted if a backup fails or fails verification.</div>
                  <button onClick={() => navigate("/settings")} className="mt-1 text-xs font-medium text-primary hover:underline">Set up notifications in Settings →</button>
                </div>
                <button onClick={dismissNotifyNudge} title="Dismiss — don't show this again" className="-mr-1 -mt-1 shrink-0 rounded p-1 text-on-surface-variant hover:bg-surface-high hover:text-on-surface"><X size={14} /></button>
              </div>
            )}

        </Card>
      )}

      {/* ---------------- RESTORE BEHAVIOUR ---------------- */}
      {tab === "restore" && (
        <Card className="p-5">
            {/* Per-container restore health-timeout override (F30): a heavy app whose
                first boot runs a long migration shouldn't be force-rolled-back. */}
            <div className="mb-2 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm">
              <div className="flex flex-wrap items-center gap-2">
                <span className="flex items-center gap-2 text-on-surface-variant"><RotateCcw size={15} className="shrink-0 text-primary" /> Wait for a healthy restore up to</span>
                <input type="number" min={30} max={3600} value={rtInput} placeholder={String(rtSecs)}
                  onChange={(e) => setRtInput(e.target.value)}
                  aria-label="Restore health timeout in seconds for this container"
                  className="w-24 rounded border border-outline-variant bg-surface-lowest px-2 py-1 text-xs text-on-surface outline-none focus:border-docker-blue focus:ring-2 focus:ring-docker-blue/40" />
                <span className="text-on-surface-variant">seconds</span>
                <Button size="sm" variant="secondary" disabled={rtSaving} onClick={saveRestoreTimeout}>
                  {rtSaving && <Loader2 size={13} className="animate-spin" />} Save
                </Button>
                {rtOverride > 0
                  ? <span className="text-[11px] text-primary">Per-container override active ({rtOverride}s). Clear the field and Save to follow the global setting.</span>
                  : <span className="text-[11px] text-on-surface-variant">Following the global setting ({rtSecs}s). Leave blank to keep following it.</span>}
              </div>
              <p className="mt-1 text-[11px] italic text-on-surface-variant">After a restore, DockBack waits this long for the container to come up healthy before rolling back to the pre-restore snapshot. Raise it for a heavy app whose first boot runs a long database migration.</p>
            </div>
            {/* F184: which uid:gid restored data ends up owned by.
                DockBack reads this from the ids an image ANNOUNCES (PUID/PGID,
                USERMAP_UID/USERMAP_GID and the rest). An image running as a
                baked-in user announces nothing, and a NAS numbers its users
                differently from an ordinary Linux host — so the operator needs to
                be able to say it outright, per container. */}
            <div className="mb-2 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm">
              <div className="flex flex-wrap items-center gap-2">
                <span className="flex items-center gap-2 text-on-surface-variant"><Users size={15} className="shrink-0 text-primary" /> Restore data owned by</span>
                <input type="text" inputMode="numeric" value={ownInput} placeholder="1000:1000"
                  onChange={(e) => setOwnInput(e.target.value)}
                  aria-label="User and group id this container's restored data is owned by"
                  className="w-32 rounded border border-outline-variant bg-surface-lowest px-2 py-1 font-mono text-xs text-on-surface outline-none focus:border-docker-blue focus:ring-2 focus:ring-docker-blue/40" />
                <Button size="sm" variant="secondary" disabled={ownSaving} onClick={saveRestoreOwnership}>
                  {ownSaving && <Loader2 size={13} className="animate-spin" />} Save
                </Button>
                {ownPinned
                  ? <span className="min-w-0 break-words text-[11px] text-primary">Pinned to {ownPinned}. Clear the field and Save to go back to what the image declares.</span>
                  : ownDeclared
                    ? <span className="min-w-0 break-words text-[11px] text-on-surface-variant">Following the container&rsquo;s own <span className="font-mono">{ownDeclared}</span>. Leave blank to keep following it.</span>
                    : <span className="min-w-0 break-words text-[11px] text-on-surface-variant">This image declares no user mapping, so nothing is aligned unless you set it here.</span>}
              </div>
              <p className="mt-1 text-[11px] italic text-on-surface-variant">
                A restore writes files with the ids they had when captured. Moving off a NAS usually changes them &mdash; a Synology share owned by
                <span className="font-mono"> 1026:100</span> restored onto a host where the app runs as <span className="font-mono">1000:1000</span> leaves the app unable to write its own data.
                Set the ids this container runs as here and DockBack aligns the restored paths to match. Numeric only, and it applies to a stack restore too.
              </p>
            </div>
        </Card>
      )}

      {/* ---------------- HOOKS & EXPORT ---------------- */}
      {tab === "hooks" && (
        <Card className="p-5">
            {/* F151: the exporter writes a PLAINTEXT copy of everything the app
                holds into its export directory, and leaves it there. DockBack's
                archive is encrypted; that directory is not. Saved immediately,
                like every other per-container toggle. */}
            {exportAvail && (
              <>
                <label className="mb-2 flex cursor-pointer items-start gap-3 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm">
                  <input type="checkbox" className="mt-0.5" checked={cleanupExport} disabled={cleanupSaving} onChange={(e) => changeCleanupExport(e.target.checked)} />
                  <span className="min-w-0">
                    Empty the export directory after each backup
                    <span className="ml-2 text-xs text-on-surface-variant">removes the plaintext copy</span>
                  </span>
                </label>
                <p className="mb-2 -mt-1 break-words text-xs italic text-on-surface-variant">
                  {cleanupExport
                    ? "After a successful backup the export directory is emptied, so the only copy of that data left is inside the encrypted archive. Exporters are usually incremental, so each run re-exports everything — fine for a small archive, slower for a very large one."
                    : "The exporter leaves a readable copy of everything it exported inside the container, and it stays there between backups. Turn this on to remove it once the encrypted archive has been written."}
                </p>
              </>
            )}
            <label className="mb-2 flex cursor-pointer items-center gap-3 rounded border border-outline-variant bg-surface-lowest px-3 py-2.5 text-sm">
              <input type="checkbox" checked={saveImage} onChange={(e) => setSaveImage(e.target.checked)} />
              Also save the container image <span className="ml-auto text-xs text-on-surface-variant">air-gapped · larger</span>
            </label>
            {/* Application-aware backup hooks */}
            <div className="mt-4 border-t border-outline-variant/50 pt-3">
              {autoHooks.length > 0 && (
                <div className="mb-2 flex flex-wrap gap-1.5">
                  {autoHooks.map((h, i) => <span key={i} className="rounded bg-secondary/10 px-2 py-0.5 text-[11px] text-secondary">{h}</span>)}
                </div>
              )}
              <button onClick={() => setHooksOpen((o) => !o)} className="flex items-center gap-1 text-xs font-medium uppercase tracking-wider text-on-surface-variant hover:text-on-surface">
                <Wrench size={13} /> Advanced — backup hooks {hooksOpen ? "▾" : "▸"}
              </button>
              {hooksOpen && (
                <div className="mt-3 space-y-3">
                  <p className="text-xs text-on-surface-variant">Commands run inside the container around the snapshot (one per line). Use for app-consistent backups — e.g. Paperless: <span className="font-mono">document_exporter /usr/src/paperless/export</span></p>
                  <div>
                    <Label>Pre-backup (quiesce)</Label>
                    <textarea value={preHooks} onChange={(e) => setPreHooks(e.target.value)} rows={2}
                      className="w-full rounded border border-outline-variant bg-surface-lowest px-3 py-2 font-mono text-xs outline-none focus:border-docker-blue" placeholder="e.g. document_exporter /usr/src/paperless/export" />
                  </div>
                  <div>
                    <Label>Post-backup (resume)</Label>
                    <textarea value={postHooks} onChange={(e) => setPostHooks(e.target.value)} rows={2}
                      className="w-full rounded border border-outline-variant bg-surface-lowest px-3 py-2 font-mono text-xs outline-none focus:border-docker-blue" placeholder="(optional)" />
                  </div>
                  <div>
                    {/* F140: the restore side. Built-in presets (Nextcloud's
                        maintenance-mode off) always run; these are extra. */}
                    <Label>After a restore (one command per line)</Label>
                    <textarea value={postRestoreHooks} onChange={(e) => setPostRestoreHooks(e.target.value)} rows={2}
                      className="w-full rounded border border-outline-variant bg-surface-lowest px-3 py-2 font-mono text-xs outline-none focus:border-docker-blue" placeholder="(optional)" />
                    <p className="mt-1 text-xs text-on-surface-variant">Runs inside the restored container once its data is back and it has started, before DockBack checks whether it came up healthy. Any built-in steps for this image run first.</p>
                  </div>
                  <div>
                    <Label>Run hooks as user (optional)</Label>
                    <input value={hookUser} onChange={(e) => setHookUser(e.target.value)} placeholder="e.g. www-data"
                      className="w-full rounded border border-outline-variant bg-surface-lowest px-3 py-2 text-sm outline-none focus:border-docker-blue" />
                  </div>
                  <Button variant="secondary" onClick={saveHooks}>Save hooks</Button>

                  <div className="mt-4 border-t border-outline-variant/40 pt-3">
                    <div className="mb-2 text-xs font-medium uppercase tracking-wider text-on-surface-variant">App-native export {exportTool && `(${exportTool})`}</div>
                    <p className="mb-2 text-xs text-on-surface-variant">A portable, version-independent backup using the app's own exporter/importer. Restore re-imports it via the app — survives version upgrades.</p>
                    <div className="space-y-2">
                      {/* F101: fill the fields from a saved preset. It never saves
                          on its own — the commands stay reviewable first. */}
                      {presets.length > 0 && (
                        <div>
                          <Label>Use preset…</Label>
                          <select
                            value=""
                            onChange={(e) => { applyPreset(e.target.value); e.target.value = ""; }}
                            className="w-full rounded border border-outline-variant bg-surface-lowest px-3 py-2 text-sm text-on-surface outline-none focus:border-docker-blue"
                          >
                            <option value="">Use preset…</option>
                            {presets.map((p) => (
                              <option key={p.id} value={p.id}>{p.name}{p.builtin ? " (built in)" : ""}{p.match ? ` — ${p.match}` : ""}</option>
                            ))}
                          </select>
                          <p className="mt-1 text-[11px] text-on-surface-variant">Fills the fields below — review them, then Save. Manage presets in Settings → Backups.</p>
                        </div>
                      )}
                      <div><Label>Export directory</Label><input value={exDir} onChange={(e) => setExDir(e.target.value)} placeholder="/usr/src/paperless/export" className="w-full rounded border border-outline-variant bg-surface-lowest px-3 py-2 font-mono text-xs outline-none focus:border-docker-blue" /></div>
                      <div><Label>Export command</Label><input value={exExport} onChange={(e) => setExExport(e.target.value)} placeholder="document_exporter /usr/src/paperless/export --no-progress-bar" className="w-full rounded border border-outline-variant bg-surface-lowest px-3 py-2 font-mono text-xs outline-none focus:border-docker-blue" /></div>
                      <div><Label>Import command</Label><input value={exImport} onChange={(e) => setExImport(e.target.value)} placeholder="document_importer /usr/src/paperless/export --no-progress-bar" className="w-full rounded border border-outline-variant bg-surface-lowest px-3 py-2 font-mono text-xs outline-none focus:border-docker-blue" /></div>
                      {/* F152: an app-native restore has no dump to tally and no
                          volume checksum — only "the importer exited 0". Where
                          the app ships a checker that can actually fail, this
                          turns the importer's word into the app's own. */}
                      <div>
                        <Label>Verify command after import (optional)</Label>
                        <input value={exVerify} onChange={(e) => setExVerify(e.target.value)} placeholder="the app's own consistency check, if it has one" className="w-full rounded border border-outline-variant bg-surface-lowest px-3 py-2 font-mono text-xs outline-none focus:border-docker-blue" />
                        <p className="mt-1 break-words text-[11px] text-on-surface-variant">
                          Runs after a successful import; a non-zero exit fails the restore and rolls it back. Only use a command that actually exits non-zero when the data is wrong — one that always succeeds looks like proof and is not.
                        </p>
                      </div>
                      <div className="flex flex-wrap gap-2">
                        <Button variant="secondary" onClick={saveExportProfile}>Save export profile</Button>
                        <Button variant="ghost" onClick={copyProfile} title="Copy this profile as JSON to reuse on another container"><Copy size={15} /> Copy profile</Button>
                        <Button variant="ghost" onClick={pasteProfile} title="Fill these fields from a profile JSON on your clipboard"><ClipboardPaste size={15} /> Paste profile</Button>
                      </div>
                      <p className="text-[11px] text-on-surface-variant">Copy a working profile as JSON and paste it onto another container (same app, different node/instance) — then Save. Review the commands before saving; they run inside that container.</p>
                    </div>
                  </div>
                </div>
              )}
            </div>
        </Card>
      )}
    </div>
  );
}




// Per-container "back up at most every" presets (maps to min_interval_hours).
const FREQ_OPTS = [
  { h: 0, label: "Every scheduled run (default)" },
  { h: 24, label: "At most once a day" },
  { h: 168, label: "At most once a week" },
  { h: 720, label: "At most once a month" },
];

// What the run button says: the state of this container's backup, else what it does.
function runButtonLabel(state: { backupInProgress: boolean; queued: boolean; starting: boolean }): string {
  if (state.backupInProgress) return "Backup in progress…";
  if (state.queued) return "Backup queued…";
  if (state.starting) return "Starting backup…";
  return "Start backup";
}

// ContainerPolicyPanel — per-container backup frequency + retention override
// (PLAN §4.2 granular control). Collapsible like "Advanced — backup hooks";
// replaces the old "Schedule in Settings" button. Overrides the global policy for
// THIS container so large apps (Jellyfin, Plex, Bookstack…) can keep fewer/shorter
// copies and back up less often, instead of eating space at the fleet default.
function ContainerPolicyPanel({ id, cid, stack, onNavigateSettings }: { id: string; cid: string; stack?: string; onNavigateSettings: () => void }) {
  const [open, setOpen] = useState(false);
  const [cp, setCp] = useState<ContainerPolicy | null>(null);
  const [ov, setOv] = useState<PolicyOverride | null>(null);
  const [saving, setSaving] = useState(false);
  const [msg, setMsg] = useState("");

  useEffect(() => { api.getContainerPolicy(id, cid).then((p) => { setCp(p); setOv(p.override); }).catch(() => {}); }, [id, cid]);

  const set = (patch: Partial<PolicyOverride>) => { if (ov) setOv({ ...ov, ...patch }); };
  const dirty = !!(cp && ov) && JSON.stringify(ov) !== JSON.stringify(cp.override);

  const toggleFrequency = (on: boolean) =>
    set(on ? { override_frequency: true, min_interval_hours: ov?.min_interval_hours || 168 } : { override_frequency: false });
  const toggleRetention = (on: boolean) => {
    const inh = cp?.inherited;
    set(on
      ? { override_retention: true,
          generations: ov?.generations || inh?.generations || 0,
          keep_daily: ov?.keep_daily || inh?.keep_daily || 0,
          keep_weekly: ov?.keep_weekly || inh?.keep_weekly || 0,
          keep_monthly: ov?.keep_monthly || inh?.keep_monthly || 0,
          keep_yearly: ov?.keep_yearly || inh?.keep_yearly || 0,
          autoprune: ov?.autoprune ?? inh?.autoprune ?? false }
      : { override_retention: false });
  };

  const save = async () => {
    if (!ov) return;
    setSaving(true); setMsg("");
    try { const p = await api.setContainerPolicy(id, cid, ov); setCp(p); setOv(p.override); setMsg("Saved."); }
    catch (e) { setMsg((e as Error).message); }
    finally { setSaving(false); }
  };

  const inh = cp?.inherited;
  const inheritedRetentionText = inh
    ? `keep ${inh.generations || 0}${(inh.keep_daily || inh.keep_weekly || inh.keep_monthly || inh.keep_yearly) ? ` · GFS ${inh.keep_daily || 0}/${inh.keep_weekly || 0}/${inh.keep_monthly || 0}/${inh.keep_yearly || 0}` : ""}${inh.autoprune ? " · auto-prune on" : ""}`
    : "—";

  const gfs = (val: number, key: keyof PolicyOverride, label: string) => (
    <div>
      <Input type="number" min={0} value={val} onChange={(e) => set({ [key]: Math.max(0, parseInt(e.target.value || "0", 10)) } as Partial<PolicyOverride>)} />
      <span className="mt-0.5 block text-center text-[11px] text-on-surface-variant">{label}</span>
    </div>
  );

  return (
    <div>
      <div className="mb-2 flex items-center gap-2 text-sm font-semibold"><CalendarClock size={15} className="text-primary" /> Backed up automatically by</div>
      {cp ? <ScheduleCoverage schedules={cp.schedules || []} stack={stack} /> : <p className="text-xs text-on-surface-variant">Loading…</p>}
      <button onClick={onNavigateSettings} className="mt-2 text-[11px] font-medium text-primary hover:underline">Change schedules in Settings →</button>

      <div className="mt-4 border-t border-outline-variant/50 pt-3">
      <button onClick={() => setOpen((o) => !o)} className="flex items-center gap-1 text-xs font-medium uppercase tracking-wider text-on-surface-variant hover:text-on-surface">
        <CalendarClock size={13} /> Advanced — backup frequency &amp; retention {open ? "▾" : "▸"}
      </button>

      {open && ov && (
        <div className="mt-3 space-y-4">
          <p className="text-xs text-on-surface-variant">
            Override the global policy for <b className="text-on-surface">this container only</b> — ideal for large apps
            (Jellyfin, Plex, Bookstack…) where keeping many copies eats space. Leave a section off to inherit the global settings.
          </p>

          {/* Frequency */}
          <div className="rounded border border-outline-variant bg-surface-lowest p-3">
            <label className="flex cursor-pointer items-center gap-2 text-sm font-medium">
              <input type="checkbox" checked={!!ov.override_frequency} onChange={(e) => toggleFrequency(e.target.checked)} />
              Override how often to back up
            </label>
            {ov.override_frequency ? (
              <div className="mt-3 space-y-1">
                <label className="flex items-center justify-between gap-2 text-sm">
                  <span className="text-on-surface-variant">Back up at most</span>
                  <Select value={String(ov.min_interval_hours ?? 0)} onChange={(e) => set({ min_interval_hours: parseInt(e.target.value, 10) })} className="!w-56">
                    {FREQ_OPTS.map((o) => <option key={o.h} value={o.h}>{o.label}</option>)}
                  </Select>
                </label>
                <p className="text-xs italic text-on-surface-variant">
                  When a schedule fires, this container is skipped if its last successful backup is newer than the interval — so it backs up less often than the rest of the fleet.
                </p>
              </div>
            ) : (
              <p className="mt-2 text-xs text-on-surface-variant">Backs up on every run of the schedules above.</p>
            )}
          </div>

          {/* Retention */}
          <div className="rounded border border-outline-variant bg-surface-lowest p-3">
            <label className="flex cursor-pointer items-center gap-2 text-sm font-medium">
              <input type="checkbox" checked={!!ov.override_retention} onChange={(e) => toggleRetention(e.target.checked)} />
              Override how many copies to keep (and for how long)
            </label>
            {ov.override_retention ? (
              <div className="mt-3 space-y-2">
                <label className="flex items-center justify-between gap-2 text-sm">
                  <span className="text-on-surface-variant">Copies to keep (newest N)</span>
                  <Input type="number" min={0} value={ov.generations} onChange={(e) => set({ generations: Math.max(0, parseInt(e.target.value || "0", 10)) })} className="!w-20 text-right" />
                </label>
                <p className="text-xs text-on-surface-variant">Additionally keep the newest per period (0 = off):</p>
                <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
                  {gfs(ov.keep_daily, "keep_daily", "daily")}
                  {gfs(ov.keep_weekly, "keep_weekly", "weekly")}
                  {gfs(ov.keep_monthly, "keep_monthly", "monthly")}
                  {gfs(ov.keep_yearly, "keep_yearly", "yearly")}
                </div>
                <label className="flex cursor-pointer items-center gap-2 pt-1 text-sm">
                  <input type="checkbox" checked={!!ov.autoprune} onChange={(e) => set({ autoprune: e.target.checked })} />
                  Auto-prune older copies after each backup
                </label>
                <p className="text-xs italic text-on-surface-variant">
                  "Copies to keep" caps the newest N; the daily/weekly/monthly counts additionally keep the newest in each recent day/week/month. The newest backup is always kept.
                </p>
              </div>
            ) : (
              <p className="mt-2 text-xs text-on-surface-variant">Inherited: <span className="text-on-surface">{inheritedRetentionText}</span></p>
            )}
          </div>

          <div className="flex items-center gap-3">
            <Button variant="primary" onClick={save} disabled={saving || !dirty}>
              {saving ? <Loader2 size={15} className="animate-spin" /> : <CheckCircle2 size={15} />} Save container policy
            </Button>
            {msg && <span className="text-sm text-on-surface-variant">{msg}</span>}
          </div>
        </div>
      )}
      </div>
    </div>
  );
}

// How a schedule reaches the container, in words.
function coverageText(schedule: ContainerSchedule, stack?: string): string {
  if (schedule.covers === "container") return "This container, by name";
  if (schedule.covers === "stack") return stack ? `Its stack “${stack}”` : "Its compose stack";
  return schedule.include_stopped ? "Every container on this server" : "Every running container on this server";
}

// ScheduleCoverage lists the schedules that back the container up, or says
// that none does.
function ScheduleCoverage({ schedules, stack }: { schedules: ContainerSchedule[]; stack?: string }) {
  if (schedules.length === 0) {
    return (
      <div className="rounded border border-warning/30 bg-warning/10 px-3 py-2.5 text-sm">
        <div className="font-medium text-warning">No schedule backs this container up</div>
        <p className="mt-0.5 text-xs text-on-surface-variant">It is backed up only when someone runs a backup. Protect it, or add it to a schedule in Settings.</p>
      </div>
    );
  }
  return <ScheduleList schedules={schedules} describe={(schedule) => coverageText(schedule, stack)} />;
}
