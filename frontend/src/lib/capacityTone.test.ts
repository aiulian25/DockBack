import { describe, expect, it } from "vitest";
import { capacityTone } from "./capacityTone";

const GB = 1024 ** 3;

describe("capacityTone", () => {
  it("is an error when the restore is refused", () => {
    expect(capacityTone({ refuse: true, after_bytes: 4 * GB, margin_bytes: 9.8 * GB })).toBe("error");
  });

  it("warns on R5's own placement, which clears the margin and is still the wrong disk", () => {
    // 58 GB into 74 GB free on a 98 GB root: 16 GB left, margin 9.8 GB. Allowed,
    // but under twice the margin — the case the panel exists to make visible.
    expect(capacityTone({ refuse: false, after_bytes: 16 * GB, margin_bytes: 9.8 * GB })).toBe("warning");
  });

  it("is calm when the disk has real room left", () => {
    // The SSD the same stack was moved to.
    expect(capacityTone({ refuse: false, after_bytes: 2142 * GB, margin_bytes: 400 * GB })).toBe("primary");
  });

  it("treats exactly twice the margin as comfortable", () => {
    expect(capacityTone({ refuse: false, after_bytes: 20 * GB, margin_bytes: 10 * GB })).toBe("primary");
    expect(capacityTone({ refuse: false, after_bytes: 20 * GB - 1, margin_bytes: 10 * GB })).toBe("warning");
  });
});
