import { describe, it, expect } from "vitest";
import type { Backup } from "../api";
import { locations, failedCopies, goodCopies, rowSummary, parseBlob } from "./backupRows";

// The list rows and the restore timeline each had their own copy of these and
// had diverged: the timeline's returned copies with no name or type, so a row
// rendered from it showed a blank label where the list showed "local".
function backup(fields: Partial<Backup>): Backup {
  return fields as Backup;
}

describe("parseBlob", () => {
  it("reads an object", () => {
    expect(parseBlob('{"a":1}')).toEqual({ a: 1 });
  });

  // A row this build cannot parse must render as "nothing known" rather than
  // taking the page down.
  it("returns null for missing or unparseable input", () => {
    expect(parseBlob(undefined)).toBeNull();
    expect(parseBlob("")).toBeNull();
    expect(parseBlob("not json")).toBeNull();
  });
});

describe("locations", () => {
  it("reads the blob when the row carries one", () => {
    const got = locations(backup({ locations: '[{"kind":"dest","name":"offsite","type":"s3","status":"ok"}]' }));
    expect(got).toHaveLength(1);
    expect(got[0].name).toBe("offsite");
  });

  // Slim rows carry pre-derived copies instead of the blob. Both shapes must
  // produce the same records or the two views disagree about the same backup.
  it("normalises the slim row's copies to the same shape", () => {
    const got = locations(backup({
      summary: { copies: [{ name: "offsite", type: "s3", status: "failed" }] } as Backup["summary"],
    }));
    expect(got).toEqual([{ kind: "dest", name: "offsite", type: "s3", status: "failed", detail: undefined, immutable: undefined }]);
  });

  it("labels a local copy as local in the slim shape", () => {
    const got = locations(backup({ summary: { copies: [{ name: "local", type: "local" }] } as Backup["summary"] }));
    expect(got[0].kind).toBe("local");
  });

  // Every archive is written locally first, so "no recorded copies" means
  // local-only, not "nowhere". The name and type must be filled in — this is
  // exactly what the timeline's copy left blank.
  it("falls back to a fully-named local copy", () => {
    for (const b of [
      backup({}),
      backup({ locations: "[]" }),
      backup({ locations: "not json" }),
      backup({ summary: { copies: [] } as unknown as Backup["summary"] }),
    ]) {
      expect(locations(b)).toEqual([{ kind: "local", name: "local", type: "local" }]);
    }
  });
});

describe("failedCopies / goodCopies", () => {
  const b = backup({
    locations: '[{"kind":"dest","name":"a","type":"s3","status":"failed"},{"kind":"dest","name":"b","type":"s3","status":"ok"},{"kind":"local","name":"local","type":"local"}]',
  });

  it("splits on the failed status", () => {
    expect(failedCopies(b).map((l) => l.name)).toEqual(["a"]);
    expect(goodCopies(b).map((l) => l.name)).toEqual(["b", "local"]);
  });

  it("counts a local-only backup as one good copy", () => {
    expect(failedCopies(backup({}))).toHaveLength(0);
    expect(goodCopies(backup({}))).toHaveLength(1);
  });
});

describe("rowSummary", () => {
  it("prefers the slim row's own digest", () => {
    const got = rowSummary(backup({
      summary: { partial: true, incremental: true, chain_depth: 3, write_only: true, db_fallback: true } as Backup["summary"],
    }));
    expect(got).toEqual({ partial: true, incremental: true, chain_depth: 3, write_only: true, db_fallback: true });
  });

  it("derives the same flags from a fat row's manifest", () => {
    const got = rowSummary(backup({
      manifest_json: JSON.stringify({
        skipped_mounts: [{ covered_by: "" }], incremental: true, chain_depth: 2, wrapped_key_pub: "k", db_fallback: "yes",
      }),
    }));
    expect(got).toEqual({ partial: true, incremental: true, chain_depth: 2, write_only: true, db_fallback: true });
  });

  // A skipped mount that another target already covers is not a partial backup.
  it("does not call a covered mount partial", () => {
    const got = rowSummary(backup({ manifest_json: JSON.stringify({ skipped_mounts: [{ covered_by: "other" }] }) }));
    expect(got.partial).toBe(false);
  });

  // Two different causes, one operator-facing fact: a database was copied live.
  it("treats the embedded-database fallback the same as the dump fallback", () => {
    expect(rowSummary(backup({ manifest_json: JSON.stringify({ sqlite_fallback: "yes" }) })).db_fallback).toBe(true);
  });

  it("reports nothing for a row with neither shape", () => {
    expect(rowSummary(backup({}))).toEqual({
      partial: false, incremental: false, chain_depth: 0, write_only: false, db_fallback: false,
    });
  });
});
