// Insights — surfaces the time-series/derived data DockBack already computes but
// never showed (Fable-UI-UX B6): backup success/verified trend, destination
// capacity forecast + history, restore-drill confidence, and RPO adherence.
// Read-only, theme-token driven, hand-drawn SVG in the Dashboard's gauge style
// (no chart dependency). All values come from /api/insights — nothing hardcoded.
import { useEffect, useMemo, useRef, useState, useCallback } from "react";
import { fmtDuration } from "../lib/format";
import { Link } from "react-router-dom";
import {
  ShieldCheck, ShieldAlert, HardDrive, Loader2, AlertTriangle, Gauge as GaugeIcon,
  CheckCircle2, XCircle, Clock, TrendingUp,
} from "lucide-react";
import { api, Insights as InsightsData, InsightsDest, InsightsDay, InsightsGrowth, fmtBytes } from "../api";
import { Card } from "../components/ui";
import { Sparkline } from "../components/Sparkline";
import { usePoll } from "../hooks/usePoll";

const RING_CIRC = 2 * Math.PI * 24;
const ringDash = (pct: number) => `${((Math.min(Math.max(pct, 0), 100) / 100) * RING_CIRC).toFixed(1)} ${RING_CIRC.toFixed(1)}`;

const dayLabel = (ts: number) => new Date(ts * 1000).toLocaleDateString(undefined, { month: "short", day: "numeric" });

export default function Insights() {
  const [data, setData] = useState<InsightsData | null>(null);
  const [err, setErr] = useState(false);

  const load = useCallback(() => api.insights().then((d) => { setData(d); setErr(false); }).catch(() => setErr(true)), []);
  useEffect(() => { load(); }, [load]);
  usePoll(load, 60000); // cheap aggregate; refresh slowly, never in hidden tabs

  if (!data && err) return <div className="py-16 text-center text-error">Couldn't load insights. Retrying…</div>;
  if (!data) return <div className="flex items-center justify-center gap-2 py-16 text-on-surface-variant"><Loader2 size={16} className="animate-spin" /> Computing insights…</div>;

  const { fleet, backups_daily, destinations, drills, rpo, top_growth } = data;
  const drillPct = drills.total > 0 ? (drills.passed / drills.total) * 100 : 0;

  return (
    <div>
      <div className="mb-5">
        <h1 className="text-[23px] font-bold tracking-tight">Insights</h1>
        <p className="mt-1.5 text-sm text-on-surface-variant">Trends across your fleet — backup health, capacity outlook, restore confidence, and RPO adherence.</p>
      </div>

      {/* Headline stat tiles */}
      <div className="mb-5 grid gap-4 [grid-template-columns:repeat(auto-fit,minmax(200px,1fr))]">
        <StatTile label="Verified success (30d)" value={`${fleet.backup_success_rate.toFixed(0)}%`}
          accent={fleet.backup_success_rate >= 99 ? "success" : fleet.backup_success_rate >= 90 ? "warning" : "error"}
          sub={`${fleet.backups_verified}/${fleet.backups_30d} backups verified`} icon={<ShieldCheck size={16} />} />
        <StatTile label="Backups (30d)" value={`${fleet.backups_30d}`} accent="primary" sub="completed runs" icon={<TrendingUp size={16} />} />
        <RingTile label="Restore-drill confidence" pct={drillPct} total={drills.total}
          sub={drills.total > 0 ? `${drills.passed}/${drills.total} passed` : "no drills yet"} />
        <StatTile label="RPO adherence" value={rpo.total > 0 ? `${rpo.meeting}/${rpo.total}` : "—"}
          accent={rpo.total === 0 ? "muted" : rpo.breaching === 0 ? "success" : "warning"}
          sub={rpo.total > 0 ? `${rpo.breaching} breaching target` : "no critical DBs configured"} icon={<GaugeIcon size={16} />} />
      </div>

      {/* Backup activity trend */}
      <Card className="mb-5 p-5">
        <div className="mb-1 flex items-center justify-between">
          <h2 className="text-sm font-bold uppercase tracking-[0.08em] text-on-surface-variant">Backup activity</h2>
          <Legend items={[["success", "Verified"], ["primary", "Backed up"], ["error", "Failed"]]} />
        </div>
        {backups_daily.length === 0
          ? <Empty>No backups in the last 90 days yet.</Empty>
          : <ActivityBars days={backups_daily} />}
      </Card>

      {/* Destination capacity forecast. The primary backups volume (id
          "local:primary", F42) is always present, so the "add an offsite
          destination" nudge is keyed off whether any EXTERNAL destination exists. */}
      <div className="mb-5">
        <h2 className="mb-2.5 text-sm font-bold uppercase tracking-[0.08em] text-on-surface-variant">Capacity outlook</h2>
        <div className="grid gap-4 [grid-template-columns:repeat(auto-fill,minmax(340px,1fr))]">
          {destinations.map((d) => <CapacityCard key={d.id} d={d} />)}
        </div>
        {destinations.every((d) => d.id === "local:primary") && (
          <Card className="mt-4 p-5"><Empty>Only the local backups volume so far. Add an offsite destination in Settings for a 3-2-1 copy and its own fill-up forecast.</Empty></Card>
        )}
      </div>

      {/* Largest & fastest-growing backups (F11) */}
      {top_growth && top_growth.length > 0 && (
        <div className="mb-5">
          <h2 className="mb-2.5 text-sm font-bold uppercase tracking-[0.08em] text-on-surface-variant">Largest &amp; fastest-growing backups</h2>
          <Card className="p-5">
            <div className="divide-y divide-outline-variant/40">
              {top_growth.map((g) => <GrowthRow key={`${g.node_id}/${g.container}`} g={g} />)}
            </div>
          </Card>
        </div>
      )}

      {/* Restore-drill history + RPO adherence */}
      <div className="grid gap-4 [grid-template-columns:repeat(auto-fill,minmax(340px,1fr))]">
        <Card className="p-5">
          <h2 className="mb-3 text-sm font-bold uppercase tracking-[0.08em] text-on-surface-variant">Recent restore drills</h2>
          {drills.recent.length === 0 ? (
            <Empty>No restore drills recorded yet. DockBack test-restores backups into an isolated sandbox on a cadence.</Empty>
          ) : (
            <div className="space-y-1.5">
              {drills.recent.map((r) => (
                <div key={r.backup_id} className="flex items-center gap-2.5 rounded-lg bg-surface-lowest px-3 py-2">
                  {r.ok ? <CheckCircle2 size={15} className="shrink-0 text-success" /> : <XCircle size={15} className="shrink-0 text-error" />}
                  <span className="min-w-0 flex-1">
                    <span className="block truncate text-sm text-on-surface">
                      {r.target || <span className="font-mono text-on-surface-variant">{r.backup_id.slice(0, 8)}</span>}
                      {r.stack && <span className="text-on-surface-variant"> · {r.stack}</span>}
                    </span>
                    <span className="block truncate text-xs text-on-surface-variant">
                      <span className={r.ok ? "text-success" : "text-error"}>{r.ok ? "Restore verified" : "Restore failed"}</span>
                      {r.detail ? ` — ${r.detail}` : ""}
                    </span>
                  </span>
                  {r.node_name && <span className="shrink-0 text-[11px] text-on-surface-variant">{r.node_name}</span>}
                </div>
              ))}
            </div>
          )}
        </Card>

        <Card className="p-5">
          <h2 className="mb-3 text-sm font-bold uppercase tracking-[0.08em] text-on-surface-variant">RPO adherence</h2>
          {rpo.total === 0 ? (
            <Empty>No critical databases configured. Mark a database container as critical to hold a fresh verified dump within a target window.</Empty>
          ) : (
            <div className="space-y-1.5">
              {rpo.containers.map((c) => (
                <div key={`${c.node}/${c.container}`} className="flex items-center gap-2.5 rounded-lg bg-surface-lowest px-3 py-2">
                  {c.met ? <ShieldCheck size={15} className="shrink-0 text-success" /> : <ShieldAlert size={15} className="shrink-0 text-warning" />}
                  <span className="min-w-0 flex-1">
                    <span className="block truncate text-sm text-on-surface">{c.container}</span>
                    <span className="block truncate text-xs text-on-surface-variant">{c.node}</span>
                  </span>
                  <span className="shrink-0 text-right">
                    <span className={`block text-xs font-semibold ${c.met ? "text-success" : "text-warning"}`}>
                      {c.measured_seconds < 0 ? "no backup" : fmtDuration(c.measured_seconds, "compact")}
                    </span>
                    <span className="block text-[11px] text-on-surface-variant">target {fmtDuration(c.target_seconds, "compact")}</span>
                  </span>
                </div>
              ))}
            </div>
          )}
        </Card>
      </div>
    </div>
  );
}

// --- small building blocks ---

function Empty({ children }: { children: React.ReactNode }) {
  return <div className="flex items-center gap-2 py-6 text-sm text-on-surface-variant"><AlertTriangle size={15} className="shrink-0 opacity-70" /> {children}</div>;
}

type Accent = "success" | "warning" | "error" | "primary" | "muted";
const accentText: Record<Accent, string> = {
  success: "text-success", warning: "text-warning", error: "text-error", primary: "text-primary", muted: "text-on-surface-variant",
};

function StatTile({ label, value, sub, accent, icon }: { label: string; value: string; sub: string; accent: Accent; icon: React.ReactNode }) {
  return (
    <Card className="p-[18px]">
      <div className="flex items-center gap-2 text-on-surface-variant"><span className={accentText[accent]}>{icon}</span><span className="text-[11px] font-bold uppercase tracking-[0.1em]">{label}</span></div>
      <div className={`mt-2 text-[30px] font-bold leading-none tracking-tight ${accentText[accent]}`}>{value}</div>
      <div className="mt-1.5 text-xs text-on-surface-variant">{sub}</div>
    </Card>
  );
}

function RingTile({ label, pct, total, sub }: { label: string; pct: number; total: number; sub: string }) {
  const color = total === 0 ? "rgb(var(--c-on-surface-variant))" : pct >= 99 ? "rgb(var(--c-success))" : pct >= 80 ? "rgb(var(--c-warning))" : "rgb(var(--c-error))";
  return (
    <Card className="flex items-center gap-3.5 p-[18px]">
      <div className="relative h-[58px] w-[58px] shrink-0">
        <svg width="58" height="58" viewBox="0 0 64 64" style={{ transform: "rotate(-90deg)" }}>
          <circle cx="32" cy="32" r="24" fill="none" stroke="rgb(var(--c-surface-highest))" strokeWidth="7" />
          <circle cx="32" cy="32" r="24" fill="none" stroke={color} strokeWidth="7" strokeLinecap="round" strokeDasharray={ringDash(total === 0 ? 0 : pct)} />
        </svg>
        <div className="absolute inset-0 grid place-items-center text-[12px] font-bold">{total === 0 ? "—" : `${pct.toFixed(0)}%`}</div>
      </div>
      <div className="min-w-0">
        <div className="text-[11px] font-bold uppercase tracking-[0.1em] text-on-surface-variant">{label}</div>
        <div className="mt-1 text-xs text-on-surface-variant">{sub}</div>
      </div>
    </Card>
  );
}

function Legend({ items }: { items: [string, string][] }) {
  return (
    <div className="flex flex-wrap items-center gap-x-3.5 gap-y-1">
      {items.map(([tok, lbl]) => (
        <span key={lbl} className="flex items-center gap-1.5 text-[11px] text-on-surface-variant">
          <span className="h-2 w-2 rounded-[3px]" style={{ background: `rgb(var(--c-${tok}))` }} /> {lbl}
        </span>
      ))}
    </div>
  );
}

// ActivityBars — stacked daily bars: verified / other-success / failed. Hover shows
// the day's counts. One y-axis (counts), 2px surface gaps between stacked segments.
function ActivityBars({ days }: { days: InsightsDay[] }) {
  const [hover, setHover] = useState<number | null>(null);
  const W = 720, H = 150, padB = 18, padT = 8;
  const max = Math.max(1, ...days.map((d) => d.total));
  const n = days.length;
  const slot = W / n;
  const bw = Math.min(22, Math.max(3, slot * 0.7));
  const y = (v: number) => padT + (H - padT - padB) * (1 - v / max);

  return (
    <div className="relative">
      <svg viewBox={`0 0 ${W} ${H}`} className="w-full" style={{ height: 170 }} preserveAspectRatio="none"
        onMouseLeave={() => setHover(null)}>
        {/* baseline */}
        <line x1="0" y1={H - padB} x2={W} y2={H - padB} stroke="rgb(var(--c-outline-variant))" strokeWidth="1" />
        {days.map((d, i) => {
          const cx = slot * (i + 0.5);
          const other = d.total - d.verified - d.failed;
          let yb = H - padB;
          const seg = (v: number, tok: string) => {
            if (v <= 0) return null;
            const h = (H - padT - padB) * (v / max);
            const yTop = yb - h;
            const r = <rect key={tok} x={cx - bw / 2} y={yTop + 1} width={bw} height={Math.max(0, h - 1)} rx={2} fill={`rgb(var(--c-${tok}))`} />;
            yb = yTop;
            return r;
          };
          return (
            <g key={d.day} onMouseEnter={() => setHover(i)}>
              {/* invisible full-height hit target */}
              <rect x={slot * i} y={padT} width={slot} height={H - padT - padB} fill="transparent" />
              {seg(d.verified, "success")}
              {seg(other, "primary")}
              {seg(d.failed, "error")}
              {hover === i && <rect x={cx - bw / 2 - 1.5} y={y(d.total) - 1.5} width={bw + 3} height={H - padB - y(d.total) + 3} rx={3} fill="none" stroke="rgb(var(--c-on-surface-variant))" strokeWidth="1" opacity="0.5" />}
            </g>
          );
        })}
      </svg>
      {hover !== null && days[hover] && (
        <div className="pointer-events-none absolute top-0 rounded-md border border-outline-variant bg-surface-high px-2.5 py-1.5 text-xs shadow-lg"
          style={{ left: `${((hover + 0.5) / n) * 100}%`, transform: "translateX(-50%)" }}>
          <div className="font-semibold text-on-surface">{dayLabel(days[hover].day)}</div>
          <div className="text-on-surface-variant">{days[hover].verified} verified · {days[hover].total - days[hover].verified - days[hover].failed} backed up{days[hover].failed > 0 ? ` · ${days[hover].failed} failed` : ""}</div>
        </div>
      )}
      <div className="mt-1 flex justify-between text-[11px] text-on-surface-variant">
        <span>{dayLabel(days[0].day)}</span>
        <span>{dayLabel(days[days.length - 1].day)}</span>
      </div>
    </div>
  );
}

// CapacityCard — used-over-time area heading toward the destination's capacity,
// with the forecast ("fills in N days") as the headline. Hover shows a point.
function CapacityCard({ d }: { d: InsightsDest }) {
  const [hover, setHover] = useState<number | null>(null);
  const svgRef = useRef<SVGSVGElement>(null);
  const pts = d.history;
  const enough = pts.length >= 2;
  const usedPct = d.total_bytes > 0 ? (d.used_bytes / d.total_bytes) * 100 : 0;

  const W = 320, H = 96, padB = 4, padT = 6;
  const chart = useMemo(() => {
    if (!enough) return null;
    const t0 = pts[0].ts, t1 = pts[pts.length - 1].ts;
    const span = Math.max(1, t1 - t0);
    const cap = d.total_bytes > 0 ? d.total_bytes : Math.max(...pts.map((p) => p.used_bytes)) * 1.1 || 1;
    const x = (ts: number) => ((ts - t0) / span) * W;
    const y = (v: number) => padT + (H - padT - padB) * (1 - Math.min(v, cap) / cap);
    const line = pts.map((p) => `${x(p.ts).toFixed(1)},${y(p.used_bytes).toFixed(1)}`);
    const area = `M ${x(t0).toFixed(1)},${(H - padB).toFixed(1)} L ${line.join(" L ")} L ${x(t1).toFixed(1)},${(H - padB).toFixed(1)} Z`;
    return { x, y, cap, line, area, t0, span };
  }, [pts, enough, d.total_bytes]);

  const near = d.days_to_full >= 0 && d.days_to_full <= 60;
  const forecastTxt = d.days_to_full < 0
    ? (d.growth_bytes_per_month <= 0 ? "Not filling up" : "Projecting…")
    : d.days_to_full === 0 ? "Full" : `Fills in ~${d.days_to_full} day${d.days_to_full === 1 ? "" : "s"}`;

  const onMove = (e: React.MouseEvent) => {
    if (!chart || !svgRef.current) return;
    const r = svgRef.current.getBoundingClientRect();
    const fx = ((e.clientX - r.left) / r.width) * W;
    let best = 0, bd = Infinity;
    pts.forEach((p, i) => { const dx = Math.abs(chart.x(p.ts) - fx); if (dx < bd) { bd = dx; best = i; } });
    setHover(best);
  };

  return (
    <Card className="p-4">
      <div className="mb-2 flex items-center gap-2.5">
        <span className="grid h-8 w-8 shrink-0 place-items-center rounded bg-docker-blue/15 text-primary"><HardDrive size={16} /></span>
        <div className="min-w-0 flex-1">
          <div className="truncate text-sm font-semibold text-on-surface">{d.name}</div>
          <div className="font-mono text-[11px] uppercase tracking-wider text-on-surface-variant">{d.type}</div>
        </div>
        <div className="shrink-0 text-right">
          <div className={`text-xs font-semibold ${near ? "text-warning" : "text-on-surface-variant"}`}>{forecastTxt}</div>
          {d.total_bytes > 0 && <div className="text-[11px] text-on-surface-variant">{usedPct.toFixed(0)}% used</div>}
        </div>
      </div>

      {!enough ? (
        <div className="flex h-[96px] items-center justify-center rounded bg-surface-lowest text-xs text-on-surface-variant">Collecting history — a forecast appears after a couple of daily readings.</div>
      ) : (
        <div className="relative">
          <svg ref={svgRef} viewBox={`0 0 ${W} ${H}`} className="w-full" style={{ height: H }} preserveAspectRatio="none"
            onMouseMove={onMove} onMouseLeave={() => setHover(null)}>
            <defs>
              <linearGradient id={`cap-${d.id}`} x1="0" y1="0" x2="0" y2="1">
                <stop offset="0%" stopColor={near ? "rgb(var(--c-warning))" : "rgb(var(--c-docker-blue))"} stopOpacity="0.28" />
                <stop offset="100%" stopColor={near ? "rgb(var(--c-warning))" : "rgb(var(--c-docker-blue))"} stopOpacity="0.02" />
              </linearGradient>
            </defs>
            {/* capacity ceiling */}
            <line x1="0" y1={padT} x2={W} y2={padT} stroke="rgb(var(--c-outline-variant))" strokeWidth="1" strokeDasharray="3 3" />
            <path d={chart!.area} fill={`url(#cap-${d.id})`} />
            <polyline points={chart!.line.join(" ")} fill="none" stroke={near ? "rgb(var(--c-warning))" : "rgb(var(--c-docker-blue))"} strokeWidth="2" strokeLinejoin="round" vectorEffect="non-scaling-stroke" />
            {hover !== null && (
              <>
                <line x1={chart!.x(pts[hover].ts)} y1={padT} x2={chart!.x(pts[hover].ts)} y2={H - padB} stroke="rgb(var(--c-on-surface-variant))" strokeWidth="1" opacity="0.4" />
                <circle cx={chart!.x(pts[hover].ts)} cy={chart!.y(pts[hover].used_bytes)} r="3" fill={near ? "rgb(var(--c-warning))" : "rgb(var(--c-docker-blue))"} stroke="rgb(var(--c-surface-container))" strokeWidth="1.5" />
              </>
            )}
          </svg>
          {hover !== null && pts[hover] && (
            <div className="pointer-events-none absolute top-0 rounded-md border border-outline-variant bg-surface-high px-2.5 py-1.5 text-xs shadow-lg"
              style={{ left: `${(chart!.x(pts[hover].ts) / W) * 100}%`, transform: "translateX(-50%)" }}>
              <div className="font-semibold text-on-surface">{fmtBytes(pts[hover].used_bytes)}</div>
              <div className="text-on-surface-variant">{dayLabel(pts[hover].ts)}</div>
            </div>
          )}
        </div>
      )}
      <div className="mt-2 flex items-center justify-between text-[11px] text-on-surface-variant">
        <span>{fmtBytes(d.used_bytes)}{d.total_bytes > 0 ? ` of ${fmtBytes(d.total_bytes)}` : ""}</span>
        <Link to="/settings" className="text-primary hover:underline">Manage</Link>
      </div>
    </Card>
  );
}

// GrowthRow — one container's backup-size trend line in the "largest & fastest-
// growing" panel (F11): name/stack, node + latest size, a sparkline, and the
// per-month growth (or "stable").
function GrowthRow({ g }: { g: InsightsGrowth }) {
  const growth = g.growth_bytes_per_month;
  return (
    <div className="flex items-center gap-3 py-2.5">
      <div className="min-w-0 flex-1">
        <div className="truncate text-sm font-medium text-on-surface">
          {g.container}{g.stack ? <span className="text-on-surface-variant"> · {g.stack}</span> : null}
        </div>
        <div className="truncate text-xs text-on-surface-variant">{g.node} · {fmtBytes(g.latest_bytes)} latest</div>
      </div>
      <Sparkline points={g.points} width={96} height={30} className={growth > 0 ? "text-warning" : "text-secondary"} />
      <div className="w-24 shrink-0 text-right text-sm font-semibold">
        {growth > 0
          ? <span className="text-warning">+{fmtBytes(growth)}/mo</span>
          : growth < 0
            ? <span className="text-secondary">−{fmtBytes(-growth)}/mo</span>
            : <span className="text-on-surface-variant">stable</span>}
      </div>
    </div>
  );
}
