import { describe, expect, it, vi } from "vitest";
import type { ToastApi } from "../components/Toast";
import { announceRunEnd, RUN_NOTICE_MS, runEndNotice } from "./runNotice";

describe("the notice a finished run gives", () => {
  it("says what finished, and how", () => {
    expect(runEndNotice("Stack “handbrake” backup", "finished", "ok")).toEqual({ kind: "success", text: "Stack “handbrake” backup finished." });
    expect(runEndNotice("Restore of web", "finished", "canceled", "by the operator")).toEqual({ kind: "info", text: "Restore of web was canceled." });
    expect(runEndNotice("Backup of db", "finished", "failed", "disk full.")).toEqual({ kind: "error", text: "Backup of db failed: disk full." });
    expect(runEndNotice("Backup of db", "finished", "failed")).toEqual({ kind: "error", text: "Backup of db failed." });
    expect(runEndNotice("Restore of node “nas”", "finished", "interrupted")).toEqual({ kind: "error", text: "Restore of node “nas” was interrupted because DockBack stopped." });
  });

  it("stays on screen long enough to be seen", () => {
    const toast = { success: vi.fn(), error: vi.fn(), info: vi.fn() } as unknown as ToastApi;
    announceRunEnd(toast, runEndNotice("Backup of db", "finished", "ok"));
    expect(toast.success).toHaveBeenCalledWith("Backup of db finished.", RUN_NOTICE_MS);
  });
});
