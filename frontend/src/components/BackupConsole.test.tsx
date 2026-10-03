import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { act } from "react";
import { createRoot, Root } from "react-dom/client";
import type { LogLine } from "../lib/logStream";

// The backup page told operators to "watch the console below" and had no
// console. It now shares this one with the container page, so what it shows —
// and what it must not show — is pinned here.

let deliver: ((line: LogLine) => void) | null = null;
vi.mock("../lib/logStream", () => ({
  subscribeLines: (fn: (line: LogLine) => void) => {
    deliver = fn;
    return () => { deliver = null; };
  },
}));

import BackupConsole from "./BackupConsole";

const LINE_LIMIT = 500;

let holder: HTMLDivElement;
let root: Root;

beforeEach(() => {
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
  holder = document.createElement("div");
  document.body.appendChild(holder);
  root = createRoot(holder);
});
afterEach(() => {
  act(() => root.unmount());
  holder.remove();
});

function emit(containerId: string, msg: string) {
  act(() => deliver?.({ time: "2026-10-03T12:00:00Z", container_id: containerId, level: "INFO", msg }));
}
const shown = () => holder.textContent || "";

describe("BackupConsole", () => {
  // The stream carries every node's output. Another container's backup must
  // not appear here, even one running at the same moment.
  it("shows only its own container's lines", () => {
    act(() => root.render(<BackupConsole containerId="mine" />));
    emit("mine", "archiving /data");
    emit("theirs", "archiving someone else");
    expect(shown()).toContain("archiving /data");
    expect(shown()).not.toContain("someone else");
  });

  it("keeps only the most recent lines of a long run", () => {
    act(() => root.render(<BackupConsole containerId="mine" />));
    act(() => {
      for (let index = 0; index < LINE_LIMIT + 20; index++) {
        deliver?.({ time: "2026-10-03T12:00:00Z", container_id: "mine", level: "INFO", msg: `line-${index};` });
      }
    });
    expect(holder.querySelectorAll(".py-0\\.5")).toHaveLength(LINE_LIMIT);
    expect(shown()).not.toContain("line-19;");
    expect(shown()).toContain("line-20;");
    expect(shown()).toContain(`line-${LINE_LIMIT + 19};`);
  });

  it("starts over when it is pointed at a different container", () => {
    act(() => root.render(<BackupConsole containerId="first" />));
    emit("first", "from the first container");
    act(() => root.render(<BackupConsole containerId="second" />));
    expect(shown()).not.toContain("from the first container");
    expect(shown()).toContain("Awaiting system events");
  });

  it("stops listening when it leaves the page", () => {
    act(() => root.render(<BackupConsole containerId="mine" />));
    expect(deliver).not.toBeNull();
    act(() => root.unmount());
    expect(deliver).toBeNull();
    root = createRoot(holder);
  });
});
