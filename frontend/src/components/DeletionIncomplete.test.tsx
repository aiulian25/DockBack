import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { createRoot, Root } from "react-dom/client";
import DeletionIncomplete from "./DeletionIncomplete";
import { api, Backup } from "../api";

const toast = { success: vi.fn(), error: vi.fn(), info: vi.fn(), undo: vi.fn(), action: vi.fn() };
vi.mock("./Toast", () => ({ useToast: () => toast }));

const BACKUP_ID = "bk-partial";
const LOCKED_COPY = "offsite-bucket";
const UNREACHABLE_COPY = "offsite-nas";
const LOCK_ENDS = Date.UTC(2026, 11, 24, 12) / 1000;

function deletingBackup(): Backup {
  const copies = [
    { kind: "dest", dest_id: "d1", name: LOCKED_COPY, type: "s3", immutable: true, lock_until: LOCK_ENDS, delete_outcome: "locked" },
    { kind: "dest", dest_id: "d2", name: UNREACHABLE_COPY, type: "smb", delete_outcome: "failed", delete_detail: "connection refused" },
  ];
  return { id: BACKUP_ID, node_id: "n1", stack: "", target_name: "app", status: "deleting", locations: JSON.stringify(copies) } as Backup;
}

describe("DeletionIncomplete", () => {
  let host: HTMLDivElement;
  let root: Root;
  const onClose = vi.fn();
  const onChanged = vi.fn();

  const open = async () => {
    await act(async () => {
      root.render(<DeletionIncomplete backup={deletingBackup()} nodeName="home" onClose={onClose} onChanged={onChanged} />);
    });
  };
  const dialog = () => document.body.querySelector('[role="dialog"]') as HTMLElement;
  const button = (label: string) =>
    Array.from(dialog().querySelectorAll("button")).find((candidate) => candidate.textContent?.trim() === label) as HTMLButtonElement;

  beforeEach(() => {
    (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
    vi.spyOn(api, "backup").mockResolvedValue(deletingBackup());
    host = document.createElement("div");
    document.body.appendChild(host);
    root = createRoot(host);
  });
  afterEach(() => {
    act(() => root.unmount());
    host.remove();
    vi.restoreAllMocks();
    vi.clearAllMocks();
  });

  it("is a dialog that names every copy still stored and why", async () => {
    await open();
    expect(dialog().getAttribute("aria-modal")).toBe("true");
    const text = dialog().textContent || "";
    expect(text).toContain(LOCKED_COPY);
    expect(text).toContain("Locked until");
    expect(text).toContain(UNREACHABLE_COPY);
    expect(text).toContain("Could not be deleted (connection refused)");
    expect(text).toContain("can no longer be restored");
  });

  it("closes once a retry removes the last copy", async () => {
    const retry = vi.spyOn(api, "deleteBackup").mockResolvedValue({ status: "deleted" });
    await open();
    await act(async () => { button("Try again now").click(); });
    expect(retry).toHaveBeenCalledWith(BACKUP_ID);
    expect(toast.success).toHaveBeenCalledWith("Backup deleted.");
    expect(onChanged).toHaveBeenCalled();
    expect(onClose).toHaveBeenCalled();
  });

  it("stays open and says what is left when a retry still cannot finish", async () => {
    vi.spyOn(api, "deleteBackup").mockResolvedValue({ status: "incomplete", kept: [{ location: UNREACHABLE_COPY, outcome: "failed" }] });
    await open();
    await act(async () => { button("Try again now").click(); });
    expect(toast.info).toHaveBeenCalledWith(expect.stringContaining(UNREACHABLE_COPY));
    expect(onClose).not.toHaveBeenCalled();
  });

  it("stops tracking only after a second, explicit confirmation", async () => {
    const forget = vi.spyOn(api, "forgetBackup").mockResolvedValue({ status: "forgotten" });
    await open();
    await act(async () => { button("Stop tracking…").click(); });
    expect(forget).not.toHaveBeenCalled();
    expect(dialog().textContent).toContain("delete them by hand");

    await act(async () => { button("Stop tracking").click(); });
    expect(forget).toHaveBeenCalledWith(BACKUP_ID);
    expect(onClose).toHaveBeenCalled();
  });
});
