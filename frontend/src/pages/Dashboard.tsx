// System Dashboard — node/machine grid. Redesigned to match
// docs/UI-UX/Dashboard nodes cards redesign/Dashboard.dc.html: a container-
// distribution hero, ring CPU/MEM gauges, a 2x2 inventory grid, and an events
// footer. All colors come from theme tokens (+ a few --dash-* accents) so the
// card recolors across Midnight / Classic / Light themes. Every stat is live.
import { useEffect, useMemo, useRef, useState } from "react";
import { useNodeActions } from "../hooks/useNodeActions";
import { useNavigate } from "react-router-dom";
import {
  Wifi, WifiOff, LineChart, Boxes, Settings as Gear,
  CheckCircle2, AlertTriangle, Image as ImageIcon, Layers, HardDrive,
  Network, Plus, RefreshCw, Pencil, Trash2, Shield, GripHorizontal,
} from "lucide-react";
import { api, Node, Coverage, Cluster, RecoveryState, fmtBytes } from "../api";
import AddNodeModal from "../components/AddNodeModal";
import BackupCloudIcon from "../components/BackupCloudIcon";
import RecoveryWizard from "../components/RecoveryWizard";
import { useToast } from "../components/Toast";
import { useEventStream } from "../hooks/useEventStream";
import { NodeAvatar } from "../components/NodeAvatar";
import ClusterBar, { ALL_CLUSTERS, clusterCounts, nodeCluster } from "../components/ClusterBar";
import ClusterBand, { rollupClusters } from "../components/ClusterBand";
import { usePoll } from "../hooks/usePoll";

const RING_CIRC = 2 * Math.PI * 24; // r=24 in the 64x64 viewBox
const ringDash = (pct: number) =>
  `${((Math.min(Math.max(pct, 0), 100) / 100) * RING_CIRC).toFixed(1)} ${RING_CIRC.toFixed(1)}`;

export default function Dashboard() {
  const [nodes, setNodes] = useState<Node[] | null>(null);
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<Node | null>(null);
  const [menuFor, setMenuFor] = useState<string | null>(null);
  const [spin, setSpin] = useState(false);
  const [cluster, setCluster] = useState(ALL_CLUSTERS); // cluster filter (PLAN §4.13)
  const [coverage, setCoverage] = useState<Coverage | null>(null); // "unprotected containers" (B2)
  // F223: is this instance empty, and how far has a recovery got? One small read
  // on mount — not polled, because neither answer changes without the operator
  // doing something that already reloads this page.
  const [recovery, setRecovery] = useState<RecoveryState | null>(null);
  // F104: registry metadata (colour/description) only — the per-cluster rollup is
  // computed in the browser from `nodes` + `coverage`, so grouping costs no extra
  // server work. Fetched once; a colour change is a Settings action, not live data.
  const [clusters, setClusters] = useState<Cluster[]>([]);
  const navigate = useNavigate();
  const toast = useToast();

  const loadCoverage = () => api.coverage().then(setCoverage).catch(() => {});

  // load(true) shows the refresh spinner (manual / initial); background reloads
  // pass false so periodic refreshes no longer flicker the spinner (A7).
  const load = async (showSpin = false) => {
    if (showSpin) setSpin(true);
    const ns = await api.nodes().catch(() => []);
    setNodes(ns);
    if (showSpin) setSpin(false);
  };

  // Live node deltas over SSE (A7): merge a node's summary/health in place, so the
  // dashboard updates within a second with no full refetch or spinner.
  const { connected } = useEventStream({
    "node.summary": (d: { node_id: string; reachable: boolean; error?: string; summary?: Node["summary"] }) => {
      setNodes((prev) => prev && prev.map((n) => (n.id === d.node_id ? { ...n, reachable: d.reachable, error: d.error, summary: d.summary } : n)));
    },
    // A backup starting/finishing changes a node's running/health counts and its
    // protection coverage (B2), so refresh both.
    "backup.status": () => { load(false); loadCoverage(); },
  });

  useEffect(() => { load(false); }, [connected]);
  // Fallback poll: slow reconcile when the stream is live, original cadence when
  // it's down (mirrors Logs) — paused entirely while the tab is hidden (Fix 9).
  usePoll(() => load(false), connected ? 60000 : 8000);

  // Coverage is derived from cached inventory + history — cheap, so poll it slowly
  // (and on backup events above) without adding load (B2).
  useEffect(() => { loadCoverage(); }, []);
  usePoll(loadCoverage, 45000);

  // Cluster metadata is edited in Settings, not by the fleet — fetch once and
  // don't poll it. A failure is silent: the rollup falls back to derived colours
  // and the dashboard renders exactly as it would without the registry (F104).
  useEffect(() => { api.listClusters().then((r) => setClusters(r.clusters)).catch(() => {}); }, []);

  // Dismiss the per-card gear menu on any outside click/touch/Escape.
  const menuRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!menuFor) return;
    const onDown = (e: MouseEvent | TouchEvent) => {
      if (menuRef.current && !menuRef.current.contains(e.target as HTMLElement)) setMenuFor(null);
    };
    const onKey = (e: KeyboardEvent) => { if (e.key === "Escape") setMenuFor(null); };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("touchstart", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("touchstart", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [menuFor]);

  const {
    pendingDelete: pendingDel, forgetNode,
    grabbedNodeId, setGrabbedNodeId, dragIndex, setDragIndex, dropIndex, setDropIndex,
    justDragged, clearDrag, reorderNodes: reorder,
  } = useNodeActions({ nodes, setNodes, reload: load });
  // The card menu closes whichever action is chosen.
  const remove = (n: Node) => { setMenuFor(null); forgetNode(n); };
  const edit = (n: Node) => { setMenuFor(null); setEditing(n); setAdding(true); };

  // F221: "back up everything on this node, now" without opening the node first
  // — the thing you want before touching a host. Each container is captured with
  // its OWN remembered options, and shared host folders once per stack; the
  // server decides all of that, so this is one call regardless of node size.
  const [backingUp, setBackingUp] = useState("");
  const backupNode = async (n: Node) => {
    setMenuFor(null);
    if (!confirm(`Back up every running container on ${n.name} now?\n\nEach is captured with its own remembered options and queued behind the usual limits, so a large node does not overwhelm itself.`)) return;
    setBackingUp(n.id);
    try {
      const res = await api.backupNodeAll(n.id);
      if (res.count === 0) {
        toast.success(`Every container on ${n.name} already has a backup queued or running.`);
      } else {
        toast.success(`Queued ${res.count} backup${res.count === 1 ? "" : "s"} on ${n.name}${res.skipped > 0 ? ` (${res.skipped} already running)` : ""}. Watch progress in Logs.`);
      }
    } catch (e) { toast.error(`Couldn't back up ${n.name}: ${(e as Error).message}`); }
    finally { setBackingUp(""); }
  };

  const allNodes = (nodes || []).filter((n) => !pendingDel.has(n.id));
  const clusterList = clusterCounts(allNodes);
  const multiCluster = clusterList.length > 1;
  const reachable = allNodes.filter((n) => n.reachable).length;
  const allOK = allNodes.length > 0 && reachable === allNodes.length;

  // Cluster filter + grouping (PLAN §4.13). When "All" is selected on a
  // multi-cluster fleet, cards are grouped under per-cluster section headers;
  // selecting a cluster shows just that group as a flat grid.
  const filtered = cluster === ALL_CLUSTERS ? allNodes : allNodes.filter((n) => nodeCluster(n) === cluster);
  const grouped = cluster === ALL_CLUSTERS && multiCluster;

  // F104 rollups: one row per cluster, folded from data already on this page —
  // no request, no waterfall. Memoized so a poll tick doesn't re-fold the fleet
  // on every render.
  const rollups = useMemo(() => rollupClusters(allNodes, coverage?.nodes), [allNodes, coverage]);
  const clusterColors = useMemo(() => {
    const m: Record<string, string> = {};
    clusters.forEach((c) => { if (c.color) m[c.name] = c.color; });
    return m;
  }, [clusters]);
  // When the filter narrows to one cluster, keep its rollup visible above the
  // flat grid — otherwise selecting a cluster would hide the very summary the
  // user just drilled into.
  const selectedRollup = cluster === ALL_CLUSTERS ? null : rollups.find((r) => r.name === cluster) || null;

  // Per-node unprotected count (B2), surfaced on each card's legend instead of a
  // fleet-wide banner so the risk is pinned to the exact node.
  const unprotByNode = useMemo(() => {
    const m: Record<string, number> = {};
    coverage?.nodes.forEach((nd) => { m[nd.node_id] = nd.unprotected.length; });
    return m;
  }, [coverage]);
  // Per-node stopped-with-data-but-no-backup count (F13) — a quieter, separate
  // risk kept out of the primary "unprotected running" number above.
  const stoppedByNode = useMemo(() => {
    const m: Record<string, number> = {};
    coverage?.nodes.forEach((nd) => { m[nd.node_id] = nd.stopped_at_risk.length; });
    return m;
  }, [coverage]);

  useEffect(() => {
    api.recoveryState().then(setRecovery).catch(() => setRecovery(null));
  }, []);
  const setRecoveryStep = async (step: RecoveryState["step"]) => {
    setRecovery((r) => (r ? { ...r, step } : r)); // the card answers immediately
    try { await api.setRecoveryStep(step); }
    catch (e) { toast.error(`Couldn't save that: ${(e as Error).message}`); }
  };

  const gridCls = "grid items-start gap-5 [grid-template-columns:repeat(auto-fill,minmax(360px,1fr))]";
  const card = (visible: Node[]) => (n: Node, index: number) => (
    <NodeCard
      key={n.id} n={n}
      unprotected={unprotByNode[n.id] || 0}
      stoppedAtRisk={stoppedByNode[n.id] || 0}
      menuOpen={menuFor === n.id}
      menuRef={menuFor === n.id ? menuRef : undefined}
      onCard={() => {
        if (justDragged.current) { justDragged.current = false; return; }
        navigate(`/servers/${n.id}`);
      }}
      onMachine={(e) => { e.stopPropagation(); navigate(`/servers/${n.id}/machine`); }}
      onMenu={(e) => { e.stopPropagation(); setMenuFor(menuFor === n.id ? null : n.id); }}
      onBackup={() => backupNode(n)} backingUp={backingUp === n.id}
      onEdit={() => edit(n)} onDelete={() => remove(n)}
      onGrab={() => setGrabbedNodeId(n.id)}
      dropEdge={dropEdgeClass(dropIndex === index ? dragIndex : null, index)}
      drag={{
        draggable: grabbedNodeId === n.id,
        onDragStart: (e) => {
          justDragged.current = true;
          e.dataTransfer.effectAllowed = "move";
          e.dataTransfer.setData("text/plain", n.id); // Firefox starts no drag without a payload
          setDragIndex(index);
        },
        onDragOver: (e) => { if (dragIndex === null) return; e.preventDefault(); setDropIndex(index); },
        onDrop: (e) => { e.preventDefault(); if (dragIndex === null) return; reorder(visible.map((v) => v.id), dragIndex, index); clearDrag(); },
        onDragEnd: clearDrag,
      }}
    />
  );
  const connectTile = (
    <button
      onClick={(e) => { e.stopPropagation(); setAdding(true); }}
      className="group flex min-h-[200px] flex-col items-center justify-center gap-3.5 rounded-[14px] border-[1.5px] border-dashed border-surface-bright text-outline transition-colors hover:border-primary hover:text-primary"
    >
      <span className="flex h-[46px] w-[46px] items-center justify-center rounded-full border-[1.5px] border-current">
        <Plus size={22} />
      </span>
      <span className="text-[12px] font-bold uppercase tracking-[0.1em]">Connect New Node</span>
    </button>
  );

  return (
    <div>
      {/* Header */}
      <div className="mb-5 flex items-start gap-4">
        <div>
          <h1 className="text-[23px] font-bold tracking-tight">System Dashboard</h1>
          <p className="mt-1.5 text-sm text-on-surface-variant">
            Monitoring {reachable} active node{reachable === 1 ? "" : "s"} across {clusterList.length || 0} cluster{clusterList.length === 1 ? "" : "s"}.
          </p>
        </div>
        <div className="ml-auto flex items-center gap-2.5">
          <div className={`flex items-center gap-2 rounded-lg border px-3.5 py-2 ${allOK ? "border-success/25 bg-success/[0.08]" : "border-warning/25 bg-warning/[0.09]"}`}>
            <span
              className="h-[7px] w-[7px] rounded-full status-pulse"
              style={{ background: allOK ? "rgb(var(--c-success))" : "rgb(var(--c-warning))", boxShadow: `0 0 0 3px ${allOK ? "rgb(var(--c-success) / 0.18)" : "rgb(var(--c-warning) / 0.18)"}` }}
            />
            <span className={`text-[11px] font-bold uppercase tracking-[0.08em] ${allOK ? "text-success" : "text-warning"}`}>
              {allOK ? "All systems nominal" : `${reachable}/${nodes?.length ?? 0} reachable`}
            </span>
          </div>
          <button
            onClick={() => load(true)}
            className="flex items-center gap-2 rounded-lg border border-outline-variant bg-surface-low px-3.5 py-2 text-[11px] font-bold uppercase tracking-[0.08em] text-on-surface-variant transition-colors hover:text-on-surface"
          >
            <RefreshCw size={13} className={spin ? "animate-spin" : ""} /> Refresh
          </button>
        </div>
      </div>


      {/* Cluster filter (only shown for multi-cluster fleets, PLAN §4.13) */}
      {multiCluster && (
        <div className="mb-5">
          <ClusterBar nodes={allNodes} selected={cluster} onSelect={setCluster} colors={clusterColors} />
        </div>
      )}

      {/* Node grid */}
      {/* F223: after a total loss the empty dashboard was the least helpful screen
          in the product. It is now the runbook. */}
      {recovery && <RecoveryWizard state={recovery} onStep={setRecoveryStep} />}

      {nodes === null ? (
        <div className="text-on-surface-variant">Loading nodes…</div>
      ) : grouped ? (
        <div className="space-y-8">
          {rollups.map((r) => {
            const inCluster = allNodes.filter((n) => nodeCluster(n) === r.name);
            return (
            <section key={r.name}>
              <div className="mb-3">
                <ClusterBand r={r} color={clusterColors[r.name]} />
              </div>
              <div className={gridCls}>{inCluster.map(card(inCluster))}</div>
            </section>
            );
          })}
          <div className={gridCls}>{connectTile}</div>
        </div>
      ) : (
        <>
          {selectedRollup && (
            <div className="mb-4">
              <ClusterBand r={selectedRollup} color={clusterColors[selectedRollup.name]} />
            </div>
          )}
          <div className={gridCls}>
            {filtered.map(card(filtered))}
            {connectTile}
          </div>
        </>
      )}

      <AddNodeModal open={adding} edit={editing} onClose={() => { setAdding(false); setEditing(null); }} onAdded={load} />
    </div>
  );
}

// The cards flow left-to-right, so the insertion marker is a side edge rather than
// the top/bottom rule the Servers table uses.
function dropEdgeClass(fromIndex: number | null, index: number): string {
  if (fromIndex === null) return "";
  if (fromIndex > index) return "border-l-2 border-l-primary";
  if (fromIndex < index) return "border-r-2 border-r-primary";
  return "";
}

const SEGMENTS = [
  { key: "running", label: "running", color: "var(--dash-seg-running)" },
  { key: "stopped", label: "stopped", color: "var(--dash-seg-stopped)" },
  { key: "paused", label: "paused", color: "var(--dash-seg-paused)" },
  { key: "restarting", label: "restarting", color: "var(--dash-seg-restarting)" },
] as const;

function NodeCard({ n, unprotected, stoppedAtRisk, menuOpen, menuRef, onCard, onMachine, onMenu, onBackup, backingUp, onEdit, onDelete, drag, onGrab, dropEdge }: {
  n: Node; unprotected: number; stoppedAtRisk: number; menuOpen: boolean; menuRef?: React.RefObject<HTMLDivElement>;
  onCard: () => void; onMachine: (e: React.MouseEvent) => void; onMenu: (e: React.MouseEvent) => void;
  onBackup: () => void; backingUp: boolean; onEdit: () => void; onDelete: () => void;
  drag: React.ComponentPropsWithoutRef<"article">; onGrab: () => void; dropEdge: string;
}) {
  const s = n.summary;
  const healthy = !!s && s.restarting === 0 && s.stopped === 0;
  const accentVar = healthy ? "rgb(var(--c-success))" : "rgb(var(--c-warning))";
  const counts: Record<string, number> = s
    ? { running: s.running, stopped: s.stopped, paused: s.paused, restarting: s.restarting }
    : { running: 0, stopped: 0, paused: 0, restarting: 0 };

  return (
    <article
      {...drag}
      onClick={onCard}
      role="button"
      title="View containers on this node"
      style={{ boxShadow: "var(--dash-card-shadow)" }}
      className={`group relative flex cursor-pointer flex-col gap-4 rounded-[14px] border border-outline-variant bg-surface-container p-[18px] transition-colors hover:border-[rgb(var(--dash-card-hover-border))] ${dropEdge}`}
    >
      <button
        type="button"
        title="Drag to reorder"
        aria-label={`Reorder ${n.name}`}
        onMouseDown={onGrab}
        onClick={(e) => e.stopPropagation()}
        className="absolute left-1/2 top-0.5 -translate-x-1/2 cursor-grab rounded p-1 text-outline opacity-0 transition-opacity hover:text-on-surface focus-visible:opacity-100 group-hover:opacity-100 active:cursor-grabbing"
      >
        <GripHorizontal size={15} />
      </button>

      {/* Header */}
      <div className="flex items-start gap-3">
        <NodeAvatar size={40} />
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <span className="h-2 w-2 shrink-0 rounded-full" style={{ background: n.reachable ? accentVar : "rgb(var(--c-error))", boxShadow: `0 0 0 3px ${n.reachable ? (healthy ? "rgb(var(--c-success) / 0.16)" : "rgb(var(--c-warning) / 0.16)") : "rgb(var(--c-error) / 0.16)"}` }} />
            <span className="truncate text-[16px] font-bold tracking-tight">{n.name}</span>
            {n.reachable
              ? <Wifi size={15} className="shrink-0 text-success" strokeWidth={1.9} />
              : <WifiOff size={15} className="shrink-0 text-error status-pulse" strokeWidth={1.9} />}
          </div>
          {/* #40: a bulk transfer saturates this node's socket proxy, so DockBack
              stops asking it for figures until the transfer is done. Said as
              "paused", never as an error — the numbers below are the last real
              ones, and the node is working, not broken. */}
          {n.streaming && (
            <div className="mt-1 flex items-start gap-1.5 text-[11.5px] text-outline">
              <BackupCloudIcon size={12} active className="mt-[2px] shrink-0" />
              <span className="min-w-0 break-words">Backup in progress — stats paused</span>
            </div>
          )}
          <code className="mt-1 block truncate font-mono text-[11.5px] text-outline">{n.address}</code>
        </div>
        <div className="relative flex shrink-0 gap-0.5 text-outline" ref={menuOpen ? menuRef : undefined} onClick={(e) => e.stopPropagation()}>
          {/* F105: this used to be a second "open node" button — identical to the
              one beside it. It now opens the Machine page: hardware + live host
              utilisation. The containers button keeps its original behaviour. */}
          <button onClick={onMachine} title="Machine — hardware & live usage" className="flex rounded-md p-[5px] transition-colors hover:bg-surface-low hover:text-on-surface"><LineChart size={16} strokeWidth={1.8} /></button>
          <button onClick={onCard} title="View containers" className="flex rounded-md p-[5px] transition-colors hover:bg-surface-low hover:text-on-surface"><Boxes size={16} strokeWidth={1.8} /></button>
          <button onClick={onMenu} title="Manage" className="flex rounded-md p-[5px] transition-colors hover:bg-surface-low hover:text-on-surface"><Gear size={16} strokeWidth={1.8} /></button>
          {menuOpen && (
            <div className="absolute right-0 top-8 z-20 w-48 overflow-hidden rounded-md border border-outline-variant bg-surface-high shadow-xl">
              {/* F221: the pre-upgrade action. Only for a reachable node — the
                  call would fail on the inventory and read as a broken button. */}
              {n.reachable && (
                <button onClick={onBackup} disabled={backingUp} className="flex w-full items-center gap-2 px-3 py-2 text-left text-sm text-on-surface hover:bg-surface-highest disabled:opacity-60">
                  <BackupCloudIcon size={14} active={backingUp} className="shrink-0" />
                  <span className="min-w-0 break-words">Back up node now</span>
                </button>
              )}
              <button onClick={onEdit} className="flex w-full items-center gap-2 px-3 py-2 text-sm text-on-surface hover:bg-surface-highest"><Pencil size={14} /> Edit</button>
              <button onClick={onDelete} className="flex w-full items-center gap-2 px-3 py-2 text-sm text-error hover:bg-surface-highest"><Trash2 size={14} /> Delete</button>
            </div>
          )}
        </div>
      </div>

      {n.reachable && s ? (
        <>
          {/* Container distribution hero */}
          <div className="rounded-[11px] border border-outline-variant bg-surface-lowest p-[14px]">
            <div className="mb-[11px] flex items-end justify-between">
              <div className="flex items-baseline gap-[7px]">
                <span className="text-[28px] font-bold leading-none tracking-tight">{s.total}</span>
                <span className="text-[11px] font-semibold uppercase tracking-[0.1em] text-outline">containers</span>
              </div>
              <div className={`flex shrink-0 items-center gap-[7px] rounded-full border px-[11px] py-[5px] ${healthy ? "border-success/25 bg-success/[0.08]" : "border-warning/25 bg-warning/[0.09]"}`}>
                <span className="h-1.5 w-1.5 rounded-full" style={{ background: accentVar }} />
                <span className={`text-[11.5px] font-semibold ${healthy ? "text-success" : "text-warning"}`}>
                  {healthy ? "All containers healthy" : healthStr(s)}
                </span>
              </div>
            </div>
            {/* Segmented bar */}
            <div className="flex h-[9px] gap-[2px] overflow-hidden rounded-[5px]" style={{ background: "var(--dash-seg-track)" }}>
              {SEGMENTS.map((seg) => {
                const v = counts[seg.key];
                return (
                  <span key={seg.key} style={{ flexGrow: v, background: seg.color, minWidth: v > 0 ? 4 : 0, display: v > 0 ? "block" : "none" }} />
                );
              })}
            </div>
            {/* Status grid — the four container states plus the two protection
                metrics (unprotected / stopped-no-backup) as ONE uniform 3×2 grid, so
                every node card is the same height regardless of how many need
                attention (no wrapping). The two protection cells turn amber only when
                their count is > 0; otherwise they read as quiet zeros. */}
            <div className="mt-3 grid grid-cols-3 gap-[7px]">
              {SEGMENTS.map((seg) => (
                <div key={seg.key} className="flex min-w-0 items-center gap-[7px] rounded-lg border border-outline-variant bg-surface-high/40 px-[9px] py-[7px]">
                  <span className="h-2 w-2 shrink-0 rounded-[3px]" style={{ background: seg.color }} />
                  <div className="min-w-0 leading-tight">
                    <div className={`text-[13px] font-bold tnum ${counts[seg.key] === 0 ? "text-on-surface-variant" : ""}`}>{counts[seg.key]}</div>
                    <div className="truncate text-[10px] capitalize text-outline">{seg.label}</div>
                  </div>
                </div>
              ))}
              {/* Running-but-unprotected (B2) — the real backup gap; amber when any exist. */}
              <div
                title={unprotected > 0 ? `${unprotected} of ${s.total} containers have no backup — open the node to protect them` : "Every container has a backup"}
                className={`flex min-w-0 items-center gap-[7px] rounded-lg border px-[9px] py-[7px] ${unprotected > 0 ? "border-warning/40 bg-warning/[0.09]" : "border-outline-variant bg-surface-high/40"}`}
              >
                <Shield size={14} strokeWidth={1.9} className={`shrink-0 ${unprotected > 0 ? "text-warning" : "text-outline"}`} />
                <div className="min-w-0 leading-tight">
                  <div className={`text-[13px] font-bold tnum ${unprotected > 0 ? "" : "text-on-surface-variant"}`}>{unprotected}/{s.total}</div>
                  <div className={`truncate text-[10px] ${unprotected > 0 ? "text-warning" : "text-outline"}`}>unprotected</div>
                </div>
              </div>
              {/* Stopped-with-data-but-no-backup (F13) — same amber cue when > 0, but a
                  grey (stopped-tier) dot keeps it a notch below the running-unprotected alert. */}
              <div
                title={stoppedAtRisk > 0 ? `${stoppedAtRisk} stopped container${stoppedAtRisk === 1 ? "" : "s"} with data and no backup — open the node to protect them` : "No stopped container is missing a backup"}
                className={`flex min-w-0 items-center gap-[7px] rounded-lg border px-[9px] py-[7px] ${stoppedAtRisk > 0 ? "border-warning/40 bg-warning/[0.09]" : "border-outline-variant bg-surface-high/40"}`}
              >
                <span className="h-2 w-2 shrink-0 rounded-[3px]" style={{ background: "var(--dash-seg-stopped)" }} />
                <div className="min-w-0 leading-tight">
                  <div className={`text-[13px] font-bold tnum ${stoppedAtRisk > 0 ? "" : "text-on-surface-variant"}`}>{stoppedAtRisk}</div>
                  <div className={`truncate text-[10px] ${stoppedAtRisk > 0 ? "text-warning" : "text-outline"}`}>no backup</div>
                </div>
              </div>
            </div>
          </div>

          {/* CPU / MEMORY ring gauges */}
          <div className="flex gap-[14px]">
            <Gauge label="CPU" sub="load" pct={s.cpu_percent} color="var(--dash-gauge-cpu)" />
            <Gauge label="Memory" sub={fmtBytes(s.mem_used)} pct={s.mem_percent} color="var(--dash-gauge-mem)" />
          </div>

          {/* Inventory grid */}
          <div className="grid grid-cols-2 gap-px overflow-hidden rounded-[11px] border border-outline-variant bg-outline-variant">
            <InvCell icon={<ImageIcon size={18} strokeWidth={1.7} />} value={s.images} label="Images" />
            <InvCell icon={<Layers size={18} strokeWidth={1.7} />} value={s.stacks} label={`Stacks · ${s.stacks_running}/${s.stacks_stopped}/${s.stacks_errored}`} />
            <InvCell icon={<HardDrive size={18} strokeWidth={1.7} />} value={s.volumes} label="Volumes" />
            <InvCell icon={<Network size={18} strokeWidth={1.7} />} value={s.networks} label="Networks" />
          </div>
        </>
      ) : (
        <div className="flex items-center gap-2 rounded-[11px] border border-error/25 bg-error/10 px-3 py-3 text-sm text-error">
          <AlertTriangle size={16} /> {n.error || "Connection error"}
        </div>
      )}
    </article>
  );
}

function healthStr(s: NonNullable<Node["summary"]>): string {
  const parts: string[] = [];
  if (s.stopped > 0) parts.push(`${s.stopped} stopped`);
  if (s.restarting > 0) parts.push(`${s.restarting} restarting`);
  return parts.join(", ") || "Degraded";
}

function Gauge({ label, sub, pct, color }: { label: string; sub: string; pct: number; color: string }) {
  return (
    <div className="flex flex-1 items-center gap-3 rounded-[11px] border border-outline-variant bg-surface-lowest px-[13px] py-3">
      {/* Ring enlarged 54→62px and stroke thinned 7→6 to widen the centre hole, so a
          double-digit reading (e.g. 82.4%) is no longer clipped by the ring. r stays 24
          so ringDash/RING_CIRC are unchanged. */}
      <div className="relative h-[62px] w-[62px] shrink-0">
        <svg width="62" height="62" viewBox="0 0 64 64" style={{ transform: "rotate(-90deg)" }}>
          <circle cx="32" cy="32" r="24" fill="none" stroke="var(--dash-gauge-track)" strokeWidth="6" />
          <circle cx="32" cy="32" r="24" fill="none" stroke={color} strokeWidth="6" strokeLinecap="round" strokeDasharray={ringDash(pct)} />
        </svg>
        <div className="absolute inset-0 grid place-items-center text-[12px] font-bold tnum">{pct.toFixed(1)}%</div>
      </div>
      <div className="min-w-0">
        <div className="text-[10.5px] font-bold uppercase tracking-[0.1em] text-outline">{label}</div>
        <div className="mt-0.5 truncate text-[12px] text-on-surface-variant">{sub}</div>
      </div>
    </div>
  );
}

function InvCell({ icon, value, label }: { icon: React.ReactNode; value: number; label: string }) {
  return (
    <div className="flex items-center gap-[11px] bg-surface-container px-[14px] py-[13px]">
      <span className="text-outline">{icon}</span>
      <div className="min-w-0">
        <div className="text-[16px] font-bold">{value}</div>
        <div className="mt-px truncate text-[11px] text-outline">{label}</div>
      </div>
    </div>
  );
}
