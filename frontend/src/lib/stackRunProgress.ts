// Reading a stack run's state out of its log lines.
//
// The stack console has no structured completion signal — it follows the run by
// matching the text of log lines — and matching prose is exactly what broke it:
// the pattern `restored$` was meant for "Stack "x" restored — all 5 services are
// running" and also matched every per-service line, `[1/5] Service "db"
// restored`. The console then declared the whole run done after the FIRST
// service and closed the stream, so a five-service restore showed one line, said
// "done", and left the operator to check the receiving host to find out whether
// the other four had worked.
//
// So the rule lives here, alone and tested, with the line that broke it named in
// the test. Until the backend grows a structured terminal event, this is the
// thing to keep honest.

/** How far along a stack run is, read from a `[N/M]` progress line. */
export type StackProgress = { done: number; total: number } | null;

/** What a log line says about whether the run is over. */
export type StackVerdict = "running" | "ok" | "fail";

// A per-service line: `[3/5] Service "redis" restored`. The brackets are the
// signal — the terminal line has none.
const perService = /^\[(\d+)\/(\d+)\]/;

/**
 * stackProgress reads "how many of how many" from a per-service line, or null
 * when the line is not one. `[1/5] Restoring service …` counts as 0 done of 5;
 * `[1/5] Service "db" restored` counts as 1 of 5.
 */
export function stackProgress(msg: string): StackProgress {
  const m = perService.exec(msg);
  if (!m) return null;
  const done = parseInt(m[1], 10);
  const total = parseInt(m[2], 10);
  if (!Number.isFinite(done) || !Number.isFinite(total) || total <= 0) return null;
  // "Restoring service X…" is that service STARTING; it is finished only once a
  // line reports it restored, so a start line reports the previous count.
  const finished = /restor(ed|ing)/i.test(msg) && !/Restoring service/i.test(msg);
  return { done: finished ? done : done - 1, total };
}

/**
 * stackVerdict decides whether a line ends the run.
 *
 * Terminal lines are anchored to the shapes the engine actually emits for the
 * WHOLE stack, never a per-service line — a bracketed `[N/M]` prefix is proof
 * the line is about one service and cannot end anything.
 */
export function stackVerdict(msg: string, level: string): StackVerdict {
  if (perService.test(msg)) return "running";

  // Failures first: a run that both restored and failed is a failure.
  if (level === "ERR" || /\brefused\b|\bfailed\b|did not become healthy/i.test(msg)) return "fail";

  // Success, for each of the three things this console follows.
  if (/^Stack .* restored — all \d+ services are running/i.test(msg)) return "ok";
  if (/snapshot complete|backup complete/i.test(msg)) return "ok";
  if (/^Stack .* canceled|CANCELED after \d+ of \d+/i.test(msg)) return "fail";
  return "running";
}
