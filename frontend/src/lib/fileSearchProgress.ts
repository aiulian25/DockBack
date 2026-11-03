import type { FileSearchResp } from "../api";

/**
 * A file search across a node's history arrives a page at a time, newest first. This
 * is the running total of the pages read so far. `moreHistory` is what keeps a
 * partial search from passing for a complete one: while it is set, older backups
 * have not been searched.
 */
export interface FileSearchProgress {
  results: FileSearchResp["results"];
  searched: number;
  skipped: number;
  scanned: number;
  truncated: boolean;
  nextCursor?: string;
  moreHistory: boolean;
}

/** Adds one page to what has been read so far, or starts afresh from null. */
export function mergeFileSearch(sofar: FileSearchProgress | null, page: FileSearchResp): FileSearchProgress {
  return {
    results: [...(sofar?.results ?? []), ...page.results],
    searched: (sofar?.searched ?? 0) + page.searched,
    skipped: (sofar?.skipped ?? 0) + page.skipped,
    scanned: (sofar?.scanned ?? 0) + (page.scanned ?? 0),
    truncated: page.truncated === true,
    nextCursor: page.next_cursor,
    moreHistory: Boolean(page.next_cursor),
  };
}
