import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, useRef } from "react";
import { createRoot, Root } from "react-dom/client";
import { MemoryRouter, useNavigate } from "react-router-dom";
import ScrollToTop from "./ScrollToTop";

let host: HTMLDivElement;
let root: Root;
let pane: HTMLElement;
let go: (to: string) => void;

function Page() {
  const ref = useRef<HTMLElement | null>(null);
  go = useNavigate();
  return (
    <>
      <main ref={(el) => { ref.current = el; if (el) pane = el; }} />
      <ScrollToTop target={ref} />
    </>
  );
}

const button = () => host.querySelector('button[aria-label="Back to top"]') as HTMLButtonElement | null;

function scrollPaneTo(top: number) {
  pane.scrollTop = top;
  act(() => { pane.dispatchEvent(new Event("scroll")); });
}

beforeEach(() => {
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  // jsdom has no layout, so it has no element scrolling either.
  HTMLElement.prototype.scrollTo = vi.fn(function (this: HTMLElement, x?: number | ScrollToOptions) {
    this.scrollTop = typeof x === "number" ? 0 : x?.top ?? 0;
  }) as typeof HTMLElement.prototype.scrollTo;
  act(() => { root.render(<MemoryRouter initialEntries={["/backups"]}><Page /></MemoryRouter>); });
});
afterEach(() => {
  act(() => root.unmount());
  host.remove();
});

describe("ScrollToTop", () => {
  it("appears only once the page is scrolled down, and takes it back to the top", () => {
    expect(button()).toBeNull();
    scrollPaneTo(1200);
    expect(button()).not.toBeNull();

    act(() => button()!.click());
    expect(pane.scrollTo).toHaveBeenLastCalledWith(expect.objectContaining({ top: 0 }));
    scrollPaneTo(pane.scrollTop);
    expect(button()).toBeNull();
  });

  it("opens a new page at its top, but leaves a link to a section to the page", () => {
    scrollPaneTo(1200);
    act(() => go("/settings"));
    expect(pane.scrollTop).toBe(0);

    scrollPaneTo(900);
    act(() => go("/settings?tab=backups#schedule"));
    expect(pane.scrollTop).toBe(900);
  });
});
