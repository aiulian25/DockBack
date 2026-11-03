import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { createRoot, Root } from "react-dom/client";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { writeStackRestoreSeed } from "../lib/stackRestoreSeed";
import StackRestore from "./StackRestore";

// The page previewed one restore and requested another: the plan and the confirm
// said "1 of 2 services, reading from NAS" while the request named neither, and
// the server reads an absent list as the whole stack — so an unticked database
// was overwritten. These render the real page over the real API client and read
// what reaches the wire, because that request is the only thing that matters.

const NODE = "n1";
const PROJECT = "blog";
const RESTORE_PATH = `/api/nodes/${NODE}/stacks/${PROJECT}/restore`;
const PLAN_DEBOUNCE_MS = 300;
const NAS_COPY = "dest-nas";
const TEST_PASSWORD = "test-password";

const STACK_PAGE_TEXT = "the stack's own page";
const OFFERED_COPIES = [
  { id: "local", name: "local", type: "local", services: 2 },
  { id: NAS_COPY, name: "NAS", type: "smb", services: 2 },
];

function planMember(service: string, order: number) {
  return {
    order, service, backup_id: `b-${service}`, target_name: `${PROJECT}-${service}`,
    created_at: 1_700_000_000, verified: "verified", data_tier: false, partial: false, incremental: false,
  };
}

const TWO_SERVICE_PLAN = {
  services: [planMember("db", 1), planMember("web", 2)],
  target_dirs: {},
  source_copies: OFFERED_COPIES,
};

function reply(status: number, body: unknown): Response {
  return { status, ok: status >= 200 && status < 300, statusText: "", text: async () => JSON.stringify(body) } as unknown as Response;
}

const STEP_UP_REPLY = () => reply(401, { error: "re-authentication required", step_up_required: true, totp_required: false });

let holder: HTMLDivElement;
let root: Root;
let restoreReplies: Response[];
let restoreRequests: { params: URLSearchParams; body: Record<string, string> }[];
let confirmDialog: ReturnType<typeof vi.fn>;

async function serveFakeApi(path: string, init?: RequestInit): Promise<Response> {
  const url = new URL(path, "http://dockback.test");
  if (url.pathname === RESTORE_PATH) {
    restoreRequests.push({ params: url.searchParams, body: JSON.parse(String(init?.body ?? "{}")) });
    return restoreReplies.shift() ?? reply(202, { status: "started" });
  }
  if (url.pathname === "/api/nodes") return reply(200, [{ id: NODE, name: "source-node", reachable: true }]);
  if (url.pathname === `/api/nodes/${NODE}/containers`) return reply(200, { containers: [], stacks: [] });
  if (url.pathname.endsWith("/restore-plan")) return reply(200, TWO_SERVICE_PLAN);
  if (url.pathname.endsWith("/restore-defaults")) return reply(200, {});
  if (url.pathname.endsWith("/groups")) return reply(200, []);
  throw new Error(`the fake API has no answer for ${path}`);
}

beforeEach(() => {
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
  vi.stubGlobal("fetch", vi.fn(serveFakeApi));
  confirmDialog = vi.fn(() => true);
  vi.stubGlobal("confirm", confirmDialog);
  sessionStorage.clear();
  restoreReplies = [];
  restoreRequests = [];
  holder = document.createElement("div");
  document.body.appendChild(holder);
  root = createRoot(holder);
});

afterEach(() => {
  act(() => root.unmount());
  holder.remove();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

// Lets every pending reply land and, past the debounce, the plan request too.
const settle = () => act(async () => { await vi.advanceTimersByTimeAsync(PLAN_DEBOUNCE_MS + 1); });

async function openPage() {
  await act(async () => {
    root.render(
      <MemoryRouter initialEntries={[`/servers/${NODE}/stacks/${PROJECT}/restore`]}>
        <Routes>
          <Route path="/servers/:id/stacks/:project/restore" element={<StackRestore />} />
          <Route path="/servers/:id/stacks/:project" element={<div>{STACK_PAGE_TEXT}</div>} />
        </Routes>
      </MemoryRouter>,
    );
  });
  await settle();
}

function buttonLabelled(label: string): HTMLButtonElement {
  const button = [...holder.querySelectorAll("button")].find((candidate) => candidate.textContent?.includes(label));
  if (!button) throw new Error(`no button labelled "${label}"`);
  return button;
}

const restoreButton = () => buttonLabelled("Restore stack");
const passwordField = () => holder.querySelector<HTMLInputElement>('input[type="password"]');

function sourcePicker(): HTMLSelectElement {
  const picker = [...holder.querySelectorAll("select")].find((select) => select.options[0]?.text.startsWith("Auto"));
  if (!picker) throw new Error("no source copy picker");
  return picker;
}

async function clickAndSettle(element: HTMLElement) {
  await act(async () => { element.click(); });
  await settle();
}

async function untick(service: string) {
  const box = holder.querySelector<HTMLInputElement>(`input[aria-label="Restore ${service}"]`);
  if (!box) throw new Error(`no checkbox for ${service}`);
  await clickAndSettle(box);
}

async function chooseCopy(id: string) {
  const picker = sourcePicker();
  await act(async () => {
    picker.value = id;
    picker.dispatchEvent(new Event("change", { bubbles: true }));
  });
}

async function confirmPassword() {
  const field = passwordField();
  if (!field) throw new Error("no password prompt");
  const setNativeValue = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
  await act(async () => {
    setNativeValue?.call(field, TEST_PASSWORD);
    field.dispatchEvent(new Event("input", { bubbles: true }));
  });
  await clickAndSettle(buttonLabelled("Confirm and restore"));
}

function expectReviewedScope(request: { params: URLSearchParams }) {
  expect(request.params.getAll("service")).toEqual(["db"]);
  expect(request.params.get("source")).toBe(NAS_COPY);
}

describe("StackRestore submits what the operator reviewed", () => {
  it("sends only the services left ticked and the chosen copy", async () => {
    await openPage();
    await untick("web");
    await chooseCopy(NAS_COPY);
    expect(holder.textContent).toContain("1 of 2 service");
    expect(holder.textContent).toContain("reading from NAS");

    await clickAndSettle(restoreButton());

    expect(restoreRequests).toHaveLength(1);
    expectReviewedScope(restoreRequests[0]);
    expect(holder.textContent).toContain(STACK_PAGE_TEXT);
  });

  // The engine reads ANY list as a partial restore and then will not write the
  // compose file it rebuilt, so a full restore has to stay a request without one.
  it("names no service and no copy for the whole stack on Auto", async () => {
    await openPage();

    await clickAndSettle(restoreButton());

    expect(restoreRequests).toHaveLength(1);
    expect(restoreRequests[0].params.has("service")).toBe(false);
    expect(restoreRequests[0].params.has("source")).toBe(false);
  });

  it("keeps the scope through the password prompt and does not ask to confirm twice", async () => {
    restoreReplies = [STEP_UP_REPLY()];
    await openPage();
    await untick("web");
    await chooseCopy(NAS_COPY);

    await clickAndSettle(restoreButton());
    await confirmPassword();

    expect(restoreRequests).toHaveLength(2);
    expectReviewedScope(restoreRequests[1]);
    expect(restoreRequests[1].body.password).toBe(TEST_PASSWORD);
    expect(restoreRequests[1].params.has("password")).toBe(false);
    expect(confirmDialog).toHaveBeenCalledTimes(1);
  });

  // The server asks about devices first and the password second, so an operator
  // can meet both on one restore. Neither answer may be lost to the other.
  it("keeps the scope and the device answer through both prompts", async () => {
    restoreReplies = [
      reply(409, { error: "the target machine lacks devices", missing_devices: ["db: /dev/dri"] }),
      STEP_UP_REPLY(),
    ];
    await openPage();
    await untick("web");
    await chooseCopy(NAS_COPY);

    await clickAndSettle(restoreButton());
    await confirmPassword();

    expect(restoreRequests).toHaveLength(3);
    expect(restoreRequests[0].params.has("confirm_missing_devices")).toBe(false);
    for (const answered of restoreRequests.slice(1)) {
      expectReviewedScope(answered);
      expect(answered.params.get("confirm_missing_devices")).toBe("true");
    }
    expect(restoreRequests[2].body.password).toBe(TEST_PASSWORD);
    expect(confirmDialog).toHaveBeenCalledTimes(2);
  });

  it("declining the device prompt sends nothing further", async () => {
    restoreReplies = [reply(409, { error: "the target machine lacks devices", missing_devices: ["db: /dev/dri"] })];
    await openPage();
    await untick("web");
    confirmDialog.mockReturnValueOnce(true).mockReturnValueOnce(false);

    await clickAndSettle(restoreButton());

    expect(restoreRequests).toHaveLength(1);
    expect(holder.textContent).not.toContain(STACK_PAGE_TEXT);
  });

  // The prompt completes the request that was confirmed. With a different choice
  // on the page it would be completing something the operator is no longer looking
  // at, so that choice has to go through the confirm again.
  it("withdraws the password prompt when the selection changes, and returns it when changed back", async () => {
    restoreReplies = [STEP_UP_REPLY()];
    await openPage();
    await clickAndSettle(restoreButton());
    expect(passwordField()).not.toBeNull();

    await untick("web");
    expect(passwordField()).toBeNull();

    const box = holder.querySelector<HTMLInputElement>('input[aria-label="Restore web"]');
    await clickAndSettle(box as HTMLInputElement);
    expect(passwordField()).not.toBeNull();
  });

  it("withdraws the password prompt when the chosen copy changes", async () => {
    restoreReplies = [STEP_UP_REPLY()];
    await openPage();
    await clickAndSettle(restoreButton());
    expect(passwordField()).not.toBeNull();

    await chooseCopy(NAS_COPY);

    expect(passwordField()).toBeNull();
  });

  // A reply can arrive after the operator has moved on: the prompt it opens must
  // not offer to complete the older choice under the newer one.
  it("does not open the password prompt for a selection changed while the request was in flight", async () => {
    let answerRestore: (response: Response) => void = () => {};
    const stalled = new Promise<Response>((resolve) => { answerRestore = resolve; });
    vi.stubGlobal("fetch", vi.fn(async (path: string, init?: RequestInit) => (
      new URL(path, "http://dockback.test").pathname === RESTORE_PATH ? stalled : serveFakeApi(path, init)
    )));
    await openPage();
    await clickAndSettle(restoreButton());

    await untick("web");
    await act(async () => { answerRestore(STEP_UP_REPLY()); });
    await settle();

    expect(passwordField()).toBeNull();
  });

  // Carried over from a backup's drawer, or left behind by a different point in
  // time. Sending it would read from wherever it falls back to while the page
  // names the copy, and quietly clearing it would swap the operator's choice.
  describe("a chosen copy these backups do not have", () => {
    const WITHDRAWN_COPY = "dest-removed";

    beforeEach(() => {
      writeStackRestoreSeed({ nodeID: NODE, project: PROJECT, source: WITHDRAWN_COPY });
    });

    it("blocks the restore and says why", async () => {
      await openPage();

      expect(restoreButton().disabled).toBe(true);
      expect(holder.textContent).toContain("no copy there");
      expect(holder.textContent).not.toContain(`reading from ${WITHDRAWN_COPY}`);
      await clickAndSettle(restoreButton());
      expect(restoreRequests).toHaveLength(0);
    });

    it("shows as the picker's value, so choosing another is a real change", async () => {
      await openPage();
      expect(sourcePicker().value).toBe(WITHDRAWN_COPY);

      await chooseCopy("");

      expect(restoreButton().disabled).toBe(false);
      await clickAndSettle(restoreButton());
      expect(restoreRequests[0].params.has("source")).toBe(false);
    });

    it("can be replaced by a copy they do have", async () => {
      await openPage();

      await chooseCopy(NAS_COPY);
      await clickAndSettle(restoreButton());

      expect(restoreRequests[0].params.get("source")).toBe(NAS_COPY);
    });
  });
});
