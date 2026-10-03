import { describe, expect, it } from "vitest";
import { isRestoreProgressLine } from "./restoreLogFilter";

describe("isRestoreProgressLine", () => {
  it("shows #17's neutralisation block", () => {
    // Verbatim from applyCloneNeutralizations. If a wording change drops these,
    // the operator is told nothing about what their clone can still do.
    expect(isRestoreProgressLine(
      "Neutralised for this clone: MAIL_DRIVER (log), MAIL_PASSWORD (cleared). This copy starts unable to act on the real world through them — re-arm by restoring in place, which replays the recorded values."
    )).toBe(true);
    expect(isRestoreProgressLine(
      "Left alone because the image sets them itself: MAIL_DRIVER. Overriding an image's own setting is how a restore breaks the application it is copying — but it means this copy may still reach out through them."
    )).toBe(true);
  });

  it("shows the lines the panel was already built around", () => {
    for (const msg of [
      "Restoring volume/bind data via sidecar",
      "Starting container",
      "Restore completed",
      "Safety snapshot taken",
      "Re-import of the database finished",
      "Integrity check passed",
    ]) {
      expect(isRestoreProgressLine(msg)).toBe(true);
    }
  });

  it("stays quiet for unrelated fleet chatter", () => {
    for (const msg of ["Schedule tick", "Pruned 3 old archives", "Uploaded to destination"]) {
      expect(isRestoreProgressLine(msg)).toBe(false);
    }
  });
});
