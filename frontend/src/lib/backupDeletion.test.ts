import { describe, expect, it } from "vitest";
import { isDeletionIncomplete, keptCopiesNotice, keptCopyReason } from "./backupDeletion";

describe("keptCopyReason", () => {
  it("says when a locked copy will go", () => {
    const lockEnds = Date.UTC(2026, 11, 24, 12) / 1000;
    const reason = keptCopyReason({ delete_outcome: "locked", lock_until: lockEnds });
    expect(reason).toContain("Locked until");
    expect(reason).toContain("2026");
    expect(reason).toContain("deletes it when the lock ends");
  });

  it("gives the destination's own reason for a failed delete", () => {
    expect(keptCopyReason({ delete_outcome: "failed", delete_detail: "connection refused" }))
      .toBe("Could not be deleted (connection refused). DockBack keeps trying.");
  });

  it("still reads as a sentence with nothing recorded", () => {
    expect(keptCopyReason({})).toBe("Could not be deleted. DockBack keeps trying.");
    expect(keptCopyReason({ delete_outcome: "locked" })).toBe("Locked. DockBack deletes it when the lock ends.");
  });
});

describe("keptCopiesNotice", () => {
  it("names the copy that is left", () => {
    const notice = keptCopiesNotice([{ location: "offsite-nas", outcome: "failed" }]);
    expect(notice).toContain("1 copy is still stored (offsite-nas)");
    expect(notice).toContain("remove it");
  });

  it("counts several", () => {
    const notice = keptCopiesNotice([{ location: "nas", outcome: "failed" }, { location: "bucket", outcome: "locked" }]);
    expect(notice).toContain("2 copies are still stored (nas, bucket)");
    expect(notice).toContain("remove them");
  });
});

describe("isDeletionIncomplete", () => {
  it("is true only for a backup being deleted", () => {
    expect(isDeletionIncomplete({ status: "deleting" })).toBe(true);
    expect(isDeletionIncomplete({ status: "success" })).toBe(false);
  });
});
