# Incremental volume backups (changed files only)

Most of a large app's data doesn't change between backups. A 30 GB media-server config that gains a few megabytes a night still re-copies and re-uploads all 30 GB every run. **Incremental volume capture** fixes that: after one full baseline, each backup stores only the files that **changed** since the previous one, plus a list of files that were **deleted** — a fraction of the size and time.

It's **opt-in per container** and it never changes how databases are handled: **database dumps and app-native exports are always captured in full**, because a logical dump is already the small, self-consistent representation of that data.

## Turning it on

On a container's page, under **Backup options**, tick **Incremental volume capture**. Set **Full backup every N** to control how often a fresh full baseline is taken (default **7**, allowed **2–30**). The choice is remembered, so scheduled and whole-node runs use it too.

- The **first** backup after you enable it is a **full baseline**.
- The next runs are **deltas** — only changed files — until the counter reaches **N**, when a fresh full is taken and the chain restarts.
- A shorter **N** means shorter chains (more independent restores); a longer **N** means longer chains between fulls.

**The periodic full doesn't re-read your data.** At the "full every N" boundary, DockBack captures only the **changed files** from the container — the same small read as any delta — and then **builds the new full locally**, by merging the stored chain (baseline + deltas + the fresh changes) into a brand-new, self-contained archive. Your 60 GB photo library is re-read from the source only when a backup actually changed it. The result is an ordinary full backup: restore, verification, drills, retention, and the standalone recovery tool treat it exactly like any other. If the local merge can't complete for any reason (say, a chain copy is unreadable), the run automatically falls back to a normal full capture from the source — the backup still succeeds, just the slower way. Opt out with **Settings → Advanced → Performance & tuning → Build periodic full backups locally**.

## How a chain is stored

Each incremental backup is a normal encrypted `.dback` archive, so nothing about security changes. Inside it carries:

- **`volumes-delta.tar`** — only the changed files (instead of the full `volumes.tar`).
- **`volumes-index.json.zst`** — the *complete* file list (path, size, modified-time) as of this backup, which the next delta diffs against.
- Manifest fields recording the **parent** backup, the **chain depth**, the **deleted** paths, and a cryptographic **pin** of the parent (so a substituted or corrupt parent is detected, not blindly trusted).

A file counts as *changed* when it's new or its **size or modified-time** differs from the parent. Changing the mount selection, or an unreadable index, safely forces a fresh full baseline rather than risk a wrong delta.

> **The index is now universal.** Ordinary (non-incremental) full backups store the same `volumes-index.json.zst` too (on by default; *Settings → Performance & tuning → "Store a file index in every backup"*). It's what powers **Find a file** across backup generations, **Compare with previous** in a backup's drawer, and file browsing that never truncates — all from one small index read, without decrypting the whole archive. Backups made before indexing existed still browse via the slower streaming path and are simply skipped by search/diff.

## Restoring is automatic

You restore an incremental backup exactly like any other — pick a version and click **Restore**. DockBack resolves the chain and, behind the scenes, applies the **full baseline first, then each delta in order**, removing files that were deleted along the way, so the container ends at the exact state of the version you chose. The restore drawer shows a short note; if a chain link is missing or fails verification, it warns you and you can pick the newest full baseline or a later generation instead.

Restore drills and verification are chain-aware too: verification checks that a delta's parent still exists and matches its pin, so a broken chain is caught **before** you rely on it, not during a restore.

## Retention keeps chains whole

Retention (GFS / generations) prunes **whole chains, never a link inside one**. A full baseline or delta that a kept backup still depends on is retained even if the age-based policy would otherwise remove it — so a delta is never orphaned. Once every backup that descends from a full has aged out, the whole chain is pruned together.

### Deleting by hand keeps chains whole too

Manually deleting a backup that newer incremental backups depend on is **refused** — deleting a baseline would make every delta after it unrestorable. The Backups drawer shows a **baseline of N deltas** chip on such backups, and the delete prompt offers **Delete the whole chain** instead: one audited operation that removes the backup and every descendant, newest-first, so no half-broken chain can ever exist. Bulk deletes are chain-aware the same way — select any mix of deltas and baselines and they delete in a safe order; a baseline whose deltas were *not* selected is refused rather than orphaning them.

## Recovering a chain without DockBack

The standalone recovery tool understands chains. Point it at the folder holding the chain's `.dback` files:

```
dockback-recover.py --key @key.txt --in <newest-delta>.dback --apply-chain <folder> --out ./restored
```

It decrypts each generation in order, writes numbered tars, and an **`APPLY_ORDER.txt`** telling you exactly how to extract the full baseline and apply each delta over it (including which paths to remove). Running the tool on a single delta without `--apply-chain` prints a clear note that the archive alone is not a complete restore.

## When to use it

Incremental capture shines for **large, slowly-changing volumes** — media-server configs, document archives, app data directories. For small volumes the savings are negligible and a plain full backup keeps every backup independently restorable. Databases don't benefit (they're always dumped in full), so a pure database container gains nothing from the toggle.
