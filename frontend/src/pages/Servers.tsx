import { useEffect, useState } from "react";
import { useNodeActions } from "../hooks/useNodeActions";
import { useNavigate } from "react-router-dom";
import { Trash2, Plus, ChevronRight, Wifi, WifiOff, Pencil, Loader2, Copy, ShieldAlert, GripVertical } from "lucide-react";
import { api, Node, Cluster, fmtAgo } from "../api";
import { copyText, hasTextSelection } from "../clipboard";
import { Button, Card, Chip } from "../components/ui";
import { useToast } from "../components/Toast";
import AddNodeModal from "../components/AddNodeModal";
import { NodeLogo } from "../components/NodeAvatar";
import ClusterBar, { ALL_CLUSTERS, clusterCounts, nodeCluster } from "../components/ClusterBar";

// Width of the loading/error/empty rows. The cluster column only exists for
// multi-cluster fleets, so the span has two values.
const BASE_COLUMN_COUNT = 6;
const CLUSTERED_COLUMN_COUNT = BASE_COLUMN_COUNT + 1;

export default function Servers() {
  // null = not loaded yet (distinguishes the initial fetch from a real empty
  // fleet); err flags a failed fetch so "no nodes" isn't shown as a load error.
  const [nodes, setNodes] = useState<Node[] | null>(null);
  const [err, setErr] = useState(false);
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<Node | null>(null);
  const [cluster, setCluster] = useState(ALL_CLUSTERS); // cluster filter (PLAN §4.13)
  const navigate = useNavigate();
  const toast = useToast();
  // F104: cluster colours, so a cluster looks the same here as on the dashboard.
  // Metadata only — the node list itself is unchanged and this never blocks it.
  const [clusters, setClusters] = useState<Cluster[]>([]);
  const load = () => api.nodes().then((n) => { setNodes(n); setErr(false); }).catch(() => setErr(true));
  const {
    pendingDelete: pendingDel, forgetNode,
    grabbedNodeId, setGrabbedNodeId, dragIndex, setDragIndex, dropIndex, setDropIndex,
    justDragged, clearDrag, reorderNodes,
  } = useNodeActions({ nodes, setNodes, reload: load });
  // The row's own click handler must not fire when the button inside it does.
  const remove = (n: Node, e: React.MouseEvent) => { e.stopPropagation(); forgetNode(n); };
  const reorder = (fromIndex: number, toIndex: number) => reorderNodes(filtered.map((n) => n.id), fromIndex, toIndex);
  useEffect(() => { load(); }, []);
  useEffect(() => { api.listClusters().then((r) => setClusters(r.clusters)).catch(() => {}); }, []);
  const clusterColors = Object.fromEntries(clusters.filter((c) => c.color).map((c) => [c.name, c.color]));

  const list = (nodes ?? []).filter((n) => !pendingDel.has(n.id));
  const multiCluster = clusterCounts(list).length > 1;
  const filtered = cluster === ALL_CLUSTERS ? list : list.filter((n) => nodeCluster(n) === cluster);
  const columnCount = multiCluster ? CLUSTERED_COLUMN_COUNT : BASE_COLUMN_COUNT;

  const edit = (n: Node, e: React.MouseEvent) => { e.stopPropagation(); setEditing(n); setAdding(true); };

  return (
    <div>
      <div className="mb-6 flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold">Servers</h1>
          <p className="mt-1 text-on-surface-variant">Managed Docker hosts. Click a node to choose what to back up.</p>
        </div>
        <Button variant="primary" onClick={() => setAdding(true)}><Plus size={16} /> New Server</Button>
      </div>

      {/* Cluster filter (only for multi-cluster fleets, PLAN §4.13) */}
      {multiCluster && (
        <div className="mb-4">
          <ClusterBar nodes={list} selected={cluster} onSelect={setCluster} colors={clusterColors} />
        </div>
      )}

      <Card>
        <table className="w-full text-sm">
          <thead className="border-b border-outline-variant/60 text-left text-xs uppercase tracking-wider text-on-surface-variant">
            <tr><th className="w-8 py-3 pl-4 pr-0"><span className="sr-only">Reorder</span></th><th className="px-5 py-3">Status</th><th className="px-5 py-3">Name</th>{multiCluster && <th className="px-5 py-3">Cluster</th>}<th className="px-5 py-3">Endpoint</th><th className="px-5 py-3">Last sync</th><th className="px-5 py-3 text-right">Actions</th></tr>
          </thead>
          <tbody>
            {nodes === null && !err && (
              <tr><td colSpan={columnCount} className="px-5 py-10 text-center text-on-surface-variant"><span className="inline-flex items-center gap-2"><Loader2 size={15} className="animate-spin" /> Loading servers…</span></td></tr>
            )}
            {err && list.length === 0 && (
              <tr><td colSpan={columnCount} className="px-5 py-10 text-center text-error">Couldn't load servers. Check your connection and try again.</td></tr>
            )}
            {nodes !== null && filtered.map((n, index) => {
              const hoveredByDrag = dropIndex === index;
              const dropAbove = hoveredByDrag && dragIndex !== null && dragIndex > index;
              const dropBelow = hoveredByDrag && dragIndex !== null && dragIndex < index;
              const topEdge = dropAbove ? "border-t-2 border-t-primary" : "";
              const bottomEdge = dropBelow ? "border-b-2 border-b-primary" : "border-b border-outline-variant/30";
              return (
              <tr
                key={n.id}
                draggable={grabbedNodeId === n.id}
                onMouseDown={() => { justDragged.current = false; }}
                onDragStart={(e) => {
                  justDragged.current = true;
                  e.dataTransfer.effectAllowed = "move";
                  e.dataTransfer.setData("text/plain", n.id); // Firefox starts no drag without a payload
                  setDragIndex(index);
                }}
                onDragOver={(e) => { if (dragIndex === null) return; e.preventDefault(); setDropIndex(index); }}
                onDrop={(e) => { e.preventDefault(); if (dragIndex === null) return; reorder(dragIndex, index); clearDrag(); }}
                onDragEnd={clearDrag}
                // Never navigate on the mouse-up that ends a drag-selection, so
                // the endpoint (or any row text) can be selected and copied, nor
                // on the click that trails a reorder.
                onClick={() => {
                  if (hasTextSelection()) return;
                  if (justDragged.current) { justDragged.current = false; return; }
                  navigate(`/servers/${n.id}`);
                }}
                className={`group cursor-pointer hover:bg-surface-high/40 ${topEdge} ${bottomEdge}`}
              >
                <td className="w-8 py-4 pl-4 pr-0">
                  <button
                    type="button"
                    title="Drag to reorder"
                    aria-label={`Reorder ${n.name}`}
                    onMouseDown={() => setGrabbedNodeId(n.id)}
                    onClick={(e) => e.stopPropagation()}
                    className="cursor-grab rounded p-1 text-outline transition-colors hover:text-on-surface active:cursor-grabbing"
                  >
                    <GripVertical size={15} />
                  </button>
                </td>
                <td className="px-5 py-4">
                  {n.host_key_changed ? (
                    <span title="This server presented a DIFFERENT SSH host key than the one pinned — possible reinstall or man-in-the-middle. DockBack refuses to connect until you reset the pinned key on the server's page.">
                      <Chip kind="err"><ShieldAlert size={12} /> Host key changed</Chip>
                    </span>
                  ) : (
                    <Chip kind={n.reachable ? "ok" : "err"}>{n.reachable ? <><Wifi size={12} /> Healthy</> : <><WifiOff size={12} /> Offline</>}</Chip>
                  )}
                </td>
                <td className="px-5 py-4 font-medium"><span className="flex items-center gap-2"><NodeLogo size={18} />{n.name}</span></td>
                {multiCluster && <td className="px-5 py-4"><span className="rounded-full border border-outline-variant bg-surface-high/40 px-2 py-0.5 text-xs text-on-surface-variant">{nodeCluster(n)}</span></td>}
                <td className="px-5 py-4 font-mono text-xs text-on-surface-variant">
                  {/* Copy-safe zone: clicking (or double-clicking to select a
                      word of) the endpoint never navigates. */}
                  <span className="inline-flex items-center gap-1.5">
                    <span className="cursor-text select-text" onClick={(e) => e.stopPropagation()}>{n.address}</span>
                    <button
                      title="Copy endpoint"
                      onClick={async (e) => {
                        e.stopPropagation();
                        (await copyText(n.address)) ? toast.success("Endpoint copied") : toast.error("Couldn't access the clipboard");
                      }}
                      className="rounded p-1 text-on-surface-variant opacity-0 transition-opacity hover:bg-surface-highest hover:text-on-surface focus-visible:opacity-100 group-hover:opacity-100"
                    >
                      <Copy size={13} />
                    </button>
                  </span>
                </td>
                <td className="px-5 py-4 tnum text-on-surface-variant">{fmtAgo(n.last_seen)}</td>
                <td className="px-5 py-4">
                  <div className="flex items-center justify-end gap-2">
                    <Button variant="ghost" onClick={(e) => edit(n, e)} title="Edit connection"><Pencil size={16} /></Button>
                    <Button variant="ghost" onClick={(e) => remove(n, e)} title="Forget node"><Trash2 size={16} /></Button>
                    <ChevronRight size={16} className="text-on-surface-variant" />
                  </div>
                </td>
              </tr>
              );
            })}
            {nodes !== null && !err && filtered.length === 0 && <tr><td colSpan={columnCount} className="px-5 py-10 text-center text-on-surface-variant">{list.length === 0 ? "No nodes yet. Add your first server." : "No nodes in this cluster."}</td></tr>}
          </tbody>
        </table>
      </Card>

      <AddNodeModal open={adding} edit={editing} onClose={() => { setAdding(false); setEditing(null); }} onAdded={load} />
    </div>
  );
}
