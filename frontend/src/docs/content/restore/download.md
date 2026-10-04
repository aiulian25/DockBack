# Download a decrypted archive

Sometimes you want the raw data, not an in-place restore — to inspect a single file, migrate by hand, or keep an air-gapped copy. The **Download (decrypted)** button in a backup's drawer gives you exactly that.

## Exports ask for your password

A decrypted export is the most sensitive thing DockBack hands out: the archive contains every credential, database and private file the container held — in plaintext, on your disk, outside the app's control. So it is gated the same way revealing the master key is. Press **Download (decrypted)**, or the download icon on any file row, and DockBack asks for your password (and your two-factor code when 2FA is on) before the transfer starts.

This closes a real gap rather than adding ceremony. Until now a browser left open on an unlocked laptop could export everything in every backup, and nothing recorded that it happened.

- **One confirmation covers a few minutes of work.** Pulling six files in a row asks once, not six times — the same re-authentication window every other protected action uses.
- **Every export is written to the audit trail**, separately from the confirmation, so *asked for* and *took* are distinguishable afterwards.
- **The link is worthless once used.** The confirmation issues a one-shot pass that expires in two minutes, works exactly once, and only for the browser session that asked — so a URL left in your history opens nothing.
- **The same applies to the configuration archive** in *Settings → Application backup*. That file is encrypted with your master key, so on its own it opens nothing — but it is every sealed credential in one place, and worth a password.

Automation is unaffected: API tokens were never permitted to reach these endpoints at all, at any scope.

## A whole stack in one download

On a stack's restore page, **Download stack** gives you one zip holding every service's newest backup, each as its own decrypted archive (`<service>-<backup id>.tar`), chosen the same way a stack restore chooses them. It asks for your password like any export, and the same one-shot rules apply. One click instead of one per service: on the night that prompted it, the Arr stack's seven archives were fetched one at a time.

## Recover just one file (Browse files)

You don't need the whole archive to get back one config file or one photo. Open a backup's drawer and expand **Browse files**: DockBack lists every file in the backup's volume data (path + size) — read straight from the archive, nothing is downloaded yet. Filter to find what you want, then click the **download** icon on a row to pull **just that file**, decrypted and streamed on its own. It's the fast path for "I deleted one file and need it back" — no multi-gigabyte download, no destructive restore.

- The list covers the captured **volume/bind data**; database dumps and app-native exports are restored through their own flows, not browsed here.
- **Indexed backups list instantly and completely** — every backup now stores a compressed file index, so the listing is one small read with **no file-count cap**. Only backups made before indexing existed fall back to streaming through the archive server-side, capped at 20,000 files (use a full download or restore beyond that).

## Find a file across backups

Don't know *which* backup holds the file? On the **Backups** page, open a node and press **Find a file**: type a name or path fragment (e.g. `config.xml`) and DockBack searches the stored file index of **every successful backup on that node** — returning every generation of every matching path with its size and date. From a result row you can download **that generation of that file** directly, write it straight back into its container (see below), or open its backup's drawer. Backups made before file indexing are skipped (the search tells you how many).

## Put a recovered file back (one click)

Finding the file was only half the job. Until now, recovering one deleted config file ended in your browser's Downloads folder, followed by a manual `docker cp` and a guess at the right ownership and mode.

Next to the download icon — in **Browse files** and in every **Find a file** result row — is an **upload** icon: *Write back into the container*. It streams that file straight from the archive back to **its original path inside the running container**, with the **ownership and permissions the backup recorded**.

Before anything is written, DockBack asks you to confirm, naming the container and the exact path. The file that is there now is **kept alongside** the restored one as `<path>.dockback-<time>.bak`, so a write-back can always be undone by renaming it back.

### What it does and does not touch

- **One file.** Nothing is stopped, no volume is recreated, and no other file changes. It is not a partial restore.
- **The path comes from the backup**, not from you — there is no field to type a destination into, so a file can only ever go back where it came from.
- **Only regular files.** A symlink or device node in the archive is refused; restore the volume instead.
- It takes the **same exclusive lock a full restore takes**, so it can never run while a backup or restore of that stack is in flight (you'll get a clear "already in progress" message).
- Every write-back is **recorded in the audit log** with the path, the container and who did it.
- Standalone **volume backups** have no container path to write back to, so the action isn't offered for them — restore the volume instead.

> The file is written into the container's **mounted volumes**, which is the same data the backup captured. If the application caches the file in memory, restart the container for it to be re-read.

## Compare two backups (what changed?)

In a backup's drawer, **Compare with previous** diffs this backup's file index against the previous backup of the same container: **added**, **changed** (with old → new sizes), and **deleted** paths — answering "what changed between Tuesday and Wednesday?" before you pick which generation to restore. The comparison reads only the two stored indexes; nothing is decrypted beyond them.

## What you get

A standard **`.tar` archive**, already **decrypted and decompressed**, containing the backup's contents:

- `manifest.json` — the backup's manifest.
- `config/inspect.json` — the container configuration.
- `volumes.tar` — the volume/bind data (a tar within the tar).
- `db/<service>.dump` — database dump(s), if any.
- `appexport.tar` — app-native export, if used.

## Using it

1. Open **Backups → server → the backup**.
2. Click **Download (decrypted)**.
3. Extract locally with any tar tool:

```bash
tar -xf dockback-<id>.tar
# inspect, then if needed unpack the inner volume archive:
tar -xf volumes.tar -C ./restored-volumes
```

## No DockBack? No problem

**Download (decrypted)** is the convenient path — but it needs a running DockBack. For the real disaster (DockBack itself is gone and all you have is a `.dback` file and your key), you are **not** locked in: a single, dependency-free recovery script decrypts any backup on any machine with Python 3 — no DockBack, no Docker. See **Restore → Restore without DockBack (offline tool)** in the sidebar.

## Security note

The downloaded archive is **decrypted** — treat it like the sensitive data it contains. Store it somewhere safe and delete it when you're done. Inside DockBack and on your destinations, the same data is always encrypted at rest.
