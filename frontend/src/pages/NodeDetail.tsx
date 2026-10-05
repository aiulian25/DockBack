// Node detail / container list — matches stitch_docker_container_backup_hub
// (the "Production-East" server screen). Keeps the per-node stat bar; every
// value is fetched live and every button is wired. Nothing is hardcoded.
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useParams, useNavigate, Link } from "react-router-dom";
import {
  Database, Terminal, Box, Boxes, Server, ChevronRight, RefreshCw, Cpu,
  CloudUpload, MoreVertical, Wifi, WifiOff, TrendingUp, CheckCircle2, XCircle,
  History, Loader2, Copy, DatabaseBackup, Search,
} from "lucide-react";
import { Layers, RotateCcw, SlidersHorizontal, HardDrive, ShieldAlert, ShieldCheck, KeyRound, FileJson, EyeOff } from "lucide-react";
import ExportDownloadButton from "../components/ExportDownloadButton";
import { followRun } from "../lib/logStream";
import { api, Container, ContainerPage, Backup, StackInfo, Destination, CoverageContainer, IgnoredItem, NodePolicy, PolicyOverride, NodeDetail as NodeDetailT, OrphanVolume, NodeHealthResp, Node as NodeT, fmtBytes, fmtAgo } from "../api";
import { Button, Card, Chip, Modal, Select } from "../components/ui";
import BackupCloudIcon from "../components/BackupCloudIcon";
import { useEventStream } from "../hooks/useEventStream";
import { useStickyScroll } from "../hooks/useStickyScroll";
import { useToast } from "../components/Toast";
import { copyText, hasTextSelection } from "../clipboard";
import UnprotectedBanner from "../components/UnprotectedBanner";
import BackupTargetsModal from "../components/BackupTargetsModal";
import { usePoll } from "../hooks/usePoll";

function ctIcon(image: string) {
  const i = image.toLowerCase();
  if (/postgres|mysql|mariadb|mongo|redis|memcached/.test(i)) return Database;
  if (/nginx|caddy|traefik|haproxy|proxy|envoy/.test(i)) return Terminal;
  if (/node|python|golang|^go|php|ruby|java|rust|deno|bun/.test(i)) return Boxes;
  return Box;
}

function StatusCell({ state }: { state: string }) {
  const map: Record<string, { dot: string; chip: string; label: string; pulse: boolean }> = {
    running: { dot: "bg-secondary", chip: "bg-secondary/10 text-secondary border-secondary/20", label: "Running", pulse: true },
    paused: { dot: "bg-warning", chip: "bg-warning/10 text-warning border-warning/20", label: "Paused", pulse: false },
    restarting: { dot: "bg-warning", chip: "bg-warning/10 text-warning border-warning/20", label: "Restarting", pulse: true },
  };
  const s = map[state] || { dot: "bg-error", chip: "bg-error/10 text-error border-error/20", label: state || "Exited", pulse: false };
  return (
    <div className="flex items-center gap-2">
      <span className={`h-2 w-2 rounded-full ${s.dot} ${s.pulse ? "status-pulse" : ""}`} />
      <span className={`rounded-sm border px-2 py-0.5 text-[11px] font-bold uppercase tracking-tight ${s.chip}`}>{s.label}</span>
    </div>
  );
}

export default function NodeDetail() {
  const { id = "" } = useParams();
  const navigate = useNavigate();
  const toast = useToast();
  const [page, setPage] = useState<ContainerPage | null>(null);
  const [q, setQ] = useState("");          // container search (server-side)
  const [pageNum, setPageNum] = useState(1);
  const PAGE_SIZE = 50;
  const [detail, setDetail] = useState<NodeDetailT | null>(null);
  const [events, setEvents] = useState<Backup[]>([]);
  const [err, setErr] = useState("");
  const [msg, setMsg] = useState("");
  const [busy, setBusy] = useState("");      // container id being backed up
  const [busyAll, setBusyAll] = useState(false);
  const [filter, setFilter] = useState("all");
  const [rowMenu, setRowMenu] = useState("");
  const [onlyUnprotected, setOnlyUnprotected] = useState(false); // "unprotected" filter chip (B2)
  const [unprot, setUnprot] = useState<CoverageContainer[]>([]);  // this node's unprotected running containers
  const [stoppedAtRisk, setStoppedAtRisk] = useState<CoverageContainer[]>([]); // stopped w/ data + no backup (F13)
  const [stale, setStale] = useState<CoverageContainer[]>([]); // running, newest backup too old
  const loadCoverage = useCallback(() => {
    api.coverage().then((cov) => {
      const node = cov.nodes.find((x) => x.node_id === id);
      setUnprot(node?.unprotected || []);
      setStoppedAtRisk(node?.stopped_at_risk || []);
      setStale(node?.stale || []);
    }).catch(() => {});
  }, [id]);

  // Containers and stacks left out of the warnings above and of whole-server
  // backups — ones whose data comes back on its own.
  const [ignored, setIgnored] = useState<IgnoredItem[]>([]);
  const [ignoredOpen, setIgnoredOpen] = useState(false);
  const loadIgnored = useCallback(() => { api.ignored(id).then(setIgnored).catch(() => setIgnored([])); }, [id]);
  useEffect(() => { loadIgnored(); }, [loadIgnored]);
  const isIgnored = (kind: IgnoredItem["kind"], name: string) => ignored.some((item) => item.kind === kind && item.name === name);
  const ignoredWithStack = (c: { name: string; stack?: string }) => isIgnored("container", c.name) || (!!c.stack && isIgnored("stack", c.stack));
  const setIgnore = async (kind: IgnoredItem["kind"], name: string, ignore: boolean) => {
    try {
      setIgnored(await (ignore ? api.ignore(id, kind, name) : api.unignore(id, kind, name)));
      toast.success(ignore ? `${name} is ignored: no more warnings, and whole-server backups leave it out.` : `${name} is no longer ignored.`);
    } catch (e) { toast.error(`Couldn't change ${name}: ${(e as Error).message}`); }
    loadCoverage();
  };
  // One-click protect (B5) from the unprotected banner: smart defaults + first backup.
  const protectContainer = async (cid: string) => {
    try {
      const res = await api.protectContainer(id, cid);
      // F38: added to a schedule that's switched off ⇒ no automatic protection.
      // Warn (not "success") and offer a one-click enable right there.
      if (res.schedule_enabled === false && res.schedule_id) {
        toast.action({
          message: res.summary,
          actionLabel: "Enable the schedule",
          onAction: async () => {
            try {
              await api.enableSchedule(res.schedule_id);
              toast.success("Schedule enabled — this container will now back up automatically.");
              loadCoverage();
            } catch (e) { toast.error(`Couldn't enable the schedule: ${(e as Error).message}`); }
          },
        });
      } else {
        toast.success(res.summary);
      }
    } catch (e) { toast.error(`Couldn't protect: ${(e as Error).message}`); }
    loadCoverage(); loadContainers(); loadEvents();
  };

  // F220: protect a whole compose project — ONE app-consistent schedule target
  // instead of one per service, plus a first backup. Same toast shape as the
  // single-container path, including F38's "the schedule is off" offer.
  const [protectingStack, setProtectingStack] = useState("");
  const protectStack = async (project: string) => {
    setProtectingStack(project);
    try {
      const res = await api.protectStack(id, project);
      if (res.schedule_enabled === false && res.schedule_id) {
        toast.action({
          message: res.summary,
          actionLabel: "Enable the schedule",
          onAction: async () => {
            try {
              await api.enableSchedule(res.schedule_id);
              toast.success("Schedule enabled — this stack will now back up automatically.");
              loadCoverage();
            } catch (e) { toast.error(`Couldn't enable the schedule: ${(e as Error).message}`); }
          },
        });
      } else {
        toast.success(res.summary);
      }
      if (res.backup_started && res.consistent) followStack(project, "backup");
    } catch (e) { toast.error(`Couldn't protect the stack: ${(e as Error).message}`); }
    finally { setProtectingStack(""); }
    loadCoverage(); loadContainers(); loadStacks();
  };

  // F220: everything unprotected on this node, in one go. Grouped so a stack is
  // protected ONCE as a stack rather than service by service — protecting six
  // members individually is exactly the outcome this replaces.
  //
  // Sequential, not concurrent: each call writes to the same schedule row, and
  // firing them in parallel would have them overwrite each other's targets. It
  // also starts a first backup per unit, which the queue should meter rather
  // than receive all at once.
  const [protectingAll, setProtectingAll] = useState(false);
  const protectAll = async () => {
    const standalone = unprot.filter((c) => !c.stack);
    const projects = Array.from(new Set(unprot.filter((c) => c.stack).map((c) => c.stack as string))).sort();
    const units = standalone.length + projects.length;
    if (units === 0) return;
    if (!confirm(
      `Protect everything unprotected on ${nodeName}?\n\n` +
      `${projects.length > 0 ? `${projects.length} stack${projects.length === 1 ? "" : "s"} (each as one app-consistent unit)` : ""}` +
      `${projects.length > 0 && standalone.length > 0 ? " and " : ""}` +
      `${standalone.length > 0 ? `${standalone.length} standalone container${standalone.length === 1 ? "" : "s"}` : ""}` +
      ` will be added to the automatic schedule, and a first backup of each starts now.`
    )) return;

    setProtectingAll(true);
    const failed: string[] = [];
    try {
      for (const project of projects) {
        try { await api.protectStack(id, project); }
        catch (e) { failed.push(`${project}: ${(e as Error).message}`); }
      }
      for (const c of standalone) {
        try { await api.protectContainer(id, c.container_id); }
        catch (e) { failed.push(`${c.name}: ${(e as Error).message}`); }
      }
    } finally { setProtectingAll(false); }

    // Failures are listed, never swallowed: "protected everything" when two of
    // them did not is the one report that must not be given.
    if (failed.length === 0) {
      toast.success(`Protected ${units} ${units === 1 ? "unit" : "units"} on ${nodeName} — first backups started.`);
    } else {
      toast.error(`Protected ${units - failed.length} of ${units}. Failed: ${failed.join("; ")}`);
    }
    loadCoverage(); loadContainers(); loadStacks(); loadEvents();
  };

  // Live network rates + rolling history for the traffic chart.
  const prevNet = useRef<{ rx: number; tx: number; t: number } | null>(null);
  const [rate, setRate] = useState<{ in: number; out: number } | null>(null);
  const [hist, setHist] = useState<number[]>([]);

  // Backup destinations + the default selection (policy destinations, falling
  // back to all enabled) used to seed the per-run target picker.
  const [dests, setDests] = useState<Destination[]>([]);
  const [defaultDests, setDefaultDests] = useState<string[]>([]);
  // The bulk action awaiting a destination choice ("Backup Stack" / "Full
  // Server Backup"). Holds the running-container count for the server case.
  // Full-server bulk backup picker; the stack backup has its own F80 panel.
  const [pending, setPending] = useState<{ kind: "server"; count: number; ignored: number } | null>(null);

  // Stacks.
  const [stacks, setStacks] = useState<StackInfo[]>([]);
  const [stackOp, setStackOp] = useState<{ project: string; kind: "backup" | "restore" } | null>(null);
  const [stackLines, setStackLines] = useState<{ level: string; msg: string }[]>([]);
  const stackLog = useStickyScroll(stackLines.length); // stack restore/revert log follows the tail
  const [stackDone, setStackDone] = useState<"" | "ok" | "fail">("");
  const [allNodes, setAllNodes] = useState<NodeT[]>([]);
  useEffect(() => { api.nodes().then(setAllNodes).catch(() => setAllNodes([])); }, []);
  const stackEs = useRef<(() => void) | null>(null);
  useEffect(() => () => stackEs.current?.(), []);
  // A stack op is only "in progress" while it's running — once it finishes
  // (ok/fail) the result banner stays visible with a dismiss, but the Stack
  // buttons must re-enable immediately (don't wait for the user to dismiss).
  const stackRunning = !!stackOp && stackDone === "";
  // The stack panel follows restores as well as backups; only a backup gets the
  // lifting cloud.
  const stackBackupRunning = stackRunning && stackOp?.kind === "backup";
  const stackRestoreRunning = stackRunning && stackOp?.kind === "restore";

  const loadStacks = () => api.stackList(id).then(setStacks).catch(() => setStacks([]));

  // Connection-health history — reachability transitions + uptime % (F40).
  const [health, setHealth] = useState<NodeHealthResp | null>(null);
  const loadHealth = () => api.nodeHealth(id, 30).then(setHealth).catch(() => setHealth(null));
  // Orphaned named volumes — data with no container (F23).
  const [orphans, setOrphans] = useState<OrphanVolume[]>([]);
  const [orphanBusy, setOrphanBusy] = useState<Record<string, boolean>>({});
  const loadOrphans = () => api.orphanVolumes(id).then((r) => setOrphans(r.volumes || [])).catch(() => setOrphans([]));
  const backupOrphan = async (name: string) => {
    setOrphanBusy((b) => ({ ...b, [name]: true }));
    try {
      await api.backupOrphanVolume(id, name);
      setMsg(`Backup started for volume "${name}". Watch progress in Logs.`);
    } catch (e) {
      setMsg((e as Error).message);
    } finally {
      setOrphanBusy((b) => ({ ...b, [name]: false }));
    }
  };
  // Server-side searched/filtered/paginated container list (PLAN §4.13).
  const loadContainers = useCallback(() => {
    api.containersPage(id, {
      q: q.trim() || undefined,
      state: filter === "all" ? undefined : filter,
      page: pageNum, page_size: PAGE_SIZE,
    }).then(setPage).catch((e) => setErr(e.message));
  }, [id, q, filter, pageNum]);
  // Fetch every running container (ignoring pagination) for node-wide bulk
  // actions, so "back up all running" is never narrowed to the visible page.
  const allRunning = useCallback(async (): Promise<Container[]> => {
    try { return (await api.containersPage(id, { state: "running", page_size: 0 })).containers || []; }
    catch { return []; }
  }, [id]);
  const loadEvents = () => api.backups(id).then((e) => setEvents(e || [])).catch(() => setEvents([]));
  const loadDetail = async () => {
    const d = await api.nodeDetail(id).catch(() => null);
    if (d?.summary) {
      const now = Date.now();
      if (prevNet.current && now > prevNet.current.t) {
        const dt = (now - prevNet.current.t) / 1000;
        const ri = Math.max(0, (d.summary.net_rx - prevNet.current.rx) / dt);
        const ro = Math.max(0, (d.summary.net_tx - prevNet.current.tx) / dt);
        setRate({ in: ri, out: ro });
        setHist((h) => [...h.slice(-15), ri]);
      }
      prevNet.current = { rx: d.summary.net_rx, tx: d.summary.net_tx, t: now };
    }
    setDetail(d);
  };

  // Live deltas over SSE (A7): this node's summary/health and its backups update
  // within a second; the poll below stays as a fallback (slower when connected).
  const { connected } = useEventStream({
    "node.summary": (d: { node_id: string }) => { if (d.node_id === id) { loadDetail(); loadHealth(); } },
    "backup.status": (d: { node_id?: string }) => { if (!d.node_id || d.node_id === id) { loadEvents(); loadContainers(); loadCoverage(); } },
  });

  useEffect(() => {
    loadDetail(); loadEvents(); loadStacks(); loadCoverage(); loadOrphans(); loadHealth();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id, connected]);
  usePoll(() => { loadDetail(); loadEvents(); loadStacks(); loadContainers(); loadCoverage(); loadOrphans(); loadHealth(); }, connected ? 20000 : 8000, [id]);

  // Reload the container page when the search/filter/page changes (debounced so
  // search-as-you-type doesn't fire a request per keystroke). Cheap: served from
  // the cached inventory (PLAN §4.13).
  useEffect(() => {
    const h = setTimeout(loadContainers, 200);
    return () => clearTimeout(h);
  }, [loadContainers]);

  // Destinations + this node's EFFECTIVE policy default for the per-run picker
  // (stack / full-server backup). Uses the node's override when set — so a node
  // that redirects where its backups go (e.g. a Synology node not copying to the
  // same Synology box) is honored here, matching scheduled runs. When overridden,
  // honor it exactly (empty = Local only); else fall back to all enabled.
  useEffect(() => {
    Promise.all([api.destinations(), api.getNodePolicy(id).catch(() => null)]).then(([d, np]) => {
      setDests(d);
      const eff = (np?.effective?.destinations || []).filter((x) => d.some((y) => y.id === x));
      const overridden = !!np?.override?.override_destinations;
      setDefaultDests(overridden ? eff : (eff.length ? eff : d.map((x) => x.id)));
    }).catch(() => {});
  }, [id]);

  // followStack attaches the live-log follower for a stack operation (logged
  // under stack:<project>).
  //
  // F224: the stack's own pages start and follow their own runs. What is left
  // here is the one stack operation this page still STARTS itself — F220's
  // "Protect", which kicks a first app-consistent backup — so the operator sees
  // it happen where they clicked rather than being sent elsewhere.
  const followStack = (project: string, kind: "backup" | "restore") => {
    setStackOp({ project, kind }); setStackLines([]); setStackDone("");
    const start = Date.now();
    // #N9/#N10: the structured verdict and the one shared set of stack line
    // rules, both in followRun. A per-service line can never end the run.
    const stop = followRun("stack:" + project, {
      mode: "stack",
      since: start - 2000,
      onLine: (l) => setStackLines((p) => [...p.slice(-80), { level: l.level, msg: l.msg }]),
      onDone: (outcome) => {
        setStackDone(outcome === "ok" ? "ok" : "fail");
        if (outcome === "ok") { loadContainers(); loadStacks(); }
      },
    });
    stackEs.current = stop;
    return stop;
  };

  // Dismiss the row action menu on any click/touch/Escape outside it. Uses a
  // ref + document listener so it works anywhere on the page, not just within
  // this component's subtree.
  const menuRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!rowMenu) return;
    const onDown = (e: MouseEvent | TouchEvent) => {
      if (menuRef.current && !menuRef.current.contains(e.target as Node)) setRowMenu("");
    };
    const onKey = (e: KeyboardEvent) => { if (e.key === "Escape") setRowMenu(""); };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("touchstart", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("touchstart", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [rowMenu]);

  const host = detail?.host;
  const sum = detail?.summary;
  const nodeName = detail?.node?.name || id;
  const ip = (detail?.node?.address || "").replace(/^.*?@/, "").replace(/^[a-z]+:\/\//, "").replace(/:\d+$/, "").replace(/\/.*/, "");
  const running = page?.counts?.running ?? 0;           // whole-node, from cache
  const unprotIds = useMemo(() => new Set(unprot.map((c) => c.container_id)), [unprot]);
  // F40: build the connection-history bar as reachable/unreachable spans over the
  // window. Each row is a transition (its state is the NEW state), so the span
  // before the first in-window row is the opposite state; a window with no
  // transitions is one solid span of the node's current state.
  const healthSegments = useMemo(() => {
    if (!health) return [] as { pct: number; reachable: boolean; start: number; end: number }[];
    const now = Math.floor(Date.now() / 1000);
    const windowStart = now - (health.days || 30) * 86400;
    const span = Math.max(1, now - windowStart);
    const oldest = [...(health.rows || [])].reverse(); // API is newest-first
    let state = oldest.length ? !oldest[0].reachable : (detail?.reachable ?? true);
    let segStart = windowStart;
    const segs: { pct: number; reachable: boolean; start: number; end: number }[] = [];
    for (const r of oldest) {
      if (r.ts <= segStart) { state = r.reachable; continue; }
      segs.push({ pct: ((r.ts - segStart) / span) * 100, reachable: state, start: segStart, end: r.ts });
      segStart = r.ts; state = r.reachable;
    }
    segs.push({ pct: ((now - segStart) / span) * 100, reachable: state, start: segStart, end: now });
    return segs;
  }, [health, detail?.reachable]);
  // Server-side filtered+paged; the "unprotected" chip further narrows the visible
  // page to unprotected rows (the accurate whole-node count is on the chip, B2).
  const filtered = (page?.containers || []).filter((c) => !onlyUnprotected || unprotIds.has(c.id));
  const total = page?.total ?? 0;                         // filtered total (pre-pagination)
  const pageCount = Math.max(1, Math.ceil(total / PAGE_SIZE));

  const backupOne = async (c: Container) => {
    setRowMenu(""); setBusy(c.id); setMsg("");
    try {
      const res = await api.createBackup(id, c.id, false);
      // A duplicate request is coalesced server-side onto the pending run.
      setMsg(res.status === "already_running" ? `A backup of ${c.name} is already running. Watch progress in Logs.`
        : res.status === "already_queued" ? `A backup of ${c.name} is already queued. Watch progress in Logs.`
          : `Backup started for ${c.name}. Watch progress in Logs.`);
    }
    catch (e) { setMsg((e as Error).message); }
    finally { setBusy(""); loadEvents(); }
  };
  // F221: ONE call, server-side. This used to loop from the browser — a request
  // per container, each hardcoding balanced compression over whatever that
  // container had remembered, no shared-bind dedup, failures swallowed, and the
  // rest of the run abandoned if the tab was closed. The endpoint resolves each
  // container's own options the way a scheduled run does.
  const [includeStopped, setIncludeStopped] = useState(false);
  const backupAll = async (destinations: string[]) => {
    setBusyAll(true); setMsg("");
    try {
      const res = await api.backupNodeAll(id, { includeStopped, destinations });
      if (res.count === 0) {
        setMsg(`Every container on ${nodeName} already has a backup queued or running.`);
      } else {
        setMsg(`Started ${res.count} backup${res.count === 1 ? "" : "s"}${res.skipped > 0 ? ` (${res.skipped} already running)` : ""}. Watch progress in Logs.`);
      }
    } catch (e) { setMsg((e as Error).message); }
    finally { setBusyAll(false); loadEvents(); }
  };

  // Open the destination picker for a bulk action; nothing runs until confirmed.
  const openServerBackup = async () => {
    const running = await allRunning();
    const run = running.filter((c) => !ignoredWithStack(c));
    if (run.length === 0) { setMsg("No running containers to back up."); return; }
    setIncludeStopped(false); // an opt-in, re-asked each time rather than remembered
    setPending({ kind: "server", count: run.length, ignored: running.length - run.length });
  };
  const confirmPending = (selected: string[]) => {
    if (!pending) return;
    backupAll(selected);
    setPending(null);
  };
  const refresh = () => { loadContainers(); loadDetail(); loadEvents(); };

  // Reset the pinned SSH host key so the next connect re-pins whatever the host
  // presents — for a legitimate host re-key (F1).
  // F88: clear the pinned volume-sidecar image so the next backup re-pins what
  // is present. Confirmed, because waving a refusal through without checking the
  // image is exactly what an attacker would want.
  const repinSidecar = async () => {
    if (!confirm(
      "Re-pin the volume sidecar image?\n\n" +
      "DockBack will trust whatever image is present on this server the next time it runs a backup, " +
      "and refuse anything different after that.\n\n" +
      "Only do this if YOU changed the image. If you didn't, the change may be a supply-chain compromise — " +
      "check the image before re-pinning."
    )) return;
    try {
      await api.repinSidecar(id);
      toast.success("Sidecar image will be re-pinned on the next backup");
      refresh();
    } catch (e) { toast.error(`Couldn't re-pin: ${(e as Error).message}`); }
  };

  const resetHostKey = async () => {
    if (!confirm("Reset the pinned SSH host key for this node? The next connection will trust and re-pin whatever key the host presents.")) return;
    try { await api.resetHostKey(id); toast.success("Pinned host key reset — it will re-pin on the next connect"); setTimeout(refresh, 1000); }
    catch (e) { toast.error(`Couldn't reset host key: ${(e as Error).message}`); }
  };

  return (
    <div>
      {/* Per-run destination picker for the full-server bulk backup. */}
      <BackupTargetsModal
        open={!!pending}
        title="Full server backup"
        subtitle={pending?.kind === "server" ? `Backs up all ${pending.count} running container(s) on ${nodeName}, each with its own remembered options${pending.ignored > 0 ? ` — ${pending.ignored} ignored left out` : ""}. Choose where the copies go.` : undefined}
        destinations={dests}
        defaultSelected={defaultDests}
        confirmLabel="Start backup"
        onConfirm={confirmPending}
        onClose={() => setPending(null)}
      >
        {/* F221: the same opt-in the schedule has. A stopped container is
            usually off on purpose — but one that crashed, or an app you run
            occasionally, holds exactly the data you would miss. */}
        <label className="flex cursor-pointer items-start gap-2 text-sm">
          <input type="checkbox" className="mt-0.5 shrink-0" checked={includeStopped} onChange={(e) => setIncludeStopped(e.target.checked)} />
          <span className="min-w-0 break-words text-on-surface-variant">
            <b className="text-on-surface">Include stopped containers</b> — off by default, since a stopped container is usually off on purpose.
          </span>
        </label>
      </BackupTargetsModal>

      <Modal open={ignoredOpen} onClose={() => setIgnoredOpen(false)} title={`Ignored on ${nodeName}`}
        footer={<Button onClick={() => setIgnoredOpen(false)}>Close</Button>}>
        <p className="mb-3 text-xs text-on-surface-variant">
          Left out of the never-backed-up and stale warnings, their alerts and the daily digest, and of whole-server backups
          (Full Server Backup and whole-server schedules). A schedule that names one still backs it up, and so does a backup
          started from its own page.
        </p>
        {ignored.length === 0 ? (
          <p className="py-4 text-center text-sm text-on-surface-variant">Nothing is ignored on this server. Use Ignore on a warning, a stack, or a container&rsquo;s menu.</p>
        ) : (
          <div className="space-y-1.5">
            {ignored.map((item) => (
              <div key={`${item.kind}:${item.name}`} className="flex items-center gap-2.5 rounded-lg bg-surface-container px-3 py-2">
                {item.kind === "stack" ? <Layers size={15} className="shrink-0 text-on-surface-variant" /> : <Box size={15} className="shrink-0 text-on-surface-variant" />}
                <div className="min-w-0 flex-1">
                  <div className="truncate text-sm text-on-surface">{item.name}</div>
                  <div className="truncate text-xs text-on-surface-variant">
                    {item.kind}{item.detail ? ` · ${item.detail}` : ""}{item.present ? "" : " · not on this server now"}
                  </div>
                </div>
                <Button size="sm" onClick={() => void setIgnore(item.kind, item.name, false)}>Stop ignoring</Button>
              </div>
            ))}
          </div>
        )}
      </Modal>


      {/* Breadcrumb */}
      <nav className="mb-6 flex items-center gap-2 text-xs uppercase tracking-wider text-on-surface-variant">
        <Link to="/servers" className="hover:text-primary">Servers</Link>
        <ChevronRight size={14} />
        <span className="font-semibold text-on-surface">{nodeName}</span>
      </nav>

      {/* Header */}
      <div className="mb-2 flex items-end justify-between">
        <div>
          <h1 className="flex items-center gap-2 text-2xl font-bold">
            <Server size={26} className="text-primary" /> {nodeName}
            {detail && (detail.reachable
              ? <Wifi size={18} className="text-success status-pulse" />
              : <WifiOff size={18} className="text-error status-pulse" />)}
          </h1>
          <p className="mt-1 font-mono text-sm text-on-surface-variant">
            {host
              ? `${host.name}${ip ? ` • ${ip}` : ""} • ${host.os} ${host.arch} • Docker ${host.docker_version} • ${host.ncpu} cores • ${fmtBytes(host.mem_total)} RAM • ${running} active`
              : (detail?.error || "Fetching host info…")}
          </p>
          {/* F88: the sidecar runs here with read access to this server's data,
              so which image it is, is worth stating on the page. */}
          {detail?.sidecar && (
            <p className="mt-1.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-on-surface-variant">
              <span className="shrink-0">Volume sidecar image</span>
              <span className="min-w-0 break-all font-mono text-on-surface">
                {detail.sidecar.image}
                {detail.sidecar.digest ? ` @ ${detail.sidecar.digest}` : ""}
              </span>
              {detail.sidecar.pinned ? (
                <>
                  <span className="shrink-0 text-outline">Pinned on first use — a changed image is refused</span>
                  <button onClick={repinSidecar} className="shrink-0 text-primary hover:underline">Re-pin</button>
                </>
              ) : (
                <span className="shrink-0 text-outline">Not pinned yet — pins on the first backup</span>
              )}
            </p>
          )}
        </div>
        <div className="flex gap-3">
          {/* F67: refused pin mismatch — a security state, not a generic outage. */}
          {allNodes.find((n) => n.id === id)?.host_key_changed && (
            <span className="flex items-center" title="This server presented a DIFFERENT SSH host key than the one pinned — possible reinstall or man-in-the-middle. Verify the host, then use Reset pinned host key to re-pin.">
              <Chip kind="err"><ShieldAlert size={13} /> Host key changed — connection refused</Chip>
            </span>
          )}
          {detail?.node?.transport === "ssh" && (
            <Button variant="ghost" onClick={resetHostKey} title="Forget the pinned SSH host key so it re-pins on the next connect (after a legitimate host re-key)"><KeyRound size={16} /> Reset pinned host key</Button>
          )}
          {/* F105: hardware + live host utilisation. The dashboard's chart icon
              lands here too; without this the page would be unreachable from the
              node itself. */}
          <Button onClick={() => navigate(`/servers/${id}/machine`)} title="Hardware and live host utilisation"><Cpu size={16} /> Machine</Button>
          {/* Step 28: before touching anything after a disaster, keep a record of
              how every container here is put together. */}
          <ExportDownloadButton icon={<FileJson size={16} />} label="Freeze evidence"
            title="Download how every container here is put together — inspect records with environment names only, networks and volumes — as one file. Confirms your password first."
            request={async (password, code) => api.evidenceURL(id, (await api.evidenceGrant(id, password, code)).ticket)} />
          <Button onClick={refresh}><RefreshCw size={16} /> Refresh</Button>
          <Button onClick={() => setIgnoredOpen(true)} title="Containers and stacks left out of the warnings and of whole-server backups">
            <EyeOff size={16} /> Ignored{ignored.length > 0 ? ` (${ignored.length})` : ""}
          </Button>
          <Button variant="primary" onClick={openServerBackup} disabled={busyAll}>
            <BackupCloudIcon size={16} active={busyAll} /> Full Server Backup
          </Button>
        </div>
      </div>

      {/* Per-node stat bar */}
      <div className="my-6 grid grid-cols-2 gap-6 rounded-lg border border-outline-variant bg-surface-container p-5 md:grid-cols-4">
        <Stat label="Aggregate CPU" value={sum ? `${sum.cpu_percent.toFixed(1)}%` : "—"} />
        <Stat label="Total RAM Usage" value={sum?.mem_total ? fmtBytes(sum.mem_used) : "—"} suffix={sum?.mem_total ? `/ ${fmtBytes(sum.mem_total)}` : ""} />
        <Stat label="Backup Success Rate" value={detail ? `${detail.backups.backup_success_rate.toFixed(0)}%` : "—"} suffix={detail ? `(${detail.backups.backups_30d} in 30d)` : ""} good />
        <Stat label="Network Inbound" value={rate === null ? "—" : `${fmtBytes(rate.in)}/s`} icon={<TrendingUp size={15} className="mb-1 text-secondary" />} />
      </div>

      {/* Connection history — reachability transitions + uptime % (F40). */}
      {health && (
        <div className="my-6 rounded-lg border border-outline-variant bg-surface-container p-5">
          <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
            <div className="flex items-center gap-2 text-sm font-semibold"><History size={16} className="text-primary" /> Connection history</div>
            <div className="flex items-center gap-4 text-xs text-on-surface-variant">
              <span>Uptime (7d): <b className="text-on-surface">{health.uptime_pct_7d.toFixed(1)}%</b></span>
              <span>Uptime (30d): <b className="text-on-surface">{health.uptime_pct_30d.toFixed(1)}%</b></span>
            </div>
          </div>
          {health.rows.length === 0 && detail == null ? (
            <p className="text-xs text-on-surface-variant">No connection history yet.</p>
          ) : (
            <>
              <div className="flex h-3 w-full overflow-hidden rounded bg-surface-highest" title="Reachability over the last 30 days (green = up, red = down)">
                {healthSegments.map((sg, i) => (
                  <div key={i} className={`h-full ${sg.reachable ? "bg-success" : "bg-error"}`} style={{ width: `${sg.pct}%` }}
                    title={`${sg.reachable ? "Up" : "Down"} — from ${fmtAgo(sg.start)} to ${fmtAgo(sg.end)}`} />
                ))}
              </div>
              <div className="mt-1 flex justify-between text-[10px] text-on-surface-variant">
                <span>{new Date((Math.floor(Date.now() / 1000) - (health.days || 30) * 86400) * 1000).toLocaleDateString()}</span>
                <span>now</span>
              </div>
              {health.rows.length === 0 && (
                <p className="mt-1 text-xs text-on-surface-variant">No reachability changes in this window — the node has been {detail?.reachable ? "reachable" : "unreachable"} throughout.</p>
              )}
            </>
          )}
        </div>
      )}

      {/* Unprotected containers on THIS node (B2, moved here from the dashboard). */}
      <UnprotectedBanner
        nodeName={nodeName}
        unprotected={unprot}
        stale={stale}
        stoppedAtRisk={stoppedAtRisk}
        onProtect={protectContainer}
        onIgnore={(c) => setIgnore("container", c.name, true)}
        onProtectAll={protectAll}
        protectingAll={protectingAll}
        onOpen={(cid) => navigate(`/servers/${id}/containers/${cid}`)}
      />

      {err && <div className="mb-4 rounded bg-error/10 px-4 py-3 text-error">{err}</div>}
      {msg && <div className="mb-4 rounded bg-secondary/10 px-4 py-3 text-secondary">{msg}</div>}

      {/* Compose stacks — one-click backup / restore */}
      {stacks.length > 0 && (
        <Card className="mb-6 overflow-hidden">
          <div className="flex items-center gap-2 border-b border-outline-variant/60 bg-surface-high/40 px-4 py-3">
            <Layers size={18} className="text-primary" />
            <h4 className="text-lg font-semibold">Stacks</h4>
            <span className="text-xs text-on-surface-variant">compose projects — back up or rebuild the whole stack at once</span>
          </div>
          <div className="divide-y divide-outline-variant/30">
            {stacks.map((st) => {
              const stackIgnored = isIgnored("stack", st.name);
              return (
              <div key={st.name} className="flex flex-wrap items-center gap-3 px-4 py-2.5 text-sm">
                <Layers size={15} className="text-on-surface-variant" />
                <span className="font-medium">{st.name}</span>
                <span className="text-xs text-on-surface-variant">{st.services} service{st.services === 1 ? "" : "s"} · {st.running} running</span>
                {st.backed_up > 0
                  ? <Chip kind="ok">{st.backed_up}/{st.services} backed up</Chip>
                  : <Chip kind="muted">not backed up</Chip>}
                {stackIgnored && <Chip kind="muted"><EyeOff size={11} /> ignored</Chip>}
                {/* F224: both open the stack's own pages now. A compose project
                    is a thing with a page, not a dialog you summon from a row. */}
                <div className="ml-auto flex flex-wrap gap-2">
                  {/* F220: offered while any service is uncovered. One click gives
                      the project ONE app-consistent schedule target and a first
                      backup — the alternative is a visit to each service's page,
                      which also loses the consistency this button buys. */}
                  <Button variant="ghost" onClick={() => setIgnore("stack", st.name, !stackIgnored)}
                    title={stackIgnored ? `Warn about ${st.name} again and include it in whole-server backups` : `Stop warning about ${st.name} and leave it out of whole-server backups`}>
                    <EyeOff size={15} /> {stackIgnored ? "Stop ignoring" : "Ignore"}
                  </Button>
                  {st.backed_up < st.services && !stackIgnored && (
                    <Button variant="secondary" onClick={() => protectStack(st.name)} disabled={stackRunning || protectingStack === st.name}
                      title={`Add ${st.name} to the automatic schedule as one app-consistent stack, and back it up now`}>
                      {protectingStack === st.name ? <Loader2 size={15} className="animate-spin" /> : <ShieldCheck size={15} />} Protect
                    </Button>
                  )}
                  <Button onClick={() => navigate(`/servers/${id}/stacks/${encodeURIComponent(st.name)}`)}><CloudUpload size={15} /> Backup Stack</Button>
                  <Button variant="primary" disabled={st.backed_up === 0}
                    onClick={() => navigate(`/servers/${id}/stacks/${encodeURIComponent(st.name)}/restore`)}><RotateCcw size={15} /> Restore Stack</Button>
                </div>
              </div>
              );
            })}
          </div>
          {stackOp && (
            <div className="border-t border-outline-variant/60 bg-surface-lowest p-4">
              <div className="mb-2 flex items-center gap-2 text-sm">
                {stackBackupRunning && <BackupCloudIcon size={16} active className="text-secondary" />}
                {stackRestoreRunning && <Loader2 size={16} className="animate-spin text-secondary" />}
                {stackDone === "ok" && <CheckCircle2 size={16} className="text-success" />}
                {stackDone === "fail" && <XCircle size={16} className="text-error" />}
                <span className={stackDone === "ok" ? "text-success" : stackDone === "fail" ? "text-error" : "text-secondary"}>
                  {stackOp.kind === "backup" ? "Backing up" : "Restoring"} stack “{stackOp.project}”{stackDone === "ok" ? " — done" : stackDone === "fail" ? " — failed" : "…"}
                </span>
                {stackDone !== "" && <button className="ml-auto text-xs text-on-surface-variant hover:text-on-surface" onClick={() => { setStackOp(null); setStackLines([]); }}>dismiss</button>}
              </div>
              {stackLines.length > 0 && (
                <div ref={stackLog.ref} onScroll={stackLog.onScroll} className="max-h-44 overflow-y-auto rounded border border-outline-variant bg-surface px-3 py-2 font-mono text-xs">
                  {stackLines.map((l, i) => <div key={i} className={l.level === "ERR" ? "text-error" : "text-on-surface-variant"}>{l.msg}</div>)}
                </div>
              )}
            </div>
          )}
        </Card>
      )}


      {/* Orphaned named volumes — data with no container (F23). */}
      {orphans.length > 0 && (
        <Card className="mb-6 overflow-hidden">
          <div className="flex items-center gap-2 border-b border-outline-variant/60 bg-surface-high/40 px-4 py-3">
            <HardDrive size={18} className="text-warning" />
            <h4 className="text-lg font-semibold">Orphaned volumes</h4>
            <span className="text-xs text-on-surface-variant">Named volumes with data but no container — back them up before they're forgotten.</span>
          </div>
          <div className="divide-y divide-outline-variant/30">
            {orphans.map((v) => (
              <div key={v.name} className="flex flex-wrap items-center gap-3 px-4 py-2.5 text-sm">
                <HardDrive size={15} className="text-on-surface-variant" />
                <span className="min-w-0 truncate font-mono text-xs" title={v.name}>{v.name}</span>
                <span className="text-xs text-on-surface-variant">{v.bytes > 0 ? fmtBytes(v.bytes) : "size n/a"}{v.driver && v.driver !== "local" ? ` · ${v.driver}` : ""}</span>
                <div className="ml-auto">
                  <Button onClick={() => backupOrphan(v.name)} disabled={!!orphanBusy[v.name]}>
                    <BackupCloudIcon size={15} active={!!orphanBusy[v.name]} /> Back up
                  </Button>
                </div>
              </div>
            ))}
          </div>
        </Card>
      )}

      {/* Containers table */}
      <Card className="overflow-hidden">
        <div className="flex flex-wrap items-center justify-between gap-3 border-b border-outline-variant bg-surface-high/40 px-4 py-3">
          <h4 className="text-lg font-semibold">Containers</h4>
          <div className="flex items-center gap-2">
            <div className="relative">
              <Search size={14} className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-on-surface-variant" />
              <input
                value={q}
                onChange={(e) => { setQ(e.target.value); setPageNum(1); }}
                placeholder="Search name, image, stack…"
                className="w-56 rounded border border-outline-variant bg-surface-lowest py-1 pl-8 pr-3 text-xs text-on-surface focus:outline-none focus:ring-1 focus:ring-primary"
              />
            </div>
            <select
              value={filter} onChange={(e) => { setFilter(e.target.value); setPageNum(1); }}
              className="rounded border border-outline-variant bg-surface-lowest px-3 py-1 text-xs text-on-surface-variant focus:outline-none focus:ring-1 focus:ring-primary"
            >
              <option value="all">All Statuses</option>
              <option value="running">Running</option>
              <option value="stopped">Stopped</option>
            </select>
            <button
              onClick={() => setOnlyUnprotected((v) => !v)}
              disabled={unprot.length === 0}
              title="Show only running containers with no backup and not covered by the schedule"
              className={`flex items-center gap-1.5 rounded border px-2.5 py-1 text-xs font-medium transition-colors ${onlyUnprotected ? "border-warning/50 bg-warning/15 text-warning" : "border-outline-variant text-on-surface-variant hover:text-on-surface"} ${unprot.length === 0 ? "cursor-not-allowed opacity-50" : ""}`}
            >
              <ShieldAlert size={13} /> Unprotected{unprot.length > 0 ? ` (${unprot.length})` : ""}
            </button>
          </div>
        </div>
        <div className="overflow-x-auto">
          <table className="w-full text-left text-sm">
            <thead>
              <tr className="border-b border-outline-variant bg-surface-lowest/50 text-xs uppercase tracking-widest text-on-surface-variant">
                <th className="px-4 py-3 font-semibold">Container Name</th>
                <th className="px-4 py-3 font-semibold">Image</th>
                <th className="px-4 py-3 font-semibold">Status</th>
                <th className="px-4 py-3 font-semibold">Uptime</th>
                <th className="px-4 py-3 text-right font-semibold">Actions</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-outline-variant/30">
              {filtered.map((c) => {
                const Icon = ctIcon(c.image);
                const isErr = c.state !== "running" && c.state !== "paused" && c.state !== "restarting";
                return (
                  <tr
                    key={c.id}
                    // Never navigate on the mouse-up that ends a drag-selection,
                    // so names/images/IDs can be selected and copied by hand.
                    onClick={() => { if (hasTextSelection()) return; navigate(`/servers/${id}/containers/${c.id}`); }}
                    title="Manage backups for this container"
                    className="group cursor-pointer transition-colors hover:bg-surface-highest"
                  >
                    <td className="px-4 py-3">
                      <div className="flex items-center gap-3">
                        <div className={`flex h-8 w-8 items-center justify-center rounded ${isErr ? "bg-error/10 text-error" : "bg-primary/10 text-primary"}`}>
                          <Icon size={18} />
                        </div>
                        <div>
                          <div className="flex flex-wrap items-center gap-1.5">
                            <span className="font-bold text-on-surface">{c.name}</span>
                            {ignoredWithStack(c) && <Chip kind="muted"><EyeOff size={11} /> ignored</Chip>}
                          </div>
                          {/* Copy-safe zone: clicking / double-clicking the ID never navigates. */}
                          <div className="cursor-text select-text font-mono text-[11px] text-on-surface-variant" onClick={(e) => e.stopPropagation()}>ID: {c.id.slice(0, 12)}</div>
                        </div>
                      </div>
                    </td>
                    <td className="px-4 py-3">
                      <span className="inline-flex items-center gap-1.5">
                        {/* Copy-safe zone: the image reference is what gets pasted
                            into compose files — selecting it must never navigate. */}
                        <span className="cursor-text select-text rounded border border-outline-variant bg-surface-highest px-2 py-0.5 font-mono text-xs" onClick={(e) => e.stopPropagation()}>{c.image}</span>
                        <button
                          title="Copy image reference"
                          onClick={async (e) => {
                            e.stopPropagation();
                            (await copyText(c.image)) ? toast.success("Image reference copied") : toast.error("Couldn't access the clipboard");
                          }}
                          className="rounded p-1 text-on-surface-variant opacity-0 transition-opacity hover:bg-surface-high hover:text-on-surface focus-visible:opacity-100 group-hover:opacity-100"
                        >
                          <Copy size={13} />
                        </button>
                      </span>
                    </td>
                    <td className="px-4 py-3"><StatusCell state={c.state} /></td>
                    <td className="px-4 py-3 text-on-surface-variant">{c.status || "—"}</td>
                    <td className="px-4 py-3 text-right">
                      <div className="relative flex items-center justify-end gap-2" onClick={(e) => e.stopPropagation()} ref={rowMenu === c.id ? menuRef : undefined}>
                        <button
                          onClick={() => navigate(`/servers/${id}/containers/${c.id}`)}
                          className="flex items-center gap-2 rounded bg-primary/10 px-3 py-1.5 text-xs font-medium text-primary transition-all hover:bg-primary hover:text-on-primary"
                        >
                          <DatabaseBackup size={15} /> Manage Backups
                        </button>
                        <button onClick={() => setRowMenu(rowMenu === c.id ? "" : c.id)} className="rounded p-1.5 text-on-surface-variant hover:bg-surface-highest hover:text-on-surface">
                          <MoreVertical size={18} />
                        </button>
                        {rowMenu === c.id && (
                          <div className="absolute right-0 top-9 z-20 w-44 overflow-hidden rounded-md border border-outline-variant bg-surface-high shadow-xl">
                            <button onClick={() => backupOne(c)} disabled={busy === c.id} className="flex w-full items-center gap-2 px-3 py-2 text-sm hover:bg-surface-highest disabled:opacity-50"><BackupCloudIcon size={14} active={busy === c.id} /> Back up now</button>
                            <button onClick={() => { setRowMenu(""); navigate(`/servers/${id}/containers/${c.id}`); }} className="flex w-full items-center gap-2 px-3 py-2 text-sm hover:bg-surface-highest"><History size={14} /> Manage backups</button>
                            <button onClick={async () => { const ok = await copyText(c.id); setRowMenu(""); setMsg(ok ? "Container ID copied." : "Couldn't access the clipboard."); }} className="flex w-full items-center gap-2 px-3 py-2 text-sm hover:bg-surface-highest"><Copy size={14} /> Copy ID</button>
                            <button onClick={() => { setRowMenu(""); void setIgnore("container", c.name, !isIgnored("container", c.name)); }} className="flex w-full items-center gap-2 px-3 py-2 text-sm hover:bg-surface-highest"><EyeOff size={14} /> {isIgnored("container", c.name) ? "Stop ignoring" : "Ignore"}</button>
                          </div>
                        )}
                      </div>
                    </td>
                  </tr>
                );
              })}
              {filtered.length === 0 && <tr><td colSpan={5} className="px-6 py-10 text-center text-on-surface-variant">{q || filter !== "all" ? "No containers match your search/filter." : "No containers."}</td></tr>}
            </tbody>
          </table>
        </div>
        <div className="flex flex-wrap items-center justify-between gap-3 border-t border-outline-variant px-4 py-3 text-xs text-on-surface-variant">
          <span>
            {total === 0 ? "No containers" : (
              <>Showing {(pageNum - 1) * PAGE_SIZE + 1}–{Math.min(pageNum * PAGE_SIZE, total)} of {total}
                {(q || filter !== "all") ? " matching" : ""} container{total === 1 ? "" : "s"}</>
            )}
          </span>
          {pageCount > 1 && (
            <div className="flex items-center gap-2">
              <button
                onClick={() => setPageNum((p) => Math.max(1, p - 1))}
                disabled={pageNum <= 1}
                className="rounded border border-outline-variant px-2.5 py-1 hover:bg-surface-highest disabled:opacity-40"
              >Prev</button>
              <span>Page {pageNum} / {pageCount}</span>
              <button
                onClick={() => setPageNum((p) => Math.min(pageCount, p + 1))}
                disabled={pageNum >= pageCount}
                className="rounded border border-outline-variant px-2.5 py-1 hover:bg-surface-highest disabled:opacity-40"
              >Next</button>
            </div>
          )}
        </div>
      </Card>

      {/* Per-node backup policy (PLAN §4.13) */}
      <NodePolicyCard id={id} dests={dests} />

      {/* Secondary panels */}
      <div className="mt-8 grid grid-cols-1 gap-4 md:grid-cols-3">
        <Card className="p-5 md:col-span-2">
          <div className="mb-6 flex items-center justify-between">
            <h5 className="text-lg font-semibold">Recent Backup Events</h5>
            <Link to="/logs" className="text-sm text-primary hover:underline">View detailed logs</Link>
          </div>
          <div className="space-y-3">
            {events.slice(0, 5).map((b) => {
              const ok = b.status === "success";
              const failed = b.status === "failed";
              return (
                <div key={b.id} className="flex items-center gap-4 rounded-lg border border-outline-variant bg-surface-low p-3">
                  <div className={`flex h-10 w-10 items-center justify-center rounded-full ${failed ? "bg-error/10 text-error" : ok ? "bg-secondary/10 text-secondary" : "bg-primary/10 text-primary"}`}>
                    {failed ? <XCircle size={18} /> : ok ? <CheckCircle2 size={18} /> : <History size={18} />}
                  </div>
                  <div className="flex-1">
                    <div className="font-bold text-on-surface">{b.target_name} backup {ok ? "completed" : b.status}</div>
                    <div className="text-xs text-on-surface-variant">{b.target_name} • {b.size_bytes ? fmtBytes(b.size_bytes) : "—"} • {fmtAgo(b.created_at)}</div>
                  </div>
                  <div className="font-mono text-[11px] text-on-surface-variant">{ok ? (b.verified === "verified" ? "Verified" : "Success") : b.status}</div>
                </div>
              );
            })}
            {events.length === 0 && <div className="rounded-lg border border-outline-variant bg-surface-low p-4 text-sm text-on-surface-variant">No backup events for this node yet.</div>}
          </div>
        </Card>

        <Card className="p-5">
          <h5 className="mb-1 text-lg font-semibold">Network Traffic</h5>
          <p className="mb-4 text-sm text-on-surface-variant">Real-time throughput for {nodeName}</p>
          <div className="flex h-32 items-end gap-1">
            {hist.length === 0 && <div className="w-full self-center text-center text-xs text-on-surface-variant">Sampling…</div>}
            {hist.map((v, i) => {
              const max = Math.max(...hist, 1);
              const h = Math.max(4, (v / max) * 100);
              return <div key={i} className={`w-full rounded-t-sm ${i === hist.length - 1 ? "bg-primary" : "bg-primary/40"}`} style={{ height: `${h}%` }} />;
            })}
          </div>
          <div className="mt-4 flex justify-between text-xs text-on-surface-variant">
            <span>Incoming: {rate ? `${fmtBytes(rate.in)}/s` : "—"}</span>
            <span>Outgoing: {rate ? `${fmtBytes(rate.out)}/s` : "—"}</span>
          </div>
        </Card>
      </div>
    </div>
  );
}

function Stat({ label, value, suffix, good, icon }: { label: string; value: string; suffix?: string; good?: boolean; icon?: React.ReactNode }) {
  return (
    <div>
      <p className="mb-2 text-xs font-medium uppercase tracking-widest text-on-surface-variant">{label}</p>
      <div className="flex items-end gap-2">
        <span className="tnum text-xl font-bold">{value}</span>
        {suffix && <span className={`mb-1.5 text-sm ${good ? "text-success" : "text-on-surface-variant"}`}>{suffix}</span>}
        {icon}
      </div>
    </div>
  );
}

// NodePolicyCard shows this node's effective backup policy and lets the operator
// override destinations and/or retention, inheriting the global policy otherwise
// (PLAN §4.13 per-node policy inheritance).
function NodePolicyCard({ id, dests }: { id: string; dests: Destination[] }) {
  const [np, setNp] = useState<NodePolicy | null>(null);
  const [ov, setOv] = useState<PolicyOverride | null>(null);
  const [saving, setSaving] = useState(false);
  const [msg, setMsg] = useState("");

  // Normalize the API response so `destinations` is never null — the backend
  // returns a nil slice as JSON null when no override exists, which would crash
  // .length/.filter/.includes on the checkbox handlers (kept the card unusable).
  const normNP = (p: NodePolicy): NodePolicy => ({
    ...p,
    override: { ...p.override, destinations: p.override?.destinations ?? [] },
    global: { ...p.global, destinations: p.global?.destinations ?? [] },
  });
  useEffect(() => { api.getNodePolicy(id).then((p) => { const n = normNP(p); setNp(n); setOv(n.override); }).catch(() => {}); }, [id]);
  if (!np || !ov) return null;
  const g = np.global;
  const set = (patch: Partial<PolicyOverride>) => setOv({ ...ov, ...patch });
  const dirty = JSON.stringify(ov) !== JSON.stringify(np.override);

  const destName = (idv: string) => dests.find((d) => d.id === idv)?.name || idv;
  const inheritedDests = g.destinations.length ? g.destinations.map(destName).join(", ") : "Local only";

  const toggleDests = (on: boolean) =>
    set(on ? { override_destinations: true, destinations: ov.destinations.length ? ov.destinations : g.destinations } : { override_destinations: false });
  const toggleRetention = (on: boolean) =>
    set(on
      ? { override_retention: true, generations: g.generations || 0, keep_daily: g.keep_daily || 0, keep_weekly: g.keep_weekly || 0, keep_monthly: g.keep_monthly || 0, keep_yearly: g.keep_yearly || 0, autoprune: g.autoprune }
      : { override_retention: false });
  // Toggling any destination checkbox works directly — the first change flips
  // this node into override mode, seeded from the inherited (global) selection so
  // the click is a delta from what the operator sees. No separate "enable
  // override first" step. Unticking "Override destinations" reverts to inherit.
  const toggleDest = (destId: string, checked: boolean) => {
    const base = ov.override_destinations ? ov.destinations : g.destinations;
    const next = checked ? [...base, destId] : base.filter((x) => x !== destId);
    set({ override_destinations: true, destinations: next });
  };

  const save = async () => {
    setSaving(true); setMsg("");
    try { const n = normNP(await api.setNodePolicy(id, ov)); setNp(n); setOv(n.override); setMsg("Saved."); }
    catch (e) { setMsg((e as Error).message); }
    finally { setSaving(false); }
  };

  const numField = (label: string, val: number, key: keyof PolicyOverride) => (
    <label className="flex items-center justify-between gap-2 text-sm">
      <span className="text-on-surface-variant">{label}</span>
      <input
        type="number" min={0} value={val}
        onChange={(e) => set({ [key]: Math.max(0, parseInt(e.target.value || "0", 10)) } as Partial<PolicyOverride>)}
        className="w-20 rounded border border-outline-variant bg-surface-lowest px-2 py-1 text-right text-on-surface outline-none focus:border-docker-blue"
      />
    </label>
  );

  return (
    <Card className="mt-8 p-5">
      <div className="mb-1 flex items-center gap-2 text-lg font-semibold"><SlidersHorizontal size={18} className="text-primary" /> Backup Policy — this node</div>
      <p className="mb-4 text-sm text-on-surface-variant">Inherits the global policy (Settings) unless you override it here. Configure the fleet once; set only the exceptions per node.</p>

      <div className="grid grid-cols-1 gap-5 md:grid-cols-2">
        {/* Destinations */}
        <div className="rounded border border-outline-variant bg-surface-lowest p-4">
          <label className="flex cursor-pointer items-center gap-2 text-sm font-medium">
            <input type="checkbox" checked={ov.override_destinations} onChange={(e) => toggleDests(e.target.checked)} />
            Override destinations
          </label>
          {/* The destination checkboxes are always clickable — ticking one flips
              this node into override mode. While inheriting they reflect the global
              policy so the operator sees the starting point. */}
          <div className="mt-3 space-y-2">
            <div className="flex items-center gap-2 rounded border border-outline-variant bg-surface px-3 py-1.5 text-sm text-on-surface-variant">
              <HardDrive size={14} /> Local <span className="ml-auto text-xs">always</span>
            </div>
            {dests.map((d) => {
              const checked = ov.override_destinations ? ov.destinations.includes(d.id) : g.destinations.includes(d.id);
              return (
                <label key={d.id} className="flex cursor-pointer items-center gap-2 rounded border border-outline-variant bg-surface px-3 py-1.5 text-sm hover:bg-surface-highest">
                  <input type="checkbox" checked={checked} onChange={(e) => toggleDest(d.id, e.target.checked)} />
                  <CloudUpload size={14} className="text-secondary" /> {d.name}
                  <span className="ml-auto text-xs uppercase text-on-surface-variant">{d.type}</span>
                </label>
              );
            })}
            {dests.length === 0 && <p className="text-xs text-on-surface-variant">No external destinations configured — add one in Settings.</p>}
            <p className="text-xs italic text-on-surface-variant">
              {ov.override_destinations
                ? "This node overrides the global destinations. Untick “Override destinations” to go back to inheriting."
                : <>Inheriting global: <span className="not-italic text-on-surface">{inheritedDests}</span>. Tick a destination to set this node’s own.</>}
            </p>
          </div>
        </div>

        {/* Retention */}
        <div className="rounded border border-outline-variant bg-surface-lowest p-4">
          <label className="flex cursor-pointer items-center gap-2 text-sm font-medium">
            <input type="checkbox" checked={ov.override_retention} onChange={(e) => toggleRetention(e.target.checked)} />
            Override retention
          </label>
          {ov.override_retention ? (
            <div className="mt-3 space-y-2">
              {numField("Generations (keep newest N)", ov.generations, "generations")}
              {numField("Daily (GFS)", ov.keep_daily, "keep_daily")}
              {numField("Weekly (GFS)", ov.keep_weekly, "keep_weekly")}
              {numField("Monthly (GFS)", ov.keep_monthly, "keep_monthly")}
              {numField("Yearly (GFS)", ov.keep_yearly, "keep_yearly")}
              <label className="flex cursor-pointer items-center gap-2 pt-1 text-sm">
                <input type="checkbox" checked={ov.autoprune} onChange={(e) => set({ autoprune: e.target.checked })} />
                Auto-prune after each backup
              </label>
            </div>
          ) : (
            <p className="mt-3 text-sm text-on-surface-variant">
              Inherited: <span className="text-on-surface">keep {g.generations || 0}</span>
              {(g.keep_daily || g.keep_weekly || g.keep_monthly || g.keep_yearly) ? <span className="text-on-surface"> · GFS {g.keep_daily || 0}/{g.keep_weekly || 0}/{g.keep_monthly || 0}/{g.keep_yearly || 0}</span> : null}
              {g.autoprune ? " · auto-prune on" : ""}
            </p>
          )}
        </div>
      </div>

      <div className="mt-4 flex items-center gap-3">
        <Button variant="primary" onClick={save} disabled={saving || !dirty}>
          {saving ? <Loader2 size={15} className="animate-spin" /> : <CheckCircle2 size={15} />} Save node policy
        </Button>
        {msg && <span className="text-sm text-on-surface-variant">{msg}</span>}
      </div>
    </Card>
  );
}
