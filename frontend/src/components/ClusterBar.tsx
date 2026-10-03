// ClusterBar — a chip filter for grouping/filtering nodes by their cluster
// (PLAN §4.13 clusters/tags). Renders nothing when there's only one cluster, so
// single-cluster ("default") setups stay clutter-free. The cluster value already
// lives on each node; this adds no schema.
import { Node } from "../api";

export const ALL_CLUSTERS = "all";

export interface ClusterCount {
  name: string;
  count: number;
}

// clusterCounts returns the distinct clusters (treating empty as "default") with
// node counts, sorted by name.
export function clusterCounts(nodes: Node[]): ClusterCount[] {
  const m = new Map<string, number>();
  for (const n of nodes) {
    const c = n.cluster || "default";
    m.set(c, (m.get(c) || 0) + 1);
  }
  return [...m.entries()]
    .map(([name, count]) => ({ name, count }))
    .sort((a, b) => a.name.localeCompare(b.name));
}

export function nodeCluster(n: Node): string {
  return n.cluster || "default";
}

// Identity colours for clusters (F104).
//
// These are drawn as a THIN RULE UNDER THE CLUSTER NAME — never as a dot, pill
// or swatch. A coloured shape beside a label reads as visual noise and competes
// with the genuine status signals; an underline identifies just as well while
// adding nothing to the layout.
//
// The set is deliberately MUTED and generous: eight desaturated tones, so even a
// large fleet stays distinguishable without any cluster shouting. Hues are kept
// clear of the reserved status palette (green/amber/red) so an identity colour
// can never be misread as health, and every colour is mid-tone so the same value
// reads correctly against both the dark and light surfaces.
//
// A cluster may set its own colour in Settings; this is the fallback, picked
// deterministically from the name so the same cluster keeps the same colour
// across pages and reloads without storing anything.
export const CLUSTER_ACCENTS = [
  "#6f9fd8", // soft blue
  "#4fa39a", // muted teal
  "#9184c9", // muted violet
  "#b98aa8", // dusty mauve
  "#5fa8c4", // muted cyan
  "#a87fb0", // muted orchid
  "#7f92a8", // slate
  "#8b93b8", // periwinkle grey
];

export function clusterAccent(name: string, explicit?: string): string {
  if (explicit) return explicit;
  let h = 0;
  for (let i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) >>> 0;
  return CLUSTER_ACCENTS[h % CLUSTER_ACCENTS.length];
}

export default function ClusterBar({ nodes, selected, onSelect, colors }: {
  nodes: Node[];
  selected: string;
  onSelect: (cluster: string) => void;
  colors?: Record<string, string>; // cluster name -> colour set in Settings (F104)
}) {
  const clusters = clusterCounts(nodes);
  if (clusters.length <= 1) return null; // nothing to filter when one cluster

  // The cluster's colour rides as a thin rule UNDER its name inside the chip —
  // no dot, no swatch. "All" carries no colour because it isn't one cluster.
  const chip = (key: string, label: string, count: number, tinted?: boolean) => {
    const active = selected === key;
    return (
      <button
        key={key}
        onClick={() => onSelect(key)}
        aria-pressed={active}
        className={`flex items-center gap-1.5 rounded-full border px-3 py-1 text-xs font-medium transition-colors ${
          active ? "border-primary bg-primary/15 text-primary" : "border-outline-variant text-on-surface-variant hover:text-on-surface"
        }`}
      >
        <span
          className={tinted ? "border-b-2 pb-px" : undefined}
          style={tinted ? { borderColor: clusterAccent(label, colors?.[label]) } : undefined}
        >
          {label}
        </span>
        <span className={`tnum rounded-full px-1.5 text-[10px] ${active ? "bg-primary/20" : "bg-surface-highest"}`}>{count}</span>
      </button>
    );
  };

  return (
    <div className="flex flex-wrap items-center gap-2">
      <span className="mr-0.5 text-[11px] font-semibold uppercase tracking-wider text-outline">Cluster</span>
      {chip(ALL_CLUSTERS, "All", nodes.length)}
      {clusters.map((c) => chip(c.name, c.name, c.count, true))}
    </div>
  );
}
