import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import {
  followRun, isConnected, lineVerdict, subscribeConnection, subscribeEvent, subscribeLines,
  verdictMessage, __resetLogStream,
} from "./logStream";

// Twelve consoles each opened their own EventSource. Browsers allow six per
// origin on plain HTTP — the default deployment — so with a couple of consoles
// open the app spent its whole connection budget on log streams and ordinary API
// fetches queued behind them. Each console also carried its own copy of the
// rules that end a run, and the copies had already drifted.

type Listener = (e: MessageEvent) => void;

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  static openCount = 0;
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: Listener | null = null;
  closed = false;
  readonly named = new Map<string, Set<Listener>>();

  constructor(public url: string, public init?: unknown) {
    FakeEventSource.instances.push(this);
    FakeEventSource.openCount++;
  }
  addEventListener(name: string, fn: Listener) {
    if (!this.named.has(name)) this.named.set(name, new Set());
    this.named.get(name)!.add(fn);
  }
  removeEventListener(name: string, fn: Listener) {
    this.named.get(name)?.delete(fn);
  }
  close() { this.closed = true; }

  // --- driving the fake from a test ---
  connect() { this.onopen?.(); }
  drop() { this.onerror?.(); }
  line(data: Record<string, unknown>) {
    this.onmessage?.({ data: JSON.stringify(data) } as MessageEvent);
  }
  event(name: string, data: unknown) {
    for (const fn of this.named.get(name) || []) fn({ data: JSON.stringify(data) } as MessageEvent);
  }
  raw(name: string, text: string) {
    for (const fn of this.named.get(name) || []) fn({ data: text } as MessageEvent);
  }
}

const live = () => FakeEventSource.instances[FakeEventSource.instances.length - 1];

// What the status endpoint answers; a run with no record unless a test says otherwise.
let statusAnswer: { status: number; body?: unknown } | "network-error";
const statusCalls: string[] = [];
const live2Connect = () => { FakeEventSource.instances[FakeEventSource.instances.length - 1]?.connect(); };
const flush = async () => { for (let i = 0; i < 8; i++) await Promise.resolve(); };

beforeEach(() => {
  statusAnswer = { status: 404 };
  statusCalls.length = 0;
  vi.stubGlobal("fetch", vi.fn(async (url: string) => {
    statusCalls.push(url);
    if (statusAnswer === "network-error") throw new Error("offline");
    const { status, body } = statusAnswer;
    return { status, ok: status < 400, json: async () => body };
  }));
  FakeEventSource.instances = [];
  FakeEventSource.openCount = 0;
  vi.stubGlobal("EventSource", FakeEventSource);
});
afterEach(() => {
  __resetLogStream();
  vi.unstubAllGlobals();
});

describe("one shared connection", () => {
  it("opens once for any number of subscribers and closes with the last", () => {
    const a = vi.fn(), b = vi.fn(), c = vi.fn();
    const stopA = subscribeLines(a);
    const stopB = subscribeLines(b);
    const stopC = subscribeEvent("node.summary", c);
    expect(FakeEventSource.openCount).toBe(1);

    live().line({ backup_id: "b1", level: "INFO", msg: "hello", time: new Date().toISOString() });
    expect(a).toHaveBeenCalledTimes(1);
    expect(b).toHaveBeenCalledTimes(1);

    stopA();
    expect(live().closed).toBe(false); // two subscribers left
    stopB();
    expect(live().closed).toBe(false); // the named-event subscriber still holds it
    stopC();
    expect(live().closed).toBe(true);
    expect(FakeEventSource.openCount).toBe(1);
  });

  it("re-opens for a later subscriber", () => {
    subscribeLines(vi.fn())();
    expect(live().closed).toBe(true);
    const stop = subscribeLines(vi.fn());
    expect(FakeEventSource.openCount).toBe(2);
    expect(live().closed).toBe(false);
    stop();
  });

  it("unsubscribing twice does not release someone else's hold", () => {
    const stopA = subscribeLines(vi.fn());
    const stopB = subscribeLines(vi.fn());
    stopA();
    stopA(); // a double cleanup must not close the stream under B
    expect(live().closed).toBe(false);
    stopB();
    expect(live().closed).toBe(true);
  });

  it("reports the connection state so a console can show live/disconnected", () => {
    const seen: boolean[] = [];
    const stopStatus = subscribeConnection((up) => seen.push(up));
    const stop = subscribeLines(vi.fn());
    expect(isConnected()).toBe(false);
    live().connect();
    expect(isConnected()).toBe(true);
    live().drop();
    expect(isConnected()).toBe(false);
    expect(seen).toEqual([true, false]);
    stop(); stopStatus();
  });

  it("a malformed frame is not a line and not an event", () => {
    const onLine = vi.fn(), onEvent = vi.fn();
    const s1 = subscribeLines(onLine);
    const s2 = subscribeEvent("run.done", onEvent);
    live().onmessage?.({ data: "{not json" } as MessageEvent);
    live().raw("run.done", "{not json");
    expect(onLine).not.toHaveBeenCalled();
    expect(onEvent).not.toHaveBeenCalled();
    s1(); s2();
  });
});

describe("followRun", () => {
  const lineOf = (id: string, msg: string, level = "INFO") =>
    ({ backup_id: id, level, msg, time: new Date().toISOString() });

  it("takes the structural verdict from run.done", () => {
    const onDone = vi.fn();
    followRun("b1", { mode: "one", onDone });
    live().event("run.done", { run_id: "b1", outcome: "failed", message: "disk full" });
    expect(onDone).toHaveBeenCalledWith("failed", "disk full");
  });

  it("ignores another run's verdict, so two consoles never end each other's run", () => {
    const mine = vi.fn(), theirs = vi.fn();
    followRun("b1", { mode: "one", onDone: mine });
    followRun("b2", { mode: "one", onDone: theirs });
    live().event("run.done", { run_id: "b2", outcome: "ok", message: "" });
    expect(mine).not.toHaveBeenCalled();
    expect(theirs).toHaveBeenCalledWith("ok", "");
  });

  it("falls back to the line rules for a run with no record", async () => {
    const cases: [string, string, string][] = [
      ["restore completed in 4s", "INFO", "ok"],
      ["restore canceled by the operator: user pressed stop", "INFO", "canceled"],
      ["restore failed: volume is read-only", "ERR", "failed"],
    ];
    for (const [msg, level, want] of cases) {
      const onDone = vi.fn();
      followRun("b1", { mode: "one", onDone });
      live().line(lineOf("b1", msg, level));
      await flush();
      expect(onDone.mock.calls[0][0], msg).toBe(want);
    }
  });

  it("trims the prefix a console would otherwise repeat back at the operator", async () => {
    const onDone = vi.fn();
    followRun("b1", { mode: "one", onDone });
    live().line(lineOf("b1", "restore failed: volume is read-only", "ERR"));
    await flush();
    expect(onDone).toHaveBeenCalledWith("failed", "volume is read-only");
  });

  it("ends a run once, whichever signal is first", async () => {
    const onDone = vi.fn();
    followRun("b1", { mode: "one", onDone });
    live().line(lineOf("b1", "restore completed"));
    await flush();
    live().event("run.done", { run_id: "b1", outcome: "failed", message: "too late" });
    live().line(lineOf("b1", "restore failed: also too late", "ERR"));
    await flush();
    expect(onDone).toHaveBeenCalledTimes(1);
    expect(onDone).toHaveBeenCalledWith("ok", "restore completed");
  });

  it("a per-service line can never end a stack run — THE bug this shares", async () => {
    const onDone = vi.fn(), onLine = vi.fn();
    followRun("stack:blog", { mode: "stack", onDone, onLine });
    live().line(lineOf("stack:blog", '[1/5] Service "db" restored'));
    live().line(lineOf("stack:blog", '[5/5] Service "web" restored'));
    await flush();
    expect(onDone).not.toHaveBeenCalled();
    expect(onLine).toHaveBeenCalledTimes(2);
    live().line(lineOf("stack:blog", 'Stack "blog" restored — all 5 services are running'));
    await flush();
    expect(onDone).toHaveBeenCalledWith("ok", expect.stringContaining("all 5 services"));
  });

  it("a partial stack failure is a failure, not a success", async () => {
    const onDone = vi.fn();
    followRun("stack:blog", { mode: "stack", onDone });
    live().line(lineOf("stack:blog", 'Stack "blog" restored, but 2 service(s) did not become healthy', "WARN"));
    await flush();
    expect(onDone.mock.calls[0][0]).toBe("failed");
  });

  it("a node rebuild is decided only by its own lines", async () => {
    const onDone = vi.fn(), onLine = vi.fn();
    followRun("node:n1", { mode: "node", alsoShow: ["stack:blog"], onDone, onLine });
    // A stack inside the rebuild fails; the rebuild carries on.
    live().line(lineOf("stack:blog", "stack restore failed: nope", "ERR"));
    await flush();
    expect(onLine).toHaveBeenCalledTimes(1);
    expect(onDone).not.toHaveBeenCalled();
    live().line(lineOf("node:n1", "…and back up in the proven order"));
    await flush();
    expect(onDone).toHaveBeenCalledWith("ok", expect.any(String));
  });

  it("shows a stack's services, which log under their own ids, without letting them end it", async () => {
    const onDone = vi.fn(), onLine = vi.fn();
    followRun("stack:shop", { mode: "stack", alsoShowContainers: ["c-web"], onDone, onLine });
    live().line({ ...lineOf("b-web", "backup complete"), container_id: "c-web" });
    live().line({ ...lineOf("b-other", "Archiving 1 volume path(s)"), container_id: "c-elsewhere" });
    await flush();
    expect(onLine).toHaveBeenCalledTimes(1);
    expect(onDone).not.toHaveBeenCalled();
    live().line(lineOf("stack:shop", "Stack backup failed for 1 of 2 service(s): db — each one's own backup log says why", "ERR"));
    await flush();
    expect(onDone.mock.calls[0][0]).toBe("failed");
  });

  it("shows only what the filter accepts, and drops replayed history", () => {
    const onLine = vi.fn();
    const now = Date.now();
    followRun("b1", { mode: "one", since: now - 1000, filter: (m) => /volume/i.test(m), onLine });
    live().line({ backup_id: "b1", level: "INFO", msg: "unrelated chatter", time: new Date(now).toISOString() });
    live().line({ backup_id: "b1", level: "INFO", msg: "volume data copied", time: new Date(now - 60_000).toISOString() });
    expect(onLine).not.toHaveBeenCalled();
    live().line({ backup_id: "b1", level: "INFO", msg: "volume data copied", time: new Date(now).toISOString() });
    expect(onLine).toHaveBeenCalledTimes(1);
  });

  it("releases the connection when the run ends and when the caller stops", () => {
    const stop = followRun("b1", { mode: "one", onDone: vi.fn() });
    expect(live().closed).toBe(false);
    live().event("run.done", { run_id: "b1", outcome: "ok", message: "" });
    expect(live().closed).toBe(true); // nothing left holding it
    stop(); // stopping an already-finished run is safe

    const stop2 = followRun("b2", { mode: "one", onDone: vi.fn() });
    stop2();
    expect(live().closed).toBe(true);
  });
});

// The rules themselves, without the plumbing.
describe("followRun and the server's record", () => {
  const lineOf = (id: string, msg: string, level = "INFO") =>
    ({ backup_id: id, level, msg, time: new Date().toISOString() });
  const statusOf = (outcome: string, message = "", started_at = Math.floor(Date.now() / 1000)) =>
    ({ status: 200, body: { outcome, message, started_at } });

  it("a hook's ERR line does not end a restore that is still running", async () => {
    statusAnswer = statusOf("running");
    const onDone = vi.fn(), onLine = vi.fn();
    followRun("b1", { mode: "one", onDone, onLine });
    live().connect();
    live().line(lineOf("b1", "post-restore hook failed: exit 1", "ERR"));
    await flush();
    expect(onLine).toHaveBeenCalledTimes(1);
    expect(onDone).not.toHaveBeenCalled();

    live().line(lineOf("b1", "still going"));
    expect(onLine).toHaveBeenCalledTimes(2);
    live().event("run.done", { run_id: "b1", outcome: "ok", message: "Restore completed" });
    expect(onDone).toHaveBeenCalledWith("ok", "Restore completed");
  });

  it("an ERR line while the record says it succeeded reports the success", async () => {
    statusAnswer = statusOf("ok", "Restore completed");
    const onDone = vi.fn();
    followRun("b1", { mode: "one", onDone });
    live().line(lineOf("b1", "post-restore hook failed: exit 1", "ERR"));
    await flush();
    expect(onDone).toHaveBeenCalledWith("ok", "Restore completed");
  });

  it("a finish missed while disconnected comes back on reconnect", async () => {
    statusAnswer = statusOf("running");
    const onDone = vi.fn();
    followRun("b1", { mode: "one", executionId: "e1", since: Date.now() - 1000, onDone });
    live().connect();
    await flush();
    live().drop();
    statusAnswer = statusOf("failed", "disk full");
    live().connect();
    await flush();
    expect(onDone).toHaveBeenCalledWith("failed", "disk full");
    expect(statusCalls[statusCalls.length - 1]).toBe("/api/restores/b1/status?execution=e1");
  });

  it("a verdict heard by nothing at all arrives with the poll", async () => {
    vi.useFakeTimers();
    try {
      statusAnswer = statusOf("running");
      const onDone = vi.fn();
      followRun("b1", { mode: "one", since: Date.now() - 1000, onDone });
      statusAnswer = statusOf("ok", "Restore completed");
      await vi.advanceTimersByTimeAsync(31_000);
      expect(onDone).toHaveBeenCalledWith("ok", "Restore completed");
    } finally {
      vi.useRealTimers();
    }
  });

  it("another execution's run.done cannot end this one", () => {
    const onDone = vi.fn();
    followRun("b1", { mode: "one", executionId: "mine", since: Date.now() - 1000, onDone });
    live().event("run.done", { run_id: "b1", execution_id: "previous", outcome: "failed", message: "old" });
    expect(onDone).not.toHaveBeenCalled();
    live().event("run.done", { run_id: "b1", execution_id: "mine", outcome: "ok", message: "done" });
    expect(onDone).toHaveBeenCalledWith("ok", "done");
  });

  it("without an execution id, an older recorded run is not this one", async () => {
    statusAnswer = statusOf("failed", "from last time", Math.floor(Date.now() / 1000) - 3600);
    const onDone = vi.fn();
    followRun("stack:blog", { mode: "stack", since: Date.now() - 1000, onDone });
    live().connect();
    await flush();
    expect(onDone).not.toHaveBeenCalled();
  });

  it("attaching to a run that has just ended learns how at once", async () => {
    statusAnswer = statusOf("canceled", "stopped by the operator");
    const onDone = vi.fn();
    followRun("b1", { mode: "one", onDone });
    await flush();
    expect(onDone).toHaveBeenCalledWith("canceled", "stopped by the operator");
  });

  it("keeps interrupted apart from failed", async () => {
    statusAnswer = statusOf("interrupted", "DockBack stopped");
    const onDone = vi.fn();
    followRun("b1", { mode: "one", onDone });
    await flush();
    expect(onDone).toHaveBeenCalledWith("interrupted", "DockBack stopped");
  });

  it("no answer from the server decides nothing, whatever the line said", async () => {
    statusAnswer = "network-error";
    const onDone = vi.fn();
    followRun("b1", { mode: "one", onDone });
    live().line(lineOf("b1", "Restore failed: boom", "ERR"));
    await flush();
    expect(onDone).not.toHaveBeenCalled();
  });

  it("a run with no record still ends on its lines", async () => {
    const onDone = vi.fn();
    followRun("stack:blog", { mode: "stack", onDone });
    live().line(lineOf("stack:blog", "Stack blog snapshot complete"));
    await flush();
    expect(onDone).toHaveBeenCalledWith("ok", "Stack blog snapshot complete");
  });

  it("stops asking once stopped", async () => {
    statusAnswer = statusOf("running");
    const onDone = vi.fn();
    const stop = followRun("b1", { mode: "one", onDone });
    await flush();
    stop();
    const calls = statusCalls.length;
    live2Connect();
    await flush();
    expect(statusCalls.length).toBe(calls);
    expect(onDone).not.toHaveBeenCalled();
  });
});

describe("lineVerdict", () => {
  it("is silent while a run is running", () => {
    expect(lineVerdict("one", "copying volume data", "INFO")).toBeNull();
    expect(lineVerdict("stack", '[2/5] Restoring service "redis"…', "INFO")).toBeNull();
    expect(lineVerdict("node", "restoring stack blog", "INFO")).toBeNull();
  });
  it("treats an error line as a failure on every path", () => {
    expect(lineVerdict("one", "something went wrong", "ERR")).toBe("failed");
    expect(lineVerdict("node", "something went wrong", "ERR")).toBe("failed");
    expect(lineVerdict("stack", "something went wrong", "ERR")).toBe("failed");
  });
  it("leaves a non-terminal message alone", () => {
    expect(verdictMessage("ok", "restore completed")).toBe("restore completed");
  });
});
