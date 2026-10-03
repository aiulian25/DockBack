# Audiobookshelf & apps with an embedded database

**Audiobookshelf** is the clearest example of a large family of self-hosted apps: a small, entirely local configuration database next to an enormous media library. Getting the backup right means treating those two things completely differently — and the same reasoning applies to Jellyfin, Plex, Calibre and anything else shaped this way.

## What actually needs backing up

| What | Where | In the backup? |
|---|---|---|
| The database (`absdatabase.sqlite`) — users, libraries, every item, listening progress, collections, **and the token secret** | `/config` | **Yes** — as a consistent snapshot |
| Covers and author images | `/metadata/items`, `/metadata/authors` | **Yes** |
| Cache, streams, temp files, logs | `/metadata/cache`, `/metadata/tmp`, `/metadata/streams`, `/metadata/logs` | No — regenerated on demand |
| The audiobooks and e-books themselves | your media mounts | **No, by design** — see below |

The configuration side is typically a few hundred megabytes. The media is often measured in terabytes.

## Why the media is deliberately left out

DockBack **skips large bind mounts by default** — anything over the size threshold on the container's page. For a media library that is the right call: your NAS is the media's system of record, it is already covered by your general file-backup strategy, and copying tens of terabytes into an encrypted archive every night protects nothing that isn't already protected.

The skipped mounts are still **recorded in the manifest**, so the backup is honestly reported as partial and you can see exactly what was left out. If you genuinely want the media inside the archive, tick those mounts on the container page.

> **Check this once:** confirm your media libraries really are covered by your NAS or general backup. The one thing worse than not backing something up is believing you did.

## Your logins survive a restore

In Audiobookshelf **2.26 and later**, the secret that signs authentication tokens lives **inside the database**, not in a `JWT_SECRET_KEY` environment variable. That has a pleasant consequence: because DockBack restores the database byte-for-byte and **never edits an application's data**, the secret comes back unchanged and **existing phone and browser sessions keep working**. Nobody has to log in again.

On much older versions the secret was an environment variable instead — that is captured too, as part of the container's configuration, so either way it is preserved.

## The consistent snapshot

The database is written continuously — progress updates, library scans — so copying the file while the app runs risks catching it mid-write. DockBack finds SQLite databases inside the captured mounts **by their file header, not their name**, and takes a **transactionally consistent snapshot** of each one with no downtime. The snapshot is validated before it is accepted; if it can't be taken cleanly, the raw file and its write-ahead log are captured instead and the log says so.

Each snapshot is recorded with its **checksum and content counts** (tables and rows). That contract is then checked twice:

- **Every verification and scrub** re-hashes the stored snapshot. If it no longer matches what was recorded at capture, the backup fails verification loudly — corruption is found during a routine check, not on the day you need it.
- **Every restore** re-reads the database after writing it back, runs an integrity check and re-counts it. **A database that comes back corrupt or short fails the restore**, rather than starting a container on data that quietly isn't what you backed up.

## Restoring onto a different server

Audiobookshelf stores each library item's path **as it appears inside the container** — `/audiobooks/…`, not the host path. This is the single thing that decides whether a move is effortless or painful.

- **Mount the media at the same container paths on the new machine** and everything just works. The host path can be completely different — a different NAS share, a different disk, a local folder. Nothing needs rescanning, and every item stays linked to its file.
- **If the container paths must change**, the items will show as missing until you re-point each library's folder in *Settings → Libraries* on the restored instance.

The restore panel lists the **recorded container paths** for exactly this reason, so you can confirm the target provides them before you commit.

### `/config` must be on local disk

Never place `/config` on a network share (CIFS/NFS). An embedded database on a network filesystem loses the file locking it depends on and **corrupts silently, days later** — long after the restore looked successful. DockBack refuses a restore when it can measure that the target's `/config` is on a network filesystem, and reminds you of the requirement when it can't.

The same applies to the staging DockBack uses while restoring: keep the work directory on local disk.

### Version: newer is fine, older is not

Audiobookshelf **migrates its database forward on start**, and ships no way back down. So:

- Restoring into the **same or a newer** version is safe. The migration runs automatically — but it is **one-way**, so afterwards you can't return to the older version without restoring again.
- Restoring into an **older** version is **blocked**. It would fail at start, after the data had already been written.

Pin the image to a specific tag rather than `latest` if you want to control exactly when that one-way step happens.

## Recommended setup

1. On the container's page, confirm the mount selection: `/config` and `/metadata` ticked, the media mounts left out, and the cache/temp/log paths excluded.
2. Check that the run captured a **consistent snapshot** of `absdatabase.sqlite` rather than a raw copy — the log names each database it snapshotted, with its table and row counts.
3. Schedule it nightly. It is a small backup; there is no reason to run it rarely.
4. Send a copy to a destination with **retention lock** enabled. The archive holds user accounts and personal listening history.
5. Rehearse the restore as an **isolated copy** on another node, with media mounted at the same container paths, and confirm you can log in without re-authenticating, that progress is exactly where you left it, and that cover art renders.

## The general rule

For any app shaped like this — small embedded database, huge media library — back up **the database consistently and the media separately**, keep the **container mount paths identical across your servers**, and keep the database **off network storage**. Those three habits make a move between machines a non-event.
