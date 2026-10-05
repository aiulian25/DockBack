import { describe, it, expect } from "vitest";
import { fmtDuration, scheduleWhen } from "./format";

// Three pages had their own copy of this and had already diverged: the compact
// one rounded hours to a decimal, the others to whole hours. Both spellings are
// kept deliberately; the point of the test is that each stays put.
describe("fmtDuration", () => {
  it("says never for a negative count", () => {
    expect(fmtDuration(-1)).toBe("never");
    expect(fmtDuration(-1, "compact")).toBe("never");
  });

  it("renders the long form used beside prose", () => {
    expect(fmtDuration(30)).toBe("30 sec");
    expect(fmtDuration(89)).toBe("89 sec");
    expect(fmtDuration(90)).toBe("2 min");
    expect(fmtDuration(900)).toBe("15 min");
    expect(fmtDuration(5400)).toBe("2 h");
    expect(fmtDuration(21600)).toBe("6 h");
    expect(fmtDuration(172800)).toBe("2 d");
  });

  it("renders the compact form used in metric tables", () => {
    expect(fmtDuration(30, "compact")).toBe("30s");
    expect(fmtDuration(900, "compact")).toBe("15m");
    expect(fmtDuration(172800, "compact")).toBe("2d");
  });

  // The one real behavioural difference between the old copies.
  it("keeps a decimal hour in the compact form, and only below ten hours", () => {
    expect(fmtDuration(12600, "compact")).toBe("3.5h");
    expect(fmtDuration(21600, "compact")).toBe("6.0h");
    expect(fmtDuration(43200, "compact")).toBe("12h");
    expect(fmtDuration(12600)).toBe("4 h");
  });

  it("switches units at the same boundaries in both forms", () => {
    for (const [seconds, longUnit, compactUnit] of [
      [89, "sec", "s"], [90, "min", "m"], [5399, "min", "m"],
      [5400, "h", "h"], [172799, "h", "h"], [172800, "d", "d"],
    ] as [number, string, string][]) {
      expect(fmtDuration(seconds).endsWith(longUnit)).toBe(true);
      expect(fmtDuration(seconds, "compact").endsWith(compactUnit)).toBe(true);
    }
  });

  it("handles zero", () => {
    expect(fmtDuration(0)).toBe("0 sec");
  });
});

describe("scheduleWhen", () => {
  const base = { time: "03:00", weekday: 0, monthday: 1, cron: "0 3 * * 0" };
  it("says when each kind of schedule fires", () => {
    expect(scheduleWhen({ ...base, kind: "daily" })).toBe("Daily at 03:00");
    expect(scheduleWhen({ ...base, kind: "weekly", weekday: 3 })).toBe("Weekly on Wednesday at 03:00");
    expect(scheduleWhen({ ...base, kind: "monthly", monthday: 15 })).toBe("Monthly on day 15 at 03:00");
    expect(scheduleWhen({ ...base, kind: "custom" })).toBe("Custom (cron 0 3 * * 0)");
  });
});
