import { describe, it, expect, vi } from "vitest";
import { onRunDone } from "./runDone";

// A stand-in for EventSource: records listeners and lets a test fire one.
function fakeES() {
  const listeners: Record<string, EventListener[]> = {};
  return {
    addEventListener: (t: string, l: EventListener) => { (listeners[t] ||= []).push(l); },
    removeEventListener: (t: string, l: EventListener) => {
      listeners[t] = (listeners[t] || []).filter((x) => x !== l);
    },
    fire(type: string, data: unknown) {
      const raw = typeof data === "string" ? data : JSON.stringify(data);
      for (const l of listeners[type] || []) l({ data: raw } as MessageEvent);
    },
    count: (t: string) => (listeners[t] || []).length,
  };
}

describe("onRunDone", () => {
  it("delivers the outcome for its own run", () => {
    const es = fakeES();
    const fn = vi.fn();
    onRunDone(es as unknown as EventSource, "stack:paperlessngx", fn);
    es.fire("run.done", { run_id: "stack:paperlessngx", outcome: "ok", message: "Stack restore completed" });
    expect(fn).toHaveBeenCalledWith("ok", "Stack restore completed");
  });

  it("ignores another run's event", () => {
    // Two consoles open at once must never end each other's run.
    const es = fakeES();
    const fn = vi.fn();
    onRunDone(es as unknown as EventSource, "stack:paperlessngx", fn);
    es.fire("run.done", { run_id: "stack:wikijs", outcome: "failed", message: "boom" });
    expect(fn).not.toHaveBeenCalled();
  });

  it("carries canceled as its own answer", () => {
    // The operator chose it; a destructive run stopped part-way is not a failure.
    const es = fakeES();
    const fn = vi.fn();
    onRunDone(es as unknown as EventSource, "b1", fn);
    es.fire("run.done", { run_id: "b1", outcome: "canceled", message: "stopped at step 2" });
    expect(fn).toHaveBeenCalledWith("canceled", "stopped at step 2");
  });

  it("a malformed or unknown frame is not a verdict", () => {
    const es = fakeES();
    const fn = vi.fn();
    onRunDone(es as unknown as EventSource, "b1", fn);
    es.fire("run.done", "{{{ not json");
    es.fire("run.done", { run_id: "b1", outcome: "weird", message: "" });
    es.fire("run.done", { outcome: "ok" });
    expect(fn).not.toHaveBeenCalled();
  });

  it("detaches on request", () => {
    const es = fakeES();
    const fn = vi.fn();
    const off = onRunDone(es as unknown as EventSource, "b1", fn);
    expect(es.count("run.done")).toBe(1);
    off();
    expect(es.count("run.done")).toBe(0);
    es.fire("run.done", { run_id: "b1", outcome: "ok", message: "" });
    expect(fn).not.toHaveBeenCalled();
  });

  it("a missing message is empty, never undefined", () => {
    const es = fakeES();
    const fn = vi.fn();
    onRunDone(es as unknown as EventSource, "b1", fn);
    es.fire("run.done", { run_id: "b1", outcome: "ok" });
    expect(fn).toHaveBeenCalledWith("ok", "");
  });
});
