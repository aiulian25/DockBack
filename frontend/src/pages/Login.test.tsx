import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { createRoot, Root } from "react-dom/client";
import { MemoryRouter } from "react-router-dom";
import Login from "./Login";

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
  vi.unstubAllGlobals();
  Reflect.deleteProperty(navigator, "credentials");
});

// The sign-in page asks whether to offer a passkey; answer as the server would.
function passkeyStatus(status: { usable: boolean; offered: boolean; origin: string }) {
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({ reason: "", ...status }), { status: 200 })));
}

async function renderLogin() {
  await act(async () => { root.render(<MemoryRouter><Login /></MemoryRouter>); });
}

// A browser that can use passkeys: jsdom has neither of these.
function passkeyCapableBrowser() {
  vi.stubGlobal("PublicKeyCredential", class {});
  Object.defineProperty(navigator, "credentials", { value: {}, configurable: true });
}

const passkeyButton = () => Array.from(host.querySelectorAll("button")).find((b) => b.textContent?.includes("passkey"));

describe("Login", () => {
  it("names both fields for assistive technology", () => {
    act(() => { root.render(<MemoryRouter><Login /></MemoryRouter>); });
    const names = Array.from(host.querySelectorAll("input")).map((input) => input.labels?.[0]?.textContent);
    expect(names).toEqual(["Username", "Password"]);
  });

  it("offers a passkey once someone here has one, at the address passkeys belong to", async () => {
    passkeyCapableBrowser();
    passkeyStatus({ usable: true, offered: true, origin: window.location.origin });
    await renderLogin();
    expect(passkeyButton()).toBeDefined();
    expect(passkeyButton()?.getAttribute("type")).toBe("button");
  });

  it("offers no passkey at another address, or when nobody has one", async () => {
    passkeyCapableBrowser();
    passkeyStatus({ usable: true, offered: true, origin: "https://elsewhere.example" });
    await renderLogin();
    expect(passkeyButton()).toBeUndefined();

    act(() => root.unmount());
    root = createRoot(host);
    passkeyStatus({ usable: true, offered: false, origin: window.location.origin });
    await renderLogin();
    expect(passkeyButton()).toBeUndefined();
  });
});
