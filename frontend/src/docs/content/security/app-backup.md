# Back up & restore DockBack itself

Container backups protect your *workloads*. This protects **DockBack's own setup** — so if the DockBack host dies, you can stand up a fresh instance and be back exactly where you were, in seconds.

A DockBack application backup captures the app's entire state:

- every **node** and its stored connection credentials,
- the complete **container-backup catalog** (every backup, where its copies live, verification status),
- your **settings**, backup **policy**, schedule, and external **destinations**,
- the **admin account** — including **2FA/TOTP** if you enabled it.

> It does **not** copy the container backup *archives* themselves (those already live in `/app/backups` and your external destinations). It copies the **index and configuration** that ties everything together.

## Automatic backups (on by default)

DockBack backs itself up without being asked:

- **Weekly**, Sunday 04:00 by default, keeping the newest 7. Change it, or switch it off, under **Settings → Advanced → Application Backup & Restore**. A schedule you have saved, including one you switched off, is never changed.
- **Straight away** the first time the schedule is active, so a new install, or one that has never had an app backup, is covered today rather than next week.
- **After a configuration change**: a new node, schedule, destination, policy, account or setting is backed up once the configuration has been unchanged for 10 minutes, so a burst of edits makes one backup.
- **Off the machine**, when an external destination is set (see below). Without one, every copy stays on this host, and the log says so.

You're told when this stops working:

- **appbackup.stale**: the newest app backup is older than 8 days, or there isn't one, once there are container backups to lose.
- **appbackup.failed**: a scheduled app backup failed, or it was made but didn't reach an external destination.

> Keep DockBack's own folder, the one holding its compose file and the `.env` with `DOCKBACK_ENCRYPTION_KEY`, outside any directory a stack manager can delete, and keep the key in your password manager. On the night that prompted these defaults, DockBack's folder was deleted along with every stack. The key survived only because the container was still running.

## Create a backup

**Settings → Application Backup & Restore → Create backup.**

Each backup is stored on the backups volume and appears in the **Available backups** list with its date, a summary (nodes / catalog entries / destinations) and size. From the list you can **Download**, **Restore** or **Delete** any entry.

**Download** gives you a single encrypted file named `dockback-config-<timestamp>.dback`. Keep a copy *off the box* — Nextcloud, a Synology share, an SMB drive, a USB stick, a password manager's file vault — so a full host loss doesn't take your only copy with it. Because it contains credentials, treat it like a secret (it's encrypted, but defence in depth matters).

## How it's protected

The whole file is **AES-256-GCM encrypted with your master `DOCKBACK_ENCRYPTION_KEY`** — the same key that protects your container backups. Two consequences:

- The file is **opaque** to anyone without that key.
- **Restore requires the same key.** A restore only succeeds if the instance you're restoring into holds the identical key. (Live sessions are stripped from the backup, so it never carries an active login.)

> This is why *Back up your encryption key* is the most important step in DockBack. Without the key, neither your container backups **nor** this application backup can be restored.

## External destinations (off-box copies)

Local backups live on the same host — fine for quick rollback, but not real disaster recovery on their own. Add an **External destination** (under *Application Backup & Restore → External destinations*) to push application backups to your own server — **Nextcloud, Synology, SMB, or S3/B2** — kept entirely separate from the destinations used for *container* backups.

- **Add destination** — pick a provider, enter its details, and **Test Connection** before saving (same form as container destinations). A live status dot shows whether each is reachable.
- **Back up to external now** — uploads a fresh encrypted backup to every *enabled* external destination.
- **Browse** — lists the backups already on a destination, each with a **Restore** action — so even after a total host loss you can stand up a new instance (same encryption key) and pull your setup straight back from the remote.

## Restore

Three ways, all identical under the hood (decrypt → validate → restart):

- **Restore** an entry from the **Available backups** (local) list,
- **Browse** an external destination and **Restore** an entry from there, or
- **Upload & restore…** a `.dback` file you kept off-box.

DockBack will:

1. **Decrypt and validate** it (a wrong key or corrupt file is rejected before anything changes).
2. **Stage** the restored database and **restart** the app to swap it in cleanly.
3. Come back up with all your nodes, backups, settings, and 2FA — as if nothing happened.

Restore **replaces all current data** and signs you out; sign back in after the app restarts (a few seconds). A staged file that fails validation is set aside and never applied, so a bad file can't stop the app from starting.

## Proving the app-backup restores

This is the one backup that rescues every *other* backup's catalog — so DockBack proves it, automatically. On a weekly cadence (configurable; **Settings → Encryption / tuning**, `appbackup.drill_interval_days`, 0 = off) an **integrity drill** decrypts the **newest** stored app-backup into a temporary folder, opens the contained database **read-only**, runs SQLite's `PRAGMA integrity_check`, and confirms the core catalog tables are readable. Nothing is ever staged or swapped in — it's a pure read-only proof.

The Application-backup card shows **Last proven** with a tick and how long ago, or a red failure with the reason. Press **Test restore now** to run it on demand (bounded to two minutes). If a drill ever fails, DockBack raises the **same high-priority alert as a bit-rot scrub failure**, naming the file — a signal to create a fresh app-backup and investigate before you actually need to restore.

## Disaster-recovery drill (recommended)

On a fresh DockBack instance configured with the **same encryption key**, upload your latest `.dback` and confirm your nodes and backup catalog reappear. Knowing your recovery works *before* you need it is the whole point. (The automatic **integrity drill** above proves the archive decrypts and opens cleanly; this end-to-end drill additionally proves a full new-instance restore.)
