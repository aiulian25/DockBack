import type { RestoreCapacity } from "../api";

/** How loudly the destination-capacity panel should speak (#37). */
export type CapacityTone = "error" | "warning" | "primary";

/**
 * capacityTone grades a placement.
 *
 * Three levels rather than two, because the interesting case is neither. R5 §3's
 * restore cleared its margin — 16 GB left on a 98 GB root — and was still the
 * wrong disk; it is exactly the placement worth colouring before an operator
 * scrolls past it. "Comfortable" is set at twice the margin: below that, the
 * restore fits but leaves no room for what the disk does next.
 */
export function capacityTone(c: Pick<RestoreCapacity, "refuse" | "after_bytes" | "margin_bytes">): CapacityTone {
  if (c.refuse) return "error";
  const comfortable = c.after_bytes >= c.margin_bytes * 2;
  if (comfortable) return "primary";
  return "warning";
}
