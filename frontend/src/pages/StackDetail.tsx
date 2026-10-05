// A compose project's own page (F224) — the thing a stack never had.
//
// Its backup used to be a dialog: stack-wide controls plus a per-service table
// whose rows expand into mount pickers, inside a box that scrolled. The container
// page has solved that exact problem with a column since the beginning, so this
// is the same layout for the same job: the services and their mounts on the left
// where there is width, the run controls on the right, its backups and its live
// console underneath.
//
// Everything here is the dialog's, moved. The per-service edits still write the
// SAME per-container settings the container page writes, so there is exactly one
// source of truth and that page reflects a toggle immediately.
import { Fragment, useCallback, useEffect, useRef, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import {
  ChevronRight, ChevronDown, Database, ExternalLink, HardDrive, Layers,
  Loader2, RotateCcw, CheckCircle2, XCircle, CalendarClock, Plus,
} from "lucide-react";
import { followRun } from "../lib/logStream";
import { stackProgress, type StackProgress } from "../lib/stackRunProgress";
import { api, Destination, MountInfo, Node, StackInfo, StackSchedule, StackScheduleTiming, StackServiceOptions, fmtAgo, fmtBytes } from "../api";
import { Button, Card, Chip } from "../components/ui";
import BackupCloudIcon from "../components/BackupCloudIcon";
import ScheduleList from "../components/ScheduleList";
import ScheduleWhenFields from "../components/ScheduleWhenFields";
import { useStickyScroll } from "../hooks/useStickyScroll";
import { usePoll } from "../hooks/usePoll";
import { useToast } from "../components/Toast";

// F115: how many services' mounts are measured at once. Measuring runs `du` in a
// sidecar per container and can take minutes on a large bind, so a wide stack
// must not spawn one per service simultaneously. The page never waits on any of
// it — rows fill in as they land.
const MOUNT_SCAN_CONCURRENCY = 3;

// Stacks at or below this size open with every service expanded: a two-service
// stack fits on screen, and hiding its mounts behind a disclosure would be
// ceremony. Anything larger stays collapsed.
const AUTO_EXPAND_MAX_SERVICES = 2;

// How many lines the run panel keeps. Every service's backup output lands here,
// so it has to hold a whole stack's run, not one container's.
const MAX_RUN_LINES = 500;

// How often the page looks for a backup of this stack started elsewhere — a
// schedule, the favourites menu, another tab — the same cadence the container
// pages poll their backups at. A stack has a running row per service at most.
const RUNNING_POLL_MS = 6000;
const RUNNING_PAGE_SIZE = 50;

// What the run buttons say: what is happening to the stack, else what they do.
function runButtonLabel(state: { starting: boolean; backupInProgress: boolean; restoreRunning: boolean }, idle: string): string {
  if (state.backupInProgress) return "Backup in progress…";
  if (state.restoreRunning) return "Restore in progress…";
  if (state.starting) return "Starting backup…";
  return idle;
}

interface MountState {
  mounts: MountInfo[] | null;
  selected: Set<string>;
  error?: string;
}

export default function StackDetail() {
  const { id: nodeID = "", project = "" } = useParams();
  const navigate = useNavigate();
  const toast = useToast();
  const [params, setParams] = useSearchParams();

  const [nodes, setNodes] = useState<Node[]>([]);
  const [stack, setStack] = useState<StackInfo | null>(null);
  const [destinations, setDestinations] = useState<Destination[]>([]);
  const [defaultDests, setDefaultDests] = useState<string[]>([]);
  const [groups, setGroups] = useState<{ id: string; at: number; services: string[]; complete: boolean }[]>([]);

  const [sel, setSel] = useState<Set<string>>(new Set());
  const [pause, setPause] = useState("");            // "" = each service's remembered setting
  const [consistent, setConsistent] = useState(false); // F33 app-consistent snapshot
  const [compression, setCompression] = useState(""); // F79: "" = each service's saved setting
  const [rows, setRows] = useState<StackServiceOptions[] | null>(null);
  const [rowsErr, setRowsErr] = useState("");
  const [mstate, setMstate] = useState<Record<string, MountState>>({});
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [starting, setStarting] = useState(false);

  // The live run, streamed under this stack's own log id.
  const [lines, setLines] = useState<{ level: string; msg: string; service?: string }[]>([]);
  const [runState, setRunState] = useState<"" | "running" | "ok" | "fail">("");
  // #N9: how many services of how many are done, read from the run's own
  // `[N/M]` lines — the answer the console could not give while a restore ran.
  const [runProgress, setRunProgress] = useState<StackProgress>(null);
  const [runKind, setRunKind] = useState<"backup" | "restore">("backup");
  // The run panel follows restores as well as backups; only a backup gets the
  // lifting cloud, a restore keeps its spinner.
  const backupRunning = runState === "running" && runKind === "backup";
  const restoreRunning = runState === "running" && runKind === "restore";
  // A backup of this stack this page did not start: it still owns the buttons.
  const [runningElsewhere, setRunningElsewhere] = useState(false);
  const backupInProgress = backupRunning || runningElsewhere;
  const esRef = useRef<(() => void) | null>(null);
  const log = useStickyScroll(lines.length);
  useEffect(() => () => esRef.current?.(), []);

  const nodeName = nodes.find((n) => n.id === nodeID)?.name || nodeID;
  const usable = destinations.filter((d) => d.enabled);

  const loadStack = useCallback(() => {
    api.stackList(nodeID).then((list) => setStack(list.find((s) => s.name === project) || null)).catch(() => setStack(null));
  }, [nodeID, project]);
  const loadGroups = useCallback(() => {
    api.stackGroups(nodeID, project).then(setGroups).catch(() => setGroups([]));
  }, [nodeID, project]);
  // The search matches stack names as substrings, so the rows are kept to this
  // stack exactly.
  const loadRunning = useCallback(() => {
    api.backupsPage({ node_id: nodeID, status: "running", q: project, page_size: RUNNING_PAGE_SIZE })
      .then((page) => setRunningElsewhere(page.items.some((b) => b.stack === project)))
      .catch(() => {});
  }, [nodeID, project]);

  useEffect(() => { api.nodes().then(setNodes).catch(() => setNodes([])); }, []);
  useEffect(() => { loadStack(); loadGroups(); loadRunning(); }, [loadStack, loadGroups, loadRunning]);
  usePoll(loadRunning, RUNNING_POLL_MS, [nodeID, project]);

  // The node's effective destination policy — a node that redirects where its
  // backups go (e.g. a Synology not copying to the same Synology box) is honored
  // here, matching scheduled runs. When overridden, honor it exactly (empty =
  // Local only); else fall back to all enabled.
  useEffect(() => {
    Promise.all([api.destinations(), api.getNodePolicy(nodeID).catch(() => null)]).then(([d, np]) => {
      setDestinations(d);
      const eff = (np?.effective?.destinations || []).filter((x) => d.some((y) => y.id === x));
      const overridden = !!np?.override?.override_destinations;
      const def = overridden ? eff : (eff.length ? eff : d.map((x) => x.id));
      setDefaultDests(def);
      setSel(new Set(def.filter((id) => d.some((y) => y.id === id && y.enabled))));
    }).catch(() => {});
  }, [nodeID]);

  // Per-service rows: the SAME remembered settings the container page shows.
  useEffect(() => {
    setRows(null); setRowsErr(""); setMstate({}); setExpanded(new Set());
    api.stackOptions(nodeID, project).then(setRows).catch((e) => { setRows([]); setRowsErr((e as Error).message); });
  }, [nodeID, project]);

  // F115: measure every service's mounts once the row list arrives — in the
  // background, a few at a time. Nothing here blocks the page or the backup
  // button; each row shows "measuring…" until its own scan returns.
  useEffect(() => {
    if (!rows || rows.length === 0) return;
    let live = true;
    const queue = rows.map((r) => r.container_id);
    if (rows.length <= AUTO_EXPAND_MAX_SERVICES) setExpanded(new Set(queue));

    const worker = async () => {
      for (;;) {
        const cid = queue.shift();
        if (!cid || !live) return;
        try {
          const ms = await api.containerMounts(nodeID, cid);
          if (!live) return;
          setMstate((p) => ({ ...p, [cid]: { mounts: ms, selected: new Set(ms.filter((m) => m.selected).map((m) => m.destination)) } }));
        } catch (e) {
          if (!live) return;
          setMstate((p) => ({ ...p, [cid]: { mounts: [], selected: new Set(), error: (e as Error).message } }));
        }
      }
    };
    for (let i = 0; i < Math.min(MOUNT_SCAN_CONCURRENCY, queue.length); i++) void worker();
    return () => { live = false; };
  }, [rows, nodeID]);

  // Follow a run logged under stack:<project>. Used for a backup started here,
  // and for a restore started on the restore page (?follow=restore), which lands
  // back on this page precisely because this is where the console lives.
  const follow = useCallback((kind: "backup" | "restore") => {
    esRef.current?.();
    setRunKind(kind); setRunState("running"); setLines([]); setRunProgress(null);
    const start = Date.now();
    // Each service's backup logs under its own id, so the stack's id alone
    // carried one line of the whole run. Its services' lines are the run.
    const services = new Map((rows || []).map((r) => [r.container_id, r.name]));
    // #N10: the structured verdict, with the shared stack line rules as the
    // fallback — run.done is not replayed to a late subscriber.
    esRef.current = followRun("stack:" + project, {
      mode: "stack",
      since: start - 2000, // skip replayed history
      alsoShowContainers: [...services.keys()],
      onLine: (l) => {
        setLines((p) => [...p, { level: l.level, msg: l.msg, service: services.get(l.container_id || "") }].slice(-MAX_RUN_LINES));
        // The count the header shows, so a five-service restore is visibly a
        // five-service restore while it runs.
        const p = stackProgress(l.msg);
        if (p) setRunProgress(p);
      },
      onDone: (outcome) => {
        setRunState(outcome === "ok" ? "ok" : "fail");
        loadStack(); loadGroups(); loadRunning();
      },
    });
  }, [project, rows, loadStack, loadGroups, loadRunning]);

  // Arriving back from the restore page: attach to the run it just started.
  useEffect(() => {
    if (params.get("follow") !== "restore") return;
    follow("restore");
    const next = new URLSearchParams(params);
    next.delete("follow");
    setParams(next, { replace: true }); // so a reload does not re-attach to nothing
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const toggleExpand = (cid: string) =>
    setExpanded((prev) => { const n = new Set(prev); n.has(cid) ? n.delete(cid) : n.add(cid); return n; });

  const toggleDest = (id: string, on: boolean) =>
    setSel((prev) => { const n = new Set(prev); on ? n.add(id) : n.delete(id); return n; });

  // Optimistic mount toggle, persisted through the SAME setting the container
  // page writes. On failure the row is re-read from the server rather than left
  // showing a tick that was never stored — a mount the user believes is included
  // but isn't is the one outcome worth being noisy about.
  const toggleMount = useCallback((cid: string, dest: string, on: boolean) => {
    setMstate((prev) => {
      const cur = prev[cid];
      if (!cur) return prev;
      const next = new Set(cur.selected);
      on ? next.add(dest) : next.delete(dest);
      void api.setMountSelection(nodeID, cid, Array.from(next)).catch(() => {
        api.containerMounts(nodeID, cid)
          .then((ms) => setMstate((p) => ({ ...p, [cid]: { mounts: ms, selected: new Set(ms.filter((m) => m.selected).map((m) => m.destination)), error: "Couldn't save that change — showing what's actually stored." } })))
          .catch(() => {});
      });
      setRows((rs) => (rs || []).map((r) => r.container_id === cid ? { ...r, mounts_selected: next.size } : r));
      return { ...prev, [cid]: { ...cur, selected: next, error: undefined } };
    });
  }, [nodeID]);

  // Optimistic per-service edit: flip the row locally, persist via the SAME
  // endpoint the container page uses, re-read on failure.
  const editRow = (cid: string, patch: { save_image?: boolean; incremental?: boolean; incremental_full_every?: number; app_export?: boolean }) => {
    setRows((prev) => (prev || []).map((r) => r.container_id === cid ? { ...r, backup_options: { ...r.backup_options, ...patch } } : r));
    api.setBackupOptions(nodeID, cid, patch).then((saved) => {
      setRows((prev) => (prev || []).map((r) => r.container_id === cid ? { ...r, backup_options: saved } : r));
    }).catch(() => {
      api.stackOptions(nodeID, project).then(setRows).catch(() => {});
    });
  };

  const startBackup = async () => {
    setStarting(true);
    try {
      await api.backupStack(nodeID, project, Array.from(sel), pause, consistent, compression);
      follow("backup");
    } catch (e) { toast.error(`Couldn't start the stack backup: ${(e as Error).message}`); }
    finally { setStarting(false); }
  };

  const dbNames = (rows || []).filter((r) => r.is_database).map((r) => r.name);
  const summary = ["Local", ...usable.filter((d) => sel.has(d.id)).map((d) => d.name)].join(", ");

  // F115: how much data this run will actually read. Only counts mounts whose
  // size was measured, and says so, so a partial scan never reads as a complete
  // figure.
  const totals = (rows || []).reduce(
    (acc, r) => {
      const st = mstate[r.container_id];
      if (!st || st.mounts === null) return { ...acc, pending: acc.pending + 1 };
      for (const m of st.mounts) {
        acc.total++;
        if (!st.selected.has(m.destination)) continue;
        acc.selected++;
        if (m.size_known) acc.bytes += m.size_bytes; else acc.unknown++;
      }
      return acc;
    },
    { bytes: 0, selected: 0, total: 0, unknown: 0, pending: 0 },
  );

  // One service's summary cell: "3 of 5 · 12.4 GB", or an honest placeholder
  // while its scan is still running.
  const mountSummary = (r: StackServiceOptions) => {
    const st = mstate[r.container_id];
    if (!st || st.mounts === null) return <span className="text-on-surface-variant">measuring…</span>;
    if (st.mounts.length === 0) return <span className="text-on-surface-variant">no mounts</span>;
    const chosen = st.mounts.filter((m) => st.selected.has(m.destination));
    const bytes = chosen.reduce((a, m) => a + (m.size_known ? m.size_bytes : 0), 0);
    return (
      <>
        <span className="font-medium text-on-surface tnum">{chosen.length}</span>
        <span className="text-on-surface-variant"> of {st.mounts.length}</span>
        {chosen.length > 0 && <span className="text-on-surface-variant"> · <span className="tnum">{fmtBytes(bytes)}</span></span>}
      </>
    );
  };

  const newest = groups.length > 0 ? groups.reduce((a, g) => (g.at > a.at ? g : a), groups[0]) : null;

  return (
    <div>
      <nav className="mb-6 flex flex-wrap items-center gap-2 text-xs uppercase tracking-wider text-on-surface-variant">
        <Link to="/servers" className="hover:text-primary">Servers</Link>
        <ChevronRight size={14} className="shrink-0" />
        <Link to={`/servers/${nodeID}`} className="hover:text-primary">{nodeName}</Link>
        <ChevronRight size={14} className="shrink-0" />
        <span className="shrink-0">Stacks</span>
        <ChevronRight size={14} className="shrink-0" />
        <span className="min-w-0 break-all font-semibold text-on-surface">{project}</span>
      </nav>

      {/* Header — what this stack is, and the three things you can do to it. */}
      <Card className="mb-5 p-5">
        <div className="flex flex-wrap items-center gap-3">
          <Layers size={20} className="shrink-0 text-primary" />
          <h1 className="min-w-0 break-all text-2xl font-bold">{project}</h1>
          <Chip kind="muted">compose project</Chip>
          <div className="ml-auto flex flex-wrap gap-2">
            <Link to={`/servers/${nodeID}/stacks/${encodeURIComponent(project)}/restore`}
              className="inline-flex items-center gap-1.5 rounded border border-outline-variant bg-surface-high/40 px-3 py-1.5 text-sm font-medium text-on-surface hover:bg-surface-high">
              <RotateCcw size={15} /> Restore stack…
            </Link>
            <Button variant="primary" onClick={startBackup} disabled={starting || runState === "running" || runningElsewhere}>
              <BackupCloudIcon size={15} active={starting || backupInProgress} /> {runButtonLabel({ starting, backupInProgress, restoreRunning }, "Back up stack")}
            </Button>
          </div>
        </div>
        <div className="mt-4 grid grid-cols-2 gap-5 border-t border-outline-variant/50 pt-4 md:grid-cols-4">
          <div>
            <div className="mb-1 text-[10px] font-bold uppercase tracking-widest text-on-surface-variant">Services</div>
            <div className="tnum">{stack ? <>{stack.services} <span className="text-on-surface-variant">&middot; {stack.running} running</span></> : "—"}</div>
          </div>
          <div>
            <div className="mb-1 text-[10px] font-bold uppercase tracking-widest text-on-surface-variant">Protected</div>
            <div>{stack
              ? (stack.backed_up >= stack.services
                ? <Chip kind="ok">{stack.backed_up}/{stack.services} backed up</Chip>
                : <Chip kind="warn">{stack.backed_up}/{stack.services} backed up</Chip>)
              : "—"}</div>
          </div>
          <div>
            <div className="mb-1 text-[10px] font-bold uppercase tracking-widest text-on-surface-variant">Last snapshot</div>
            <div className="tnum">{newest ? fmtAgo(newest.at) : <span className="text-on-surface-variant">none</span>}</div>
          </div>
          <div>
            <div className="mb-1 text-[10px] font-bold uppercase tracking-widest text-on-surface-variant">This run reads</div>
            <div className="tnum">{totals.pending > 0 ? <span className="text-on-surface-variant">measuring…</span> : fmtBytes(totals.bytes)}</div>
          </div>
        </div>
      </Card>

      <div className="grid grid-cols-1 gap-5 lg:grid-cols-3">
        {/* LEFT — services (with their mounts), this stack's snapshots, the run log. */}
        <div className="space-y-5 lg:col-span-2">
          <Card className="overflow-hidden">
            <div className="flex flex-wrap items-center gap-x-2 gap-y-1 border-b border-outline-variant/60 px-5 py-3">
              <h2 className="text-base font-bold">Services</h2>
              <span className="min-w-0 text-xs text-on-surface-variant">
                each row is that container&rsquo;s own saved options &mdash; editing here writes the same settings its page does
              </span>
              {rows === null && <span className="ml-auto flex shrink-0 items-center gap-1 text-xs text-on-surface-variant"><Loader2 size={12} className="animate-spin" /> Loading…</span>}
            </div>
            {rowsErr && <p className="px-5 py-3 text-xs text-error">{rowsErr}</p>}
            {rows !== null && rows.length === 0 && !rowsErr && (
              <p className="px-5 py-6 text-center text-sm text-on-surface-variant">No containers found for this stack.</p>
            )}
            {rows !== null && rows.length > 0 && (
              <div className="overflow-x-auto">
                <table className="w-full text-left text-xs">
                  <thead className="border-b border-outline-variant bg-surface-lowest/60 uppercase tracking-wider text-on-surface-variant">
                    <tr>
                      <th className="px-4 py-2.5 font-semibold">Service</th>
                      <th className="px-4 py-2.5 font-semibold">Compression</th>
                      <th className="px-4 py-2.5 font-semibold">Save image</th>
                      <th className="px-4 py-2.5 font-semibold">Incremental</th>
                      <th className="px-4 py-2.5 font-semibold">App export</th>
                      <th className="px-4 py-2.5 font-semibold">Mounts</th>
                      <th className="px-4 py-2.5" />
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-outline-variant/30">
                    {rows.map((r) => (
                      <Fragment key={r.container_id}>
                        <tr>
                          <td className="px-4 py-2.5">
                            <div className="flex flex-wrap items-center gap-1.5">
                              <span className="min-w-0 break-all font-medium text-on-surface">{r.name}</span>
                              {r.is_database && <span className="shrink-0 rounded-sm bg-secondary/15 px-1.5 py-0.5 text-[10px] font-semibold uppercase text-secondary" title={`Dumped live with native ${r.engine} tools — never paused.`}>{r.engine || "database"}</span>}
                              {(r.covered_binds || 0) > 0 && <span className="shrink-0 rounded-sm bg-success/15 px-1.5 py-0.5 text-[10px] font-semibold uppercase text-success" title="Shared bind(s) captured via another container's backups — captured once per stack run.">covered</span>}
                              {/* #23: two builds of one image sharing a directory. The
                                  sentence comes from the server, so this badge and the
                                  backup's own finding can never describe it differently. */}
                              {r.version_skew && <span className="shrink-0 rounded-sm bg-warning/15 px-1.5 py-0.5 text-[10px] font-semibold uppercase text-warning" title={r.version_skew}>version skew</span>}
                            </div>
                          </td>
                          <td className="px-4 py-2.5 text-on-surface-variant">{compression || r.backup_options.compression}{compression && <span title="Overridden for this run by the stack-wide compression on the right."> *</span>}</td>
                          <td className="px-4 py-2.5">
                            <input type="checkbox" checked={r.backup_options.save_image} onChange={(e) => editRow(r.container_id, { save_image: e.target.checked })} />
                          </td>
                          <td className="px-4 py-2.5">
                            <span className="flex flex-wrap items-center gap-1.5">
                              <input type="checkbox" checked={!!r.backup_options.incremental} onChange={(e) => editRow(r.container_id, { incremental: e.target.checked })} />
                              {r.backup_options.incremental && (
                                <span className="flex items-center gap-1 text-on-surface-variant">
                                  full every
                                  <input type="number" min={2} max={30} value={r.backup_options.incremental_full_every || 7}
                                    onChange={(e) => editRow(r.container_id, { incremental_full_every: Math.min(30, Math.max(2, parseInt(e.target.value || "7", 10))) })}
                                    className="w-12 rounded border border-outline-variant bg-surface-lowest px-1 py-0.5 text-xs" />
                                  backups
                                </span>
                              )}
                            </span>
                          </td>
                          <td className="px-4 py-2.5">
                            <input type="checkbox" checked={r.backup_options.app_export} disabled={!r.export_available}
                              title={r.export_available ? undefined : "No app-native export profile for this image — configure one on the container page."}
                              onChange={(e) => editRow(r.container_id, { app_export: e.target.checked })} />
                          </td>
                          {/* F115: the mounts cell IS the disclosure — the thing
                              you most want to open is the thing you click. */}
                          <td className="px-4 py-2.5">
                            <button
                              type="button"
                              onClick={() => toggleExpand(r.container_id)}
                              aria-expanded={expanded.has(r.container_id)}
                              disabled={mstate[r.container_id]?.mounts?.length === 0}
                              className="flex items-center gap-1 rounded px-1 py-0.5 text-left hover:bg-surface-highest disabled:cursor-default disabled:hover:bg-transparent"
                              title={expanded.has(r.container_id) ? "Hide this service's mounts" : "Choose which mounts to back up"}
                            >
                              <ChevronRight size={12} className={`shrink-0 text-on-surface-variant transition-transform motion-reduce:transition-none ${expanded.has(r.container_id) ? "rotate-90" : ""}`} />
                              <span className="whitespace-nowrap">{mountSummary(r)}</span>
                            </button>
                          </td>
                          <td className="px-4 py-2.5">
                            <Link to={`/servers/${nodeID}/containers/${r.container_id}`} title="Open container settings" className="text-on-surface-variant hover:text-primary">
                              <ExternalLink size={13} />
                            </Link>
                          </td>
                        </tr>
                        {/* F115: the picker itself — the same rows, badges and
                            wording as the container page. */}
                        {expanded.has(r.container_id) && (
                          <tr className="bg-surface-lowest/50">
                            <td colSpan={7} className="px-4 pb-3 pt-1">
                              {mstate[r.container_id]?.error && (
                                <p className="mb-1.5 text-[11px] text-error">{mstate[r.container_id].error}</p>
                              )}
                              {!mstate[r.container_id] || mstate[r.container_id].mounts === null ? (
                                <p className="flex items-center gap-1.5 text-[11px] text-on-surface-variant">
                                  <Loader2 size={11} className="animate-spin motion-reduce:animate-none" /> Measuring sizes…
                                </p>
                              ) : mstate[r.container_id].mounts!.length === 0 ? (
                                <p className="text-[11px] text-on-surface-variant">No named volumes or writable bind mounts — only this container's configuration is captured.</p>
                              ) : (
                                <div className="flex flex-col gap-1">
                                  {mstate[r.container_id].mounts!.map((m) => {
                                    const on = mstate[r.container_id].selected.has(m.destination);
                                    return (
                                      <label key={m.destination} className={`flex cursor-pointer items-start gap-2.5 rounded border border-outline-variant bg-surface-lowest px-2.5 py-1.5 ${on ? "" : "opacity-75"}`}>
                                        <input type="checkbox" className="mt-0.5 shrink-0" checked={on}
                                          onChange={(e) => toggleMount(r.container_id, m.destination, e.target.checked)} />
                                        <span className="min-w-0 flex-1">
                                          <span className="flex flex-wrap items-center gap-1.5">
                                            <span className="break-all font-mono text-[11px] text-on-surface">{m.destination}</span>
                                            <span className="text-[10px] uppercase text-on-surface-variant">{m.type}</span>
                                            {m.unreadable && <span className="shrink-0 rounded-sm bg-warning/15 px-1.5 py-0.5 text-[10px] font-semibold uppercase text-warning" title="The backup reader can't access some files here (likely a UID/GID mismatch). Those files would be missing from the backup.">permission</span>}
                                            {m.covered_by && <span className="shrink-0 rounded-sm bg-success/15 px-1.5 py-0.5 text-[10px] font-semibold uppercase text-success" title={`Captured in ${m.covered_by}'s backups — a stack run captures it once.`}>covered</span>}
                                            {m.snapshotable && <span className="shrink-0 rounded-sm bg-primary/15 px-1.5 py-0.5 text-[10px] font-semibold uppercase text-primary" title={`This volume sits on ${m.fs_type}, which supports atomic filesystem snapshots.`}>{m.fs_type}</span>}
                                            <span className="ml-auto shrink-0 tnum text-[11px] text-on-surface-variant">{m.size_known ? fmtBytes(m.size_bytes) : "size n/a"}</span>
                                          </span>
                                          {m.covered_by
                                            ? <span className="mt-0.5 block text-[11px] text-success">backed up via {m.covered_by} — captured once per stack run</span>
                                            : m.reason && <span className="mt-0.5 block break-words text-[11px] text-warning">{m.reason}</span>}
                                          {m.shared_with && m.shared_with.length > 0 && (
                                            <span className="mt-0.5 block break-words text-[11px] text-on-surface-variant">also mounted in {m.shared_with.join(", ")}</span>
                                          )}
                                        </span>
                                      </label>
                                    );
                                  })}
                                </div>
                              )}
                            </td>
                          </tr>
                        )}
                      </Fragment>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
            {rows !== null && rows.length > 0 && (
              <p className="border-t border-outline-variant/40 px-5 py-3 text-[11px] text-on-surface-variant">
                These are each container&rsquo;s remembered backup settings &mdash; the same ones on its own page. Ticking a mount saves
                immediately and applies to this run, scheduled runs, and manual runs alike.
              </p>
            )}
          </Card>

          {/* This stack's app-consistent snapshots — one point in time across
              every service, which is the thing a stack restore reads. */}
          <Card className="overflow-hidden">
            <div className="flex flex-wrap items-center gap-2 border-b border-outline-variant/60 px-5 py-3">
              <h2 className="text-base font-bold">Stack snapshots</h2>
              <span className="min-w-0 text-xs text-on-surface-variant">app-consistent groups &mdash; one point in time across every service</span>
            </div>
            {groups.length === 0 ? (
              <p className="px-5 py-6 text-center text-sm text-on-surface-variant">
                No app-consistent snapshot yet. Tick <b className="text-on-surface">App-consistent snapshot</b> and back the stack up to create one.
              </p>
            ) : (
              <table className="w-full text-left text-sm">
                <thead className="border-b border-outline-variant bg-surface-lowest/60 text-xs uppercase tracking-wider text-on-surface-variant">
                  <tr>
                    <th className="px-4 py-2.5 font-semibold">Captured</th>
                    <th className="px-4 py-2.5 font-semibold">Services</th>
                    <th className="px-4 py-2.5 font-semibold">Covers the stack</th>
                    <th className="px-4 py-2.5 text-right font-semibold" />
                  </tr>
                </thead>
                <tbody className="divide-y divide-outline-variant/30">
                  {groups.map((g) => (
                    <tr key={g.id}>
                      <td className="px-4 py-2.5">
                        <div className="tnum">{fmtAgo(g.at)}</div>
                        <div className="tnum text-xs text-on-surface-variant">{new Date(g.at * 1000).toLocaleString()}</div>
                      </td>
                      <td className="px-4 py-2.5">
                        <div className="tnum">{g.services.length}</div>
                        <div className="break-words text-xs text-on-surface-variant">{g.services.join(", ")}</div>
                      </td>
                      <td className="px-4 py-2.5">
                        {g.complete ? <Chip kind="ok"><CheckCircle2 size={11} /> complete</Chip> : <Chip kind="warn">partial</Chip>}
                      </td>
                      <td className="px-4 py-2.5 text-right">
                        <Link to={`/servers/${nodeID}/stacks/${encodeURIComponent(project)}/restore`}
                          className="inline-flex shrink-0 items-center gap-1.5 rounded border border-outline-variant px-2.5 py-1 text-xs text-on-surface-variant hover:text-on-surface">
                          <RotateCcw size={12} /> Restore this point
                        </Link>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </Card>

          {/* The live run. */}
          {runState !== "" && (
            <Card className="overflow-hidden">
              <div className="flex flex-wrap items-center gap-2 border-b border-outline-variant/60 bg-surface-low px-4 py-2.5 text-sm">
                {backupRunning && <BackupCloudIcon size={16} active className="shrink-0 text-secondary" />}
                {restoreRunning && <Loader2 size={16} className="shrink-0 animate-spin text-secondary" />}
                {runState === "ok" && <CheckCircle2 size={16} className="shrink-0 text-success" />}
                {runState === "fail" && <XCircle size={16} className="shrink-0 text-error" />}
                <span className={`min-w-0 break-words ${runState === "ok" ? "text-success" : runState === "fail" ? "text-error" : "text-secondary"}`}>
                  {runKind === "backup" ? "Backing up" : "Restoring"} stack &ldquo;{project}&rdquo;
                  {runState === "ok" ? " — done" : runState === "fail" ? " — failed" : "…"}
                </span>
                {/* #N9: the live count. Shown while running so a stack of five
                    reads as a stack of five, and kept on the terminal states so
                    the last thing on screen says how much actually landed. */}
                {runProgress && (
                  <span className="shrink-0 rounded-full border border-outline-variant/60 bg-surface px-2 py-0.5 text-xs tabular-nums text-on-surface-variant">
                    {runProgress.done} of {runProgress.total} service{runProgress.total === 1 ? "" : "s"}
                  </span>
                )}
                <button onClick={() => { esRef.current?.(); setRunState(""); setLines([]); setRunProgress(null); }}
                  className="ml-auto shrink-0 text-xs text-on-surface-variant hover:text-on-surface">dismiss</button>
              </div>
              <div ref={log.ref} onScroll={log.onScroll} className="max-h-64 overflow-y-auto bg-surface-lowest p-4 font-mono text-xs leading-relaxed">
                {lines.length === 0 && <div className="text-on-surface-variant">Waiting for the first line…</div>}
                {lines.map((l, i) => (
                  <div key={i} className={`break-words ${l.level === "ERR" ? "text-error" : l.level === "WARN" ? "text-warning" : "text-on-surface-variant"}`}>
                    {l.service && <span className="mr-2 font-semibold text-primary">{l.service}</span>}{l.msg}
                  </div>
                ))}
              </div>
            </Card>
          )}
        </div>

        {/* RIGHT — the run controls. */}
        <div className="space-y-5">
          <Card className="p-5">
            <h2 className="mb-4 text-base font-bold">Run stack backup</h2>

            <div className="mb-4">
              <div className="mb-1.5 text-xs font-medium uppercase tracking-wider text-on-surface-variant">Destinations</div>
              <div className="mb-1.5 flex items-center gap-3 rounded border border-outline-variant bg-surface-low px-3 py-2">
                <HardDrive size={16} className="shrink-0 text-on-surface-variant" />
                <span className="text-sm font-medium">Local</span>
                <span className="ml-auto shrink-0 text-xs text-on-surface-variant">always kept</span>
              </div>
              {usable.length === 0 ? (
                <p className="rounded border border-outline-variant bg-surface-low px-3 py-2 text-xs text-on-surface-variant">
                  No external destinations yet — add Synology / Nextcloud / S3 in Settings to send an offsite copy.
                </p>
              ) : (
                <div className="space-y-1.5">
                  {usable.map((d) => (
                    <label key={d.id} className="flex cursor-pointer items-center gap-2.5 rounded border border-outline-variant bg-surface-low px-3 py-2 hover:bg-surface-highest">
                      <input type="checkbox" className="shrink-0" checked={sel.has(d.id)} onChange={(e) => toggleDest(d.id, e.target.checked)} />
                      <span className={`h-2 w-2 shrink-0 rounded-full ${d.reachable ? "bg-success" : "bg-error status-pulse"}`} title={d.reachable ? "Reachable" : "Unreachable"} />
                      <span className="min-w-0 break-words text-sm font-medium">{d.name}</span>
                      <span className="shrink-0 text-xs uppercase tracking-wide text-on-surface-variant">{d.type}</span>
                      {d.total_bytes > 0 && <span className="ml-auto shrink-0 tnum text-xs text-on-surface-variant">{fmtBytes(d.free_bytes)} free</span>}
                    </label>
                  ))}
                </div>
              )}
            </div>

            <label className="mb-3 flex cursor-pointer items-start gap-2.5 rounded border border-outline-variant bg-surface-low px-3 py-2 text-sm">
              <input type="checkbox" className="mt-0.5 shrink-0" checked={consistent} onChange={(e) => setConsistent(e.target.checked)} />
              <span className="min-w-0">
                <span className="font-medium">App-consistent snapshot (quiesce during capture)</span>
                <span className="mt-0.5 block break-words text-xs text-on-surface-variant">
                  Pauses the app while its database and volumes are captured together, for a single coherent
                  point-in-time. Adds brief downtime. Off = each service is backed up independently and concurrently.
                </span>
              </span>
            </label>

            <div className="mb-2">
              <div className="mb-1 text-xs font-medium uppercase tracking-wider text-on-surface-variant">Compression (this run)</div>
              <select value={compression} onChange={(e) => setCompression(e.target.value)}
                className="w-full rounded border border-outline-variant bg-surface-low px-2 py-2 text-sm focus:outline-none focus:ring-1 focus:ring-primary">
                <option value="">Each service's saved setting</option>
                <option value="fast">Fast (zstd)</option>
                <option value="balanced">Balanced (zstd)</option>
                <option value="max">Max (zstd)</option>
                <option value="max-long">Max + long-range (zstd)</option>
                <option value="gzip">gzip (universal compatibility)</option>
                <option value="xz">xz (maximum ratio, slow)</option>
              </select>
            </div>

            <div className="mb-3">
              <div className="mb-1 text-xs font-medium uppercase tracking-wider text-on-surface-variant">
                {consistent ? "Quiesce mode for the app tier" : "Consistency during volume backup"}
              </div>
              <select value={pause} onChange={(e) => setPause(e.target.value)}
                className="w-full rounded border border-outline-variant bg-surface-low px-2 py-2 text-sm focus:outline-none focus:ring-1 focus:ring-primary">
                <option value="">Each container's setting</option>
                <option value="none">Live copy (no pause)</option>
                <option value="pause">Pause during copy (default, recommended)</option>
                <option value="stop">Stop during copy (full quiesce)</option>
              </select>
            </div>

            {dbNames.length > 0 && (
              <div className="mb-3 flex items-start gap-2 rounded border border-secondary/30 bg-secondary/10 px-3 py-2 text-xs text-on-surface-variant">
                <Database size={14} className="mt-0.5 shrink-0 text-secondary" />
                <span className="min-w-0 break-words">
                  {dbNames.length === 1 ? <><span className="font-medium text-on-surface">{dbNames[0]}</span> is a database</> : <><span className="font-medium text-on-surface">{dbNames.join(", ")}</span> are databases</>}
                  {" "}— dumped live with native tools (no downtime); never paused or stopped, so they must be running.
                </span>
              </div>
            )}

            {/* F115: how much this run will actually read. Never claims a total it
                hasn't finished measuring. */}
            {rows !== null && rows.length > 0 && (
              <div className="mb-3 flex flex-wrap items-center gap-x-2 gap-y-1 rounded border border-primary/30 bg-primary/10 px-3 py-2 text-xs">
                <span className="text-on-surface-variant">This run will capture</span>
                {totals.pending > 0 ? (
                  <span className="flex items-center gap-1.5 font-medium text-on-surface">
                    <Loader2 size={12} className="animate-spin motion-reduce:animate-none" /> measuring {totals.pending} service{totals.pending === 1 ? "" : "s"}…
                  </span>
                ) : (
                  <>
                    <span className="tnum text-sm font-semibold text-on-surface">{fmtBytes(totals.bytes)}</span>
                    <span className="min-w-0 break-words text-on-surface-variant">
                      {totals.selected} of {totals.total} mount{totals.total === 1 ? "" : "s"} across {rows.length} service{rows.length === 1 ? "" : "s"} · before compression
                    </span>
                    {totals.unknown > 0 && (
                      <span className="min-w-0 break-words text-warning">{totals.unknown} mount{totals.unknown === 1 ? "" : "s"} couldn't be measured, so the real total is larger</span>
                    )}
                  </>
                )}
              </div>
            )}

            <Button variant="primary" className="w-full justify-center" onClick={startBackup} disabled={starting || runState === "running" || runningElsewhere}>
              <BackupCloudIcon size={15} active={starting || backupInProgress} /> {runButtonLabel({ starting, backupInProgress, restoreRunning }, "Start backup")}
            </Button>
            <p className="mt-2 break-words text-[11px] text-on-surface-variant">
              Backing up to: <span className="font-medium text-on-surface">{summary}</span>. You can leave this page &mdash; the run
              continues, and each service&rsquo;s output stays on its own page and under Logs.
            </p>
          </Card>

          <StackSchedulePanel nodeID={nodeID} project={project} services={stack?.services ?? rows?.length ?? 0} />
        </div>
      </div>
    </div>
  );
}

// What the stack's own schedule starts as. App-consistent is decided by the
// number of services: one service has nothing to keep in step with.
const NEW_STACK_SCHEDULE = { enabled: true, kind: "daily", time: "03:00", weekday: 0, monthday: 1, cron: "0 3 * * *" };

// How a schedule reaches the stack, in words.
function stackCoverageText(schedule: StackSchedule): string {
  if (schedule.covers === "stack") return schedule.consistent ? "This stack, app-consistent" : "This stack, each service on its own";
  if (schedule.covers === "container") return `${(schedule.services || []).join(", ")}, by name`;
  return schedule.include_stopped ? "Every container on this server, one by one" : "Every running container on this server, one by one";
}

// StackSchedulePanel is when the stack backs up: its own schedule, set here, and
// every other schedule that reaches it. A stack Protect added to a shared
// schedule stays in it after it gets its own, so it would run on both — each of
// those can be taken out here.
function StackSchedulePanel({ nodeID, project, services }: { nodeID: string; project: string; services: number }) {
  const toast = useToast();
  const [schedules, setSchedules] = useState<StackSchedule[] | null>(null);
  const [draft, setDraft] = useState<StackScheduleTiming | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api.stackSchedules(nodeID, project).then(setSchedules).catch(() => setSchedules([]));
  }, [nodeID, project]);

  const own = schedules?.find((schedule) => schedule.own);
  const others = (schedules || []).filter((schedule) => !schedule.own);
  const sharedNames = others.filter((schedule) => schedule.covers === "stack").map((schedule) => schedule.name);

  const edit = (from?: StackSchedule) => setDraft(from
    ? { enabled: from.enabled, kind: from.kind, time: from.time, weekday: from.weekday, monthday: from.monthday, cron: from.cron, consistent: !!from.consistent }
    : { ...NEW_STACK_SCHEDULE, consistent: services > 1 });

  const apply = async (change: () => Promise<StackSchedule[]>, done: string) => {
    setBusy(true);
    try {
      setSchedules(await change());
      setDraft(null);
      toast.success(done);
    } catch (e) {
      toast.error((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  const save = (timing: StackScheduleTiming) => apply(() => api.setStackSchedule(nodeID, project, timing), "Stack schedule saved.");
  const takeOut = (schedule: StackSchedule) => {
    const question = schedule.own
      ? `Remove the schedule "${schedule.name}"? The stack stops backing up on it.`
      : `Take ${project} out of "${schedule.name}"? Its other targets stay as they are.`;
    if (!confirm(question)) return;
    void apply(() => api.removeStackFromSchedule(nodeID, project, schedule.id), schedule.own ? "Schedule removed." : `Taken out of ${schedule.name}.`);
  };

  return (
    <Card className="p-5">
      <h2 className="mb-3 flex items-center gap-2 text-base font-bold"><CalendarClock size={16} className="shrink-0 text-primary" /> Schedule</h2>
      {schedules === null && <p className="text-xs text-on-surface-variant">Loading…</p>}

      {schedules !== null && draft && (
        <div className="space-y-3 rounded border border-outline-variant bg-surface-lowest p-3">
          <ScheduleWhenFields value={draft} onChange={(patch) => setDraft({ ...draft, ...patch })} />
          <label className="flex cursor-pointer items-start gap-2 text-sm">
            <input type="checkbox" className="mt-0.5 shrink-0" checked={draft.consistent} onChange={(e) => setDraft({ ...draft, consistent: e.target.checked })} />
            <span className="min-w-0">
              App-consistent snapshot
              <span className="block break-words text-xs text-on-surface-variant">Every service in one quiesce window, so they restore to the same moment. Off = each service backed up on its own.</span>
            </span>
          </label>
          <label className="flex cursor-pointer items-center gap-2 text-sm">
            <input type="checkbox" checked={draft.enabled} onChange={(e) => setDraft({ ...draft, enabled: e.target.checked })} /> On
          </label>
          {sharedNames.length > 0 && (
            <p className="break-words text-xs text-warning">It also runs as part of {sharedNames.join(", ")} — take it out below if it should run on this schedule only.</p>
          )}
          <p className="text-xs text-on-surface-variant">Scheduled runs go to this server&rsquo;s default destinations.</p>
          <div className="flex gap-2">
            <Button variant="primary" onClick={() => void save(draft)} disabled={busy}>Save</Button>
            <Button variant="ghost" onClick={() => setDraft(null)} disabled={busy}>Cancel</Button>
          </div>
        </div>
      )}

      {schedules !== null && !draft && own && (
        <ScheduleList schedules={[own]} describe={stackCoverageText} action={() => (
          <span className="flex gap-1.5">
            <Button size="sm" onClick={() => edit(own)} disabled={busy}>Edit</Button>
            <Button size="sm" variant="ghost" onClick={() => takeOut(own)} disabled={busy}>Remove</Button>
          </span>
        )} />
      )}

      {schedules !== null && !draft && !own && (
        <div>
          <Button onClick={() => edit()} disabled={busy}><Plus size={14} /> Give this stack its own schedule</Button>
          <p className="mt-1.5 break-words text-xs text-on-surface-variant">Back it up on its own timing, every service at one moment.</p>
        </div>
      )}

      {schedules !== null && others.length > 0 && (
        <div className="mt-4">
          <div className="mb-1.5 text-[10px] font-bold uppercase tracking-widest text-on-surface-variant">{own ? "Also backed up by" : "Backed up automatically by"}</div>
          <ScheduleList schedules={others} describe={stackCoverageText} action={(schedule) => schedule.covers === "stack" && (
            <Button size="sm" variant="ghost" onClick={() => takeOut(schedule)} disabled={busy}>Take out</Button>
          )} />
        </div>
      )}

      {schedules !== null && !own && others.length === 0 && (
        <div className="mt-3 rounded border border-warning/30 bg-warning/10 px-3 py-2.5 text-sm">
          <div className="font-medium text-warning">No schedule backs this stack up</div>
          <p className="mt-0.5 text-xs text-on-surface-variant">It is backed up only when someone runs a backup.</p>
        </div>
      )}
    </Card>
  );
}
