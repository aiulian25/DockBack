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

beforeEach(() => {
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

  it("falls back to the line rules when no event arrives", () => {
    const cases: [string, string, string][] = [
      ["restore completed in 4s", "INFO", "ok"],
      ["restore canceled by the operator: user pressed stop", "INFO", "canceled"],
      ["restore failed: volume is read-only", "ERR", "failed"],
    ];
    for (const [msg, level, want] of cases) {
      const onDone = vi.fn();
      followRun("b1", { mode: "one", onDone });
      live().line(lineOf("b1", msg, level));
      expect(onDone.mock.calls[0][0], msg).toBe(want);
    }
  });

  it("trims the prefix a console would otherwise repeat back at the operator", () => {
    const onDone = vi.fn();
    followRun("b1", { mode: "one", onDone });
    live().line(lineOf("b1", "restore failed: volume is read-only", "ERR"));
    expect(onDone).toHaveBeenCalledWith("failed", "volume is read-only");
  });

  it("ends a run once, whichever signal is first", () => {
    const onDone = vi.fn();
    followRun("b1", { mode: "one", onDone });
    live().line(lineOf("b1", "restore completed"));
    live().event("run.done", { run_id: "b1", outcome: "failed", message: "too late" });
    live().line(lineOf("b1", "restore failed: also too late", "ERR"));
    expect(onDone).toHaveBeenCalledTimes(1);
    expect(onDone).toHaveBeenCalledWith("ok", "restore completed");
  });

  it("a per-service line can never end a stack run — THE bug this shares", () => {
    const onDone = vi.fn(), onLine = vi.fn();
    followRun("stack:blog", { mode: "stack", onDone, onLine });
    live().line(lineOf("stack:blog", '[1/5] Service "db" restored'));
    live().line(lineOf("stack:blog", '[5/5] Service "web" restored'));
    expect(onDone).not.toHaveBeenCalled();
    expect(onLine).toHaveBeenCalledTimes(2);
    live().line(lineOf("stack:blog", 'Stack "blog" restored — all 5 services are running'));
    expect(onDone).toHaveBeenCalledWith("ok", expect.stringContaining("all 5 services"));
  });

  it("a partial stack failure is a failure, not a success", () => {
    const onDone = vi.fn();
    followRun("stack:blog", { mode: "stack", onDone });
    live().line(lineOf("stack:blog", 'Stack "blog" restored, but 2 service(s) did not become healthy', "WARN"));
    expect(onDone.mock.calls[0][0]).toBe("failed");
  });

  it("a node rebuild is decided only by its own lines", () => {
    const onDone = vi.fn(), onLine = vi.fn();
    followRun("node:n1", { mode: "node", alsoShow: ["stack:blog"], onDone, onLine });
    // A stack inside the rebuild fails; the rebuild carries on.
    live().line(lineOf("stack:blog", "stack restore failed: nope", "ERR"));
    expect(onLine).toHaveBeenCalledTimes(1);
    expect(onDone).not.toHaveBeenCalled();
    live().line(lineOf("node:n1", "…and back up in the proven order"));
    expect(onDone).toHaveBeenCalledWith("ok", expect.any(String));
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
