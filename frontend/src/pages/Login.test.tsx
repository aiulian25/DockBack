import { afterEach, beforeEach, describe, expect, it } from "vitest";
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
});

describe("Login", () => {
  it("names both fields for assistive technology", () => {
    act(() => { root.render(<MemoryRouter><Login /></MemoryRouter>); });
    const names = Array.from(host.querySelectorAll("input")).map((input) => input.labels?.[0]?.textContent);
    expect(names).toEqual(["Username", "Password"]);
  });
});
