/**
 * StateChip renders a container's Docker state as a coloured badge.
 *
 * Anything that is not "running" is shown in the error colour rather than a
 * neutral one: for a page about backups, a container that is paused, exited or
 * restarting is the thing worth noticing, not a detail to be read past.
 */
export default function StateChip({ state }: { state?: string }) {
  const running = state === "running";
  const tone = running ? "bg-success/15 text-success" : "bg-error/15 text-error";
  return <span className={`rounded px-2 py-0.5 text-xs font-bold uppercase tracking-wide ${tone}`}>{state || "—"}</span>;
}
