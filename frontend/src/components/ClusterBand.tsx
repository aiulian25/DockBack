// ClusterBand — the per-cluster rollup header on the Dashboard (F104).
//
// It answers the one question a fleet owner asks at a glance: is this blast
// radius covered? Reachability, protection and the oldest last-backup for the
// cluster, beside the cluster's own name and colour.
//
// PERFORMANCE: this makes NO request of its own. Every number is derived from
// the nodes + coverage payloads the Dashboard already loads, so grouping a fleet
// by cluster costs zero extra server work and adds no waterfall.
//
// ACCESSIBILITY: the cluster's colour is decorative identity only. Health is
// carried by the green/amber/red status palette AND by a word, never by colour
// alone, and the two palettes are deliberately disjoint. The cluster's own colour
// appears ONLY as a thin rule under its name — never a dot, swatch or edge stripe.
import { Node, CoverageNode } from "../api";
import { clusterAccent, nodeCluster } from "./ClusterBar";

export interface ClusterRollup {
  name: string;
  nodes: number;
  reachable: number;
  running: number;
  protected: number;
  oldestBackup: number; // unix seconds of the STALEST node's newest backup; 0 = some node has none
  neverBackedUp: number; // nodes in this cluster with no successful backup at all
}

// rollupClusters folds the fleet into one row per cluster. Pure, so it is
// unit-testable and cheap enough to run on every render.
export function rollupClusters(nodes: Node[], coverage?: CoverageNode[] | null): ClusterRollup[] {
  const cov = new Map<string, CoverageNode>();
  coverage?.forEach((c) => cov.set(c.node_id, c));

  const by = new Map<string, ClusterRollup>();
  for (const n of nodes) {
    const name = nodeCluster(n);
    let r = by.get(name);
    if (!r) {
      r = { name, nodes: 0, reachable: 0, running: 0, protected: 0, oldestBackup: 0, neverBackedUp: 0 };
      by.set(name, r);
    }
    r.nodes++;
    if (n.reachable) r.reachable++;
    const c = cov.get(n.id);
    if (!c) continue;
    r.running += c.running;
    r.protected += c.protected;
    if (!c.last_backup_at) r.neverBackedUp++;
    else if (r.oldestBackup === 0 || c.last_backup_at < r.oldestBackup) r.oldestBackup = c.last_backup_at;
  }
  return [...by.values()].sort((a, b) => a.name.localeCompare(b.name));
}

// relAge renders a coarse age. Locale-safe by construction: it composes a number
// with a unit word rather than formatting a date, so it never depends on a
// locale's date order and stays short in any translation.
function relAge(seconds: number): { text: string; tone: "ok" | "warn" | "bad" } {
  const h = Math.floor(seconds / 3600);
  if (h < 1) return { text: "< 1 h", tone: "ok" };
  if (h < 48) return { text: `${h} h`, tone: h < 26 ? "ok" : "warn" };
  const d = Math.floor(h / 24);
  return { text: `${d} d`, tone: d < 7 ? "warn" : "bad" };
}

const TONE = {
  ok: "text-success",
  warn: "text-warning",
  bad: "text-error",
} as const;

// Bar renders a two-segment proportion. `good` is the healthy share; the rest is
// drawn in `restTone`. An empty total renders an inert track rather than a
// misleading full bar.
function Bar({ good, total, restTone }: { good: number; total: number; restTone: string }) {
  const pct = total > 0 ? Math.round((good / total) * 100) : 0;
  return (
    <div className="mt-1.5 flex h-[4px] gap-[2px] overflow-hidden bg-surface-highest" aria-hidden>
      {total > 0 && <span className="bg-success" style={{ width: `${pct}%` }} />}
      {total > 0 && pct < 100 && <span className={restTone} style={{ width: `${100 - pct}%` }} />}
    </div>
  );
}

function Metric({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="min-w-0">
      <div className="truncate text-[10px] font-bold uppercase tracking-[0.09em] text-outline">{label}</div>
      {children}
    </div>
  );
}

export default function ClusterBand({ r, color, count }: {
  r: ClusterRollup;
  color?: string;
  count?: number; // node count from the registry when it differs (e.g. filtered view)
}) {
  const accent = clusterAccent(r.name, color);
  const unreachable = r.nodes - r.reachable;
  const unprotected = r.running - r.protected;

  // "Oldest backup" is the STALEST node in the cluster — the weakest link, which
  // is what a blast-radius rollup should surface. A node that has never been
  // backed up isn't an age at all, so it is reported as a count instead of being
  // folded into a misleadingly recent number.
  const age = r.oldestBackup ? relAge(Math.max(0, Math.floor(Date.now() / 1000) - r.oldestBackup)) : null;

  return (
    <div className="grid grid-cols-2 items-center gap-x-5 gap-y-3 rounded-xl border border-outline-variant bg-surface-high px-4 py-3 sm:grid-cols-[minmax(0,1.3fr)_repeat(3,minmax(0,0.8fr))]">
      <div className="min-w-0">
        {/* The cluster's identity colour is a thin rule under its NAME — no dot,
            no swatch, no edge stripe. It sizes itself to the text, so it reads as
            part of the label rather than as another element in the layout. */}
        <h2 className="truncate text-[15px] font-bold tracking-tight">
          <span className="border-b-2 pb-0.5" style={{ borderColor: accent }}>{r.name}</span>
        </h2>
        <div className="mt-1.5 truncate text-xs text-on-surface-variant">
          {count ?? r.nodes} node{(count ?? r.nodes) === 1 ? "" : "s"} · {r.running} running container{r.running === 1 ? "" : "s"}
        </div>
      </div>

      <Metric label="Reachable">
        <div className={`tnum text-[17px] font-bold ${unreachable > 0 ? TONE.bad : TONE.ok}`}>
          {r.reachable}
          <span className="ml-0.5 text-[11.5px] font-semibold text-on-surface-variant"> / {r.nodes}</span>
        </div>
        <Bar good={r.reachable} total={r.nodes} restTone="bg-error" />
      </Metric>

      <Metric label="Protected">
        <div className={`tnum text-[17px] font-bold ${unprotected > 0 ? TONE.warn : TONE.ok}`}>
          {r.protected}
          <span className="ml-0.5 text-[11.5px] font-semibold text-on-surface-variant"> / {r.running}</span>
        </div>
        <Bar good={r.protected} total={r.running} restTone="bg-warning" />
      </Metric>

      <Metric label="Oldest backup">
        {r.neverBackedUp > 0 ? (
          <div className={`tnum text-[17px] font-bold ${TONE.bad}`}>
            {r.neverBackedUp}
            <span className="ml-0.5 text-[11.5px] font-semibold text-on-surface-variant"> never</span>
          </div>
        ) : age ? (
          <div className={`tnum text-[17px] font-bold ${TONE[age.tone]}`}>{age.text}</div>
        ) : (
          <div className="text-[17px] font-bold text-on-surface-variant">—</div>
        )}
        <div className="mt-1.5 h-[4px]" aria-hidden />
      </Metric>
    </div>
  );
}
