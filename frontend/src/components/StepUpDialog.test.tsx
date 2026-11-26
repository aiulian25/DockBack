import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { createRoot, Root } from "react-dom/client";
import { STEP_UP_CANCELLED_MESSAGE, useStepUp } from "./StepUpDialog";
import { StepUpCredentials, StepUpError } from "../api";

const PASSWORD = "correct-horse";

type Action = (credentials?: StepUpCredentials) => Promise<string>;

let outcome: Promise<string>;

function Harness({ action }: { action: Action }) {
  const { confirmed, dialog } = useStepUp();
  return (
    <>
      <button onClick={() => { outcome = confirmed(action); outcome.catch(() => {}); }}>Save</button>
      {dialog}
    </>
  );
}

/** An action the server refuses until the right password comes with it. */
function protectedAction(): ReturnType<typeof vi.fn<Action>> {
  return vi.fn<Action>(async (credentials) => {
    if (!credentials) throw new StepUpError("re-authentication required", false);
    if (credentials.password !== PASSWORD) throw new StepUpError("password is incorrect", false);
    return "saved";
  });
}

describe("useStepUp", () => {
  let host: HTMLDivElement;
  let root: Root;

  const mount = async (action: Action) => {
    await act(async () => { root.render(<Harness action={action} />); });
  };
  const save = async () => {
    await act(async () => { (host.querySelector("button") as HTMLButtonElement).click(); });
  };
  const dialog = () => document.body.querySelector('[role="dialog"]') as HTMLElement | null;
  const typePassword = async (value: string) => {
    const input = dialog()!.querySelector('input[type="password"]') as HTMLInputElement;
    const setValue = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
    await act(async () => {
      setValue.call(input, value);
      input.dispatchEvent(new Event("input", { bubbles: true }));
    });
  };
  const confirm = async () => {
    const button = Array.from(dialog()!.querySelectorAll("button")).find((candidate) => candidate.textContent?.includes("Confirm")) as HTMLButtonElement;
    await act(async () => { button.click(); });
  };

  beforeEach(() => {
    (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
    host = document.createElement("div");
    document.body.appendChild(host);
    root = createRoot(host);
  });
  afterEach(() => {
    act(() => root.unmount());
    host.remove();
  });

  it("asks nothing when the server does not", async () => {
    const action = vi.fn<Action>(async () => "saved");
    await mount(action);
    await save();
    expect(dialog()).toBeNull();
    await expect(outcome).resolves.toBe("saved");
    expect(action).toHaveBeenCalledTimes(1);
  });

  it("asks for the password and retries the same action with it", async () => {
    const action = protectedAction();
    await mount(action);
    await save();
    expect(dialog()?.getAttribute("aria-modal")).toBe("true");

    await typePassword(PASSWORD);
    await confirm();
    await expect(outcome).resolves.toBe("saved");
    expect(action).toHaveBeenLastCalledWith({ password: PASSWORD, code: "" });
    expect(dialog()).toBeNull();
  });

  it("keeps asking, with the reason, after a wrong password", async () => {
    await mount(protectedAction());
    await save();
    await typePassword("wrong");
    await confirm();
    expect(dialog()?.textContent).toContain("password is incorrect");
  });

  it("fails the action when the prompt is closed", async () => {
    await mount(protectedAction());
    await save();
    await act(async () => { dialog()!.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true })); });
    await expect(outcome).rejects.toThrow(STEP_UP_CANCELLED_MESSAGE);
    expect(dialog()).toBeNull();
  });

  it("passes other failures straight through", async () => {
    await mount(vi.fn<Action>(async () => { throw new Error("server unreachable"); }));
    await save();
    await expect(outcome).rejects.toThrow("server unreachable");
    expect(dialog()).toBeNull();
  });
});
