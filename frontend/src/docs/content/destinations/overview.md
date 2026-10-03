# Overview, 3-2-1 & least privilege

External destinations are offsite/network places your backups are mirrored to, in addition to the local copy. Manage them in **Settings → External Backup Destinations → Add Storage Provider**.

## The 3-2-1 strategy

Keep **3** copies of your data, on **2** different media, with **1** offsite. DockBack makes this easy: every backup keeps a **Local** copy and is mirrored to **every checked destination**. Add a NAS and a cloud destination, check both in your policy, and each backup is automatically in three places. If one location is lost or corrupt, restore falls back to another (see *Restoring → How restore works*).

## Complete an offsite copy on demand ("Send offsite now")

If a destination is unreachable when a backup runs, the backup is still saved and **verified locally** but flagged **Degraded** (not fully 3-2-1). You don't have to re-run the whole backup to fix that. Open the backup (**Backups → a node → a backup**); in the **Restore from** section, **Send offsite now** reuses the already-stored, encrypted archive and mirrors it — no re-capture, re-compress, re-encrypt or re-verify, so it doesn't waste resources. Progress streams in place, just like a restore.

Every pending destination is listed with a **checkbox** (all ticked by default) so you can send to one, several, or all of them in a single run — and destinations that already hold a good copy are omitted, so nothing is uploaded twice. This also works for a **local-only** backup (e.g. one taken before you added a destination) and for **older backups that failed to mirror** in the past — as long as the local copy still exists.

## Backfilling an existing history

"Send offsite now" is per-backup. When you **add a new destination** (or recover one with *Scan & adopt*), you usually want your **whole existing history** on it — clicking through every backup would be tedious. The destination card has a **Backfill** button that does it in one action.

Backfill finds every successful backup that has **no healthy copy** on this destination and mirrors them **oldest-first**, **one upload at a time**, reusing the same stored-archive mirror path (no re-capture). It respects each node's policy — a node configured to **exclude** this destination is skipped — and it honors the destination's **upload window and bandwidth cap** (see *Per-destination bandwidth & upload window*), so a backfill may **pause until the window opens**. The card shows live progress (**Backfill · 34/120**), and you get a summary alert if any item fails.

Notes:

- A backup that already has a good copy here is skipped; one whose earlier copy **failed** is re-attempted.
- On a **WORM / immutable (object-lock)** destination, each backfilled copy is written as a fresh **locked** object, just like a normal mirror.
- Only one backfill runs per destination at a time; starting a second while one is in progress is refused until it finishes.

## Scan & adopt — recover backups after a DB loss

Your backups live on the destination as self-describing, encrypted archives — but DockBack finds them through its own catalog (the SQLite database). If that database is lost (a bad restore of an old app-backup, a wiped volume) while the archives on your S3 bucket / Synology / Nextcloud are perfectly intact, those newer archives become **orphaned** — present on disk but invisible in the app.

**Scan & adopt** fixes that. On **Settings → each destination card**, click **Scan & adopt**. DockBack lists the destination's objects, and for every `.dback` archive whose manifest sidecar **verifies under your current encryption key** but has **no catalog row**, it re-creates the entry — reading the node, target, size and timestamp straight from the manifest. The backup then appears on the **Backups** page and can be restored normally.

- It **never overwrites** an existing backup, and re-running it is safe — already-catalogued archives are simply counted as *skipped*.
- An archive whose manifest was sealed/signed with a **different master key** (e.g. from another deployment) is **skipped**, never adopted — so you never get a row you couldn't actually decrypt.
- Adopted backups are marked **unverified** (they were re-registered from the manifest, not test-restored); run a **scrub** or restore-drill to confirm them end-to-end.

This turns "my DockBack DB is gone but my bucket is intact" from unrecoverable-through-the-app into one click.

### What it skipped, and why

Two numbers — *"42 adopted, 7 skipped"* — don't tell you whether those 7 are harmless duplicates or backups you can no longer read. After the scan, the destination card lists **Skipped archives**: the object key, the reason, and, where it could be read, the **master key fingerprint** the archive belongs to.

| Reason shown | What it means |
|---|---|
| *encrypted with a different key* | A genuine DockBack backup, sealed or signed under a **different master key** — from before a key rotation, or from another deployment writing to the same destination. |
| *already in the catalog* | The ordinary case on a re-run. Nothing to do. |
| *manifest couldn't be read* | The sidecar is missing, corrupt, unsigned, or is a sealed manifest belonging to another key (sealed manifests are opaque, so nothing can be read from them — including a fingerprint). |
| *archive file missing* | The manifest is valid but its `.dback` is gone from the destination. |

Foreign-key skips are listed first and called out, because they are the ones that usually need a decision: **re-import that key** (*Settings → Encryption*) and scan again to adopt them, or find out which other DockBack is writing to this destination.

> **Only the key fingerprint is read from an archive that isn't yours.** A foreign manifest may belong to someone else's instance, so its container names, stacks and volume paths are never displayed — just enough to identify the key, which is what makes the skip actionable. Long scans report at most the first 200 skipped objects.

## Organizing older backups (per-stack folders)

Newer backups are stored as `<node>/<stack>/<container>/<timestamp>_<id>.dback`; backups made before that layout existed sit at older flat paths, splitting a destination's folder into two shapes. **Settings → Backups → Organize existing backups** converges them: one background pass copies each old archive (and its manifest sidecar) to the canonical path on **every** location, verifies the copy, repoints the catalog, and only then deletes the old objects — so an interrupted run can at worst leave a harmless duplicate, never a missing backup. It's safe to re-run any time; already-organized backups are untouched. **Immutable (WORM) copies can't be moved** and are skipped with a note — they age out naturally under retention.

## Per-destination bandwidth & upload window

Each destination has optional **advanced** settings (in the Add/Edit dialog) to keep a metered or slow offsite target from saturating your uplink, while a fast LAN NAS stays unlimited:

- **Max upload rate (Mbit/s)** — caps the mirror speed to *this* destination only (0 = unlimited). It applies on top of any global `DOCKBACK_MAX_UPLOAD_MBPS` aggregate cap, so the tighter of the two wins.
- **Upload window (from / to)** — only mirror to this destination during a time window (server local time; a window like `22:00`–`06:00` that crosses midnight is fine). Leave both blank for no window.

Outside its window, a backup still completes and is **verified locally**; the offsite copy is marked **Deferred** (not failed) and is sent automatically on the next backup run that falls inside the window. To push it immediately regardless of the window, use **Send offsite now** — a deliberate on-demand push overrides the window.

## Supported providers

| Provider | Protocol | Use |
|----------|----------|-----|
| **Synology NAS** | SMB | Synology shared folders |
| **Samba / SMB share** | SMB | Any SMB/CIFS server |
| **Nextcloud** | WebDAV | Nextcloud (and WebDAV servers) |
| **S3 / Backblaze B2** | S3 API | AWS S3, Backblaze B2, MinIO, Wasabi |
| **SSH (SFTP)** | SFTP over SSH | Any Linux box / Pi / NAS with sshd — pinned host key |

All of these run as userspace clients (no kernel mounts), so they work inside the hardened, read-only container.

## Least-privilege guarantee

> DockBack **only ever reads, writes, and deletes inside the sub-path you configure** on a destination. It never touches any other files on that server. Empty keys and path-traversal (`..`) are rejected at the storage layer, so a bug or bad input can't escape your backup folder.

For defence in depth, also scope the **credential** itself: a dedicated user/share/bucket/app-password that can only see the backup folder. Each provider's guide explains how.

## What a destination operator can see

The **archive** (`.dback`) is always AES-256-GCM encrypted — a storage provider only ever sees opaque bytes, sizes, and object names. But each backup also ships a small **manifest sidecar** describing it (container name, image, stack, volume paths, database engine and extensions). Whether that sidecar is readable is a per-destination choice:

- **Seal manifests on this destination** (checkbox when adding/editing a destination) stores the sidecar as `.manifest.json.enc`, encrypted with your master key — the provider learns nothing about what's inside, not even the container's name beyond what the object path itself reveals. New **S3 and WebDAV** destinations default to sealed (a third party holds the bytes); new **LAN shares** (SMB/Synology) default to readable, and destinations created before this option keep following the global *Encrypt manifests* setting in *Settings → Encryption*.
- A **readable** sidecar (`.manifest.json` + an HMAC `.sig`) is self-describing for hand-recovery — you can open it in any editor mid-disaster — and still tamper-evident.

Either way nothing is lost: **Scan & adopt**, scrubs, key rotation, and the offline recovery script (`dockback-recover.py --manifest <file>.manifest.json.enc --key <hex>`) all handle both forms. Your **local** copies always follow the global setting, never a destination's choice.

## Making copies hard to delete

For S3 there is **Object Lock** — real WORM, enforced by the bucket, which refuses a delete even when asked with your own credentials. Nothing equivalent exists on a NAS share or an SSH box, which is exactly where most homelab copies live.

For **SMB / Synology** and **SSH (SFTP)** destinations you can set a **Retention lock (days)** when adding or editing the destination. After each upload DockBack makes that copy read-only and records when the lock expires. Two things follow:

- **DockBack's own retention will not prune a locked copy.** This is the most common way a copy actually disappears, and it is now closed — the backup is kept whole until the lock expires, then pruned normally.
- **Overwrite-in-place is refused.** That is the shape ransomware takes on a mounted share: open every file, encrypt, write back. A read-only file (or the SMB read-only attribute) turns that into an error.

### Be clear about what this is not

**A filesystem retention lock is a speed bump, not S3 Object Lock.** Object Lock is enforced by the storage server. This is a file permission on the target machine, so:

- **Root on the target can remove it**, always.
- On Linux/Unix, permission to **delete** a file comes from its **folder**, not the file — so anyone with write access to the backup folder can still delete a locked copy, they just have to mean it (`rm -f` rather than `rm`).
- On SMB, a client with write access to the share can clear the read-only attribute.
- For SFTP, DockBack additionally attempts `chattr +i`, a genuine kernel-level immutable flag. It needs privileges the SSH user usually does not have, so it is best-effort — when it fails, the read-only permission still stands and nothing is reported as broken.

If a copy's lock could **not** be applied, DockBack does **not** mark it as locked, and says so in the run log — you will never see an `Immutable` badge on a copy that nothing is protecting.

> **Locked copies occupy disk for the whole period.** They are exempt from pruning, so choose a period you can afford to store. The maximum is 3650 days, and a lock cannot be lifted from the UI once applied — the file can only be freed on the target machine itself.

For genuinely tamper-proof offsite copies, an S3 bucket with Object Lock and an append-only key remains the strongest option; a retention lock on the NAS is a worthwhile second layer, not a replacement.

## Credentials at rest

Destination credentials are encrypted with your master key before they're stored. Test a destination with a real write/read/delete round-trip before saving — see *Testing & capacity*.
