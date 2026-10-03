// Backups — grouped by node as cards (like the dashboard); click a node card to
// see all its backups. Detail drawer shows the verification report, manifest,
// and destructive restore / download / delete actions.
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { locations, rowSummary, failedCopies, parseBlob as safe } from "../lib/backupRows";
import { ipFromNodeAddr } from "../lib/nodeAddress";
import {
  ShieldCheck, ShieldAlert, ShieldX, Download, Trash2, RotateCcw, X,
  Wifi, WifiOff, ChevronLeft, ChevronRight, Link2, Network, Loader2,
  CheckCircle2, AlertTriangle, Layers, KeyRound, Search, Lock, Play, Clock, Pin, Tag, ChevronDown, FolderOpen, FileText, Info, FlaskConical, Timer,
} from "lucide-react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { baseDirProblem } from "../lib/absoluteBase";
import { followRun, subscribeLines } from "../lib/logStream";
import { api, StepUpError, AppPreconditions, ArchiveEntry, Backup, BackupPage, BackupDiffResp, CertRef, DrillStatus, Destination, FileSearchResp, Finding, Node, RestoreCompatError, RestoreReadiness, RestoreVerifyFailedError, RunningRestore, TestClone, fmtAgo, fmtBytes } from "../api";
import { isRestoreProgressLine } from "../lib/restoreLogFilter";
import { Button, Card, Chip, Label, Select } from "../components/ui";
import BackupCloudIcon from "../components/BackupCloudIcon";
import { useToast } from "../components/Toast";
import { useEventStream } from "../hooks/useEventStream";
import { useStickyScroll } from "../hooks/useStickyScroll";
import { NodeAvatar } from "../components/NodeAvatar";
import RestoreTimeline from "../components/RestoreTimeline";
import { RunLogPanel } from "../components/RunLog";
import { writeStackRestoreSeed } from "../lib/stackRestoreSeed";
import { usePoll } from "../hooks/usePoll";
import ExportDownloadButton from "../components/ExportDownloadButton";
import RestoreFileButton from "../components/RestoreFileButton";
import StepUpPrompt from "../components/StepUpPrompt";


function verifyChip(v: string) {
  if (v === "verified") return <Chip kind="ok"><ShieldCheck size={12} /> Verified</Chip>;
  if (v === "failed") return <Chip kind="err"><ShieldX size={12} /> Failed</Chip>;
  return <Chip kind="warn"><ShieldAlert size={12} /> Unverified</Chip>;
}
function statusChip(s: string) {
  if (s === "success") return <Chip kind="ok">Success</Chip>;
  if (s === "running") return <Chip kind="info"><BackupCloudIcon size={12} active /> Running</Chip>;
  if (s === "failed") return <Chip kind="err">Failed</Chip>;
  return <Chip kind="muted">{s}</Chip>;
}
// F50: a single A–F restore-confidence grade. A/B green, C amber, D/F red.
function gradeTone(g: string) {
  return g === "A" || g === "B" ? "bg-success/15 text-success"
    : g === "C" ? "bg-warning/15 text-warning"
    : "bg-error/15 text-error";
}
function gradeBadge(c?: { grade: string; reasons: string[] }) {
  if (!c) return null;
  const title = c.grade === "A"
    ? "Restore confidence A — verified, drill-proven, an offsite copy, and complete."
    : `Restore confidence ${c.grade}. What's missing: ${(c.reasons || []).join("; ")}`;
  return (
    <span title={title} aria-label={`Restore confidence ${c.grade}`}
      className={`inline-grid h-5 min-w-[1.25rem] place-items-center rounded px-1 text-xs font-bold ${gradeTone(c.grade)}`}>
      {c.grade}
    </span>
  );
}

interface Group {
  nodeId: string; name: string; node?: Node;
  count: number; total: number; verified: number; failed: number; running: number; last: number;
}

export default function Backups() {
  const toast = useToast();
  const [list, setList] = useState<Backup[]>([]);
  const [nodes, setNodes] = useState<Node[]>([]);
  // Ids with a pending soft-delete (undo window). Filtered out at render so the
  // 5s poll can't resurrect a row the user just deleted (A3).
  const [pendingDel, setPendingDel] = useState<Set<string>>(new Set());
  const [selected, setSelected] = useState<string | null>(null);
  const [sel, setSel] = useState<Backup | null>(null);

  // Selected-node drill-down: server-side searched/filtered/paginated so the
  // history view scales to thousands of rows (PLAN §4.13).
  const [q, setQ] = useState("");
  const [statusF, setStatusF] = useState("");
  const [verifiedF, setVerifiedF] = useState("");
  const [bpage, setBpage] = useState(1);
  const [pageData, setPageData] = useState<BackupPage | null>(null);
  const PAGE_SIZE = 25;

  // Per-backup restore-drill results (PLAN §9.4), keyed by backup id, refreshed
  // live so an async drill's outcome appears on its row when it finishes.
  const [drills, setDrills] = useState<Record<string, DrillStatus>>({});
  const running = useRef<Record<string, number>>({}); // backup id -> "Run" click time (ms)
  const [, forceRerender] = useState(0);
  const bump = () => forceRerender((n) => n + 1);

  // Multi-select for bulk delete (scoped to the visible page — server-side
  // pagination). Cleared whenever the visible set changes so a stale id from a
  // different page/filter can never be deleted by accident.
  const [checked, setChecked] = useState<Set<string>>(new Set());

  // F70: "Find a file" — cross-backup search over the stored file indexes of
  // this node's backups. Reset when the node drill-down changes.
  const [fsOpen, setFsOpen] = useState(false);
  const [fsQ, setFsQ] = useState("");
  const [fsBusy, setFsBusy] = useState(false);
  const [fsRes, setFsRes] = useState<FileSearchResp | null>(null);
  const [fsErr, setFsErr] = useState("");
  useEffect(() => { setFsOpen(false); setFsQ(""); setFsRes(null); setFsErr(""); }, [selected]);
  const runFileSearch = async () => {
    if (!selected || !fsQ.trim() || fsBusy) return;
    setFsBusy(true); setFsErr(""); setFsRes(null);
    try { setFsRes(await api.searchFile(selected, fsQ.trim())); }
    catch (e) { setFsErr((e as Error).message); }
    finally { setFsBusy(false); }
  };
  // Open a hit's backup in the detail drawer (full row fetched by id, since the
  // hit may be older than the recent list window).
  const openHitBackup = async (id: string) => {
    try { setSel(await api.backup(id)); } catch { /* row gone — ignore */ }
  };

  const load = () => {
    api.backups().then((d) => setList(d || [])).catch(() => setList([]));
    api.nodes().then(setNodes).catch(() => setNodes([]));
  };
  useEffect(() => { load(); }, []);

  // Deep-link from the container timeline (C9): "/backups?node=<id>&open=<backupId>"
  // selects that node and opens the restore drawer pre-targeted to that backup,
  // then clears the params so a refresh doesn't reopen it. Waits for the full
  // list to resolve so the backup can be found.
  const [sp, setSp] = useSearchParams();
  const deepLinked = useRef(false);
  useEffect(() => {
    if (deepLinked.current) return;
    const node = sp.get("node");
    const open = sp.get("open");
    if (!node && !open) return;
    if (open && list.length === 0) return; // wait for backups to load
    if (node) { setSelected(node); setQ(""); setStatusF(""); setVerifiedF(""); setBpage(1); }
    if (open) { const b = list.find((x) => x.id === open); if (b) setSel(b); }
    deepLinked.current = true;
    setSp({}, { replace: true });
  }, [list, sp, setSp]);

  // F100: which restores are running RIGHT NOW, server-side. A restore outlives
  // the page that started it, so without this a reload during a 40-minute
  // restore left a UI that looked idle while a destructive operation continued —
  // with no way to stop it short of restarting DockBack.
  const [runningRestores, setRunningRestores] = useState<RunningRestore[]>([]);
  const loadRestores = useCallback(() => {
    api.runningRestores().then((r) => setRunningRestores(r.running || [])).catch(() => {});
  }, []);
  useEffect(() => { loadRestores(); }, [loadRestores]);
  usePoll(loadRestores, 15000, [loadRestores]);

  const loadDrills = useCallback(() => {
    api.drills().then((arr) => {
      const m: Record<string, DrillStatus> = {};
      for (const d of arr) {
        m[d.backup_id] = d;
        const started = running.current[d.backup_id];
        if (started && d.ran_at * 1000 > started) delete running.current[d.backup_id]; // finished
      }
      setDrills(m);
    }).catch(() => {});
  }, []);
  useEffect(() => { loadDrills(); }, [loadDrills]);

  const runDrill = async (b: Backup) => {
    running.current[b.id] = Date.now();
    bump();
    try { await api.drillBackup(b.id); } catch { delete running.current[b.id]; bump(); return; }
    setTimeout(loadDrills, 4000);
  };

  // Fetch the selected node's page (server-side search/filter/pagination).
  const loadPage = useCallback(() => {
    if (!selected) { setPageData(null); return; }
    api.backupsPage({
      node_id: selected, q: q.trim() || undefined,
      status: statusF || undefined, verified: verifiedF || undefined,
      page: bpage, page_size: PAGE_SIZE,
    }).then(setPageData).catch(() => setPageData(null));
  }, [selected, q, statusF, verifiedF, bpage]);

  // Reload when the node/search/filters/page change (debounced).
  useEffect(() => {
    const h = setTimeout(loadPage, 200);
    return () => clearTimeout(h);
  }, [loadPage]);

  // Pin / unpin a backup ("keep forever" — F2). Optimistic local flip, then the
  // server call, then a reload to reconcile (also updates the open drawer copy).
  const togglePin = async (b: Backup, e: React.MouseEvent) => {
    e.stopPropagation();
    const next = !b.pinned;
    try { await api.pinBackup(b.id, next); }
    catch (err) { toast.error(`Couldn't ${next ? "pin" : "unpin"} backup: ${(err as Error).message}`); return; }
    finally { load(); loadPage(); }
    setSel((s) => (s && s.id === b.id ? { ...s, pinned: next } : s));
  };

  // Live backup deltas over SSE (A7): a start/finish refetches the list + page
  // immediately. Polling stays as a fallback — slow when the stream is live, the
  // original 5s cadence when it's down.
  const { connected } = useEventStream({
    // A finished drill announces itself on this event too: its verdict changes
    // the row's drill badge and its confidence grade.
    "backup.status": () => { load(); loadPage(); loadDrills(); },
  });
  usePoll(() => { load(); loadPage(); }, connected ? 30000 : 5000, [loadPage]);
  // Drill results arrive on the stream, so this is only a fallback for when the
  // stream is down. A drill runs for minutes; polling every 5 seconds for the
  // whole time a Backups tab is open is twelve requests a minute for a row that
  // changes once.
  usePoll(loadDrills, connected ? 30000 : 5000, [loadDrills, connected]);

  const nodeById = useMemo(() => {
    const m: Record<string, Node> = {};
    nodes.forEach((n) => { m[n.id] = n; });
    return m;
  }, [nodes]);

  const groups = useMemo(() => {
    const g: Record<string, Group> = {};
    for (const b of list) {
      if (pendingDel.has(b.id)) continue; // hide soft-deleted (undo pending)
      const n = nodeById[b.node_id];
      if (!g[b.node_id]) g[b.node_id] = { nodeId: b.node_id, name: n?.name || b.node_id, node: n, count: 0, total: 0, verified: 0, failed: 0, running: 0, last: 0 };
      const e = g[b.node_id];
      e.count++; e.total += b.size_bytes;
      if (b.verified === "verified") e.verified++;
      if (b.status === "failed") e.failed++;
      if (b.status === "running") e.running++;
      if (b.created_at > e.last) e.last = b.created_at;
    }
    return Object.values(g).sort((a, b) => b.last - a.last);
  }, [list, nodeById, pendingDel]);

  const unpend = (ids: string[]) => setPendingDel((p) => { const n = new Set(p); ids.forEach((i) => n.delete(i)); return n; });

  // Soft delete with a 5s Undo (A3): the row is hidden immediately and the actual
  // API call is deferred, so nothing is destroyed until the window elapses.
  const del = (b: Backup) => {
    setSel(null);
    setPendingDel((p) => new Set(p).add(b.id));
    toast.undo({
      message: `Deleted backup of ${b.target_name}`,
      onUndo: () => unpend([b.id]),
      onCommit: async () => {
        try { await api.deleteBackup(b.id); }
        catch (e) {
          const msg = (e as Error).message;
          // F63: the server refused because newer incremental backups depend on
          // this one — offer the audited whole-chain delete instead.
          if (msg.includes("baseline of")) {
            toast.action({
              message: `This backup is the baseline of newer incremental backups. Deleting it alone would make them unrestorable.`,
              actionLabel: "Delete the whole chain",
              onAction: async () => {
                try { await api.deleteBackup(b.id, true); toast.success("Chain deleted."); }
                catch (e2) { toast.error(`Couldn't delete the chain: ${(e2 as Error).message}`); }
                finally { load(); loadPage(); }
              },
            });
          } else {
            toast.error(`Couldn't delete backup: ${msg}`);
          }
        }
        finally { unpend([b.id]); load(); loadPage(); }
      },
    });
  };

  const bulkDelete = () => {
    const ids = [...checked];
    if (ids.length === 0) return;
    setChecked(new Set());
    setPendingDel((p) => new Set([...p, ...ids]));
    toast.undo({
      message: `Deleted ${ids.length} backup${ids.length === 1 ? "" : "s"}`,
      onUndo: () => unpend(ids),
      onCommit: async () => {
        try {
          const res = await api.deleteBackups(ids);
          const failedN = res.failed?.length || 0;
          if (failedN) toast.error(`${failedN} backup${failedN === 1 ? "" : "s"} could not be deleted: ${res.failed.map((f) => f.error).join(", ")}`);
        } catch (e) {
          toast.error(`Bulk delete failed: ${(e as Error).message}`);
        } finally {
          unpend(ids); load(); loadPage();
        }
      },
    });
  };

  // Per-row restore-drill status + trigger (PLAN §9.4). stopPropagation so the
  // control doesn't open the row's detail drawer.
  const drillCell = (b: Backup) => {
    if (b.status !== "success") return <span className="text-xs text-on-surface-variant">—</span>;
    const d = drills[b.id];
    const isRunning = !!running.current[b.id];
    return (
      <div className="flex items-center gap-2" onClick={(e) => e.stopPropagation()}>
        {isRunning ? (
          <Chip kind="info"><Loader2 size={12} className="animate-spin" /> Drilling…</Chip>
        ) : !d ? (
          <span className="text-xs text-on-surface-variant">Never</span>
        ) : d.ok ? (
          <Chip kind="ok">Passed {fmtAgo(d.ran_at)}</Chip>
        ) : (
          <span title={d.detail}><Chip kind="err">Failed {fmtAgo(d.ran_at)}</Chip></span>
        )}
        <Button variant="ghost" onClick={() => runDrill(b)} disabled={isRunning}
          title="Test-restore this backup into an isolated sandbox">
          <Play size={13} /> {d || isRunning ? "Re-run" : "Run"}
        </Button>
      </div>
    );
  };

  const selectedRows = (pageData?.items || []).filter((b) => !pendingDel.has(b.id));
  const selTotal = pageData?.total ?? 0;
  const pageCount = Math.max(1, Math.ceil(selTotal / PAGE_SIZE));
  const selGroup = groups.find((g) => g.nodeId === selected);
  const openNode = (nodeId: string) => { setSelected(nodeId); setQ(""); setStatusF(""); setVerifiedF(""); setBpage(1); };

  // Reset the selection whenever the visible page changes (node / filters / page).
  useEffect(() => { setChecked(new Set()); }, [selected, bpage, q, statusF, verifiedF]);
  const toggleRow = (id: string) => setChecked((prev) => { const n = new Set(prev); n.has(id) ? n.delete(id) : n.add(id); return n; });
  const allOnPageSelected = selectedRows.length > 0 && selectedRows.every((b) => checked.has(b.id));
  const toggleAll = () => setChecked((prev) => {
    const n = new Set(prev);
    if (selectedRows.every((b) => n.has(b.id))) selectedRows.forEach((b) => n.delete(b.id));
    else selectedRows.forEach((b) => n.add(b.id));
    return n;
  });

  // F100: open the drawer for a running restore. The row may be older than the
  // recent-list window (a long restore of an old backup), so fall back to
  // fetching it by id rather than silently doing nothing.
  const openRunningRestore = async (run: RunningRestore) => {
    const stack = run.id.startsWith("stack:") ? run.id.slice("stack:".length) : "";
    const row = stack
      ? list.find((x) => x.stack === stack && x.status === "success")
      : list.find((x) => x.id === run.id);
    if (row) { setSelected(row.node_id); setSel(row); return; }
    if (stack) return; // a stack with nothing in the recent list — the banner still reports it
    try { const full = await api.backup(run.id); setSelected(full.node_id); setSel(full); }
    catch { /* the backup row is gone; the banner keeps reporting the run */ }
  };

  return (
    <div>
      <h1 className="text-2xl font-bold">Backups</h1>
      <p className="mb-6 mt-1 text-on-surface-variant">Every backup is verified by test-restore before it is trusted.</p>

      {/* F100: a restore outlives the page that started it. Without this, reloading
          mid-restore left a UI that looked idle while a destructive operation ran
          on — and no way to reach the Cancel button. Hidden while its own drawer
          is open, which already shows the live panel. */}
      {runningRestores.filter((r) => !sel || (r.id !== sel.id && r.id !== `stack:${sel.stack}`)).map((r) => (
        <div key={r.id} className="mb-4 flex flex-wrap items-center gap-x-3 gap-y-2 rounded border border-info/40 bg-info/10 px-3 py-2 text-sm text-info">
          <Loader2 size={15} className="shrink-0 animate-spin" />
          <span className="min-w-0 break-words">
            <span className="font-medium">Restore in progress</span>
            {r.label ? <>: {r.label}</> : null}
            {r.started_at ? <span className="text-on-surface-variant"> · started {fmtAgo(r.started_at)}</span> : null}
          </span>
          {!r.id.startsWith("node:") && (
            <Button variant="secondary" className="ml-auto h-7 shrink-0 px-2 py-0 text-xs" onClick={() => openRunningRestore(r)}>
              Open progress
            </Button>
          )}
        </div>
      ))}

      {!selected ? (
        groups.length === 0 ? (
          <Card className="p-10 text-center text-on-surface-variant">No backups yet. Pick a container on a server to back it up.</Card>
        ) : (
          <div className="tile-grid [--tile-min:22rem]">
            {groups.map((g) => <NodeBackupCard key={g.nodeId} g={g} onClick={() => openNode(g.nodeId)} />)}
          </div>
        )
      ) : (
        <>
          <button onClick={() => setSelected(null)} className="mb-4 inline-flex items-center gap-1 text-sm text-on-surface-variant hover:text-on-surface">
            <ChevronLeft size={16} /> All nodes
          </button>
          <div className="mb-4 flex items-center gap-3">
            <NodeAvatar size={36} />
            <div>
              <div className="text-lg font-bold">{selGroup?.name || selected}</div>
              <div className="text-xs text-on-surface-variant">{selTotal} backup{selTotal === 1 ? "" : "s"}{(q || statusF || verifiedF) ? " matching" : ""} · {fmtBytes(selGroup?.total || 0)}</div>
            </div>
          </div>

          {/* Server-side search + filters (PLAN §4.13) */}
          <div className="mb-3 flex flex-wrap items-center gap-2">
            <div className="relative">
              <Search size={14} className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-on-surface-variant" />
              <input
                value={q}
                onChange={(e) => { setQ(e.target.value); setBpage(1); }}
                placeholder="Search target or stack…"
                className="w-64 rounded border border-outline-variant bg-surface-lowest py-1.5 pl-8 pr-3 text-sm text-on-surface focus:outline-none focus:ring-1 focus:ring-primary"
              />
            </div>
            <select value={statusF} onChange={(e) => { setStatusF(e.target.value); setBpage(1); }}
              className="rounded border border-outline-variant bg-surface-lowest px-3 py-1.5 text-sm text-on-surface-variant focus:outline-none focus:ring-1 focus:ring-primary">
              <option value="">All statuses</option>
              <option value="success">Success</option>
              <option value="running">Running</option>
              <option value="failed">Failed</option>
            </select>
            <select value={verifiedF} onChange={(e) => { setVerifiedF(e.target.value); setBpage(1); }}
              className="rounded border border-outline-variant bg-surface-lowest px-3 py-1.5 text-sm text-on-surface-variant focus:outline-none focus:ring-1 focus:ring-primary">
              <option value="">Any verification</option>
              <option value="verified">Verified</option>
              <option value="unverified">Unverified</option>
              <option value="failed">Failed</option>
            </select>
            {/* F70: search file CONTENTS-of-backups by path, across generations. */}
            <button
              onClick={() => setFsOpen((v) => !v)}
              className={`inline-flex items-center gap-1.5 rounded border px-3 py-1.5 text-sm ${fsOpen ? "border-primary/50 bg-primary/10 text-primary" : "border-outline-variant bg-surface-lowest text-on-surface-variant hover:text-on-surface"}`}
            >
              <FileText size={14} /> Find a file
            </button>
          </div>

          {/* F70: cross-backup file search over the stored indexes. */}
          {fsOpen && (
            <Card className="mb-3 p-3">
              <div className="flex items-center gap-2">
                <div className="relative flex-1">
                  <Search size={14} className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-on-surface-variant" />
                  <input
                    value={fsQ}
                    onChange={(e) => setFsQ(e.target.value)}
                    onKeyDown={(e) => { if (e.key === "Enter") runFileSearch(); }}
                    placeholder="File name or path fragment (e.g. config.xml)…"
                    autoFocus
                    className="w-full rounded border border-outline-variant bg-surface-lowest py-1.5 pl-8 pr-3 text-sm text-on-surface focus:outline-none focus:ring-1 focus:ring-primary"
                  />
                </div>
                <Button variant="secondary" onClick={runFileSearch} disabled={fsBusy || !fsQ.trim()}>
                  {fsBusy ? <Loader2 size={14} className="animate-spin" /> : <Search size={14} />} Search
                </Button>
              </div>
              <p className="mt-1.5 text-[11px] text-on-surface-variant">
                Searches the stored file index of every successful backup on this node — every generation of a matching file is listed, with a one-click single-file download. Backups made before file indexing existed are skipped.
              </p>
              {fsErr && <p className="mt-2 text-xs text-error">Search failed: {fsErr}</p>}
              {fsRes && fsRes.results.length === 0 && (
                <p className="mt-2 text-xs text-on-surface-variant">
                  No file matching "{fsQ.trim()}" in {fsRes.searched} indexed backup{fsRes.searched === 1 ? "" : "s"}{fsRes.skipped > 0 ? ` (${fsRes.skipped} pre-index backup${fsRes.skipped === 1 ? "" : "s"} skipped)` : ""}.
                </p>
              )}
              {fsRes && fsRes.results.length > 0 && (
                <>
                  <div className="mt-2 max-h-80 overflow-y-auto rounded border border-outline-variant/60">
                    {fsRes.results.map((h, i) => (
                      <div key={`${h.backup_id}-${h.path}-${i}`} className="flex items-center gap-2 border-b border-outline-variant/30 px-2.5 py-1.5 text-xs last:border-b-0 hover:bg-surface-high/30">
                        <button onClick={() => openHitBackup(h.backup_id)} title="Open this backup" className="shrink-0 tnum text-on-surface-variant hover:text-primary hover:underline">
                          {new Date(h.created_at * 1000).toLocaleString()}
                        </button>
                        <span className="shrink-0 rounded bg-surface-high px-1.5 py-0.5 text-[11px] text-on-surface-variant">{h.target}</span>
                        <span className="min-w-0 flex-1 truncate font-mono text-on-surface" title={h.path}>{h.path}</span>
                        <span className="shrink-0 tnum text-on-surface-variant">{fmtBytes(h.size)}</span>
                        <ExportDownloadButton backupId={h.backup_id} purpose="extract" path={h.path}
                          title="Download this file from this generation" iconOnly icon={<Download size={14} />} />
                        {/* F96: write THIS generation of the file back. The hit
                            names its own container, so the target is unambiguous
                            even when the search spans many services. */}
                        <RestoreFileButton
                          backupId={h.backup_id} path={h.path} container={h.target}
                          nodeId={selected || undefined} targetId={h.target}
                        />
                      </div>
                    ))}
                  </div>
                  <p className="mt-1.5 text-[11px] text-on-surface-variant">
                    {fsRes.results.length} generation{fsRes.results.length === 1 ? "" : "s"} across {fsRes.searched} indexed backup{fsRes.searched === 1 ? "" : "s"}
                    {fsRes.skipped > 0 ? ` · ${fsRes.skipped} pre-index backup${fsRes.skipped === 1 ? "" : "s"} skipped` : ""}
                    {fsRes.truncated ? " · result capped — narrow the search" : ""}.
                  </p>
                </>
              )}
            </Card>
          )}
          {/* Bulk-select action bar — appears once one or more rows are ticked. */}
          {checked.size > 0 && (
            <div className="mb-3 flex flex-wrap items-center gap-3 rounded border border-outline-variant bg-surface-container px-4 py-2.5 text-sm">
              <span className="font-medium">{checked.size} selected</span>
              <Button variant="danger" size="sm" onClick={bulkDelete}>
                <Trash2 size={14} /> Delete selected
              </Button>
              <button className="text-on-surface-variant hover:text-on-surface" onClick={() => setChecked(new Set())}>Clear</button>
            </div>
          )}
          <Card>
            <table className="w-full text-sm">
              <thead className="border-b border-outline-variant/60 text-left text-xs uppercase tracking-wider text-on-surface-variant">
                <tr>
                  <th className="px-4 py-3 w-10"><input type="checkbox" aria-label="Select all backups on this page" checked={allOnPageSelected} onChange={toggleAll} /></th>
                  <th className="px-5 py-3">Status</th><th className="px-5 py-3">Verified</th><th className="px-5 py-3">Target</th>
                  <th className="px-5 py-3">Size</th><th className="px-5 py-3">Created</th><th className="px-5 py-3">Restore drill</th><th className="px-5 py-3"></th>
                </tr>
              </thead>
              <tbody>
                {selectedRows.map((b) => (
                  <tr key={b.id} onClick={() => setSel(b)} className={`cursor-pointer border-b border-outline-variant/30 hover:bg-surface-high/40 ${checked.has(b.id) ? "bg-surface-high/30" : ""}`}>
                    <td className="px-4 py-4" onClick={(e) => e.stopPropagation()}>
                      <input type="checkbox" checked={checked.has(b.id)} onChange={() => toggleRow(b.id)} aria-label={`Select backup of ${b.target_name}`} />
                    </td>
                    <td className="px-5 py-4">{statusChip(b.status)}</td>
                    <td className="px-5 py-4">
                      <div className="flex items-center gap-1.5">
                        {gradeBadge(b.confidence)}
                        {verifyChip(b.verified)}
                        {failedCopies(b).length > 0 && (
                          <span title={`Off-site incomplete: ${failedCopies(b).map((l) => l.name).join(", ")} not uploaded`}>
                            <Chip kind="warn"><AlertTriangle size={12} /> Degraded</Chip>
                          </span>
                        )}
                        {deferredCopies(b).length > 0 && (
                          <span title={`Deferred to upload window: ${deferredCopies(b).map((l) => l.name).join(", ")} — will mirror when the window opens, or use Send offsite now.`}>
                            <Chip kind="muted"><Clock size={12} /> Deferred</Chip>
                          </span>
                        )}
                        {immutableCopies(b).length > 0 && (
                          <span title={immutableCopies(b).every(isWormCopy)
                            ? `Immutable (WORM) copy on: ${immutableCopies(b).map((l) => l.name).join(", ")} — cannot be deleted until the object-lock expires.`
                            : `Protected copy on: ${immutableCopies(b).map((l) => l.name).join(", ")} — read-only and exempt from pruning until the lock expires. On a filesystem destination this is a speed bump, not WORM: root on the target can still remove it.`}>
                            <Chip kind="ok"><Lock size={12} /> {immutableCopies(b).every(isWormCopy) ? "Immutable" : "Locked"}</Chip>
                          </span>
                        )}
                        {b.key_mismatch && (
                          <span title="Encrypted with a master key that isn't the current one — restoring needs the original DOCKBACK_ENCRYPTION_KEY.">
                            <Chip kind="err"><KeyRound size={12} /> Key mismatch</Chip>
                          </span>
                        )}
                        {/* F86: sealed to an offline key — restoring needs the private key. */}
                        {rowSummary(b).write_only && (
                          <span title="Write-only encrypted: DockBack sealed this backup to a public key and cannot read it. Restoring it needs the offline private key from its recovery sheet.">
                            <Chip kind="info"><Lock size={12} /> write-only</Chip>
                          </span>
                        )}
                        {/* F69 ransomware tripwire: this run's delta looked like a mass-change event. */}
                        {b.suspect && (
                          <span title={`Possible mass-change/ransomware event: ${b.suspect}. Retention for this container is on hold — review its backups, then clear the hold on the container page.`}>
                            <Chip kind="err"><AlertTriangle size={12} /> suspect</Chip>
                          </span>
                        )}
                        {/* F83: skips covered by another container's backups (shared binds) don't count as Partial. */}
                        {rowSummary(b).partial && (
                          <span title="Partial backup — one or more data mounts were not captured (e.g. a large media bind excluded by default). Open to see which.">
                            <Chip kind="warn"><AlertTriangle size={12} /> Partial</Chip>
                          </span>
                        )}
                        {/* F103: detected as a database, but its dump tools were
                            missing — the files were copied from a RUNNING engine
                            and may be torn. Without this it looked like an
                            ordinary app backup and could still grade A. */}
                        {rowSummary(b).db_fallback && (
                          <span title="A database in this backup was copied as raw files from a running engine rather than snapshotted consistently, which can be torn. Either the container's own dump tools were missing, or — for an embedded database — the volume sidecar image has no sqlite3. Fix whichever applies and back up again; the grade explains which.">
                            <Chip kind="warn"><AlertTriangle size={12} /> raw DB files</Chip>
                          </span>
                        )}
                        {rowSummary(b).incremental && (
                          <span title="Incremental backup — captures only files changed since the previous backup. Restore automatically applies the full baseline plus every delta in this chain.">
                            <Chip kind="info"><Layers size={12} /> delta · {rowSummary(b).chain_depth} since full</Chip>
                          </span>
                        )}
                        {b.pinned && (
                          <span title="Pinned — exempt from pruning (kept forever regardless of the retention policy).">
                            <Chip kind="ok"><Pin size={12} /> Pinned</Chip>
                          </span>
                        )}
                        {b.label && (
                          <span title={b.label} className="inline-flex max-w-[12rem] items-center gap-1 truncate rounded bg-surface-high px-1.5 py-0.5 text-xs text-on-surface-variant">
                            <Tag size={11} className="shrink-0" /> <span className="truncate">{b.label}</span>
                          </span>
                        )}
                      </div>
                    </td>
                    <td className="px-5 py-4">
                      <div className="font-medium">{b.target_name}{b.stack && <span className="text-on-surface-variant"> · {b.stack}</span>}</div>
                      {failedCopies(b).map((l, j) => (
                        <div key={j} className="mt-0.5 flex items-center gap-1 text-xs text-warning">
                          <AlertTriangle size={11} className="shrink-0" />
                          <span>{l.name} ({l.type}) — {l.detail || "offsite upload failed"}</span>
                        </div>
                      ))}
                    </td>
                    <td className="px-5 py-4 tnum">{fmtBytes(b.size_bytes)}</td>
                    <td className="px-5 py-4 tnum text-on-surface-variant">{fmtAgo(b.created_at)}</td>
                    <td className="px-5 py-4">{drillCell(b)}</td>
                    <td className="px-5 py-4 text-right">
                      <div className="flex items-center justify-end gap-1.5">
                        {b.status === "success" && (
                          <button
                            onClick={(e) => togglePin(b, e)}
                            aria-pressed={!!b.pinned}
                            title={b.pinned ? "Unpin — allow the retention policy to prune this backup" : "Pin (keep forever) — exempt this backup from all pruning"}
                            className={`rounded p-1.5 hover:bg-surface-high ${b.pinned ? "text-primary" : "text-on-surface-variant"}`}>
                            <Pin size={15} className={b.pinned ? "fill-current" : ""} />
                          </button>
                        )}
                        <Button variant="secondary" size="sm"
                          onClick={(e) => { e.stopPropagation(); setSel(b); }}
                          title="Restore this backup — opens options (choose target node, download, delete, view report)">
                          <RotateCcw size={14} className="text-primary" /> Restore…
                        </Button>
                      </div>
                    </td>
                  </tr>
                ))}
                {selectedRows.length === 0 && <tr><td colSpan={8} className="px-5 py-10 text-center text-on-surface-variant">{(q || statusF || verifiedF) ? "No backups match your search/filter." : "No backups for this node."}</td></tr>}
              </tbody>
            </table>
            {pageCount > 1 && (
              <div className="flex items-center justify-between gap-3 border-t border-outline-variant px-5 py-3 text-xs text-on-surface-variant">
                <span>Showing {(bpage - 1) * PAGE_SIZE + 1}–{Math.min(bpage * PAGE_SIZE, selTotal)} of {selTotal}</span>
                <div className="flex items-center gap-2">
                  <button onClick={() => setBpage((p) => Math.max(1, p - 1))} disabled={bpage <= 1}
                    className="rounded border border-outline-variant px-2.5 py-1 hover:bg-surface-highest disabled:opacity-40">Prev</button>
                  <span>Page {bpage} / {pageCount}</span>
                  <button onClick={() => setBpage((p) => Math.min(pageCount, p + 1))} disabled={bpage >= pageCount}
                    className="rounded border border-outline-variant px-2.5 py-1 hover:bg-surface-highest disabled:opacity-40">Next</button>
                </div>
              </div>
            )}
          </Card>
        </>
      )}

      {sel && <Detail b={sel} nodeName={nodeById[sel.node_id]?.name || sel.node_id} nodes={nodes} drills={drills}
        siblings={list.filter((x) => x.node_id === sel.node_id && x.target_name === sel.target_name && x.status === "success")}
        stackServices={sel.stack ? new Set(list.filter((x) => x.node_id === sel.node_id && x.stack === sel.stack && x.status === "success").map((x) => x.target_name)).size : 0}
        onPick={setSel} onClose={() => setSel(null)} onDelete={() => del(sel)} onChanged={load}
        runningRestores={runningRestores} onRestoresChanged={loadRestores} />}
    </div>
  );
}

function NodeBackupCard({ g, onClick }: { g: Group; onClick: () => void }) {
  const isSocket = !g.node || g.node.transport === "local-proxy" || g.node.address?.includes("docker.sock");
  return (
    <Card onClick={onClick} className="cursor-pointer p-5 transition-all hover:border-primary/50">
      <div className="mb-4 flex items-start justify-between">
        <div className="flex items-center gap-3">
          <NodeAvatar size={40} />
          <div>
            <div className="flex items-center gap-2 text-lg font-bold">
              {g.name}
              {g.node && (g.node.reachable ? <Wifi size={14} className="text-success" /> : <WifiOff size={14} className="text-error" />)}
            </div>
            {g.node && <div className="flex items-center gap-1.5 font-mono text-xs text-on-surface-variant">{isSocket ? <Link2 size={11} /> : <Network size={11} />}{g.node.address}</div>}
          </div>
        </div>
        <ChevronRight size={18} className="text-on-surface-variant" />
      </div>
      <div className="flex items-end justify-between border-t border-outline-variant/50 pt-4">
        <div>
          <div className="tnum text-xl font-bold">{g.count}</div>
          <div className="text-xs text-on-surface-variant">backup{g.count === 1 ? "" : "s"} · {fmtBytes(g.total)}</div>
        </div>
        <div className="flex flex-wrap justify-end gap-1.5">
          {g.verified > 0 && <Chip kind="ok"><ShieldCheck size={11} /> {g.verified}</Chip>}
          {g.running > 0 && <Chip kind="info"><BackupCloudIcon size={11} active /> {g.running}</Chip>}
          {g.failed > 0 && <Chip kind="err"><ShieldX size={11} /> {g.failed}</Chip>}
        </div>
      </div>
      <div className="mt-3 text-xs text-on-surface-variant">Last backup {fmtAgo(g.last)}</div>
    </Card>
  );
}

function Detail({ b: row, nodeName, nodes, drills, siblings, stackServices, onPick, onClose, onDelete, onChanged, runningRestores, onRestoresChanged }: { b: Backup; nodeName: string; nodes: Node[]; drills: Record<string, DrillStatus>; siblings: Backup[]; stackServices: number; onPick: (b: Backup) => void; onClose: () => void; onDelete: () => void; onChanged: () => void; runningRestores: RunningRestore[]; onRestoresChanged: () => void }) {
  // Perf Fix 8: list rows are SLIM (no manifest/verification/locations blobs) —
  // fetch the full row once on open and re-fetch when a row-level signal that
  // changes the blobs moves (verify/scrub/mirror update verification/locations).
  // `b` below merges the row's fresh scalars with the fetched blobs, so every
  // existing read in this large component works unchanged for both shapes.
  const [full, setFull] = useState<Backup | null>(null);
  useEffect(() => {
    let gone = false;
    api.backup(row.id).then((f) => { if (!gone) setFull(f); }).catch(() => {});
    return () => { gone = true; };
  }, [row.id, row.verified, row.last_verified_at, row.status, row.locations]);
  const b: Backup = full && full.id === row.id
    ? { ...row, manifest_json: full.manifest_json, verification_json: full.verification_json, locations: full.locations, summary: undefined, chain_dependents: full.chain_dependents }
    : row;
  const man = safe(b.manifest_json);
  const ver = safe(b.verification_json);
  // F219: the one-click test clone — an isolated copy DockBack names and reaps.
  const [testing, setTesting] = useState(false);
  const [clones, setClones] = useState<TestClone[]>([]);
  const [cloneTTL, setCloneTTL] = useState(24);
  const [cloneBusy, setCloneBusy] = useState("");
  // Overrides the operator has already agreed to for THIS backup, remembered for
  // as long as the drawer is on it.
  //
  // Both gates sit before the F206 step-up, so a protected container hits them
  // first: acknowledge, get asked for the password, and the retry from the
  // password prompt carries no acknowledgement — the same question comes back.
  // (That was already true of the compatibility gate before F218 added a second
  // one; the prompt's retry passes only credentials, by design, because the
  // destructive confirm is deliberately not repeated there either.) Remembering
  // the answer is what makes the two consistent. Reset when the drawer moves to
  // another backup — an acknowledgement is about one archive, not a session.
  const [acks, setAcks] = useState({ incompatible: false, unverified: false });
  // F218: the verify leg of "Verify & restore" — its own state, because it is
  // not a restore yet and must not light up the restore progress panel.
  const [vrState, setVrState] = useState<"idle" | "verifying">("idle");
  const [vrLines, setVrLines] = useState<{ level: string; msg: string }[]>([]);
  const [vrErr, setVrErr] = useState("");
  const vrEsRef = useRef<(() => void) | null>(null);
  const [rState, setRState] = useState<"idle" | "running" | "done" | "failed" | "canceled">("idle");
  const [canceling, setCanceling] = useState(false);
  const [rMode, setRMode] = useState<"one" | "stack">("one");
  const [rLines, setRLines] = useState<{ level: string; msg: string }[]>([]);
  const [rErr, setRErr] = useState("");
  const rLog = useStickyScroll(rLines.length); // restore/stack live log follows the tail
  const [source, setSource] = useState("");
  // F86: the offline private key pasted for a write-only restore. Component state
  // only — cleared when the drawer unmounts, never persisted or sent anywhere but
  // the single restore request.
  const [privKey, setPrivKey] = useState("");
  // F206: an in-place restore of a protected container (critical data, or one
  // marked write-only-required) asks for the password before it overwrites
  // anything. Restoring AS A COPY never does — the safe path stays the fast one.
  const [restoreStepUp, setRestoreStepUp] = useState<{ totp: boolean; err: string } | null>(null);
  // F204: the recovery-key drill. Proves the pasted key actually opens this
  // backup — one DEK unwrap and one frame decrypted, no restore. The key stays
  // in the same component state the restore already uses; nothing new is kept.
  const [keyCheck, setKeyCheck] = useState<{ ok: boolean; fingerprint: string; message: string } | null>(null);
  const [keyChecking, setKeyChecking] = useState(false);
  const [keyStepUp, setKeyStepUp] = useState<{ totp: boolean; err: string } | null>(null);
  // A key verified against THIS text — cleared as soon as the field is edited, so
  // a stale pass can never be read as covering a key that has since changed.
  const onPrivKeyChange = (v: string) => { setPrivKey(v); setKeyCheck(null); setKeyStepUp(null); };
  const verifyKey = async (pw = "", code = "") => {
    if (!privKey.trim()) return;
    setKeyChecking(true);
    try {
      const r = await api.verifyRecoveryKey(b.id, privKey.trim(), pw ? { password: pw, code } : undefined);
      setKeyStepUp(null);
      setKeyCheck({ ok: r.ok, fingerprint: r.fingerprint || "", message: r.ok ? (r.message || "This key opens this backup.") : (r.error || "This key did not open this backup.") });
    } catch (e) {
      if (e instanceof StepUpError) setKeyStepUp({ totp: e.totp_required, err: pw ? e.message : "" });
      // A drill that could not RUN (unreachable copy, not write-only) is an
      // error about the attempt, never a verdict on the key.
      else setKeyCheck({ ok: false, fingerprint: "", message: (e as Error).message });
    } finally { setKeyChecking(false); }
  };
  // F94: cross-host portability warnings for the currently-selected target.
  const [portability, setPortability] = useState<string[]>([]);
  // F95: what the image that will actually run expects but this backup lacks.
  const [imageDrift, setImageDrift] = useState<string[]>([]);
  // F2: pin ("keep forever") + editable label, seeded from the backup and kept in
  // local state so the drawer reflects edits immediately without a full reload.
  const [pinned, setPinned] = useState(!!b.pinned);
  const [pinBusy, setPinBusy] = useState(false);
  const [label, setLabel] = useState(b.label || "");
  const [labelSaving, setLabelSaving] = useState(false);
  const togglePinD = async () => {
    const next = !pinned;
    setPinBusy(true); setPinned(next);
    try { await api.pinBackup(b.id, next); onChanged(); }
    catch { setPinned(!next); }
    finally { setPinBusy(false); }
  };
  const saveLabel = async () => {
    setLabelSaving(true);
    try { await api.labelBackup(b.id, label.trim()); onChanged(); }
    catch { /* surfaced on next load */ }
    finally { setLabelSaving(false); }
  };
  const [restoreNode, setRestoreNode] = useState(b.node_id); // target node — defaults to origin (PLAN §4.8 cross-host)

  // F94: ask what the chosen target cannot honor. Only fires when the node
  // actually differs, so the ordinary same-host restore adds no request at all.
  // F119 rides along here rather than in its own effect, but unlike portability
  // it matters for a SAME-host restore too — two containers sharing a directory
  // is the common case, and the whole point is that it is easy to miss. So the
  // request now fires for either node, and only the portability half is gated.
  useEffect(() => {
    let live = true;
    const crossHost = restoreNode !== b.node_id;
    api.restoreReadiness(b.id, crossHost ? restoreNode : undefined)
      .then((r) => {
        if (!live) return;
        setPortability(crossHost ? (r.portability || []) : []);
        setSharedData(r.shared_data || []);
        setPortConflicts(r.port_conflicts || []);
        setRestoreBlock(r.restore_block || "");
        setAddressVars(r.address_vars || []);
      })
      .catch(() => { if (live) { setPortability([]); setSharedData([]); setPortConflicts([]); setRestoreBlock(""); setAddressVars([]); } });
    return () => { live = false; };
  }, [b.id, b.node_id, restoreNode]);
  // F119: other containers that use the same directories this restore writes
  // into. Fetched with the same call as portability — no extra request.
  const [sharedData, setSharedData] = useState<string[]>([]);
  // F144: host ports the target already has taken. Depends on the chosen node,
  // so it rides on the same request rather than the per-backup one below.
  const [portConflicts, setPortConflicts] = useState<string[]>([]);
  // F174: a database whose own environment cannot initialize an empty data
  // directory — which restoring from a dump requires it to do. Independent of
  // the target node, but it rides on this request rather than earning its own.
  const [restoreBlock, setRestoreBlock] = useState("");
  // F177: the environment variables this container records an address in, on a
  // cross-host restore. Names only — the manifest carries no values.
  const [addressVars, setAddressVars] = useState<string[]>([]);

  // F95: image drift is independent of the target node — the recorded image is
  // either still available or it isn't — so it is fetched once per backup.
  // F110 rides along on the same request: the application's preconditions are
  // derived from the manifest alone, so they cost nothing extra and apply to a
  // same-host restore too (an edited compose file moves a mount just as easily).
  useEffect(() => {
    let live = true;
    api.restoreReadiness(b.id)
      .then((r) => {
        if (!live) return;
        setImageDrift(r.image_drift || []);
        setAppPre(r.app_preconditions || null);
        setCerts(r.certificates || []);
      })
      .catch(() => { if (live) { setImageDrift([]); setAppPre(null); setCerts([]); } });
    return () => { live = false; };
  }, [b.id]);
  // F110: what this application needs from wherever it lands.
  const [appPre, setAppPre] = useState<AppPreconditions | null>(null);
  // F143: the TLS certificates inside this backup. Manifest-derived, so it is
  // fetched once per backup like the drift check, not per target node.
  const [certs, setCerts] = useState<CertRef[]>([]);
  // F114: the new address, when the operator is actually moving the app to one.
  // Blank is the normal case — a move that keeps the same domain needs nothing.
  const [newSiteAddress, setNewSiteAddress] = useState("");
  // F160: the address of a service this application DEPENDS ON, when that is
  // what moved. Blank is the normal case and changes nothing.
  const [newUpstreamAddress, setNewUpstreamAddress] = useState("");
  const [snapshot, setSnapshot] = useState(true); // safety snapshot before overwrite (PLAN §3.7), on by default
  const [recreate, setRecreate] = useState(false); // "Revert update": recreate from this backup's image digest, rolling back a bad upgrade
  const [asCopy, setAsCopy] = useState(false); // "Restore as a copy": isolated clone under a new name (F10)
  const [asName, setAsName] = useState(""); // the clone's container name
  // "Reconstruct stack on host": on a DR recreate, rebuild the on-host stack
  // folder + compose file so a restore onto a fresh machine reproduces the
  // organized <base>/<stack>/ layout. Opt-in (default off). The base dir (for
  // non-compose containers) is remembered client-side for convenience.
  const [reconstructHost, setReconstructHost] = useState(false);
  // #8: off by default — reproducing the source is the default, and the
  // capture-time finding is what tells the operator this choice exists.
  const [promoteRestart, setPromoteRestart] = useState(false);
  const [injectHealthchecks, setInjectHealthchecks] = useState(false);
  const [hostBaseDir, setHostBaseDir] = useState(() => { try { return localStorage.getItem("dockback.hostBaseDir") || ""; } catch { return ""; } });
  const knownWorkingDir: string = man?.stack_working_dir || "";
  // Was this a compose deployment? The compose project label (b.stack / manifest)
  // is the reliable signal — NOT the newer stack_working_dir field, which is absent
  // on backups made before it existed even though the restore still recovers the
  // directory from the container's compose labels. A `docker run` container has no
  // project, so it's the only case that needs a base directory.
  const isComposeContainer: boolean = !!(b.stack || man?.stack || man?.service);
  // "Remap machine IP": on a cross-host restore, rewrite the source machine's IP to
  // the target machine's IP in the recreated config + compose, so a service pinned
  // to the old host's address doesn't fail to bind. Opt-in; IPs prefilled from the
  // origin/target node addresses (blank when a node is addressed by name).
  const [remapIP, setRemapIP] = useState(false);
  // F195: the domain rewrite — works for every container, profile or not.
  const [remapDomain, setRemapDomain] = useState(false);
  const [domainFrom, setDomainFrom] = useState("");
  const [domainTo, setDomainTo] = useState("");
  const [remapFrom, setRemapFrom] = useState("");
  const [remapTo, setRemapTo] = useState("");
  const crossHostSel = restoreNode !== b.node_id;
  // Prefill the IP fields from the node addresses when the user first enables remap.
  useEffect(() => {
    if (!remapIP) return;
    setRemapFrom((cur) => cur || ipFromNodeAddr(nodes.find((n) => n.id === b.node_id)?.address));
    setRemapTo((cur) => cur || ipFromNodeAddr(nodes.find((n) => n.id === restoreNode)?.address));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [remapIP]);
  // Refresh the target prefill when the target node changes (unless user edited it).
  useEffect(() => { if (remapIP) setRemapTo(ipFromNodeAddr(nodes.find((n) => n.id === restoreNode)?.address)); /* eslint-disable-next-line react-hooks/exhaustive-deps */ }, [restoreNode]);
  // "Remap stack paths" (F81): move bind-mount sources, the reconstructed compose,
  // and the stack folder from the old machine's base directory to this machine's.
  const [remapPath, setRemapPath] = useState(false);
  const [pathFrom, setPathFrom] = useState("");
  const [pathTo, setPathTo] = useState("");
  // Prefill on first enable: From = parent of the recorded compose working dir;
  // To = the reconstruction base directory when one is set.
  useEffect(() => {
    if (!remapPath) return;
    const wd: string = man?.stack_working_dir || "";
    setPathFrom((cur) => cur || (wd.includes("/") ? wd.slice(0, wd.lastIndexOf("/")) : ""));
    setPathTo((cur) => cur || hostBaseDir.trim());
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [remapPath]);
  const [readiness, setReadiness] = useState<RestoreReadiness | null>(null); // F11: image-availability verdict
  const [readyBusy, setReadyBusy] = useState(false);
  const skipped: any[] = man?.skipped_mounts || [];
  // F83: a skip covered by another container's backups (shared bind captured
  // once by its owner) is intentional — shown separately, never as Partial.
  const uncoveredSkips = skipped.filter((sk: any) => !sk.covered_by);
  const coveredSkips = skipped.filter((sk: any) => sk.covered_by);
  // Things found in the SOURCE while backing it up. Distinct from the skips
  // above, which describe this archive: these describe the deployment it came
  // from, and DockBack reports them rather than reproducing them silently — for
  // several of them the obvious fix is the dangerous move.
  const findings: Finding[] = man?.findings || [];

  // What "Revert update" would actually put back, and how firmly the image is
  // pinned — so a floating tag (:latest/:release) or a never-pushed local build
  // can't silently drift a revert onto a different version than this backup
  // captured. Priority: a bundled image (exact, offline) > a recorded digest
  // (exact) > tag only (re-pulls whatever the tag points to now = may drift).
  const revertTarget = useMemo(() => {
    const image: string = man?.image || "";
    const digest: string = man?.image_digest || "";
    const bundled = !!man?.image_tar;
    const afterColon = image.includes(":") ? image.slice(image.lastIndexOf(":") + 1) : "";
    const tag = afterColon && !afterColon.includes("/") ? afterColon : (image ? "latest" : "");
    const sha = (/sha256:([0-9a-f]{6,})/i.exec(digest)?.[1] || "").slice(0, 12);
    const floating = ["latest", "release", "stable", "main", "master", "edge"].includes(tag);
    const pin: "bundled" | "digest" | "tag" = bundled ? "bundled" : (digest.includes("@sha256:") ? "digest" : "tag");
    return { image, tag, sha, bundled, floating, pin };
  }, [man]);
  const [verifying, setVerifying] = useState(false); // re-verify/scrub in progress (PLAN §9.4)
  const [mState, setMState] = useState<"idle" | "running" | "done" | "failed">("idle"); // on-demand mirror to destinations
  const [mMsg, setMMsg] = useState("");
  const [mLines, setMLines] = useState<{ level: string; msg: string }[]>([]);
  const mLog = useStickyScroll(mLines.length); // "Send offsite now" mirror log follows the tail
  const [dests, setDests] = useState<Destination[]>([]);
  const [mSel, setMSel] = useState<Set<string>>(new Set()); // destinations ticked for "Send offsite now"
  // F21: lazily browse the backup's files (only fetched when the panel is opened).
  const [browseOpen, setBrowseOpen] = useState(false);
  const [entries, setEntries] = useState<ArchiveEntry[] | null>(null);
  const [entriesLoading, setEntriesLoading] = useState(false);
  const [entriesErr, setEntriesErr] = useState("");
  const [fileFilter, setFileFilter] = useState("");
  useEffect(() => { setBrowseOpen(false); setEntries(null); setEntriesErr(""); setFileFilter(""); }, [b.id]);

  // F70: compare this generation with the previous one — an index-only diff
  // (added/changed/deleted), lazy-loaded when the section is opened.
  const prevGen = useMemo(
    () => [...siblings].filter((x) => x.id !== b.id && x.created_at < b.created_at).sort((x, y) => y.created_at - x.created_at)[0] || null,
    [siblings, b.id, b.created_at]
  );
  const [diffOpen, setDiffOpen] = useState(false);
  const [diffData, setDiffData] = useState<BackupDiffResp | null>(null);
  const [diffBusy, setDiffBusy] = useState(false);
  const [diffErr, setDiffErr] = useState("");
  useEffect(() => { setDiffOpen(false); setDiffData(null); setDiffErr(""); }, [b.id]);
  const toggleDiff = async () => {
    const next = !diffOpen;
    setDiffOpen(next);
    if (next && !diffData && !diffBusy && prevGen) {
      setDiffBusy(true); setDiffErr("");
      try { setDiffData(await api.diffBackups(prevGen.id, b.id)); }
      catch (e) { setDiffErr((e as Error).message); }
      finally { setDiffBusy(false); }
    }
  };
  const toggleBrowse = async () => {
    const next = !browseOpen;
    setBrowseOpen(next);
    if (next && entries === null && !entriesLoading) {
      setEntriesLoading(true); setEntriesErr("");
      try { const r = await api.backupEntries(b.id); setEntries((r.entries || []).filter((e) => !e.dir)); }
      catch (e) { setEntriesErr((e as Error).message); }
      finally { setEntriesLoading(false); }
    }
  };
  useEffect(() => { setRestoreNode(b.node_id); }, [b.node_id]);
  useEffect(() => { setReadiness(null); setReadyBusy(false); }, [b.id]); // re-check per backup (F11)
  useEffect(() => { api.destinations().then(setDests).catch(() => setDests([])); }, []);

  // F11: verify the backup's image can still be obtained for a restore (present
  // locally, pullable by digest/tag, or bundled) — on demand, never pulls.
  const checkReadiness = async () => {
    setReadyBusy(true);
    try { setReadiness(await api.restoreReadiness(b.id)); }
    catch (e) { setReadiness({ image_ok: false, has_image_tar: !!man?.image_tar, detail: (e as Error).message }); }
    finally { setReadyBusy(false); }
  };

  const verifyNow = async () => {
    setVerifying(true);
    try { await api.verifyBackup(b.id); setTimeout(onChanged, 1500); }
    catch { /* surfaced via the verification report on refresh */ }
    finally { setTimeout(() => setVerifying(false), 1500); }
  };
  // F50: run a restore drill from the confidence section (sandbox test-restore).
  const [drilling, setDrilling] = useState(false);
  const drillNow = async () => {
    setDrilling(true);
    try { await api.drillBackup(b.id); setTimeout(onChanged, 2000); }
    catch { /* surfaced via the drill result on refresh */ }
    finally { setTimeout(() => setDrilling(false), 2000); }
  };
  // F44: re-verify one specific copy (local or a destination).
  const [verifyingCopy, setVerifyingCopy] = useState("");
  const verifyCopy = async (src: string) => {
    setVerifyingCopy(src);
    try { await api.verifyBackup(b.id, src); setTimeout(onChanged, 1500); }
    catch { /* surfaced on refresh */ }
    finally { setTimeout(() => setVerifyingCopy(""), 1500); }
  };
  // F219: what this node is already running as a test clone. One call when the
  // drawer opens on a node, not on a poll — a clone is created by a click, and
  // the reaper's ten-minute sweep is not something to watch a spinner for. An
  // unreachable node simply reports nothing rather than claiming zero clones.
  const loadClones = useCallback(() => {
    api.testClones(b.node_id)
      .then((r) => { setClones(r.clones || []); setCloneTTL(r.ttl_hours || 24); })
      .catch(() => { /* node down: say nothing rather than "no clones" */ });
  }, [b.node_id]);
  useEffect(() => { loadClones(); }, [loadClones]);

  // "Test restore": nothing is overwritten, so the confirm names what is created
  // and when it goes, rather than warning about destruction that cannot happen.
  const testRestore = async (confirmUnverified = false) => {
    const hrs = cloneTTL;
    if (!confirmUnverified && !confirm(
      `TEST RESTORE — bring ${b.target_name} up from this backup as an isolated copy on node ${nodes.find((n) => n.id === b.node_id)?.name || b.node_id}.\n\n` +
      `The copy gets a fresh empty filesystem populated from the backup, no published ports, and a throwaway network. The running ${b.target_name} is NOT touched.\n\n` +
      `It is removed automatically after ${hrs} hour${hrs === 1 ? "" : "s"}, along with the volumes created for it. Continue?`
    )) return;
    setTesting(true); setRErr("");
    try {
      await api.restore(b.id, {
        node_id: b.node_id, target_id: man?.container_id || "", volumes: true, database: true,
        confirm: true, test_clone: true, source,
        ...(confirmUnverified || acks.unverified ? { confirm_unverified: true } : {}),
        ...(privKey.trim() ? { private_key: privKey.trim() } : {}),
      });
      // The clone is built asynchronously; give the engine a moment to create the
      // container before asking the node what it now has.
      setTimeout(loadClones, 4000);
      setTimeout(loadClones, 15000);
    } catch (e) {
      // F218's known-bad gate applies here too — the server refuses a backup whose
      // last verification failed until it is acknowledged. Worth asking for even
      // on this path (the archive may not extract at all), but NOT in the
      // destructive wording: nothing is being put back over anything.
      if (e instanceof RestoreVerifyFailedError) {
        setTesting(false);
        if (confirm(`This backup's last verification FAILED\n\n${e.message}\n\nA test copy touches nothing that exists, but it may not come up, or may come up holding damaged data. Bring it up anyway?`)) {
          setAcks((a) => ({ ...a, unverified: true }));
          return testRestore(true);
        }
        return;
      }
      setRErr(`Couldn't start the test restore: ${(e as Error).message}`);
    } finally { setTesting(false); }
  };

  const removeClone = async (c: TestClone) => {
    if (!confirm(`Remove the test clone "${c.name}" now, with the volumes created for it?\n\nIt would go by itself at its expiry. Nothing else is affected.`)) return;
    setCloneBusy(c.id);
    try { await api.removeTestClone(c.node_id, c.id); setClones((p) => p.filter((x) => x.id !== c.id)); }
    catch (e) { setRErr(`Couldn't remove the clone: ${(e as Error).message}`); }
    finally { setCloneBusy(""); }
  };

  // Both refs hold an UNSUBSCRIBE function now, not a connection: every console
  // shares one stream (lib/logStream), so "stop following" is what closing was.
  const stopRef = useRef<(() => void) | null>(null);
  useEffect(() => () => stopRef.current?.(), []);
  useEffect(() => () => vrEsRef.current?.(), []);

  // "Send offsite now": re-run only the mirror step for this already-stored local
  // backup (no re-capture). Streams the engine's mirror log and refreshes when done.
  const mirrorNow = async () => {
    setMState("running"); setMMsg(""); setMLines([]);
    const start = Date.now();
    const stop = subscribeLines((l) => {
      if (l.backup_id !== b.id) return;
      if (new Date(l.time).getTime() < start - 2000) return; // skip replayed history
      if (!/mirror|mirroring|offsite|destination|no progress|degraded/i.test(l.msg)) return;
      setMLines((p) => [...p.slice(-60), { level: l.level, msg: l.msg }]);
      if (/mirror complete|mirror finished/i.test(l.msg)) { setMState("done"); setMMsg(l.msg); stopRef.current?.(); onChanged(); }
      else if (/mirror failed|already have a copy|nothing to mirror/i.test(l.msg)) { setMState(/already have a copy|nothing to mirror/i.test(l.msg) ? "done" : "failed"); setMMsg(l.msg); stopRef.current?.(); onChanged(); }
    });
    stopRef.current = stop;
    try { await api.mirrorBackup(b.id, [...mSel]); }
    catch (err) { setMState("failed"); setMMsg((err as Error).message); stop(); }
  };
  // Enabled destinations that don't already hold a good copy of this backup — i.e.
  // the candidates "Send offsite now" can upload to. Split failed-before vs new so
  // the checkbox list can label each.
  // A deferred copy holds no data either, so it's still a valid "Send offsite now"
  // target — treat only a genuinely-good copy as already-held (F14).
  const goodDestIds = new Set((locations(b) || []).filter((l) => l.kind === "dest" && !noDataLoc(l)).map((l) => l.dest_id));
  const failedDestIds = new Set((locations(b) || []).filter((l) => l.kind === "dest" && l.status === "failed").map((l) => l.dest_id));
  const mirrorTargets = dests.filter((d) => d.enabled && !goodDestIds.has(d.id));
  const toggleMSel = (id: string) => setMSel((prev) => { const n = new Set(prev); n.has(id) ? n.delete(id) : n.add(id); return n; });
  // Default every pending destination to ticked when the list loads / backup changes.
  useEffect(() => {
    const good = new Set((locations(b) || []).filter((l) => l.kind === "dest" && !noDataLoc(l)).map((l) => l.dest_id));
    setMSel(new Set(dests.filter((d) => d.enabled && !good.has(d.id)).map((d) => d.id)));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [dests, b.id]);

  // Live progress for a RUNNING backup, viewable from anywhere (Backups → click
  // the running backup). The log stream replays recent history on connect, so
  // you immediately see what's happened so far, then live lines. Backups run
  // server-side — opening/closing this never affects the backup.
  const [bLines, setBLines] = useState<{ time: string; level: string; msg: string }[]>([]);
  const bLog = useStickyScroll(bLines.length); // running-backup live log follows the tail
  useEffect(() => {
    if (b.status !== "running") return;
    setBLines([]);
    return subscribeLines((l) => {
      if (l.backup_id !== b.id) return;
      setBLines((p) => [...p.slice(-400), { time: l.time, level: l.level, msg: l.msg }]);
      if (/Verification (PASSED|FAILED)|Backup stored|DEGRADED/i.test(l.msg)) onChanged();
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [b.id, b.status]);

  // Versions of this same container (newest first), so the user can pick which
  // point-in-time to restore from without leaving the drawer.
  const versions = useMemo(
    () => [...siblings].sort((a, c) => c.created_at - a.created_at),
    [siblings]
  );
  // The copies this version physically lives in — the user can pick which to
  // read from (local is fastest; offsite survives local loss). "failed" and
  // "deferred" copies hold no data — not selectable as a source (F14).
  const allCopies = locations(b);
  const copies = allCopies.filter((l) => !noDataLoc(l));
  const failed = allCopies.filter((l) => l.status === "failed");
  const deferred = allCopies.filter((l) => l.status === "deferred");
  // Reset the source choice when switching versions (a copy may not exist there).
  useEffect(() => { setSource(""); }, [b.id]);
  // F218: a verdict belongs to the backup it was about — switching rows in the
  // drawer must not leave the previous one's refusal on screen.
  useEffect(() => { setVrErr(""); setVrLines([]); setAcks({ incompatible: false, unverified: false }); }, [b.id]);

  // Stop the running restore. The run id is what the engine logs under: the
  // backup id for a single restore, "stack:<project>" for a stack restore.
  const cancelRestore = async () => {
    const runId = rMode === "stack" ? `stack:${b.stack}` : b.id;
    if (!confirm(
      `Cancel this restore?\n\n` +
      `DockBack stops at the next safe point and does NOT roll anything back automatically. ` +
      `Data already written stays written — the log will name the pre-restore snapshot (if one was taken) so you can undo it deliberately.`
    )) return;
    setCanceling(true);
    try { await api.cancelRestore(runId); }
    catch (e) { setRErr(`Couldn't cancel: ${(e as Error).message}`); }
    finally { setCanceling(false); }
  };

  // F86: the offline private key for a WRITE-ONLY backup. Component state only —
  // it is sent with the one restore request and never stored, cached or logged.
  const writeOnly = !!man?.wrapped_key_pub;
  // F208: a standalone named-volume backup (F23) — "volume:<name>". It restores by
  // recreating the volume and extracting over its contents; there is no container
  // to recreate, revert, or start, and no "copy" to bring up beside it. The drawer
  // used to offer all of those and the server quietly ignored them.
  const isVolume = b.target_name.startsWith("volume:");
  const volumeName = isVolume ? b.target_name.slice("volume:".length) : "";
  // F208: the drawer keeps its state when you click a different row, so a
  // "Restore as a copy" tick left over from a container backup would follow you
  // onto a volume backup — where the control is hidden but the request would
  // still be sent, and the server now refuses it. Cleared with the selection.
  useEffect(() => { if (isVolume) setAsCopy(false); }, [b.id, isVolume]);

  // runRestore performs the restore, streaming live progress. On a 409 from the
  // extension/engine compatibility gate (F10) it stops, surfaces the specific
  // warning, and — only if the user accepts — retries once with the override.
  const runRestore = async (confirmIncompatible: boolean, stepUp?: { password: string; code: string }, confirmUnverified?: boolean) => {
    const targetId = man?.container_id || "";
    setRMode("one"); setRState("running"); setRLines([]); setRErr("");

    // Stream the engine's restore logs for THIS backup; show live progress and
    // a terminal success/failure result.
    const start = Date.now();
    // #N10: the structured verdict and the fallback line rules both live in
    // followRun now, so this console and the three others cannot disagree.
    stopRef.current = followRun(b.id, {
      mode: "one",
      since: start - 2000, // skip replayed history
      filter: isRestoreProgressLine,
      onLine: (l) => setRLines((p) => [...p.slice(-60), { level: l.level, msg: l.msg }]),
      onDone: (outcome, message) => {
        if (outcome === "ok") setRState("done");
        else if (outcome === "canceled") { setRState("canceled"); setRErr(message); }
        else { setRState("failed"); setRErr(message); }
        onChanged(); onRestoresChanged();
      },
    });
    try {
      const clone = asCopy && asName.trim() ? { as_name: asName.trim(), isolated: true } : {};
      const hostRebuild = !asCopy && reconstructHost ? { reconstruct_host: true, ...(hostBaseDir.trim() ? { host_base_dir: hostBaseDir.trim() } : {}) } : {};
      const restartPromotion = !asCopy && promoteRestart ? { promote_restart_policy: true } : {};
      const probeInjection = !asCopy && injectHealthchecks ? { inject_healthchecks: true } : {};
      const ipRemap = !asCopy && remapIP ? { remap_ip: true, ...(remapFrom.trim() ? { remap_from_ip: remapFrom.trim() } : {}), ...(remapTo.trim() ? { remap_to_ip: remapTo.trim() } : {}) } : {};
      const domainRemap = !asCopy && remapDomain && domainFrom.trim() && domainTo.trim()
        ? { remap_domain: true, remap_from_domain: domainFrom.trim(), remap_to_domain: domainTo.trim() } : {};
      const pathRemap = !asCopy && remapPath ? { remap_path: true, ...(pathFrom.trim() ? { remap_from_path: pathFrom.trim() } : {}), ...(pathTo.trim() ? { remap_to_path: pathTo.trim() } : {}) } : {};
      // F114: never sent for a clone — an isolated copy is a dry run on a
      // throwaway name, so pointing it at the real address would be wrong.
      const siteAddr = !asCopy && newSiteAddress.trim() ? { new_site_address: newSiteAddress.trim() } : {};
      // F160: a DEPENDENCY's new address. Sent for a clone too — unlike the app's
      // own address, this points the copy at something it only reads from, and a
      // dry run that cannot reach its data source proves nothing.
      const upAddr = newUpstreamAddress.trim() ? { new_upstream_address: newUpstreamAddress.trim() } : {};
      await api.restore(b.id, { node_id: restoreNode, target_id: targetId, volumes: true, database: true, confirm: true, snapshot, recreate, source, ...(confirmIncompatible || acks.incompatible ? { confirm_incompatible: true } : {}), ...(confirmUnverified || acks.unverified ? { confirm_unverified: true } : {}), ...clone, ...hostRebuild, ...restartPromotion, ...probeInjection, ...ipRemap, ...domainRemap, ...pathRemap, ...siteAddr, ...upAddr, ...(privKey.trim() ? { private_key: privKey.trim() } : {}), ...(stepUp ? { password: stepUp.password, code: stepUp.code } : {}) });
      setRestoreStepUp(null); // F206: satisfied — clear the prompt
      onRestoresChanged(); // F100: the run is now registered — reachable after a reload
    } catch (e) {
      stopRef.current?.();
      if (e instanceof RestoreCompatError) {
        setRState("idle"); setRLines([]);
        if (confirm(`Restore compatibility warning\n\n${e.compat_warning}\n\nRestoring anyway can corrupt or fail. Continue?`)) {
          setAcks((a) => ({ ...a, incompatible: true }));
          return runRestore(true, stepUp, confirmUnverified);
        }
        return;
      }
      // F218: the archive itself is known bad — its last verification failed.
      // Overriding is the operator's to do (a damaged copy can still beat
      // nothing), but the confirm has to say what it is agreeing to.
      if (e instanceof RestoreVerifyFailedError) {
        setRState("idle"); setRLines([]);
        if (confirm(`This backup's last verification FAILED\n\n${e.message}\n\nRestoring it can put CORRUPT data back. Restore anyway?`)) {
          setAcks((a) => ({ ...a, unverified: true }));
          return runRestore(confirmIncompatible, stepUp, true);
        }
        return;
      }
      // F206: overwriting a PROTECTED container asks for the password again.
      // Handled as a prompt, not a failure — the operator is entitled to do
      // this, they just have to prove who they are first. The confirm dialog has
      // already been accepted at this point, so re-submitting after the password
      // does not ask them to confirm the overwrite twice.
      if (e instanceof StepUpError) {
        setRState("idle"); setRLines([]);
        setRestoreStepUp({ totp: e.totp_required, err: stepUp ? e.message : "" });
        return;
      }
      setRState("failed"); setRErr((e as Error).message);
    }
  };

  // F218 — "Verify & restore": scrub the archive, and go straight into the
  // restore the operator already confirmed if it passes.
  //
  // The dead end this replaces: the Restore button was disabled for anything not
  // verified, with no explanation and no path out except noticing "Verify now",
  // pressing it, waiting without feedback, and coming back. Three round trips to
  // learn something the app could have chained.
  //
  // The verdict is read from the ROW, not from the log line, because the scrub
  // runs server-side in a goroutine and a browser that connects to the stream a
  // moment late would miss its only terminal line and hang forever — in front of
  // a destructive action. The stream is used for the visible progress; the row's
  // last_verified_at moving is what decides.
  const verifyThenRestore = async () => {
    const before = b.last_verified_at || 0;
    setVrState("verifying"); setVrLines([]); setVrErr("");

    const start = Date.now();
    vrEsRef.current?.();
    const unsubscribe = subscribeLines((l) => {
      if (l.backup_id !== b.id) return;
      if (new Date(l.time).getTime() < start - 2000) return; // skip replayed history
      if (!/scrub|verif|integrity|re-read|checksum/i.test(l.msg)) return;
      setVrLines((p) => [...p.slice(-40), { level: l.level, msg: l.msg }]);
    });
    vrEsRef.current = unsubscribe;
    const stop = () => { unsubscribe(); if (vrEsRef.current === unsubscribe) vrEsRef.current = null; };

    try {
      await api.verifyBackup(b.id);
    } catch (e) {
      stop(); setVrState("idle"); setVrErr(`Couldn't start verification: ${(e as Error).message}`);
      return;
    }

    // The server gives the scrub 30 minutes; this waits the same, so a large
    // archive is not abandoned by the one side that can still act on it.
    const deadline = Date.now() + 30 * 60 * 1000;
    while (Date.now() < deadline) {
      await new Promise((r) => setTimeout(r, 2500));
      let fresh: Backup | null = null;
      try { fresh = await api.backup(b.id); } catch { continue; } // a blip is not a verdict
      if ((fresh.last_verified_at || 0) <= before) continue; // still running
      stop();
      setVrState("idle");
      onChanged(); // pull the fresh report into the drawer either way
      if (fresh.verified === "verified") return runRestore(false);
      // AC2: a failed verification does not restore. The report is in the
      // drawer's own Verification section, which the refresh above just updated.
      setVrErr("Verification FAILED — this backup did not re-read intact, so nothing was restored. The report below has the detail.");
      return;
    }
    stop(); setVrState("idle");
    setVrErr("Verification is still running after 30 minutes. Nothing was restored — check the verification report, then restore from here once it lands.");
  };

  // F218: the terms of the restore are frozen once a chain is under way. The
  // verify leg can run for minutes, and the operator confirmed a restore WITH a
  // safety snapshot — it must not be able to turn into one without.
  const optsLocked = rState === "running" || vrState === "verifying";
  // Only a successfully captured archive is restorable at all. A failed or
  // canceled run has no complete archive behind it, whatever its verify state.
  const restorable = b.status === "success";
  const needsVerifyFirst = restorable && b.verified !== "verified";
  // Written out rather than composed, so a translation is not asked to survive a
  // mechanical lowercasing of its first letter.
  const restoreLabel = asCopy
    ? (needsVerifyFirst ? "Verify & restore as a copy" : "Restore as a copy")
    : recreate
      ? (b.stack
        ? (needsVerifyFirst ? "Verify & revert this service only" : "Revert this service only")
        : (needsVerifyFirst ? "Verify & revert update" : "Revert update"))
      : (b.stack
        ? (needsVerifyFirst ? "Verify & restore this service only" : "Restore this service only")
        : (needsVerifyFirst ? "Verify & restore" : "Restore"));

  // verifyFirst (F218): the backup is not verified, so the ONE confirm the
  // operator gives covers both steps — verify, and if it passes, restore. The
  // confirm text is the same destructive warning either way; only the extra
  // sentence differs, so what they agree to never depends on which button it was.
  const restore = async (verifyFirst = false) => {
    const targetNodeName = nodes.find((n) => n.id === restoreNode)?.name || restoreNode;
    const crossHost = restoreNode !== b.node_id;
    const verifyNote = verifyFirst
      ? "\n\nIt will be verified first. If verification fails, NOTHING is restored."
      : "";
    // Restore as a copy (F10): creates a new, isolated container; the original is
    // untouched, so the confirm text is non-destructive and there's no snapshot.
    if (asCopy) {
      const nm = asName.trim();
      if (!nm) { setRErr("Enter a name for the copy."); setRState("failed"); return; }
      if (nm === b.target_name) { setRErr("The copy's name must differ from the original."); setRState("failed"); return; }
      if (!confirm(`RESTORE AS A COPY — create a new, isolated container "${nm}" from this backup on node ${targetNodeName}, with no published ports and on a throwaway network. This creates a copy; the running ${b.target_name} is untouched.${verifyNote} Continue?`)) return;
      return verifyFirst ? verifyThenRestore() : runRestore(false);
    }
    // F208: a standalone volume has no container to recreate or start, and the
    // extract writes OVER what is there rather than replacing the volume, so the
    // container wording would promise the wrong thing twice.
    const head = isVolume
      ? `RESTORE is destructive. It will write this backup's contents into the named volume "${volumeName}" on node ${targetNodeName}, overwriting every file the backup contains. Files added since the backup that are not in it are left in place.`
      : recreate
      ? `REVERT UPDATE — recreate ${b.target_name} from this backup's saved image${man?.image_digest ? " (digest-pinned)" : ""} and restore its data onto node ${targetNodeName}${crossHost ? " (CROSS-HOST)" : ""}, rolling a broken upgrade back to this version.`
      : `RESTORE is destructive. It will restore ${b.target_name}'s volumes/database from this backup onto node ${targetNodeName}${crossHost ? " (CROSS-HOST — recreating it there)" : ""} and start it.`;
    if (!confirm(`${head}${snapshot ? "\n\nA safety snapshot of the current state will be taken first, so you can roll back." : "\n\nNo safety snapshot will be taken — this overwrite is NOT reversible."}${verifyNote} Continue?`)) return;
    return verifyFirst ? verifyThenRestore() : runRestore(false);
  };

  // F100: re-attach to a restore that is ALREADY running — one this page did not
  // start, because the page was reloaded (or opened elsewhere) while it ran.
  //
  // Two differences from following a restore we launched ourselves:
  //  * the persisted run log (F8) is loaded first, so the panel opens showing
  //    what has happened so far rather than an empty box waiting for the next
  //    line — which after a reload could be minutes away;
  //  * there is no "skip replayed history" cutoff, for the same reason.
  const attachToRun = useCallback((runId: string, mode: "one" | "stack") => {
    stopRef.current?.();
    setRMode(mode); setRState("running"); setRErr("");

    api.runLog(runId)
      .then((r) => setRLines((r.lines || []).slice(-80).map((l) => ({ level: l.level, msg: l.msg }))))
      .catch(() => { /* no persisted log yet — the stream fills it in */ });

    // No `since` here, deliberately: this console attaches to a run already in
    // flight and wants the replayed history the stream sends on connect.
    stopRef.current = followRun(runId, {
      mode,
      onLine: (l) => setRLines((p) => [...p.slice(-80), { level: l.level, msg: l.msg }]),
      onDone: (outcome, message) => {
        if (outcome === "ok") setRState("done");
        else if (outcome === "canceled") { setRState("canceled"); setRErr(message); }
        else { setRState("failed"); setRErr(message); }
        onChanged(); onRestoresChanged();
      },
    });
  }, [onChanged, onRestoresChanged]);

  // Attach exactly once per run, and only when this drawer is not already
  // following something — a restore started in THIS session owns the panel and
  // must not be reset under it.
  const attached = useRef("");
  useEffect(() => {
    if (rState !== "idle") return;
    const stackRun = b.stack ? `stack:${b.stack}` : "";
    const hit = runningRestores.find((r) => r.id === b.id) ||
      (stackRun ? runningRestores.find((r) => r.id === stackRun) : undefined);
    if (!hit || attached.current === hit.id) return;
    attached.current = hit.id;
    attachToRun(hit.id, hit.id === b.id ? "one" : "stack");
  }, [runningRestores, b.id, b.stack, rState, attachToRun]);

  // F224: escalating from one backup to the whole stack opens the stack's own
  // RESTORE PAGE, carrying the choices already made here so nothing is
  // configured twice — out-of-band rather than in the URL, and deliberately
  // without the offline private key: handing key material to a storage API to
  // save one paste is the wrong trade, and that page asks for it where it is
  // used. See lib/stackRestoreSeed.
  const navigate = useNavigate();
  const openStackRestore = () => {
    if (!b.stack) return;
    writeStackRestoreSeed({
      nodeID: b.node_id, project: b.stack,
      group: safe(b.manifest_json)?.consistency_group || undefined,
      targetNode: restoreNode, recreate, snapshot,
      reconstructHost, hostBaseDir: hostBaseDir.trim() || undefined,
      remapIP, remapFrom: remapFrom.trim(), remapTo: remapTo.trim(),
      remapPath, pathFrom: pathFrom.trim(), pathTo: pathTo.trim(),
      // F214: the drawer was already reading from a chosen copy — carry it
      // rather than silently reverting the whole stack to auto.
      source: source || undefined,
      // F215: the choices already made HERE. They were silently dropped on
      // escalation — the operator typed a domain and a new address, clicked
      // "Restore stack", and found both fields empty again.
      remapDomain,
      domainFrom: domainFrom.trim() || undefined,
      domainTo: domainTo.trim() || undefined,
      newSiteAddress: newSiteAddress.trim() || undefined,
      newUpstreamAddress: newUpstreamAddress.trim() || undefined,
    });
    navigate(`/servers/${b.node_id}/stacks/${encodeURIComponent(b.stack)}/restore`);
  };


  return (
    <div className="fixed inset-0 z-40 flex justify-end bg-black/50" onClick={onClose}>
      <div className="h-full w-full max-w-xl overflow-y-auto border-l border-outline-variant bg-surface-container p-5" onClick={(e) => e.stopPropagation()}>
        <div className="mb-4 flex items-start justify-between">
          <div>
            <h2 className="text-xl font-bold">{b.target_name}</h2>
            <div className="text-xs text-on-surface-variant">on <span className="font-semibold text-on-surface">{nodeName}</span></div>
            <div className="mt-1 font-mono text-xs text-on-surface-variant">{b.id}</div>
          </div>
          <button onClick={onClose} className="text-on-surface-variant hover:text-on-surface"><X size={20} /></button>
        </div>

        <div className="mb-4 flex flex-wrap gap-2">{statusChip(b.status)}{verifyChip(b.verified)}<Chip kind="muted">{fmtBytes(b.size_bytes)}</Chip>{uncoveredSkips.length > 0 && <Chip kind="warn">Partial · {uncoveredSkips.length} not captured</Chip>}{coveredSkips.length > 0 && <span title="Shared folder(s) captured once via another container's backups — not data loss."><Chip kind="ok">{coveredSkips.length} captured via {coveredSkips[0].covered_by}</Chip></span>}{(b.chain_dependents || 0) > 0 && <span title={`${b.chain_dependents} newer incremental backup(s) build on this one — deleting it offers a whole-chain delete instead of orphaning them.`}><Chip kind="muted"><Layers size={12} /> baseline of {b.chain_dependents} delta{b.chain_dependents === 1 ? "" : "s"}</Chip></span>}{pinned && <Chip kind="ok"><Pin size={12} /> Pinned</Chip>}{man?.has_original_compose && <span title="The genuine host compose file(s) were captured from the source host — find them under config/original-compose/ when browsing this backup's files, alongside the stack's .env, and prefer them over the reconstruction."><Chip kind="ok"><FileText size={12} /> original compose included</Chip></span>}</div>
        {b.error && <div className="mb-4 rounded bg-error/10 px-3 py-2 text-sm text-error">{b.error}</div>}

        {/* F50: restore-confidence grade + what would raise it. */}
        {b.confidence && (
          <div className="mb-4 rounded border border-outline-variant/60 bg-surface-lowest p-3">
            <div className="mb-1.5 flex items-center gap-2">
              <span className={`inline-grid h-6 min-w-[1.5rem] place-items-center rounded px-1.5 text-sm font-bold ${gradeTone(b.confidence.grade)}`}>{b.confidence.grade}</span>
              <span className="font-medium text-on-surface">Restore confidence</span>
            </div>
            {(b.confidence.reasons || []).length === 0 ? (
              <p className="text-xs text-on-surface-variant">All checks passed — verified, drill-proven, an offsite copy, and complete.</p>
            ) : (
              <>
                <p className="mb-1 text-xs font-medium text-on-surface-variant">What's missing:</p>
                <ul className="flex flex-col gap-1.5 text-xs">
                  {(b.confidence.reasons || []).map((rz, i) => (
                    <li key={i} className="flex flex-wrap items-center gap-2">
                      <span className="text-on-surface-variant">• {rz}</span>
                      {/never drilled|drill failed/i.test(rz) && (
                        <button onClick={drillNow} disabled={drilling} className="flex items-center gap-1 rounded border border-outline-variant px-1.5 py-0.5 text-on-surface-variant hover:text-on-surface disabled:opacity-50">
                          {drilling ? <Loader2 size={12} className="animate-spin" /> : <Play size={12} />} Run drill
                        </button>
                      )}
                      {/offsite/i.test(rz) && mirrorTargets.length > 0 && (
                        <button onClick={mirrorNow} disabled={mState === "running"} className="flex items-center gap-1 rounded border border-outline-variant px-1.5 py-0.5 text-on-surface-variant hover:text-on-surface disabled:opacity-50">
                          <BackupCloudIcon size={12} active={mState === "running"} /> Send offsite
                        </button>
                      )}
                      {/verif|verified/i.test(rz) && (
                        <button onClick={verifyNow} disabled={verifying} className="flex items-center gap-1 rounded border border-outline-variant px-1.5 py-0.5 text-on-surface-variant hover:text-on-surface disabled:opacity-50">
                          {verifying ? <Loader2 size={12} className="animate-spin" /> : <ShieldCheck size={12} />} Verify now
                        </button>
                      )}
                    </li>
                  ))}
                </ul>
              </>
            )}
          </div>
        )}

        {b.status === "success" && (
          <div className="mb-4 rounded border border-outline-variant/60 bg-surface-lowest p-3">
            <label className="flex cursor-pointer items-center gap-2 text-sm font-medium">
              <input type="checkbox" checked={pinned} disabled={pinBusy} onChange={togglePinD} />
              <Pin size={14} className={pinned ? "text-primary" : "text-on-surface-variant"} /> Pin (keep forever)
            </label>
            <p className="mb-2 mt-1 text-xs text-on-surface-variant">Pinned — exempt from pruning: this backup is never removed by retention (scheduled prune, auto-prune, or “Prune now”) until you unpin it.</p>
            <Label>Label</Label>
            <div className="flex items-center gap-2">
              <input
                value={label} onChange={(e) => setLabel(e.target.value)} maxLength={200}
                onKeyDown={(e) => { if (e.key === "Enter") saveLabel(); }}
                placeholder="e.g. pre-Immich-upgrade"
                className="w-full rounded border border-outline-variant bg-surface-lowest px-3 py-1.5 text-sm text-on-surface outline-none focus:border-docker-blue" />
              <Button variant="secondary" size="sm" disabled={labelSaving || label.trim() === (b.label || "")} onClick={saveLabel}>
                {labelSaving ? <Loader2 size={14} className="animate-spin" /> : <Tag size={14} />} Save
              </Button>
            </div>
            <p className="mt-1 text-xs text-on-surface-variant">Shown on the backup row and searchable in the Backups search box.</p>
          </div>
        )}

        {b.status === "running" && (
          <Section title="Live progress">
            <div ref={bLog.ref} onScroll={bLog.onScroll} className="h-72 overflow-y-auto rounded border border-outline-variant/50 bg-surface-lowest p-3 font-mono text-xs leading-relaxed">
              {bLines.length === 0 && <div className="text-on-surface-variant">Connecting to live output…</div>}
              {bLines.map((l, i) => (
                <div key={i} className="flex gap-2 py-0.5">
                  <span className="shrink-0 text-on-surface-variant">{(l.time.split("T")[1] || "").replace("Z", "")}</span>
                  <span className={`shrink-0 font-semibold ${l.level === "ERR" ? "text-error" : l.level === "WARN" ? "text-warning" : "text-secondary"}`}>{l.level}</span>
                  <span className="text-on-surface">{l.msg}</span>
                </div>
              ))}
            </div>
            <p className="mt-2 text-xs text-on-surface-variant">Backups run in the background — you can close this or navigate away without interrupting it.</p>
          </Section>
        )}

        {b.status !== "running" && (
          <Section title="Restore from">
            {versions.length > 1 && (
              <div className="mb-4">
                <RestoreTimeline backups={versions} drills={drills} selectedId={b.id} onPick={onPick} />
              </div>
            )}
            {writeOnly && (
              <div className="mb-4 rounded border border-primary/30 bg-primary/[0.06] p-3">
                <div className="mb-1 flex items-center gap-2 text-sm font-semibold text-primary">
                  <Lock size={14} /> Offline private key
                </div>
                <p className="mb-2 text-xs text-on-surface-variant">
                  This backup is <b className="text-on-surface">write-only encrypted</b>: DockBack sealed it to a public key
                  and cannot open it itself. Paste the private key from its recovery sheet
                  {man?.backup_pub_fp ? <> (keypair <span className="font-mono">{man.backup_pub_fp}</span>)</> : null}.
                  It is used for this restore only — never saved, never logged.
                </p>
                <textarea
                  value={privKey}
                  onChange={(e) => onPrivKeyChange(e.target.value)}
                  rows={2}
                  spellCheck={false}
                  autoComplete="off"
                  placeholder="base64 private key"
                  aria-label="Offline private key"
                  className="w-full break-all rounded border border-outline-variant bg-surface-lowest px-3 py-2 font-mono text-xs text-on-surface focus:outline-none focus:ring-1 focus:ring-primary"
                />
                {/* F204: check the key before you need it. A restore is the worst
                    moment to discover a recovery sheet is the wrong one. */}
                <div className="mt-2 flex flex-wrap items-center gap-2">
                  <Button variant="secondary" className="h-8 px-2.5 py-0 text-xs" disabled={keyChecking || !privKey.trim()} onClick={() => verifyKey()}>
                    {keyChecking ? <Loader2 size={13} className="animate-spin" /> : <ShieldCheck size={13} />} Verify this key
                  </Button>
                  <span className="min-w-0 flex-1 text-[11px] text-outline">Unwraps this backup&rsquo;s key and decrypts one frame &mdash; seconds, no restore, nothing changed.</span>
                </div>
                {keyCheck && (
                  <div className={`mt-2 flex items-start gap-2 rounded px-2.5 py-2 text-xs ${keyCheck.ok ? "bg-success/10 text-success" : "bg-error/10 text-error"}`} role="status">
                    {keyCheck.ok ? <CheckCircle2 size={14} className="mt-0.5 shrink-0" /> : <AlertTriangle size={14} className="mt-0.5 shrink-0" />}
                    <span className="min-w-0 break-words">
                      {keyCheck.message}
                      {keyCheck.fingerprint && <> <span className="font-mono opacity-80">({keyCheck.fingerprint})</span></>}
                    </span>
                  </div>
                )}
                {keyStepUp && (
                  <div className="mt-2 max-w-md">
                    <StepUpPrompt totp={keyStepUp.totp} busy={keyChecking} error={keyStepUp.err} confirmLabel="Verify key"
                      onConfirm={(pw, code) => verifyKey(pw, code)} />
                  </div>
                )}
                <p className="mt-2 text-[11px] text-outline">Required to restore this backup.</p>
              </div>
            )}
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <div>
                <Label>Version (point in time)</Label>
                <Select value={b.id} onChange={(e) => { const v = versions.find((x) => x.id === e.target.value); if (v) onPick(v); }}>
                  {versions.length === 0 && <option value={b.id}>{new Date(b.created_at * 1000).toLocaleString()}</option>}
                  {versions.map((v, i) => (
                    <option key={v.id} value={v.id}>
                      {new Date(v.created_at * 1000).toLocaleString()} · {fmtBytes(v.size_bytes)}{i === 0 ? " (latest)" : ""}
                    </option>
                  ))}
                </Select>
              </div>
              <div>
                <Label>Source copy</Label>
                <Select value={source} onChange={(e) => setSource(e.target.value)}>
                  <option value="">Auto — fastest available (local first)</option>
                  {copies.map((l, i) => (
                    <option key={i} value={l.kind === "local" ? "local" : (l.dest_id || "")}>
                      {l.kind === "local" ? "Local (fastest)" : `${l.name} (${l.type})`}
                    </option>
                  ))}
                </Select>
              </div>
            </div>
            {/* F95: the image that will actually run has moved on. A recreate
                replays this backup's OLD configuration into it, which is exactly
                how a container comes back and dies at boot. */}
            {imageDrift.length > 0 && (
              <div className="mb-3 rounded bg-warning/10 px-3 py-2 text-xs text-warning">
                <div className="flex items-center gap-2 font-medium">
                  <AlertTriangle size={14} className="shrink-0" /> Image changed since this backup — may need new configuration
                </div>
                <ul className="mt-1 space-y-0.5 pl-6">
                  {imageDrift.map((w, i) => <li key={i} className="break-words">• {w}</li>)}
                </ul>
              </div>
            )}
            {/* F110: the application's own preconditions. Distinct from the host
                portability list below — that asks whether this machine can run
                the container, this asks whether the app's data will still mean
                anything once it does. Shown for same-host restores too, because
                an edited compose file moves a mount just as easily as a new
                machine does. Advisory: only the local-disk requirement is
                enforced, and only when the target's filesystem was measured. */}
            {appPre && (appPre.notes?.length || appPre.data_paths?.length) ? (
              <div className={`mb-3 rounded px-3 py-2 text-xs text-on-surface ${appPre.blocking ? "bg-warning/10" : "bg-primary/10"}`}>
                <div className={`flex items-center gap-2 font-medium ${appPre.blocking ? "text-warning" : "text-primary"}`}>
                  {appPre.blocking ? <AlertTriangle size={14} className="shrink-0" /> : <Info size={14} className="shrink-0" />}
                  <span className="break-words">
                    {/* F114: a blocking precondition means the app will be
                        UNREACHABLE if it's ignored, not merely imperfect — the
                        heading says which, because the two deserve different
                        amounts of the operator's attention. */}
                    {appPre.blocking
                      ? `${appPre.app} — read this, or it may not be reachable after the restore`
                      : `${appPre.app} — check these before restoring`}
                  </span>
                </div>
                {(appPre.notes || []).length > 0 && (
                  <ul className="mt-1 space-y-0.5 pl-6">
                    {(appPre.notes || []).map((n, i) => <li key={i} className="break-words">• {n}</li>)}
                  </ul>
                )}
                {(appPre.data_paths || []).length > 0 && (
                  <div className="mt-2 pl-6">
                    <div className="text-on-surface-variant">Mount the data at these container paths on the target:</div>
                    <div className="mt-1 flex flex-wrap gap-1">
                      {(appPre.data_paths || []).map((p, i) => (
                        <code key={i} className="break-all rounded bg-surface-variant px-1.5 py-0.5 font-mono text-[11px]">{p}</code>
                      ))}
                    </div>
                  </div>
                )}
                {/* F160: the other direction — the address of something this
                    application reads from. Offered separately from the field
                    below because the two are answered at opposite moments: this
                    one when the DEPENDENCY moved and the app did not. */}
                {appPre.upstream_prompt && (
                  <div className="mt-2 pl-6">
                    <Label>{appPre.upstream_prompt}</Label>
                    <input
                      className="mt-1 w-full rounded border border-outline bg-surface px-2 py-1 font-mono text-xs text-on-surface placeholder:font-sans placeholder:text-on-surface-variant"
                      value={newUpstreamAddress}
                      onChange={(ev) => setNewUpstreamAddress(ev.target.value)}
                      placeholder="http://10.168.1.50:32400 — leave blank if it has not moved"
                      spellCheck={false}
                      autoCapitalize="off"
                      autoCorrect="off"
                    />
                    <p className="mt-1 break-words text-on-surface-variant">
                      Only the recorded address changes. Nothing is re-authenticated — the stored credential is bound to that
                      service's identity, not to where it lives, so no re-linking or setup is needed.
                    </p>
                  </div>
                )}
                {/* F114: offered only for an app that records its own address
                    somewhere. Blank — the default — changes nothing at all. */}
                {appPre.address_prompt && (
                  <div className="mt-2 pl-6">
                    <Label>{appPre.address_prompt}</Label>
                    <input
                      className="mt-1 w-full rounded border border-outline bg-surface px-2 py-1 font-mono text-xs text-on-surface placeholder:font-sans placeholder:text-on-surface-variant"
                      value={newSiteAddress}
                      onChange={(ev) => setNewSiteAddress(ev.target.value)}
                      placeholder="https://cloud.example.com — leave blank to keep the current address"
                      spellCheck={false}
                      autoCapitalize="off"
                      autoCorrect="off"
                    />
                    <p className="mt-1 break-words text-on-surface-variant">
                      Only fill this in if the address is genuinely changing. Re-pointing DNS or your reverse proxy at the
                      new host keeps the same address, and needs nothing here. Settings DockBack cannot undo are printed
                      for you to run, never applied automatically.
                    </p>
                  </div>
                )}
              </div>
            ) : null}
            {/* F119: this restore writes into directories another container also
                uses. Not a block — restoring one half of a pair is legitimate —
                but it should be a decision rather than a surprise. */}
            {sharedData.length > 0 && (
              <div className="mb-3 rounded bg-warning/10 px-3 py-2 text-xs text-warning">
                <div className="flex items-center gap-2 font-medium">
                  <AlertTriangle size={14} className="shrink-0" /> Other containers use this data too
                </div>
                <ul className="mt-1 space-y-0.5 pl-6">
                  {sharedData.map((w, i) => <li key={i} className="break-words">• {w}</li>)}
                </ul>
              </div>
            )}
            {/* F174: restoring a database from its dump empties the data
                directory so the engine re-initializes — and an engine refuses to
                initialize an empty directory with no root password in its
                environment. A container whose directory was initialized long ago
                runs fine without one, so this only ever surfaces at the moment of
                a restore. Said here, where it costs one environment variable,
                rather than after the directory is gone. */}
            {restoreBlock && (
              <div className="mb-3 rounded bg-error/10 px-3 py-2 text-xs text-error">
                <div className="flex items-center gap-2 font-medium">
                  <AlertTriangle size={14} className="shrink-0" /> This restore will be refused
                </div>
                <p className="mt-1 break-words pl-6">{restoreBlock}</p>
                <div className="mt-1 pl-6 text-on-surface-variant">
                  Any value works: it sets the root password on the re-initialized engine, and your application does not use it —
                  its own user and password are recreated from the container's environment, unchanged. Nothing has been touched,
                  and nothing will be until you add the variable and restore again.
                </div>
              </div>
            )}
            {/* F177: this container records where it lives in its own
                environment, and it is about to move. Shown before the restore,
                because the new address can be supplied in this same panel —
                after it, the same thing is a line in a compose file. */}
            {addressVars.length > 0 && (
              <div className="mb-3 rounded bg-warning/10 px-3 py-2 text-xs text-warning">
                <div className="flex items-center gap-2 font-medium">
                  <AlertTriangle size={14} className="shrink-0" /> This container records its own address
                </div>
                <ul className="mt-1 space-y-0.5 pl-6">
                  {addressVars.map((v, i) => <li key={i} className="break-all font-mono">• {v}</li>)}
                </ul>
                <div className="mt-1 pl-6 text-on-surface-variant">
                  Restored as captured, so they still name the machine this backup came from. If the address changes, set it above where
                  the app offers it, or in the compose file afterwards.
                </div>
              </div>
            )}
            {/* F143: the TLS certificates inside this archive. Shown before the
                restore because the one thing a faithful restore cannot fix is a
                certificate that expired while the backup sat in storage — and
                finding that out from a browser is the expensive way. */}
            {certs.length > 0 && <CertificatePanel certs={certs} />}
            {/* F144: something on the target already holds a port this container
                publishes. Not a block — the holder is often exactly what this
                restore replaces — but it is the difference between a planned
                swap and a container that is created and then will not start. */}
            {portConflicts.length > 0 && (
              <div className="mb-3 rounded bg-warning/10 px-3 py-2 text-xs text-warning">
                <div className="flex items-center gap-2 font-medium">
                  <AlertTriangle size={14} className="shrink-0" /> Ports already in use on the target
                </div>
                <ul className="mt-1 space-y-0.5 pl-6">
                  {portConflicts.map((w, i) => <li key={i} className="break-words">• {w}</li>)}
                </ul>
                <div className="mt-1 pl-6 text-on-surface-variant">
                  Stop whatever is holding them first, or restore this as a copy on different ports to test it without contending for the live ones.
                </div>
              </div>
            )}
            {nodes.length > 1 && (
              <div className="mt-3">
                <Label>Restore to (node)</Label>
                <Select value={restoreNode} onChange={(e) => setRestoreNode(e.target.value)}>
                  {nodes.map((n) => <option key={n.id} value={n.id}>{n.name}{n.id === b.node_id ? " (original)" : ""}</option>)}
                </Select>
                {/* F94: what the chosen target cannot honor. Fetched only when the
                    node actually differs, so the common same-host restore costs
                    nothing. Advisory — it never blocks the restore. */}
                {restoreNode !== b.node_id && portability.length > 0 && (
                  <div className="mt-2 rounded bg-warning/10 px-3 py-2 text-xs text-warning">
                    <div className="flex items-center gap-2 font-medium">
                      <AlertTriangle size={14} className="shrink-0" /> Target host may not support this container
                    </div>
                    <ul className="mt-1 space-y-0.5 pl-6">
                      {portability.map((w, i) => <li key={i} className="break-words">• {w}</li>)}
                    </ul>
                    <div className="mt-1 pl-6 text-on-surface-variant">
                      You can still restore — this is a warning, not a block. The container may fail to start until the target provides these.
                    </div>
                  </div>
                )}
                {restoreNode !== b.node_id && (
                  isVolume ? (
                    /* F208: no image, no networks, no container — a volume restore
                       on another node just (re)creates the named volume there. */
                    <p className="mt-1 text-xs text-warning">Cross-host restore — the named volume <span className="font-mono">{volumeName}</span> is created on <b>{nodes.find((n) => n.id === restoreNode)?.name || restoreNode}</b> if it isn&rsquo;t there yet, and this backup&rsquo;s contents are written into it. A volume of that name already on the target is overwritten in place (snapshot-protected).</p>
                  ) : (
                  <p className="mt-1 text-xs text-warning">Cross-host restore — {b.target_name} will be recreated on <b>{nodes.find((n) => n.id === restoreNode)?.name || restoreNode}</b> (image re-pulled by digest, or loaded from the bundled tarball). Networks and volumes are recreated under their original names. If that stack already runs there, it is overwritten in place (snapshot-protected), not run in parallel.</p>
                  )
                )}
              </div>
            )}
            {/* man is null until the full-row fetch lands (list rows carry no
                manifest blob), and stays null for a row without a manifest —
                so every read here is optional. */}
            {man?.incremental && (
              (() => {
                const chainBroken = (ver?.checks || []).some((c: any) => c.name === "incremental-chain" && !c.ok);
                return chainBroken ? (
                  <p className="mt-2 flex items-start gap-2 rounded border border-error/30 bg-error/10 px-3 py-2 text-xs text-error">
                    <AlertTriangle size={14} className="mt-0.5 shrink-0" /> This backup needs its parent chain — a link is missing or failed verification. Restore the newest full baseline or a later generation instead.
                  </p>
                ) : (
                  <p className="mt-2 flex items-start gap-2 text-xs text-on-surface-variant">
                    <Layers size={14} className="mt-0.5 shrink-0 text-primary" /> Incremental backup (delta {man?.chain_depth || 0} since the full baseline). DockBack restores the full baseline plus every delta in the chain automatically — no extra steps.
                  </p>
                );
              })()
            )}
            <p className="mt-2 text-xs text-on-surface-variant">
              This backup has {copies.length} cop{copies.length === 1 ? "y" : "ies"}. Local is fastest; an offsite copy lets you recover even if the local disk is gone. A chosen copy that's unreachable or fails its integrity check automatically falls back to the others.
            </p>
            {immutableCopies(b).length > 0 && (
              <div className="mt-2 rounded bg-success/10 px-3 py-2 text-xs text-success">
                <div className="flex items-center gap-2 font-medium"><Lock size={14} className="shrink-0" /> Protected copy present</div>
                <ul className="mt-1 space-y-0.5 pl-6">
                  {immutableCopies(b).map((l, i) => (
                    <li key={i} className="break-words">
                      • <span className="font-medium">{l.name}</span> —{" "}
                      {isWormCopy(l)
                        ? <>WORM-locked{l.lock_until ? ` until ${new Date(l.lock_until * 1000).toLocaleDateString()}` : ""} (cannot be deleted by anyone, incl. this app, until then).</>
                        : <>read-only and exempt from pruning{l.lock_until ? ` until ${new Date(l.lock_until * 1000).toLocaleDateString()}` : ""}. A filesystem lock, not WORM — root on that machine can still remove it.</>}
                    </li>
                  ))}
                </ul>
              </div>
            )}
            {failed.length > 0 && (
              <div className="mt-2 rounded bg-warning/10 px-3 py-2 text-xs text-warning">
                <div className="flex items-center gap-2 font-medium"><AlertTriangle size={14} className="shrink-0" /> Degraded — not fully off-site (3-2-1)</div>
                <ul className="mt-1 space-y-0.5 pl-6">
                  {failed.map((l, i) => (
                    <li key={i}>• <span className="font-medium">{l.name}</span> ({l.type}) — {l.detail || "offsite upload failed"}</li>
                  ))}
                </ul>
                <div className="mt-1 pl-6 text-on-surface-variant">The local copy is verified and safe. Retry the offsite copy now (below) — no need to re-run the backup — or it retries on the next run.</div>
              </div>
            )}
            {deferred.length > 0 && (
              <div className="mt-2 rounded bg-secondary/10 px-3 py-2 text-xs text-secondary">
                <div className="flex items-center gap-2 font-medium"><Clock size={14} className="shrink-0" /> Deferred — waiting for the upload window</div>
                <ul className="mt-1 space-y-0.5 pl-6">
                  {deferred.map((l, i) => (
                    <li key={i}>• <span className="font-medium">{l.name}</span> ({l.type}) — {l.detail || "outside upload window"}</li>
                  ))}
                </ul>
                <div className="mt-1 pl-6 text-on-surface-variant">The local copy is verified and safe. This destination will be mirrored automatically when its window opens — or push it now with <b>Send offsite now</b> below (overrides the window).</div>
              </div>
            )}
            {mirrorTargets.length > 0 && (
              <div className="mt-2 rounded border border-outline-variant/60 bg-surface-lowest px-3 py-2.5">
                <div className="text-xs text-on-surface-variant">
                  {failed.length > 0 ? "Retry the offsite copy" : "Send this backup offsite"} — reuses the stored archive, no re-backup. Choose destinations:
                </div>
                <div className="mt-2 space-y-1">
                  {mirrorTargets.map((d) => (
                    <label key={d.id} className="flex cursor-pointer items-center gap-2 text-xs">
                      <input type="checkbox" checked={mSel.has(d.id)} onChange={() => toggleMSel(d.id)} disabled={mState === "running"} />
                      <span className="font-medium text-on-surface">{d.name}</span>
                      <span className="text-on-surface-variant">({d.type})</span>
                      {failedDestIds.has(d.id)
                        ? <span className="text-warning">retry — failed last time</span>
                        : <span className="text-on-surface-variant">no copy yet</span>}
                    </label>
                  ))}
                </div>
                <div className="mt-2.5 flex items-center gap-3">
                  <Button variant={failed.length > 0 ? "primary" : "secondary"} size="sm" disabled={mState === "running" || mSel.size === 0} onClick={mirrorNow}>
                    <BackupCloudIcon size={14} active={mState === "running"} /> Send offsite now ({mSel.size})
                  </Button>
                  {mirrorTargets.length > 1 && mState !== "running" && (
                    <button className="text-xs text-on-surface-variant hover:text-on-surface"
                      onClick={() => setMSel(mSel.size === mirrorTargets.length ? new Set() : new Set(mirrorTargets.map((d) => d.id)))}>
                      {mSel.size === mirrorTargets.length ? "Deselect all" : "Select all"}
                    </button>
                  )}
                </div>
                {mState !== "idle" && (
                  <div className="mt-2">
                    {mState === "running" && <div className="flex items-center gap-2 text-xs text-secondary"><BackupCloudIcon size={13} active /> Mirroring to destinations…</div>}
                    {mState === "done" && <div className="flex items-center gap-2 text-xs text-success"><CheckCircle2 size={13} className="shrink-0" /> {mMsg || "Offsite copy complete."}</div>}
                    {mState === "failed" && <div className="flex items-start gap-2 text-xs text-error"><AlertTriangle size={13} className="mt-0.5 shrink-0" /> {mMsg || "Mirror failed."}</div>}
                    {mLines.length > 0 && (
                      <div ref={mLog.ref} onScroll={mLog.onScroll} className="mt-1.5 max-h-32 overflow-y-auto rounded border border-outline-variant bg-surface-lowest p-2 font-mono text-[11px]">
                        {mLines.map((l, i) => <div key={i} className={l.level === "ERR" ? "text-error" : l.level === "WARN" ? "text-warning" : "text-on-surface-variant"}>{l.msg}</div>)}
                      </div>
                    )}
                  </div>
                )}
              </div>
            )}
          </Section>
        )}

        {b.status !== "running" && (
          <div className="mb-5"><RunLogPanel backupId={b.id} /></div>
        )}

        <Section title="Verification report (always-on)">
          {b.last_verified_at ? (
            <div className="mb-2 text-xs text-on-surface-variant">Last verified <span className="text-on-surface">{fmtAgo(b.last_verified_at)}</span> — scrubs re-check stored copies for bit-rot.</div>
          ) : null}
          {/* F44: per-copy verification — each copy (local + destinations) scrubs and
              reports on its own, so an offsite copy that silently rots is caught. */}
          {copies.length > 0 && (
            <div className="mb-3 flex flex-col gap-1.5">
              {copies.map((l, i) => {
                const src = l.kind === "local" ? "local" : (l.dest_id || "");
                return (
                  <div key={i} className="flex flex-wrap items-center gap-2 rounded border border-outline-variant/50 px-2.5 py-1.5 text-xs">
                    <span className="font-medium text-on-surface">{l.kind === "local" ? "Local" : l.name}</span>
                    <span className="text-on-surface-variant">({l.type})</span>
                    {l.verify_ok === false ? (
                      <span className="flex items-center gap-1 text-error" title="copy failed verification — do not rely on this destination's copy"><ShieldX size={13} /> copy failed verification{l.verified_at ? <> · {fmtAgo(l.verified_at)}</> : null}</span>
                    ) : l.verify_ok === true ? (
                      <span className="flex items-center gap-1 text-success"><ShieldCheck size={13} /> copy verified{l.verified_at ? <> · {fmtAgo(l.verified_at)}</> : null}</span>
                    ) : (
                      <span className="text-on-surface-variant">not checked yet</span>
                    )}
                    <button
                      className="ml-auto flex items-center gap-1 rounded px-1.5 py-0.5 text-on-surface-variant hover:bg-surface-highest hover:text-on-surface disabled:opacity-50"
                      onClick={() => verifyCopy(src)}
                      disabled={verifyingCopy === src || b.status === "running"}
                    >
                      {verifyingCopy === src ? <Loader2 size={13} className="animate-spin" /> : <ShieldCheck size={13} />} Verify this copy
                    </button>
                  </div>
                );
              })}
            </div>
          )}
          {ver?.checks ? (
            <ul className="space-y-1.5">
              {ver.checks.map((c: any, i: number) => (
                <li key={i} className="flex items-center gap-2 text-sm">
                  {c.ok ? <ShieldCheck size={15} className="text-success" /> : <ShieldX size={15} className="text-error" />}
                  <span className="font-medium">{c.name}</span>
                  <span className="ml-auto font-mono text-xs text-on-surface-variant">{c.info}</span>
                </li>
              ))}
            </ul>
          ) : <div className="text-sm text-on-surface-variant">No report.</div>}
        </Section>

        {man && (
          <Section title="Manifest">
            <dl className="space-y-1.5 text-sm">
              <Row k="Image" v={man.image} />
              <Row k="Image digest" v={man.image_digest || "—"} mono />
              {/* F89: the recorded network topology. Shown because a restore that
                  silently landed on a different subnet — or lost an `internal`
                  isolation flag — is exactly what this records to prevent. */}
              {(man.networks || []).length > 0 && (
                <div className="flex gap-3">
                  <dt className="w-32 shrink-0 text-on-surface-variant">Networks</dt>
                  <dd className="min-w-0 break-words">
                    {(man.networks as { name: string; subnet?: string; ipv4?: string; internal?: boolean }[])
                      .map((n) => (
                        <span key={n.name} className="mr-3 inline-block">
                          <span className="font-mono">{n.name}</span>
                          {n.subnet ? <span className="text-on-surface-variant"> {n.subnet}</span> : null}
                          {n.ipv4 ? <span className="text-on-surface-variant"> @{n.ipv4}</span> : null}
                          {n.internal ? <span className="text-on-surface-variant"> (internal)</span> : null}
                        </span>
                      ))}
                  </dd>
                </div>
              )}
              {/* F87: the dump's structural contract, recorded AT CAPTURE. It is
                  what a restore is held to, so it is worth being able to read it
                  here rather than only seeing the verdict after a restore. */}
              {(man.databases || []).filter((d: { dump_sha256?: string; dump_primary_keys?: number }) => d.dump_sha256 || d.dump_primary_keys)
                .map((d: { service?: string; dump_complete?: boolean; dump_primary_keys?: number; dump_foreign_keys?: number }, i: number) => (
                  <div key={i} className="flex gap-3">
                    <dt className="w-32 shrink-0 text-on-surface-variant">
                      Database dump{d.service ? ` · ${d.service}` : ""}
                    </dt>
                    <dd className="break-words">
                      {d.dump_complete ? (
                        <span className="text-success">complete</span>
                      ) : (
                        <span className="text-error">INCOMPLETE — do not rely on this backup</span>
                      )}
                      {(d.dump_primary_keys || d.dump_foreign_keys) ? (
                        <span className="text-on-surface-variant">
                          {" · "}{d.dump_primary_keys || 0} primary keys, {d.dump_foreign_keys || 0} foreign keys
                        </span>
                      ) : null}
                    </dd>
                  </div>
                ))}
              <div className="flex gap-3">
                <dt className="w-32 shrink-0 text-on-surface-variant">Revert pin</dt>
                <dd className="break-all">
                  {revertTarget.pin === "bundled" ? (
                    <span className="text-success">Image bundled in backup — exact & offline-proof</span>
                  ) : revertTarget.pin === "digest" ? (
                    <span className="text-on-surface">Digest-pinned — reverts to this exact image{revertTarget.sha ? ` (${revertTarget.sha}…)` : ""}</span>
                  ) : (
                    <span className="text-warning">Tag only{revertTarget.tag ? ` (:${revertTarget.tag})` : ""} — no digest; revert may drift to whatever {revertTarget.tag ? `:${revertTarget.tag}` : "the tag"} points to now</span>
                  )}
                </dd>
              </div>
              {/* Restore readiness (F11): is the image still obtainable for a restore? */}
              <div className="flex gap-3">
                <dt className="w-32 shrink-0 text-on-surface-variant">Restore readiness</dt>
                <dd className="min-w-0 flex-1">
                  {!readiness ? (
                    <Button variant="secondary" size="sm" onClick={checkReadiness} disabled={readyBusy}
                      title="Verify the container image is still available (present locally, pullable, or bundled) — no pull, no changes.">
                      {readyBusy ? <Loader2 size={14} className="animate-spin" /> : <ShieldCheck size={14} />} Check restore readiness
                    </Button>
                  ) : readiness.image_ok ? (
                    <div className="flex items-start gap-1.5 text-success">
                      <ShieldCheck size={14} className="mt-0.5 shrink-0" />
                      <span>Image ready<span className="text-on-surface-variant"> — {readiness.detail}</span></span>
                    </div>
                  ) : (
                    <div className="flex items-start gap-1.5 text-warning">
                      <AlertTriangle size={14} className="mt-0.5 shrink-0" />
                      <span className="break-words">{readiness.detail}</span>
                    </div>
                  )}
                  {readiness && !readyBusy && (
                    <button onClick={checkReadiness} className="mt-1 text-[11px] text-on-surface-variant underline hover:text-on-surface">Re-check</button>
                  )}
                </dd>
              </div>
              <Row k="Encryption" v={man.format?.encryption} />
              <Row k="Compression" v={man.format?.compression} />
              <Row k="Key fingerprint" v={man.key_fingerprint} mono />
              {/* F90: a volume backed by NFS/CIFS is materially different from a
                  local one — the driver type is appended so a restore target can
                  be judged without opening the manifest JSON. */}
              <Row k="Volumes" v={(man.volumes || []).map((v: any) => {
                const label = v.name || v.destination;
                const kind = v.options?.type || (v.options && Object.keys(v.options).length ? v.driver : "");
                return kind ? `${label} (${kind})` : label;
              }).join(", ") || "none"} />
              {(man.volumes || []).some((v: any) => v.options_redacted?.length) && (
                <Row k="Volume credentials" v="not stored — recreate these volumes by hand before restoring" />
              )}
              {man.volumes_sha256 && <Row k="Volumes SHA-256" v={man.volumes_sha256} mono />}
              <Row k="Databases" v={(man.databases || []).map((d: any) => `${d.service} (${[d.engine, d.version].filter(Boolean).join(" ")})`).join(", ") || "none"} />
              {(man.databases || []).some((d: any) => d.extensions?.length) && (
                <Row k="DB extensions" v={(man.databases || []).flatMap((d: any) => d.extensions || []).join(", ")} />
              )}
              <Row k="Cipher SHA-256" v={man.cipher_sha256} mono />
            </dl>
          </Section>
        )}

        {findings.length > 0 && (
          <Section title="Findings on the source">
            <div className="space-y-2">
              {findings.map((f, i) => (
                <div key={f.code + (f.subject || "") + i} className={`rounded px-3 py-2 text-xs ${findingTone(f.severity)}`}>
                  <div className="flex items-start gap-2">
                    {f.severity === "info"
                      ? <Info size={14} className="mt-0.5 shrink-0" />
                      : <AlertTriangle size={14} className="mt-0.5 shrink-0" />}
                    {/* min-w-0 so a long Synology path wraps inside the block
                        instead of widening the drawer, and the message wraps on
                        words wherever a translation makes it longer. */}
                    <div className="min-w-0 flex-1">
                      {f.subject && <div className="break-all font-mono text-[11px] opacity-90">{f.subject}</div>}
                      <div className="break-words">{f.message}</div>
                    </div>
                  </div>
                </div>
              ))}
            </div>
            <div className="mt-2 text-xs text-on-surface-variant">
              These describe the container this backup was taken from, not the backup itself. DockBack reports them and changes nothing — for some of them the obvious fix makes things worse.
            </div>
          </Section>
        )}

        {uncoveredSkips.length > 0 && (
          <Section title="Not captured (partial backup)">
            <div className="rounded bg-warning/10 px-3 py-2 text-xs text-warning">
              <div className="flex items-center gap-2 font-medium"><AlertTriangle size={14} className="shrink-0" /> {uncoveredSkips.length} mount(s) were not included in this backup</div>
              <ul className="mt-1 space-y-0.5 pl-6">
                {uncoveredSkips.map((m: any, i: number) => (
                  <li key={i}>• <span className="font-mono">{m.destination}</span>{m.type === "bind" && m.source ? <span className="text-on-surface-variant"> ({m.source})</span> : null} — {m.reason}</li>
                ))}
              </ul>
              <div className="mt-1 pl-6 text-on-surface-variant">Restoring this backup will not recover the data in these mounts. To include one, select it in the container's backup options and run a new backup.</div>
            </div>
          </Section>
        )}
        {coveredSkips.length > 0 && (
          <Section title="Captured via another container">
            <div className="rounded bg-surface-lowest px-3 py-2 text-xs text-on-surface-variant">
              <div className="font-medium text-on-surface">Shared folder(s) captured once — not data loss</div>
              <ul className="mt-1 space-y-0.5 pl-4">
                {coveredSkips.map((m: any, i: number) => (
                  <li key={i}>• <span className="font-mono">{m.destination}</span>{m.source ? <span> ({m.source})</span> : null} — captured via <span className="font-medium text-on-surface">{m.covered_by}</span>'s backups; restore it from there.</li>
                ))}
              </ul>
            </div>
          </Section>
        )}

        {/* F218: these are the TERMS of the restore — the safety snapshot above
            all. They used to render only for a verified backup, which was
            consistent while an unverified one could not be restored at all. Now
            that "Verify & restore" exists, hiding them would mean confirming a
            destructive action whose settings the operator cannot see, so the gate
            is what it always should have been: a successfully captured archive. */}
        {b.status === "success" && (
          <div className="mt-6 space-y-2">
            {/* Restore as a copy (F10): bring the backup up as a new, isolated
                container without touching the running one.

                F208: never for a standalone volume. The server has no copy path
                for one — it discarded the new name and overwrote the original,
                while this control promised the opposite. It now refuses the
                request; offering it here would only produce that error. */}
            {!isVolume && (<>
            <label className="flex cursor-pointer items-start gap-2 rounded border border-outline-variant/50 bg-surface-lowest px-3 py-2 text-sm text-on-surface-variant">
              <input type="checkbox" className="mt-0.5" checked={asCopy} onChange={(e) => { setAsCopy(e.target.checked); if (e.target.checked && !asName.trim()) setAsName(`${b.target_name}-restored`); }} disabled={optsLocked} />
              <span>
                <span className="font-medium text-on-surface">Restore as a copy</span> — bring this backup up as a <b>new, isolated</b> container instead of overwriting {b.target_name}: no published ports, throwaway network. The running container is <b>untouched</b>.
              </span>
            </label>
            {asCopy && (
              <div className="pl-6">
                <label className="mb-1 block text-xs font-medium text-on-surface-variant">New container name</label>
                <input value={asName} onChange={(e) => setAsName(e.target.value)} disabled={optsLocked} placeholder={`${b.target_name}-restored`}
                  className="w-full rounded border border-outline-variant bg-surface-lowest px-3 py-2 font-mono text-sm focus:outline-none focus:ring-1 focus:ring-primary" />
                <p className="mt-1 text-[11px] italic text-on-surface-variant">Inspect the restored data or test an upgrade side-by-side. The copy gets its own fresh volumes and publishes no host ports — reach it with <span className="font-mono">docker exec</span> or by attaching it to a network.</p>
              </div>
            )}
            </>)}
            {!asCopy && (<>
            {/* F208: "Revert update" recreates a container from the backup's saved
                image. A standalone volume has neither — the server ignores the
                flag entirely, so showing it only invites a click that does
                nothing. */}
            {!isVolume && (
            <label className="flex cursor-pointer items-start gap-2 rounded border border-outline-variant/50 bg-surface-lowest px-3 py-2 text-sm text-on-surface-variant">
              <input type="checkbox" className="mt-0.5" checked={recreate} onChange={(e) => setRecreate(e.target.checked)} disabled={optsLocked} />
              <span>
                <span className="font-medium text-on-surface">Revert update</span> — recreate {b.stack && stackServices > 1 ? "the service(s)" : b.target_name} from {revertTarget.pin === "tag" ? "this backup's saved image" : "this backup's exact image"}, rolling a broken upgrade back to this version.
                <span className="text-on-surface-variant/80"> Leave off to restore data into the current container without changing its image.</span>
              </span>
            </label>
            )}
            {recreate && (
              revertTarget.pin === "tag" ? (
                <div className="rounded bg-warning/10 px-3 py-2 text-xs text-warning">
                  <div className="flex items-start gap-2"><AlertTriangle size={14} className="mt-0.5 shrink-0" />
                    <span>
                      This backup has <b>no pinned image digest</b>{revertTarget.floating ? <> and its image uses the floating tag <span className="font-mono">:{revertTarget.tag}</span></> : null}. Revert will re-pull <span className="font-mono break-all">{revertTarget.image || "the recorded tag"}</span>, which may now point to a <b>different version</b> than this backup captured — so it may not truly roll the update back.
                      {" "}For an exact, offline-proof revert, enable <b>Also save the container image</b> on future backups (or restore into the current container without reverting).
                    </span>
                  </div>
                </div>
              ) : (
                <div className={`rounded px-3 py-2 text-xs ${revertTarget.pin === "bundled" ? "bg-success/10 text-success" : "bg-secondary/10 text-secondary"}`}>
                  <div className="flex items-start gap-2">{revertTarget.pin === "bundled" ? <Lock size={14} className="mt-0.5 shrink-0" /> : <RotateCcw size={14} className="mt-0.5 shrink-0" />}
                    <span>
                      Reverts to <span className="font-mono break-all">{revertTarget.image}</span>{revertTarget.sha ? <> pinned to <span className="font-mono">{revertTarget.sha}…</span></> : null} — the <b>exact image</b> from this backup{revertTarget.pin === "bundled" ? ", bundled in the archive so it restores with no registry (works offline)" : ", regardless of what the tag points to now"}.
                    </span>
                  </div>
                </div>
              )
            )}
            <label className="flex cursor-pointer items-start gap-2 text-sm text-on-surface-variant">
              <input type="checkbox" className="mt-0.5" checked={snapshot} onChange={(e) => setSnapshot(e.target.checked)} disabled={optsLocked} />
              {/* F208: this now applies to a standalone volume too — until then the
                  tick was accepted and discarded, and the volume was overwritten
                  with no rollback point either way. */}
              <span className="min-w-0">Snapshot current state before {recreate ? "reverting" : "restoring"} <span className="text-on-surface-variant/80">— a local backup of what&rsquo;s there now, so a bad {recreate ? "revert" : "restore"} is reversible (recommended).{isVolume ? " Restoring it puts back every file this restore is about to overwrite." : ""}</span></span>
            </label>
            {/* #8: a restart policy of `no` or `on-failure` does not start the
                container when the daemon does, so on a plain host it is gone after
                a reboot. Offered, never assumed: on a NAS whose package manager
                starts its own stacks the recorded policy is correct. */}
            <label className="flex cursor-pointer items-start gap-2 rounded border border-outline-variant/50 bg-surface-lowest px-3 py-2 text-sm text-on-surface-variant">
              <input type="checkbox" className="mt-0.5" checked={promoteRestart} onChange={(e) => setPromoteRestart(e.target.checked)} disabled={optsLocked} />
              <span>
                <span className="font-medium text-on-surface">Start automatically after a reboot</span> — if this container&rsquo;s restart policy is <span className="font-mono">no</span> or <span className="font-mono">on-failure</span>, set it to <span className="font-mono">unless-stopped</span>. Those policies only restart it after a bad exit; they do not start it when the Docker daemon starts.
                <span className="text-on-surface-variant/80"> Leave off to reproduce exactly what the backup recorded.</span>
              </span>
            </label>
            {/* #16: a database with no probe reports nothing better than "running",
                so `depends_on: service_started` lets its dependants start against a
                server that is still initialising. Offered, never assumed — and
                never over a probe the container already has. */}
            <label className="flex cursor-pointer items-start gap-2 rounded border border-outline-variant/50 bg-surface-lowest px-3 py-2 text-sm text-on-surface-variant">
              <input type="checkbox" className="mt-0.5 shrink-0" checked={injectHealthchecks} onChange={(e) => setInjectHealthchecks(e.target.checked)} disabled={optsLocked} />
              <span className="min-w-0 break-words">
                <span className="font-medium text-on-surface">Add a readiness check if this database has none</span> — a database with no healthcheck reports nothing better than &ldquo;running&rdquo;, so anything waiting on it starts before it can answer.
                <span className="text-on-surface-variant/80"> Only for MariaDB, PostgreSQL and Redis, only when the container defines no check of its own, and never in place of one it already has.</span>
              </span>
            </label>
            {/* Reconstruct the on-host stack layout (folder + compose file) when the
                container is recreated — for restoring onto a fresh machine. Opt-in. */}
            <label className="flex cursor-pointer items-start gap-2 rounded border border-outline-variant/50 bg-surface-lowest px-3 py-2 text-sm text-on-surface-variant">
              <input type="checkbox" className="mt-0.5" checked={reconstructHost} onChange={(e) => setReconstructHost(e.target.checked)} disabled={optsLocked} />
              <span>
                <span className="font-medium text-on-surface">Reconstruct stack folder on host</span> — when the container is recreated, also rebuild its on-host project directory and drop the reconstructed <span className="font-mono">docker-compose.yml</span> back into it, so a restore onto a fresh machine reproduces your organized layout.
                <span className="text-on-surface-variant/80"> Never overwrites an existing compose file (writes a clearly-named copy alongside).</span>
              </span>
            </label>
            {reconstructHost && (
              <div className="rounded bg-secondary/10 px-3 py-2 text-xs text-secondary">
                {knownWorkingDir ? (
                  <span>Rebuilds <span className="font-mono break-all">{knownWorkingDir}</span> and writes <span className="font-mono">{man?.compose_file || "docker-compose.yml"}</span> there. Folders are created and owned to match their parent directory.</span>
                ) : isComposeContainer ? (
                  <span>Rebuilds this stack's original directory (recorded in the backup{b.stack ? <> for project <span className="font-mono break-all">{b.stack}</span></> : null}) and writes its <span className="font-mono">docker-compose.yml</span> there. Folders are created and owned to match their parent directory.</span>
                ) : (
                  <div className="flex flex-col gap-1.5">
                    <span>This container wasn't started with <span className="font-mono">docker compose</span>, so there's no recorded directory. Choose a base directory — the folder will be <span className="font-mono break-all">{(hostBaseDir.replace(/\/+$/, "") || "/your/base")}/{b.target_name}</span>:</span>
                    <input
                      value={hostBaseDir}
                      onChange={(e) => { setHostBaseDir(e.target.value); try { localStorage.setItem("dockback.hostBaseDir", e.target.value); } catch { /* ignore */ } }}
                      placeholder="/opt/docker"
                      spellCheck={false}
                      disabled={optsLocked}
                      className={`w-full rounded border bg-surface px-2 py-1 font-mono text-on-surface placeholder:text-on-surface-variant/50 ${baseDirProblem(hostBaseDir) ? "border-error" : "border-outline-variant/60"}`}
                    />
                    {baseDirProblem(hostBaseDir) && <span className="break-words text-error">{baseDirProblem(hostBaseDir)}</span>}
                  </div>
                )}
              </div>
            )}
            {/* Remap machine IP (cross-host): rewrite the source host's IP to the
                target host's IP so a service pinned to the old address comes up. */}
            <label className="flex cursor-pointer items-start gap-2 rounded border border-outline-variant/50 bg-surface-lowest px-3 py-2 text-sm text-on-surface-variant">
              <input type="checkbox" className="mt-0.5" checked={remapIP} onChange={(e) => setRemapIP(e.target.checked)} disabled={optsLocked} />
              <span>
                <span className="font-medium text-on-surface">Remap machine IP</span> — rewrite the source machine's IP to the target machine's IP in the recreated config (published ports, environment, extra hosts){reconstructHost ? " and the reconstructed compose file" : ""}, so a service pinned to the old host's address doesn't fail to start.
                {!crossHostSel && <span className="text-on-surface-variant/80"> Mainly for a cross-host restore — the selected target is the same node this backup came from.</span>}
              </span>
            </label>
            {remapIP && (
              <div className="flex flex-col gap-2 rounded bg-secondary/10 px-3 py-2 text-xs text-secondary">
                <div className="grid grid-cols-[auto_1fr] items-center gap-x-2 gap-y-1.5">
                  <span className="whitespace-nowrap">From (source host):</span>
                  <input value={remapFrom} onChange={(e) => setRemapFrom(e.target.value)} placeholder="10.168.1.10" spellCheck={false} disabled={optsLocked}
                    className="w-full rounded border border-outline-variant/60 bg-surface px-2 py-1 font-mono text-on-surface placeholder:text-on-surface-variant/50" />
                  <span className="whitespace-nowrap">To (target host):</span>
                  <input value={remapTo} onChange={(e) => setRemapTo(e.target.value)} placeholder="10.168.1.20" spellCheck={false} disabled={optsLocked}
                    className="w-full rounded border border-outline-variant/60 bg-surface px-2 py-1 font-mono text-on-surface placeholder:text-on-surface-variant/50" />
                </div>
                <span className="text-on-surface-variant/80">Prefilled from the node addresses when they use an IP; a blank field is filled at start, resolving a hostname-registered node to its IP — and the restore refuses rather than silently skipping if neither yields one. Only exact matches of the source IP are changed; the binding stays on a specific interface (never widened to all).</span>
              </div>
            )}
            {/* F195: Remap domain — the address mechanism for every container,
                profile or not. Literal from→to over environment values, so an
                upstream naming a different machine is never touched. */}
            <label className="flex cursor-pointer items-start gap-2 rounded border border-outline-variant/50 bg-surface-lowest px-3 py-2 text-sm text-on-surface-variant">
              <input type="checkbox" className="mt-0.5" checked={remapDomain} onChange={(e) => setRemapDomain(e.target.checked)} disabled={optsLocked} />
              <span>
                <span className="font-medium text-on-surface">Remap domain</span> — rewrite the old domain to the new one wherever this container&rsquo;s environment carries it (base URLs, CORS and trusted-host lists). Works for every container; only values naming the old domain change.
              </span>
            </label>
            {remapDomain && (
              <div className="flex flex-col gap-2 rounded bg-secondary/10 px-3 py-2 text-xs text-secondary">
                <div className="grid grid-cols-[auto_1fr] items-center gap-x-2 gap-y-1.5">
                  <span className="whitespace-nowrap">Current domain:</span>
                  <input value={domainFrom} onChange={(e) => setDomainFrom(e.target.value)} placeholder="app.old-home.net" spellCheck={false} disabled={optsLocked}
                    className="w-full rounded border border-outline-variant/60 bg-surface px-2 py-1 font-mono text-on-surface placeholder:text-on-surface-variant/50" />
                  <span className="whitespace-nowrap">New domain:</span>
                  <input value={domainTo} onChange={(e) => setDomainTo(e.target.value)} placeholder="app.new-home.net" spellCheck={false} disabled={optsLocked}
                    className="w-full rounded border border-outline-variant/60 bg-surface px-2 py-1 font-mono text-on-surface placeholder:text-on-surface-variant/50" />
                </div>
                <span className="text-on-surface-variant/80">Plain domain names only — no scheme, port or path. Subdomains are separate hosts and need their own pass. Every change is logged.</span>
              </div>
            )}
            {/* Remap stack paths (F81): move bind sources + compose + stack folder
                from the old machine's base directory to this machine's layout. */}
            <label className="flex cursor-pointer items-start gap-2 rounded border border-outline-variant/50 bg-surface-lowest px-3 py-2 text-sm text-on-surface-variant">
              <input type="checkbox" className="mt-0.5" checked={remapPath} onChange={(e) => setRemapPath(e.target.checked)} disabled={optsLocked} />
              <span>
                <span className="font-medium text-on-surface">Remap stack paths</span> — rewrites bind-mount folders{reconstructHost ? ", the reconstructed compose file, and the stack folder location" : " (and, with host reconstruction on, the compose file and stack folder)"} from the old machine's layout to this machine's — exact matches only, every change is logged. Without it, Docker recreates the old machine's folders here.
                {!crossHostSel && <span className="text-on-surface-variant/80"> Mainly for a cross-host restore — the selected target is the same node this backup came from.</span>}
              </span>
            </label>
            {remapPath && (
              <div className="flex flex-col gap-2 rounded bg-secondary/10 px-3 py-2 text-xs text-secondary">
                <div className="grid grid-cols-[auto_1fr] items-center gap-x-2 gap-y-1.5">
                  <span className="whitespace-nowrap">From base folder:</span>
                  <input value={pathFrom} onChange={(e) => setPathFrom(e.target.value)} placeholder="/opt/docker" spellCheck={false} disabled={optsLocked}
                    className={`w-full rounded border bg-surface px-2 py-1 font-mono text-on-surface placeholder:text-on-surface-variant/50 ${baseDirProblem(pathFrom) ? "border-error" : "border-outline-variant/60"}`} />
                  {baseDirProblem(pathFrom) && <span className="col-span-2 break-words text-error">{baseDirProblem(pathFrom)}</span>}
                  <span className="whitespace-nowrap">To base folder:</span>
                  <input value={pathTo} onChange={(e) => setPathTo(e.target.value)} placeholder="/opt/stacks" spellCheck={false} disabled={optsLocked}
                    className={`w-full rounded border bg-surface px-2 py-1 font-mono text-on-surface placeholder:text-on-surface-variant/50 ${baseDirProblem(pathTo) ? "border-error" : "border-outline-variant/60"}`} />
                  {baseDirProblem(pathTo) && <span className="col-span-2 break-words text-error">{baseDirProblem(pathTo)}</span>}
                </div>
                <span className="text-on-surface-variant/80">From is prefilled with the parent of the stack's recorded compose folder. Only paths under the From base move; system folders are refused as a target; container-side paths are never touched.</span>
              </div>
            )}
            </>)}
          </div>
        )}

        {b.stack && (
          <p className="mt-2 text-[11px] text-on-surface-variant">
            The options above apply to <b>Restore this service only</b>. <b>Restore stack</b> opens the stack dialog pre-filled with these same choices, plus the per-service restore plan.
          </p>
        )}
        {/* F206: this container's live data is protected — overwriting it asks
            for the password first. Said BEFORE the operator commits, so the
            prompt is a stated condition rather than a surprise. Restoring as a
            copy is never gated, so the note names that as the way around it. */}
        {readiness?.step_up_required && !asCopy && !restoreStepUp && (
          <p className="mt-2 flex items-start gap-2 rounded bg-primary/[0.08] px-2.5 py-2 text-xs text-on-surface-variant">
            <Lock size={14} className="mt-0.5 shrink-0 text-primary" />
            <span className="min-w-0">
              <b className="text-on-surface">{b.target_name}</b> is protected: overwriting its live data asks for your password first. <b>Restore as a copy</b> does not &mdash; it leaves the original untouched.
            </span>
          </p>
        )}
        {restoreStepUp && (
          <div className="mt-3 max-w-md">
            <StepUpPrompt totp={restoreStepUp.totp} busy={rState === "running"} error={restoreStepUp.err}
              confirmLabel="Confirm and restore"
              onConfirm={(pw, code) => runRestore(false, { password: pw, code })} />
          </div>
        )}
        <div className="mt-3 flex flex-wrap gap-2">
          {b.status === "running" ? (
            <Button variant="danger" onClick={async () => { try { await api.cancelBackup(b.id); onChanged(); } catch {} }}>
              <X size={16} /> Cancel Backup
            </Button>
          ) : (
            <>
              {/* Gate on b.stack ALONE: the true service count lives in the
                  dialog's server-side plan. Counting siblings from the loaded
                  page window made this button vanish under pagination/filters —
                  turning the PRIMARY action into a single-service restore. */}
              {b.stack && (
                <Button variant="primary" disabled={optsLocked} onClick={openStackRestore}>
                  {rState === "running" && rMode === "stack" ? <Loader2 size={16} className="animate-spin" /> : <Layers size={16} />}
                  {stackServices > 1 ? `Restore stack (${stackServices} services)…` : "Restore stack…"}
                </Button>
              )}
              <Button variant={b.stack ? "secondary" : "primary"} disabled={optsLocked || !restorable} onClick={() => restore(needsVerifyFirst)}
                title={needsVerifyFirst ? "This backup has not been verified — it is verified first, and restored only if it passes" : undefined}>
                {(rState === "running" && rMode === "one") || vrState === "verifying"
                  ? <Loader2 size={16} className="animate-spin" />
                  : needsVerifyFirst ? <ShieldCheck size={16} /> : <RotateCcw size={16} />}
                {restoreLabel}
              </Button>
              {/* F219: proving a backup is not a destructive act, so it does not
                  wear the destructive path's shape. One click: the server names
                  the copy, isolates it, and removes it when its time is up.
                  Never offered for a standalone volume — there is no container
                  to bring up, and the server has no copy path for one (F208). */}
              {!isVolume && (
                <Button variant="secondary" disabled={optsLocked || testing || !restorable} onClick={() => testRestore()}
                  title={`Bring this backup up as an isolated copy that touches nothing and is removed automatically after ${cloneTTL} hour${cloneTTL === 1 ? "" : "s"}`}>
                  {testing ? <Loader2 size={16} className="animate-spin" /> : <FlaskConical size={16} />} Test restore
                </Button>
              )}
              <ExportDownloadButton backupId={b.id} purpose="download" label="Download (decrypted)"
                title="Streams the archive in plaintext — confirms your password first" icon={<Download size={16} />} />
              <Button variant="secondary" onClick={verifyNow} disabled={verifying} title="Re-read and re-verify this backup now (scrub)">
                {verifying ? <Loader2 size={16} className="animate-spin" /> : <ShieldCheck size={16} />} Verify now
              </Button>
              <Button variant="danger" onClick={onDelete} disabled={optsLocked}><Trash2 size={16} /> Delete</Button>
            </>
          )}
        </div>
        {/* F219: what a previous test left running on this node, and the way to
            end it early. Listed from the HOST, so a clone survives — and is
            still listed by — a restart of DockBack itself. */}
        {clones.length > 0 && (
          <div className="mt-3 rounded border border-outline-variant/60 bg-surface-lowest p-3">
            <div className="mb-2 flex items-center gap-2 text-xs font-semibold uppercase tracking-widest text-on-surface-variant">
              <FlaskConical size={13} className="text-primary" /> Test clones on this node
            </div>
            <div className="flex flex-col gap-1.5">
              {clones.map((c) => {
                const left = c.expires_at - Math.floor(Date.now() / 1000);
                return (
                  <div key={c.id} className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs">
                    <span className="min-w-0 break-all font-mono text-on-surface">{c.name}</span>
                    <Chip kind={c.state === "running" ? "ok" : "muted"}>{c.state}</Chip>
                    <span className="flex items-center gap-1 text-on-surface-variant">
                      <Timer size={12} className="shrink-0" />
                      {left > 3600
                        ? <>removed in {Math.round(left / 3600)}h</>
                        : left > 0
                          ? <>removed in {Math.max(1, Math.round(left / 60))} min</>
                          : <>due for removal</>}
                    </span>
                    <button onClick={() => removeClone(c)} disabled={cloneBusy === c.id}
                      className="ml-auto shrink-0 rounded border border-error/40 px-2 py-0.5 text-error hover:bg-error/10 disabled:opacity-50">
                      {cloneBusy === c.id ? "Removing…" : "Delete now"}
                    </button>
                  </div>
                );
              })}
            </div>
            <p className="mt-2 break-words text-[11px] text-on-surface-variant">
              Each was created by <b>Test restore</b> and goes by itself at its expiry, together with the volumes Docker made for it. Nothing else on the node is touched.
            </p>
          </div>
        )}
        {/* F218: why the button says what it says, before it is pressed. */}
        {needsVerifyFirst && rState === "idle" && vrState === "idle" && (
          <p className="mt-2 flex items-start gap-2 rounded bg-surface-lowest px-2.5 py-2 text-xs text-on-surface-variant">
            <ShieldCheck size={14} className="mt-0.5 shrink-0 text-primary" />
            <span className="min-w-0 break-words">
              {b.verified === "failed"
                ? <>This backup&rsquo;s last verification <b className="text-error">FAILED</b>. It is re-read and re-checked first; it is restored only if it passes this time, and refused again if it does not.</>
                : <>This backup has <b className="text-on-surface">not been verified</b>. It is re-read and checked first, and restored only if it passes &mdash; so a damaged archive is found before it overwrites anything, not after.</>}
            </span>
          </p>
        )}
        {/* F218: the verify leg. Its own panel — this is not a restore yet. */}
        {vrState === "verifying" && (
          <div className="mt-3">
            <div className="flex flex-wrap items-center gap-2 rounded bg-secondary/10 px-3 py-2 text-sm text-secondary">
              <Loader2 size={16} className="shrink-0 animate-spin" />
              <span className="min-w-0 break-words">Verifying {b.target_name} &mdash; re-reading and re-checking the archive. The restore starts by itself if it passes.</span>
            </div>
            {vrLines.length > 0 && (
              <div className="mt-2 max-h-32 overflow-y-auto rounded border border-outline-variant bg-surface-lowest p-3 font-mono text-xs">
                {vrLines.map((l, i) => (
                  <div key={i} className={`break-words ${l.level === "ERR" ? "text-error" : "text-on-surface-variant"}`}>{l.msg}</div>
                ))}
              </div>
            )}
          </div>
        )}
        {vrErr && vrState === "idle" && (
          <div className="mt-3 flex items-start gap-2 rounded border border-error/40 bg-error/10 px-3 py-2 text-sm text-error">
            <ShieldX size={16} className="mt-0.5 shrink-0" />
            <span className="min-w-0 break-words">{vrErr}</span>
          </div>
        )}
        {b.stack && rState === "idle" && (
          <p className="mt-2 text-xs text-on-surface-variant">
            <b>Restore stack</b> recreates and starts every service of <span className="font-mono">{b.stack}</span> (databases first) — the dialog shows the exact per-service plan, preselecting this backup's point in time when it was captured as an app-consistent snapshot. No need to restore each one or use a terminal.
          </p>
        )}

        {/* F21: Browse files — lazily list the backup's volume files and download one. */}
        {b.status === "success" && (
          <div className="mt-4 rounded border border-outline-variant bg-surface-lowest">
            <button onClick={toggleBrowse} className="flex w-full items-center gap-2 px-3 py-2.5 text-left text-sm font-medium hover:bg-surface-high/40">
              {browseOpen ? <ChevronDown size={15} className="text-on-surface-variant" /> : <ChevronRight size={15} className="text-on-surface-variant" />}
              <FolderOpen size={15} className="text-primary" /> Browse files
              <span className="ml-auto text-xs font-normal text-on-surface-variant">recover a single file without the whole archive</span>
            </button>
            {browseOpen && (
              <div className="border-t border-outline-variant/60 p-3">
                {entriesLoading && <div className="flex items-center gap-2 text-xs text-on-surface-variant"><Loader2 size={13} className="animate-spin" /> Reading the archive…</div>}
                {entriesErr && <div className="text-xs text-error">Couldn't list files: {entriesErr}</div>}
                {entries && !entriesLoading && entries.length === 0 && (
                  <div className="text-xs text-on-surface-variant">This backup has no browsable volume files (it may be a database-only or app-export backup).</div>
                )}
                {entries && entries.length > 0 && (
                  <>
                    <div className="mb-2 flex items-center gap-2">
                      <Search size={13} className="text-on-surface-variant" />
                      <input value={fileFilter} onChange={(e) => setFileFilter(e.target.value)} placeholder="Filter files…"
                        className="w-full rounded border border-outline-variant bg-surface px-2 py-1 text-xs outline-none focus:border-primary" />
                      <span className="shrink-0 text-xs text-on-surface-variant">{entries.length} file{entries.length === 1 ? "" : "s"}</span>
                    </div>
                    <div className="max-h-72 overflow-y-auto rounded border border-outline-variant/60">
                      {entries
                        .filter((e) => !fileFilter || e.name.toLowerCase().includes(fileFilter.toLowerCase()))
                        .slice(0, 2000)
                        .map((e) => (
                          <div key={e.name} className="flex items-center gap-2 border-b border-outline-variant/30 px-2.5 py-1.5 text-xs last:border-b-0 hover:bg-surface-high/30">
                            <span className="min-w-0 flex-1 truncate font-mono text-on-surface" title={e.name}>{e.name}</span>
                            <span className="shrink-0 tnum text-on-surface-variant">{fmtBytes(e.size)}</span>
                            <ExportDownloadButton backupId={b.id} purpose="extract" path={e.name}
                              title="Download this file" iconOnly icon={<Download size={14} />} />
                            {/* F96: the step that used to be a manual `docker cp`. */}
                            <RestoreFileButton
                              backupId={b.id} path={e.name} container={b.target_name}
                              nodeId={restoreNode || b.node_id}
                              targetId={man?.container_id || b.target_name}
                              source={source}
                            />
                          </div>
                        ))}
                    </div>
                    <p className="mt-2 text-[11px] text-on-surface-variant">Each file is decrypted and streamed on its own — the whole archive is never downloaded.</p>
                  </>
                )}
              </div>
            )}
          </div>
        )}

        {/* F70: Compare generations — added/changed/deleted vs the previous backup, from the stored indexes. */}
        {b.status === "success" && prevGen && (
          <div className="mt-4 rounded border border-outline-variant bg-surface-lowest">
            <button onClick={toggleDiff} className="flex w-full items-center gap-2 px-3 py-2.5 text-left text-sm font-medium hover:bg-surface-high/40">
              {diffOpen ? <ChevronDown size={15} className="text-on-surface-variant" /> : <ChevronRight size={15} className="text-on-surface-variant" />}
              <Layers size={15} className="text-primary" /> Compare with previous
              <span className="ml-auto text-xs font-normal text-on-surface-variant">what changed since {fmtAgo(prevGen.created_at)}</span>
            </button>
            {diffOpen && (
              <div className="border-t border-outline-variant/60 p-3">
                {diffBusy && <div className="flex items-center gap-2 text-xs text-on-surface-variant"><Loader2 size={13} className="animate-spin" /> Comparing file indexes…</div>}
                {diffErr && <div className="text-xs text-error">{diffErr}</div>}
                {diffData && (
                  <>
                    <div className="mb-2 flex flex-wrap items-center gap-2 text-xs">
                      <Chip kind="ok">{diffData.counts.added} added</Chip>
                      <Chip kind="warn">{diffData.counts.changed} changed</Chip>
                      <Chip kind="err">{diffData.counts.deleted} deleted</Chip>
                      <span className="text-on-surface-variant">
                        {new Date(diffData.a.created_at * 1000).toLocaleString()} → {new Date(diffData.b.created_at * 1000).toLocaleString()}
                      </span>
                    </div>
                    {diffData.counts.added + diffData.counts.changed + diffData.counts.deleted === 0 && (
                      <p className="text-xs text-on-surface-variant">No file changes between these two backups.</p>
                    )}
                    <div className="max-h-80 overflow-y-auto overflow-x-auto rounded border border-outline-variant/60">
                      {(diffData.added || []).map((d) => (
                        <div key={"a" + d.path} className="flex items-center gap-2 border-b border-outline-variant/30 px-2.5 py-1 text-xs last:border-b-0">
                          <span className="w-3 shrink-0 font-mono font-bold text-success">+</span>
                          <span className="min-w-0 flex-1 truncate font-mono text-on-surface" title={d.path}>{d.path}</span>
                          <span className="shrink-0 tnum text-on-surface-variant">{fmtBytes(d.size)}</span>
                        </div>
                      ))}
                      {(diffData.changed || []).map((d) => (
                        <div key={"c" + d.path} className="flex items-center gap-2 border-b border-outline-variant/30 px-2.5 py-1 text-xs last:border-b-0">
                          <span className="w-3 shrink-0 font-mono font-bold text-warning">~</span>
                          <span className="min-w-0 flex-1 truncate font-mono text-on-surface" title={d.path}>{d.path}</span>
                          <span className="shrink-0 tnum text-on-surface-variant">{fmtBytes(d.prev_size || 0)} → {fmtBytes(d.size)}</span>
                        </div>
                      ))}
                      {(diffData.deleted || []).map((p) => (
                        <div key={"d" + p} className="flex items-center gap-2 border-b border-outline-variant/30 px-2.5 py-1 text-xs last:border-b-0">
                          <span className="w-3 shrink-0 font-mono font-bold text-error">−</span>
                          <span className="min-w-0 flex-1 truncate font-mono text-on-surface line-through decoration-error/60" title={p}>{p}</span>
                        </div>
                      ))}
                    </div>
                    {diffData.truncated && <p className="mt-1.5 text-[11px] text-warning">Long change list truncated — full counts shown in the chips above.</p>}
                  </>
                )}
              </div>
            )}
          </div>
        )}

        {/* Live restore progress + result */}
        {rState !== "idle" && (
          <div className="mt-4">
            {rState === "running" && (
              <div className="flex flex-wrap items-center gap-2 rounded bg-secondary/10 px-3 py-2 text-sm text-secondary">
                <Loader2 size={16} className="animate-spin shrink-0" />
                <span className="min-w-0">{rMode === "stack" ? `Restoring stack ${b.stack}…` : `Restoring ${b.target_name}…`}</span>
                {/* A restore can legitimately need stopping — most often while the
                    health gate waits for a container that won't come up. */}
                <Button variant="danger" className="ml-auto h-7 px-2 py-0 text-xs" disabled={canceling} onClick={cancelRestore}>
                  {canceling ? <Loader2 size={13} className="animate-spin" /> : <X size={13} />} {canceling ? "Canceling…" : "Cancel restore"}
                </Button>
              </div>
            )}
            {rState === "canceled" && (
              <div className="flex items-start gap-2 rounded bg-warning/10 px-3 py-2 text-sm text-warning">
                <AlertTriangle size={16} className="mt-0.5 shrink-0" />
                <span>Restore canceled{rErr ? `: ${rErr}` : "."} Nothing was rolled back automatically — read the log above for exactly what state it was left in.</span>
              </div>
            )}
            {rState === "done" && <div className="flex items-center gap-2 rounded bg-success/10 px-3 py-2 text-sm text-success"><CheckCircle2 size={16} /> {rMode === "stack" ? `Stack ${b.stack} restored — all services are running.` : asCopy ? `Copy "${asName.trim()}" created and running (isolated) — ${b.target_name} was untouched.` : `Restore complete — ${b.target_name} is running with restored data.`}</div>}
            {rState === "failed" && <div className="flex items-start gap-2 rounded bg-error/10 px-3 py-2 text-sm text-error"><AlertTriangle size={16} className="mt-0.5 shrink-0" /> Restore failed{rErr ? `: ${rErr}` : ""}.</div>}
            {rLines.length > 0 && (
              <div ref={rLog.ref} onScroll={rLog.onScroll} className="mt-2 max-h-40 overflow-y-auto rounded border border-outline-variant bg-surface-lowest p-3 font-mono text-xs">
                {rLines.map((l, i) => (
                  <div key={i} className={l.level === "ERR" ? "text-error" : "text-on-surface-variant"}>{l.msg}</div>
                ))}
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

// CertificatePanel lists the TLS certificates travelling inside a backup (F143).
//
// Its job is one question, answered before the restore: is what is in here still
// usable? A certificate that expired while the archive sat in storage restores
// perfectly and still leaves every browser refusing the connection, and that is
// the single thing about a proxy backup a faithful restore cannot fix for you.
//
// Paths are shown; hostnames are not, because they are not recorded — the
// manifest counts the names a certificate covers rather than publishing an
// operator's internal domains to every destination the archive is copied to.
function CertificatePanel({ certs }: { certs: CertRef[] }) {
  const now = Date.now();
  const DAY = 86400000;
  const state = (c: CertRef): { tone: string; label: string } => {
    const t = c.not_after ? Date.parse(c.not_after) : NaN;
    if (Number.isNaN(t)) return { tone: "text-on-surface-variant", label: "expiry not recorded" };
    if (t < now) return { tone: "text-error", label: "expired" };
    const days = Math.floor((t - now) / DAY);
    if (days <= 30) return { tone: "text-warning", label: `expires in ${days} day${days === 1 ? "" : "s"}` };
    return { tone: "text-on-surface-variant", label: `valid for ${days} more days` };
  };
  const anyExpired = certs.some((c) => { const t = c.not_after ? Date.parse(c.not_after) : NaN; return !Number.isNaN(t) && t < now; });
  const missingKey = certs.some((c) => !c.has_key);

  return (
    <div className={`mb-3 rounded px-3 py-2 text-xs ${anyExpired ? "bg-warning/10" : "bg-primary/10"}`}>
      <div className={`flex items-center gap-2 font-medium ${anyExpired ? "text-warning" : "text-primary"}`}>
        {anyExpired ? <AlertTriangle size={14} className="shrink-0" /> : <Info size={14} className="shrink-0" />}
        <span className="break-words">
          {certs.length} TLS certificate{certs.length === 1 ? "" : "s"} inside this backup
          {anyExpired ? " — at least one has expired" : ""}
        </span>
      </div>
      <ul className="mt-1 space-y-1 pl-6">
        {certs.map((c, i) => {
          const st = state(c);
          return (
            <li key={i} className="break-words text-on-surface">
              <code className="break-all font-mono text-[11px]">{c.path}</code>
              <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
                <span className={st.tone}>{st.label}</span>
                {c.not_after && <span className="text-on-surface-variant">({new Date(c.not_after).toLocaleDateString()})</span>}
                {c.issuer && <span className="text-on-surface-variant">· issued by {c.issuer}</span>}
                {!!c.names && <span className="text-on-surface-variant">· covers {c.names} name{c.names === 1 ? "" : "s"}</span>}
                {!c.has_key && <span className="text-warning">· no private key captured beside it</span>}
              </div>
            </li>
          );
        })}
      </ul>
      <div className="mt-1 pl-6 text-on-surface-variant">
        {anyExpired
          ? "An expired certificate restores exactly as captured — the restore is correct, the certificate aged. Renew it after the restore; renewal runs from the restored configuration, so the machine needs to reach the certificate authority (and your DNS provider's API, if you use DNS-01 validation)."
          : "Certificates are bound to domain names, not to a machine, so they stay valid wherever this is restored — nothing needs reissuing."}
        {missingKey ? " A certificate with no private key beside it cannot serve traffic; check that the key lives inside a captured mount." : ""}
      </div>
    </div>
  );
}

// Severity -> the drawer's existing palette. danger and warn deliberately share
// the warn styling: the register's severities rank what an operator should DO,
// and both of those mean "look at this", while a third colour would imply a
// distinction the messages themselves make better than a border can.
function findingTone(severity: string): string {
  if (severity === "info") return "bg-surface-lowest text-on-surface-variant";
  return "bg-warning/10 text-warning";
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return <div className="mb-5"><div className="mb-2 text-xs font-semibold uppercase tracking-wider text-on-surface-variant">{title}</div><Card className="p-4">{children}</Card></div>;
}
function Row({ k, v, mono }: { k: string; v: string; mono?: boolean }) {
  return <div className="flex gap-3"><dt className="w-32 shrink-0 text-on-surface-variant">{k}</dt><dd className={`break-all ${mono ? "font-mono text-xs" : ""}`}>{v}</dd></div>;
}
// Offsite copies deferred to their upload window — postponed, not failed (F14).
function deferredCopies(b: Backup) { return locations(b).filter((l) => l.status === "deferred"); }
// A location holds no restorable data when it failed or was deferred (F14).
function noDataLoc(l: { status?: string }) { return l.status === "failed" || l.status === "deferred"; }
// Immutable copies present (WORM / object-lock, PLAN §9.1).
function immutableCopies(b: Backup) { return locations(b).filter((l) => l.immutable && l.status !== "failed"); }
// F97: a filesystem retention lock (local/SMB/SFTP) is NOT the same guarantee as
// S3 Object Lock — it is a permission on the target machine, which root can undo.
// The UI must not badge the two identically, or the weaker one reads as WORM.
function isWormCopy(l: { type?: string }) { return l.type === "s3"; }
