// C9 — "Time machine" restore timeline. A horizontal, scrubbable history of a
// single container's restorable recovery points: each successful backup is a
// point, colored by state (verified / drilled-good / degraded-offsite /
// key-mismatch / unverified); a flag marks where the image digest changed (a
// version bump, e.g. "2.7 → 3.0.1"); size deltas ride under each point. Hover
// shows the manifest summary; clicking scrubs to that point and hands off to the
// caller (which opens the existing restore/revert flow). Purely presentational —
// no data fetching, no destructive actions of its own.
import { useMemo, useState } from "react";
import { failedCopies, goodCopies } from "../lib/backupRows";
import { ShieldCheck, KeyRound, AlertTriangle, ArrowUpRight, CheckCircle2 } from "lucide-react";
import { Backup, DrillStatus, fmtBytes } from "../api";

// Geometry (px). Even index spacing (a filmstrip) rather than time-proportional
// so clustered backups never overlap, while still reading oldest→newest L→R.
const PAD_X = 44;
const STEP = 92;
const H = 132;
const BASE_Y = 82;
const R = 7;
const R_SEL = 10;

// Slim list rows (perf Fix 8) carry a pre-derived summary instead of blobs;
// fat rows (older backend / full fetches) keep working via the JSON parse.
function manifest(b: Backup): any {
  if (!b.manifest_json && b.summary) return { image: b.summary.image, image_digest: b.summary.image_digest };
  try { return JSON.parse(b.manifest_json || "null"); } catch { return null; }
}

// versionLabel extracts a human tag from an image ref: "immich:v3.0.1" -> "v3.0.1".
// Falls back to the short digest when the image is untagged/floating.
function versionLabel(image: string, digest: string): string {
  const afterColon = image.includes(":") ? image.slice(image.lastIndexOf(":") + 1) : "";
  const tag = afterColon && !afterColon.includes("/") ? afterColon : "";
  if (tag) return tag;
  const sha = /sha256:([0-9a-f]{6,})/i.exec(digest)?.[1] || "";
  return sha ? sha.slice(0, 7) : "image";
}

type PointState = "key" | "degraded" | "drill-failed" | "drilled" | "verified" | "unverified";

interface Pt {
  b: Backup;
  x: number;
  image: string;
  digest: string;
  version: string;
  state: PointState;
  bumped: boolean; // image digest differs from the previous (older) point
  delta: number;   // size_bytes change vs the previous point (0 for the first)
}

// State priority — worst/most-actionable wins, so a degraded or key-mismatch
// point never hides behind a green "verified".
function pointState(b: Backup, drill?: DrillStatus): PointState {
  if (b.key_mismatch) return "key";
  if (failedCopies(b).length > 0) return "degraded";
  if (drill && !drill.ok) return "drill-failed";
  if (drill && drill.ok) return "drilled";
  if (b.verified === "verified") return "verified";
  return "unverified";
}

const STATE_COLOR: Record<PointState, string> = {
  key: "text-error",
  degraded: "text-warning",
  "drill-failed": "text-error",
  drilled: "text-success",
  verified: "text-success",
  unverified: "text-on-surface-variant",
};
const STATE_LABEL: Record<PointState, string> = {
  key: "Key mismatch — needs the original encryption key",
  degraded: "Degraded — an offsite copy is missing",
  "drill-failed": "Restore drill failed",
  drilled: "Drill-proven restorable",
  verified: "Verified by test-restore",
  unverified: "Stored, not yet verified",
};

export default function RestoreTimeline({ backups, drills, selectedId, onPick }: {
  backups: Backup[];
  drills?: Record<string, DrillStatus>;
  selectedId?: string;
  onPick: (b: Backup) => void;
}) {
  const [hover, setHover] = useState<number | null>(null);

  // Only successful backups are restorable recovery points. Oldest→newest (L→R).
  const pts = useMemo<Pt[]>(() => {
    const ok = backups.filter((b) => b.status === "success").sort((a, c) => a.created_at - c.created_at);
    let prevDigest = "";
    let prevSize = 0;
    return ok.map((b, i) => {
      const m = manifest(b);
      const image = (m?.image as string) || "";
      const digest = (m?.image_digest as string) || "";
      const bumped = i > 0 && !!digest && !!prevDigest && digest !== prevDigest;
      const delta = i > 0 ? b.size_bytes - prevSize : 0;
      prevDigest = digest || prevDigest;
      prevSize = b.size_bytes;
      return { b, x: PAD_X + i * STEP, image, digest, version: versionLabel(image, digest), state: pointState(b, drills?.[b.id]), bumped, delta };
    });
  }, [backups, drills]);

  if (pts.length === 0) return null;

  const width = PAD_X * 2 + Math.max(0, pts.length - 1) * STEP;
  const active = hover != null ? hover : pts.findIndex((p) => p.b.id === selectedId);
  const bumps = pts.filter((p) => p.bumped);

  return (
    <div>
      <div className="overflow-x-auto pb-1">
        <div className="relative" style={{ width, height: H }}>
          <svg width={width} height={H} className="block" role="img" aria-label="Backup recovery timeline">
            {/* baseline */}
            <line x1={PAD_X} y1={BASE_Y} x2={width - PAD_X} y2={BASE_Y} className="text-outline-variant" stroke="currentColor" strokeWidth={2} />
            {/* connectors — dashed + accent where the image version changed */}
            {pts.slice(1).map((p, i) => (
              <line key={`c${i}`} x1={pts[i].x} y1={BASE_Y} x2={p.x} y2={BASE_Y}
                className={p.bumped ? "text-primary" : "text-outline-variant"} stroke="currentColor"
                strokeWidth={p.bumped ? 2 : 2} strokeDasharray={p.bumped ? "3 3" : undefined} />
            ))}
            {/* version-bump flags between the two points where the digest changed */}
            {pts.slice(1).map((p, i) =>
              p.bumped ? (
                <g key={`v${i}`} className="text-primary" transform={`translate(${(pts[i].x + p.x) / 2}, ${BASE_Y})`}>
                  <line x1={0} y1={0} x2={0} y2={-30} stroke="currentColor" strokeWidth={1.5} />
                  <circle cx={0} cy={-34} r={3} fill="currentColor" />
                </g>
              ) : null
            )}
            {/* points */}
            {pts.map((p, i) => {
              const sel = p.b.id === selectedId;
              const r = sel ? R_SEL : R;
              const ring = p.state === "drilled"; // drill-proven gets an outer ring
              return (
                <g key={p.b.id} className={`${STATE_COLOR[p.state]} cursor-pointer`}
                  tabIndex={0} role="button"
                  aria-label={`${p.b.target_name} — ${new Date(p.b.created_at * 1000).toLocaleString()} — ${STATE_LABEL[p.state]}`}
                  onMouseEnter={() => setHover(i)} onMouseLeave={() => setHover((h) => (h === i ? null : h))}
                  onFocus={() => setHover(i)} onBlur={() => setHover((h) => (h === i ? null : h))}
                  onClick={() => onPick(p.b)}
                  onKeyDown={(e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); onPick(p.b); } }}>
                  {/* generous invisible hit target */}
                  <circle cx={p.x} cy={BASE_Y} r={18} fill="transparent" />
                  {sel && <circle cx={p.x} cy={BASE_Y} r={r + 5} fill="none" stroke="currentColor" strokeWidth={1.5} opacity={0.5} />}
                  {ring && <circle cx={p.x} cy={BASE_Y} r={r + 3} fill="none" stroke="currentColor" strokeWidth={1.5} />}
                  <circle cx={p.x} cy={BASE_Y} r={r} fill="currentColor" />
                  {/* version label under the newest of a run / on a bump */}
                  <text x={p.x} y={BASE_Y + 22} textAnchor="middle" className="fill-current text-on-surface-variant" style={{ fontSize: 10 }}>
                    {p.version}
                  </text>
                  {/* size delta under the version */}
                  {i > 0 && p.delta !== 0 && (
                    <text x={p.x} y={BASE_Y + 35} textAnchor="middle" className={`fill-current ${p.delta > 0 ? "text-warning" : "text-secondary"}`} style={{ fontSize: 9 }}>
                      {p.delta > 0 ? "▲" : "▼"} {fmtBytes(Math.abs(p.delta))}
                    </text>
                  )}
                </g>
              );
            })}
          </svg>

          {/* hover/focus tooltip — manifest summary for the active point */}
          {active >= 0 && active < pts.length && (
            <TimelineTip p={pts[active]} drill={drills?.[pts[active].b.id]} />
          )}
        </div>
      </div>

      {/* legend + count */}
      <div className="mt-1 flex flex-wrap items-center gap-x-4 gap-y-1 text-[11px] text-on-surface-variant">
        <LegendDot cls="text-success" label="Verified" />
        <LegendDot cls="text-success" ring label="Drill-proven" />
        <LegendDot cls="text-warning" label="Degraded offsite" />
        <LegendDot cls="text-error" label="Key mismatch / drill failed" />
        <LegendDot cls="text-on-surface-variant" label="Unverified" />
        {bumps.length > 0 && (
          <span className="flex items-center gap-1 text-primary"><ArrowUpRight size={12} /> version change</span>
        )}
        <span className="ml-auto">{pts.length} recovery point{pts.length === 1 ? "" : "s"} · click a point to restore</span>
      </div>
    </div>
  );
}

function LegendDot({ cls, ring, label }: { cls: string; ring?: boolean; label: string }) {
  return (
    <span className="flex items-center gap-1.5">
      <span className={`relative inline-block h-2.5 w-2.5 rounded-full ${cls}`} style={{ background: "currentColor" }}>
        {ring && <span className="absolute -inset-1 rounded-full border border-current" />}
      </span>
      {label}
    </span>
  );
}

function TimelineTip({ p, drill }: { p: Pt; drill?: DrillStatus }) {
  const good = goodCopies(p.b).length;
  const failed = failedCopies(p.b).length;
  const stateIcon =
    p.state === "key" ? <KeyRound size={12} className="text-error" /> :
    p.state === "degraded" ? <AlertTriangle size={12} className="text-warning" /> :
    p.state === "drill-failed" ? <AlertTriangle size={12} className="text-error" /> :
    p.state === "drilled" ? <ShieldCheck size={12} className="text-success" /> :
    p.state === "verified" ? <ShieldCheck size={12} className="text-success" /> :
    <CheckCircle2 size={12} className="text-on-surface-variant" />;
  return (
    <div
      className="pointer-events-none absolute z-10 w-56 -translate-x-1/2 rounded-lg border border-outline-variant bg-surface-lowest p-2.5 text-xs shadow-xl"
      style={{ left: p.x, bottom: H - (BASE_Y - R) + 10 }}
    >
      <div className="font-semibold text-on-surface">{new Date(p.b.created_at * 1000).toLocaleString()}</div>
      <div className="mt-1 flex items-center gap-1.5">{stateIcon}<span className="text-on-surface-variant">{STATE_LABEL[p.state]}</span></div>
      {p.image && <div className="mt-1 truncate font-mono text-[11px] text-on-surface-variant" title={p.image}>{p.image}</div>}
      {p.bumped && <div className="mt-1 flex items-center gap-1 text-primary"><ArrowUpRight size={11} /> version changed to {p.version}</div>}
      <div className="mt-1 flex items-center justify-between text-on-surface-variant">
        <span>{fmtBytes(p.b.size_bytes)}</span>
        {p.delta !== 0 && <span className={p.delta > 0 ? "text-warning" : "text-secondary"}>{p.delta > 0 ? "+" : "−"}{fmtBytes(Math.abs(p.delta))}</span>}
      </div>
      <div className="mt-1 text-on-surface-variant">{good} cop{good === 1 ? "y" : "ies"}{failed > 0 ? ` · ${failed} offsite missing` : ""}</div>
      {drill && <div className={`mt-1 ${drill.ok ? "text-success" : "text-error"}`}>Drill {drill.ok ? "passed" : "failed"}</div>}
    </div>
  );
}
