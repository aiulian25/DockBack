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
import { parseRunDone, RunOutcome } from "./runDone";
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
};

/**
 * followRun watches one run and reports when it is over.
 *
 * The structured `run.done` event is the verdict when it arrives. The line rules
 * remain as the fallback, deliberately: run.done is not replayed to a console
 * that connects late.
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

  const stop = () => {
    detachDone?.();
    detachDone = null;
    stopLines?.();
    stopLines = null;
  };

  const finish = (outcome: FollowOutcome, message: string) => {
    if (finished) return; // whichever signal arrives first wins; the run ends once
    finished = true;
    stop();
    opts.onDone?.(outcome, message);
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
    if (outcome) finish(outcome, verdictMessage(outcome, line.msg));
  });

  // The structural verdict rides the same connection, dispatched through this
  // module's own fan-out so it survives a reconnect.
  detachDone = subscribeEvent("run.done", (data) => {
    const d = parseRunDone(data, runId);
    if (d) finish(d.outcome, d.message);
  });

  return () => { if (!finished) { finished = true; stop(); } };
}

/** Test seam: drop all state so one test cannot leak into the next. */
export function __resetLogStream() {
  lineSubs.clear();
  statusSubs.clear();
  eventSubs.clear();
  refCount = 0;
  close();
}
