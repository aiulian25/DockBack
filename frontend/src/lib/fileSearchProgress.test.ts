import { describe, expect, it } from "vitest";
import { mergeFileSearch } from "./fileSearchProgress";
import type { FileSearchHit } from "../api";

const hit = (backup_id: string): FileSearchHit => ({ backup_id, created_at: 1, target: "app", path: "config.xml", size: 1, mtime: 1 });

describe("mergeFileSearch", () => {
  it("says older history is unread while a cursor remains, and adds pages up", () => {
    const first = mergeFileSearch(null, { results: [], searched: 900, skipped: 100, scanned: 1000, next_cursor: "5.b1" });
    expect(first.moreHistory).toBe(true);
    expect(first.nextCursor).toBe("5.b1");

    const second = mergeFileSearch(first, { results: [hit("old")], searched: 4, skipped: 1, scanned: 5 });
    expect(second.moreHistory).toBe(false);
    expect(second.results.map((h) => h.backup_id)).toEqual(["old"]);
    expect([second.searched, second.skipped, second.scanned]).toEqual([904, 101, 1005]);
  });

  it("keeps a capped page apart from a complete one", () => {
    const capped = mergeFileSearch(null, { results: [hit("a")], searched: 1, skipped: 0, scanned: 1, truncated: true });
    expect(capped.truncated).toBe(true);
    expect(capped.moreHistory).toBe(false);
  });

  it("reads a response from before paging as one page", () => {
    const legacy = mergeFileSearch(null, { results: [hit("a")], searched: 3, skipped: 0 });
    expect([legacy.scanned, legacy.moreHistory]).toEqual([0, false]);
  });
});
