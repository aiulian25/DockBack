// Sparkline — a tiny hand-built inline-SVG trend line for a series of size points
// (F11). Purely presentational; follows the same no-dependency SVG approach as
// RestoreTimeline. Renders nothing for fewer than 2 points.
import { SizePoint } from "../api";

export function Sparkline({ points, width = 120, height = 34, className = "text-primary" }: {
  points: SizePoint[];
  width?: number;
  height?: number;
  className?: string;
}) {
  if (!points || points.length < 2) return null;

  const pad = 3;
  const xs = points.map((p) => p.ts);
  const ys = points.map((p) => p.bytes);
  const minX = Math.min(...xs), maxX = Math.max(...xs);
  const minY = Math.min(...ys), maxY = Math.max(...ys);
  const spanX = maxX - minX || 1;
  const spanY = maxY - minY || 1;
  const px = (x: number) => pad + ((x - minX) / spanX) * (width - 2 * pad);
  const py = (y: number) => height - pad - ((y - minY) / spanY) * (height - 2 * pad);

  const line = points.map((p, i) => `${i === 0 ? "M" : "L"}${px(p.ts).toFixed(1)},${py(p.bytes).toFixed(1)}`).join(" ");
  const area = `${line} L${px(maxX).toFixed(1)},${(height - pad).toFixed(1)} L${px(minX).toFixed(1)},${(height - pad).toFixed(1)} Z`;
  const last = points[points.length - 1];

  return (
    <svg width={width} height={height} className={className} role="img" aria-label="Backup size trend">
      <path d={area} fill="currentColor" opacity={0.12} />
      <path d={line} fill="none" stroke="currentColor" strokeWidth={1.5} strokeLinejoin="round" strokeLinecap="round" />
      <circle cx={px(last.ts)} cy={py(last.bytes)} r={2.5} fill="currentColor" />
    </svg>
  );
}
