import type { Backup } from "../api";

/** One copy of a backup, as the list rows and the timeline read it. */
export interface BackupLocation {
  kind: string;
  name: string;
  type: string;
  dest_id?: string;
  status?: string;
  detail?: string;
  immutable?: boolean;
  lock_until?: number;
  verified_at?: number;
  verify_ok?: boolean;
}

/** The per-row flags the list chips are drawn from. */
export interface BackupRowSummary {
  partial: boolean;
  incremental: boolean;
  chain_depth: number;
  write_only: boolean;
  db_fallback: boolean;
}

/**
 * parseBlob reads one of a row's JSON blobs.
 *
 * These arrive from the server as opaque strings and an older row may hold a
 * shape this build no longer understands. A row that fails to parse must render
 * as "nothing known", never take the page down. The return is deliberately
 * untyped: the blobs are versioned server-side and each caller reads the few
 * fields it knows about.
 */
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export function parseBlob(raw: string | undefined): any {
  if (!raw) return null;
  try {
    return JSON.parse(raw);
  } catch {
    return null;
  }
}

const LOCAL_ONLY: BackupLocation[] = [{ kind: "local", name: "local", type: "local" }];

/**
 * locations lists where a backup actually lives.
 *
 * Slim list rows carry pre-derived copies instead of the full blob, so both
 * shapes are normalised to the same records here. A row with no recorded copies
 * is a local-only backup, not a backup with nowhere to be — every archive is
 * written locally first.
 */
export function locations(b: Backup): BackupLocation[] {
  if (!b.locations && b.summary) {
    const copies = (b.summary.copies || []).map((copy) => ({
      kind: copy.type === "local" ? "local" : "dest",
      name: copy.name,
      type: copy.type || "",
      status: copy.status,
      detail: copy.detail,
      immutable: copy.immutable,
    }));
    return copies.length ? copies : LOCAL_ONLY;
  }
  const parsed = parseBlob(b.locations);
  const list = Array.isArray(parsed) ? (parsed as BackupLocation[]) : [];
  return list.length ? list : LOCAL_ONLY;
}

/** A backup is degraded when an intended offsite copy did not upload. */
export function failedCopies(b: Backup): BackupLocation[] {
  return locations(b).filter((l) => l.status === "failed");
}

/** Copies that hold restorable data right now. */
export function goodCopies(b: Backup): BackupLocation[] {
  return locations(b).filter((l) => l.status !== "failed");
}

/**
 * rowSummary reads the list-row digest, deriving it from the blobs for fat rows
 * (an older backend, or the full-row drawer fetch) so both shapes render
 * identically.
 */
export function rowSummary(b: Backup): BackupRowSummary {
  if (b.summary) {
    return {
      partial: b.summary.partial,
      incremental: !!b.summary.incremental,
      chain_depth: b.summary.chain_depth || 0,
      write_only: !!b.summary.write_only,
      db_fallback: !!b.summary.db_fallback,
    };
  }
  const manifest = parseBlob(b.manifest_json);
  const skipped = (manifest?.skipped_mounts || []) as { covered_by?: string }[];
  return {
    partial: skipped.some((mount) => !mount.covered_by),
    incremental: !!manifest?.incremental,
    chain_depth: manifest?.chain_depth || 0,
    write_only: !!manifest?.wrapped_key_pub,
    // A database container whose dump tools were missing, and an embedded
    // database no consistent snapshot could be taken of, share this flag: from
    // the operator's side both mean "a database in here was copied as a live
    // file", which is the fact worth seeing in a list.
    db_fallback: !!manifest?.db_fallback || !!manifest?.sqlite_fallback,
  };
}
