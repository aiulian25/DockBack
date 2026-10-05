// The structured "this run is over" signal.
//
// Consoles used to learn a run's outcome by matching the TEXT of its log lines,
// which produced three separate defects in one investigation: a pattern ending
// `restored$` closed the stream after `[1/5] Service "db" restored`, a pattern
// reading `stack .* restored` reported a partial failure as success, and 27
// lines logged at "ERROR" reached nothing at all. Each was a message the backend
// can reword at any time.
//
// The server now says it once, structurally, on the named-event channel that
// already carries node.summary and backup.status. The prose is unchanged — an
// operator still reads it — but nothing DECIDES on it.
//
// The text matching each console already has stays in place as a fallback, and
// deliberately: run.done is not replayed to a client that connects late. A
// console that hears the event acts on it; one that does not carries on exactly
// as it did.

/** How a run ended. Canceled is its own answer — the operator chose it. */
export type RunOutcome = "ok" | "failed" | "canceled";

export type RunDone = { run_id: string; outcome: RunOutcome; message: string };

/**
 * onRunDone calls fn when the run identified by runId reports that it is over.
 *
 * runId is what the console is already following: a backup id,
 * `stack:<project>` or `node:<id>`. Events for any other run are ignored, so
 * two consoles open at once never end each other's run.
 *
 * Returns a function that detaches the listener. Closing the EventSource
 * detaches it too, so a caller that only ever closes the stream can ignore it.
 */
export function onRunDone(
  es: EventSource,
  runId: string,
  fn: (outcome: RunOutcome, message: string) => void,
): () => void {
  const handler = (e: MessageEvent) => {
    let data: unknown;
    try { data = JSON.parse(e.data); } catch { return; } // a malformed frame is not a verdict
    const d = parseRunDone(data, runId);
    if (d) fn(d.outcome, d.message);
  };
  es.addEventListener("run.done", handler as EventListener);
  return () => es.removeEventListener("run.done", handler as EventListener);
}

/**
 * parseRunDone reads a run.done payload and returns the verdict when it belongs
 * to this run, or null. Pure, so the shared log stream can dispatch it through
 * its own fan-out and the rule still lives in one place.
 */
export function parseRunDone(data: unknown, runId: string): { outcome: RunOutcome; message: string } | null {
  const d = data as RunDone | null;
  if (!d || typeof d !== "object") return null;
  if (d.run_id !== runId) return null;
  if (d.outcome !== "ok" && d.outcome !== "failed" && d.outcome !== "canceled") return null;
  return { outcome: d.outcome, message: d.message ?? "" };
}
