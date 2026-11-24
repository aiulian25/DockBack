// What a delete leaves behind. A backup whose deletion could not remove every copy
// stays listed until the last one is gone; these helpers say so in plain words.
import type { Backup } from "../api";
import type { BackupLocation } from "./backupRows";

/** The status of a backup that was deleted while a copy could not be removed. */
export const BACKUP_STATUS_DELETING = "deleting";

/** A copy kept because its retention lock still runs. Every other kept copy failed to delete. */
const COPY_LOCKED = "locked";

const MS_PER_SECOND = 1000;

/** One copy a delete had to leave, as the delete endpoints report it. */
export interface KeptCopy {
  location: string;
  outcome: string;
  detail?: string;
}

export function isDeletionIncomplete(b: Pick<Backup, "status">): boolean {
  return b.status === BACKUP_STATUS_DELETING;
}

/** Why one copy outlived its backup's deletion, and what happens to it next. */
export function keptCopyReason(copy: Pick<BackupLocation, "delete_outcome" | "delete_detail" | "lock_until">): string {
  const isLocked = copy.delete_outcome === COPY_LOCKED;
  if (isLocked && copy.lock_until) {
    const lockEnds = new Date(copy.lock_until * MS_PER_SECOND).toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
    return `Locked until ${lockEnds}. DockBack deletes it when the lock ends.`;
  }
  if (isLocked) return "Locked. DockBack deletes it when the lock ends.";
  if (copy.delete_detail) return `Could not be deleted (${copy.delete_detail}). DockBack keeps trying.`;
  return "Could not be deleted. DockBack keeps trying.";
}

/** One sentence for the notice shown right after a delete that left copies. */
export function keptCopiesNotice(kept: KeptCopy[]): string {
  const names = kept.map((copy) => copy.location).join(", ");
  const copies = kept.length === 1 ? "1 copy is" : `${kept.length} copies are`;
  return `Deleted what could be deleted. ${copies} still stored (${names}), so the backup stays listed as "Deletion incomplete" until DockBack can remove ${kept.length === 1 ? "it" : "them"}.`;
}
