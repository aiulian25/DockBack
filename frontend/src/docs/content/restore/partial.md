# Volumes-only or database-only

A backup can hold both volume/bind data and a database dump. Depending on what went wrong, you may want to restore only one of them.

## What each option does

- **Volumes** — restores the container's volume/bind **files** from `volumes.tar`. The container is stopped for a consistent write, the data is unpacked via a sidecar, then it's started again.
- **Database** — re-imports the **consistent dump**. The engine is stopped, its data directory is wiped so it re-initializes fresh, then the dump is imported over a local connection. This guarantees healthy, consistent data rather than copied raw files.

## Choosing

In the restore drawer, restore proceeds according to the backup's contents:

- A **database** container is restored from its dump (never from raw files).
- An **app** container has its **files/volumes** restored.
- App-native backups are restored via the application's own **importer** (see *App-native exports*).

If a backup contains both, you can restore the database and the files together, or target just what you need.

## When to use partial restore

- A bad data migration corrupted the database, but the uploaded files are fine → restore **database** only.
- Files were deleted or corrupted, but the database is healthy → restore **volumes** only.

For a container that no longer exists at all, DockBack recreates it first — see *Disaster recovery*.

## Partial backups are labelled honestly

Some data is deliberately left out of a backup — most often a **large bind mount** (e.g. a media library) that's excluded by default because it's better handled separately. When that happens, the backup carries a **Partial** badge on the Backups list and in its details, with a *"Not captured"* list naming each excluded mount and why. This means a near-empty backup can never quietly look like a complete one: you always know exactly what a restore will and won't bring back. To start capturing an excluded mount, tick it in the container's backup options and run a new backup.

A backup is **also** marked Partial when a selected bind mount contained **files the backup reader couldn't access** — a permission/ownership mismatch, typically a UID/GID difference or an NFS share exported with `root_squash`. Those unreadable files are silently missing from the copy, so DockBack records the mount under *"Not captured"* with a *"permission denied"* reason rather than letting the backup look complete. To fix it, align the host directory's ownership with the user the container runs as (or the export's squash setting) and run a new backup; once the reader can read every file, the mount is captured in full and the Partial badge clears.
