import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, useState } from "react";
import { createRoot, Root } from "react-dom/client";
import { Button, Input, Label, Modal } from "./ui";
import StepUpPrompt from "./StepUpPrompt";

let host: HTMLDivElement;
let root: Root;

beforeEach(() => {
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
});
afterEach(() => {
  act(() => root.unmount());
  host.remove();
  document.body.innerHTML = "";
});

const render = (node: React.ReactNode) => act(() => { root.render(node); });
const press = (key: string, shiftKey = false) =>
  act(() => { (document.activeElement ?? document.body).dispatchEvent(new KeyboardEvent("keydown", { key, shiftKey, bubbles: true })); });
const dialog = () => document.querySelector<HTMLElement>('[role="dialog"]');
const button = (name: string) => Array.from(document.querySelectorAll("button")).find((b) => b.textContent?.includes(name))!;

describe("Label", () => {
  it("names the control that follows it", () => {
    render(<div><Label>Username</Label><Input /></div>);
    const input = host.querySelector("input")!;
    expect(input.labels?.[0]?.textContent).toBe("Username");
  });

  it("names a control wrapped in the next element too", () => {
    render(<div><Label>Address</Label><div><Input /></div></div>);
    expect(host.querySelector("input")!.labels?.[0]?.textContent).toBe("Address");
  });

  it("honours an explicit htmlFor, and leaves a label with no control alone", () => {
    render(<div><Label htmlFor="mine">Code</Label><Input id="mine" /><Label>Heading only</Label></div>);
    expect(host.querySelector<HTMLInputElement>("#mine")!.labels?.[0]?.textContent).toBe("Code");
    const labels = Array.from(host.querySelectorAll("label"));
    expect(labels[labels.length - 1].htmlFor).toBe("");
  });
});

function DialogHarness({ dismissible = true, onClose }: { dismissible?: boolean; onClose?: () => void }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <Button onClick={() => setOpen(true)}>Open it</Button>
      <Button>Behind the dialog</Button>
      <Modal open={open} title="Delete backups" dismissible={dismissible}
        onClose={() => { onClose?.(); setOpen(false); }}
        footer={<><Button onClick={() => setOpen(false)}>Cancel</Button><Button>Delete</Button></>}>
        <Label>Type the name</Label><Input />
      </Modal>
    </>
  );
}

const openFrom = (name: string) => {
  const trigger = button(name);
  act(() => { trigger.focus(); trigger.click(); });
  return trigger;
};

describe("Modal", () => {
  it("is a dialog named by its title", () => {
    render(<DialogHarness />);
    openFrom("Open it");
    const shown = dialog()!;
    expect(shown.getAttribute("aria-modal")).toBe("true");
    expect(document.getElementById(shown.getAttribute("aria-labelledby")!)?.textContent).toBe("Delete backups");
  });

  it("takes focus on open and keeps Tab inside, both ways", () => {
    render(<DialogHarness />);
    openFrom("Open it");
    const input = dialog()!.querySelector("input")!;
    expect(document.activeElement).toBe(input);
    act(() => button("Delete").focus());
    press("Tab");
    expect(document.activeElement).toBe(input);
    press("Tab", true);
    expect(document.activeElement).toBe(button("Delete"));
  });

  it("puts the page behind it out of reach, and gives it back", () => {
    render(<DialogHarness />);
    openFrom("Open it");
    expect(host.hasAttribute("inert")).toBe(true);
    press("Escape");
    expect(host.hasAttribute("inert")).toBe(false);
  });

  it("closes on Escape and returns focus to what opened it", () => {
    render(<DialogHarness />);
    const trigger = openFrom("Open it");
    press("Escape");
    expect(dialog()).toBeNull();
    expect(document.activeElement).toBe(trigger);
  });

  it("cannot be dismissed while it says so, but its own buttons still work", () => {
    const onClose = vi.fn();
    render(<DialogHarness dismissible={false} onClose={onClose} />);
    openFrom("Open it");
    press("Escape");
    act(() => document.querySelector<HTMLElement>('[role="dialog"]')!.parentElement!.click());
    expect(onClose).not.toHaveBeenCalled();
    expect(dialog()).not.toBeNull();
    act(() => button("Cancel").click());
    expect(dialog()).toBeNull();
  });

  it("lets a step-up inside it take focus, and names its fields", () => {
    render(
      <Modal open title="Reveal key" onClose={() => {}}>
        <StepUpPrompt totp onConfirm={() => {}} />
      </Modal>,
    );
    const password = document.querySelector<HTMLInputElement>('input[type="password"]')!;
    expect(document.activeElement).toBe(password);
    expect(password.labels?.[0]?.textContent).toBe("Password");
    const code = document.querySelector<HTMLInputElement>('input[inputmode="numeric"]')!;
    expect(code.labels?.[0]?.textContent).toBe("Two-factor code");
  });

  it("a dialog inside a dialog is in front, and keeps Escape to itself", () => {
    const outerClosed = vi.fn(), innerClosed = vi.fn();
    render(
      <Modal open title="Outer" onClose={outerClosed}>
        <Modal open title="Inner" onClose={innerClosed}><Input /></Modal>
      </Modal>,
    );
    const titled = (title: string) => Array.from(document.querySelectorAll<HTMLElement>('[role="dialog"]'))
      .find((d) => d.querySelector("h2")?.textContent === title)!;
    const outer = titled("Outer"), inner = titled("Inner");
    expect(inner.closest("[inert]")).toBeNull();
    expect(outer.closest("[inert]")).not.toBeNull();
    expect(document.activeElement).toBe(inner.querySelector("input"));
    press("Escape");
    expect(innerClosed).toHaveBeenCalledTimes(1);
    expect(outerClosed).not.toHaveBeenCalled();
  });
});
