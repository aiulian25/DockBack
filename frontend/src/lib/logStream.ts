// One connection to /api/logs/stream, shared by every console in the app.
//
// Twelve components each opened their own EventSource. Browsers allow six
// connections per origin on plain HTTP — the default deployment — so with a
// couple of consoles open the app spent its whole budget on log streams and
// ordinary API fetches queued behind them until one closed. Worse, each console
// carried its own copy of the rules that decide when a run is over, and those
// copies had already drifted: one pattern ended a five-service restore after the
// first service, another reported a partial failure as success.
//
// So there is one stream, reference-counted, and ONE set of terminal rules. The
// rules themselves live in the tested helpers next door (stackRunProgress,
// restoreLogFilter); this module decides which of them applies and says the
// verdict once.
import { isRunOutcome, parseRunDone, RunOutcome } from "./runDone";
import { stackVerdict } from "./stackRunProgress";

/** A line as the server writes it onto the stream. */
export type LogLine = {
  time: string;
  backup_id?: string;
  container_id?: string;
  node_name?: string;
  level: string;
  msg: string;
};

const STREAM_URL = "/api/logs/stream";

let source: EventSource | null = null;
let refCount = 0;
let connected = false;

const lineSubs = new Set<(line: LogLine) => void>();
const statusSubs = new Set<(connected: boolean) => void>();
// Named-event subscribers, kept per event name so the listener on the live
// stream can be attached once and fan out.
const eventSubs = new Map<string, Set<(data: unknown) => void>>();
const attached = new Map<string, EventListener>();

function setConnected(next: boolean) {
  if (connected === next) return;
  connected = next;
  for (const fn of statusSubs) fn(next);
}

function attachEvent(name: string) {
  if (!source || attached.has(name)) return;
  const listener = ((e: MessageEvent) => {
    const subs = eventSubs.get(name);
    if (!subs || subs.size === 0) return;
    let data: unknown;
    try { data = JSON.parse(e.data); } catch { return; } // a malformed frame is not an event
    for (const fn of [...subs]) fn(data);
  }) as EventListener;
  source.addEventListener(name, listener);
  attached.set(name, listener);
}

function open() {
  if (source) return;
  const es = new EventSource(STREAM_URL, { withCredentials: true });
  source = es;
  es.onopen = () => setConnected(true);
  // EventSource reconnects on its own; consoles fall back to polling meanwhile.
  es.onerror = () => setConnected(false);
  es.onmessage = (e) => {
    let line: LogLine;
    try { line = JSON.parse(e.data) as LogLine; } catch { return; }
    for (const fn of [...lineSubs]) fn(line);
  };
  for (const name of eventSubs.keys()) attachEvent(name);
}

function close() {
  if (!source) return;
  for (const [name, listener] of attached) source.removeEventListener(name, listener);
  attached.clear();
  source.close();
  source = null;
  setConnected(false);
}

/** retain/release drive the single connection: it exists while anyone needs it. */
function retain() {
  refCount++;
  if (refCount === 1) open();
}

function release() {
  refCount = Math.max(0, refCount - 1);
  if (refCount === 0) close();
}

/** subscribeLines receives every log line. Returns an unsubscribe function. */
export function subscribeLines(fn: (line: LogLine) => void): () => void {
  lineSubs.add(fn);
  retain();
  let live = true;
  return () => {
    if (!live) return; // unsubscribing twice must not release someone else's hold
    live = false;
    lineSubs.delete(fn);
    release();
  };
}

/** subscribeEvent receives one named event's parsed payload (node.summary, …). */
export function subscribeEvent(name: string, fn: (data: any) => void): () => void {
  let subs = eventSubs.get(name);
  if (!subs) { subs = new Set(); eventSubs.set(name, subs); }
  subs.add(fn);
  retain();
  attachEvent(name);
  let live = true;
  return () => {
    if (!live) return;
    live = false;
    subs.delete(fn);
    release();
  };
}

/** isConnected reports the stream's current state (for a live/disconnected chip). */
export function isConnected(): boolean {
  return connected;
}

/** subscribeConnection reports connection changes so a component can re-render. */
export function subscribeConnection(fn: (connected: boolean) => void): () => void {
  statusSubs.add(fn);
  return () => { statusSubs.delete(fn); };
}

// ---------------------------------------------------------------------------
// Following one run
// ---------------------------------------------------------------------------

/** How a run ended, as a console shows it. */
export type FollowOutcome = RunOutcome;

/**
 * Which family of terminal rules applies:
 *   one   — a single container's restore, keyed by backup id
 *   stack — a compose project, where a `[N/M]` line can never end the run
 *   node  — a whole-machine rebuild, whose own lines alone decide the outcome
 */
export type FollowMode = "one" | "stack" | "node";

// The single-restore terminal rules. These were copied into four consoles; the
// copies disagreed, and one of them treated any line containing "canceled" as
// the end of the run.
const ONE_DONE = /restore completed/i;
const ONE_CANCELED = /restore canceled|canceled while waiting/i;
const ONE_FAILED = /restore failed|refusing restore/i;
const CANCELED_PREFIX = /^restore canceled by the operator:\s*/i;
const FAILED_PREFIX = /^restore failed:\s*/i;

// A whole-node rebuild reports its own completion; a stack it runs inside itself
// must never end it, which is why the caller filters by run id first.
const NODE_DONE = /back up in the proven order/i;
const NODE_FAILED = /failed:/i;

/** lineVerdict is the one place a log line is turned into a verdict. */
export function lineVerdict(mode: FollowMode, msg: string, level: string): FollowOutcome | null {
  if (mode === "stack") {
    const v = stackVerdict(msg, level);
    if (v === "ok") return "ok";
    if (v === "fail") return "failed";
    return null;
  }
  if (mode === "node") {
    if (NODE_DONE.test(msg)) return "ok";
    if (NODE_FAILED.test(msg) || level === "ERR") return "failed";
    return null;
  }
  if (ONE_DONE.test(msg)) return "ok";
  if (ONE_CANCELED.test(msg)) return "canceled";
  if (ONE_FAILED.test(msg) || level === "ERR") return "failed";
  return null;
}

/** The message a console shows for a terminal line, with its prefix trimmed. */
export function verdictMessage(outcome: FollowOutcome, msg: string): string {
  if (outcome === "canceled") return msg.replace(CANCELED_PREFIX, "");
  if (outcome === "failed") return msg.replace(FAILED_PREFIX, "");
  return msg;
}

export type FollowOptions = {
  mode: FollowMode;
  /** Called for each line belonging to this run (after `filter`). */
  onLine?: (line: LogLine) => void;
  /** Called once, when the run ends. The follower detaches itself first. */
  onDone?: (outcome: FollowOutcome, message: string) => void;
  /**
   * Drop lines older than this timestamp. The stream replays recent history on
   * connect; a console following a run it just started does not want it, and one
   * attaching to a run already in flight does.
   */
  since?: number;
  /** Extra run ids whose lines are shown but can never end the run. */
  alsoShow?: string[];
  /**
   * Containers whose lines are shown but can never end the run: a stack's
   * services, whose backups each log under their own backup id.
   */
  alsoShowContainers?: string[];
  /** Show only lines this accepts (e.g. isRestoreProgressLine). */
  filter?: (msg: string) => boolean;
  /**
   * Which restore under this reused run id is being followed, so another run's
   * verdict cannot end it. Without one, a recorded run older than `since` is ignored.
   */
  executionId?: string;
};

/** How often a follower asks for the outcome, in case it heard nothing at all. */
const RUN_STATUS_POLL_MS = 30_000;
const SECONDS_TO_MS = 1000;
const HTTP_NOT_FOUND = 404;

type RunStatus = { outcome: string; message?: string; started_at: number };

/** The server's record of a run: its status, "none" if it has none, or null if unknown. */
async function fetchRunStatus(runId: string, executionId?: string): Promise<RunStatus | "none" | null> {
  const query = executionId ? `?execution=${encodeURIComponent(executionId)}` : "";
  try {
    const res = await fetch(`/api/restores/${encodeURIComponent(runId)}/status${query}`, { credentials: "same-origin" });
    if (res.status === HTTP_NOT_FOUND) return "none";
    return res.ok ? ((await res.json()) as RunStatus) : null;
  } catch {
    return null;
  }
}

const activeFollowers = new Set<() => void>();

/**
 * followRun watches one run and reports when it is over.
 *
 * For a restore the server's record decides, heard as `run.done` or by asking. A log
 * line that reads as final only prompts the question: a post-restore hook can log an
 * error and the restore still succeed, so "still running" means carry on. A run with
 * no record, such as a stack backup, falls back to the line rules. Asking also
 * happens on reconnect and on a slow poll, which recovers a verdict missed while
 * disconnected, since run.done is not replayed.
 *
 * Returns a function that stops following. It is safe to call after the run has
 * already ended.
 */
export function followRun(runId: string, opts: FollowOptions): () => void {
  const also = new Set(opts.alsoShow || []);
  const alsoContainers = new Set(opts.alsoShowContainers || []);
  let finished = false;
  let stopLines: (() => void) | null = null;
  let detachDone: (() => void) | null = null;
  let detachConnection: (() => void) | null = null;
  let poll: ReturnType<typeof setInterval> | null = null;

  const stop = () => {
    detachDone?.();
    detachDone = null;
    stopLines?.();
    stopLines = null;
    detachConnection?.();
    detachConnection = null;
    if (poll !== null) clearInterval(poll);
    poll = null;
    activeFollowers.delete(cancel);
  };

  const finish = (outcome: FollowOutcome, message: string) => {
    if (finished) return; // whichever signal arrives first wins; the run ends once
    finished = true;
    stop();
    opts.onDone?.(outcome, message);
  };

  const cancel = () => { if (!finished) { finished = true; stop(); } };

  const isEarlierRun = (status: RunStatus) =>
    !opts.executionId && opts.since !== undefined && status.started_at * SECONDS_TO_MS < opts.since;

  // lineSays is the verdict a log line suggests, used only if the server has no record.
  const reconcile = async (lineSays?: { outcome: FollowOutcome; message: string }) => {
    const status = await fetchRunStatus(runId, opts.executionId);
    if (finished || status === null) return;
    if (status === "none" || isEarlierRun(status)) {
      if (lineSays) finish(lineSays.outcome, lineSays.message);
      return;
    }
    if (isRunOutcome(status.outcome)) finish(status.outcome, status.message ?? "");
  };

  stopLines = subscribeLines((line) => {
    const own = line.backup_id === runId;
    const related = also.has(line.backup_id || "") || alsoContainers.has(line.container_id || "");
    if (!own && !related) return;
    if (opts.since !== undefined && new Date(line.time).getTime() < opts.since) return;
    if (opts.filter && !opts.filter(line.msg)) return;
    opts.onLine?.(line);
    if (!own) return; // another run's progress never ends this one
    const outcome = lineVerdict(opts.mode, line.msg, line.level);
    if (outcome) void reconcile({ outcome, message: verdictMessage(outcome, line.msg) });
  });

  // The structural verdict rides the same connection, dispatched through this
  // module's own fan-out so it survives a reconnect.
  detachDone = subscribeEvent("run.done", (data) => {
    const d = parseRunDone(data, runId);
    if (!d) return;
    if (opts.executionId && d.executionId && d.executionId !== opts.executionId) return;
    finish(d.outcome, d.message);
  });

  detachConnection = subscribeConnection((connected) => { if (connected) void reconcile(); });
  poll = setInterval(() => { void reconcile(); }, RUN_STATUS_POLL_MS);
  activeFollowers.add(cancel);
  // Attaching to a run already under way: it may have ended just before now.
  if (opts.since === undefined) void reconcile();

  return cancel;
}

/** Test seam: drop all state so one test cannot leak into the next. */
export function __resetLogStream() {
  activeFollowers.forEach((cancel) => cancel());
  lineSubs.clear();
  statusSubs.clear();
  eventSubs.clear();
  refCount = 0;
  close();
}
