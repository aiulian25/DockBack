# Calibre & apps that share a library

**Calibre** is worth its own page because it demonstrates three things at once that break restores elsewhere too: two containers sharing one library, a database that must never sit on network storage, and the ownership mismatch that makes an app come up perfectly and then refuse to save anything.

## Which Calibre do you run?

"Calibre in Docker" is really two different projects, and many people run **both**:

- **`linuxserver/calibre`** — the full desktop application over a browser VNC session, with the built-in Content Server. It **manages** the library.
- **`calibre-web` / `calibre-web-automated`** — a web reader with its own users and permissions. It **reads** a library someone else manages.

If you run both against one library, they are **one unit**. calibre-web's `app.db` refers to books by the ids in the library's `metadata.db`, so restoring one without the other leaves a catalogue full of entries pointing at books that are no longer there.

DockBack now says so. When a restore would write into a directory another container also uses, the restore panel names that container and the shared path before you commit. **Restore them together.**

## What actually holds your library

| File | What it is |
|---|---|
| `metadata.db` in the library folder | Every book's title, author, tags, series, ratings and comments. The crown jewel — the book files alone are not a library. |
| `<Author>/<Title>/…` | The book files, covers and `.opf` sidecars. |
| `app.db` (calibre-web) | **Users, password hashes and permissions** — separate from `metadata.db`. |
| `gdrive.db` (calibre-web) | **Google Drive access tokens**, if you connected Drive. |

`metadata.db` is WAL-mode SQLite and is written constantly. DockBack finds it **by its file header, not its name**, and takes a transactionally consistent snapshot — never a copy of the live file, which can be torn mid-write.

## A corrupt database now fails the backup

Every SQLite database DockBack snapshots is checked with `PRAGMA integrity_check` as it is captured. If one comes back **damaged, the backup fails**.

That is deliberate. The damage is at the *source* — a backup would preserve it faithfully, not repair it — and you want to know while the original is still there to fix, not during a recovery a year later. Library corruption is exactly the failure that otherwise hides for months.

A database that merely **could not be snapshotted** (locked, in use) is a different thing and never fails a run: it says nothing about your data, so the raw file is captured as before and the log says which one and why.

You can turn the failure off in **Settings → Performance & tuning** if an abandoned cache database is blocking backups you actually want. The archive then records the damage instead of refusing, so it still declares itself rather than looking clean.

## Ownership: the reason a restore "works" but nothing saves

This is the commonest broken restore there is, and it is not a backup problem at all.

Images built on the LinuxServer base run the application as the user in `PUID`/`PGID`. A restore preserves the *original* machine's ownership — right when the new host uses the same ids, wrong when it doesn't. A Synology whose user is `1026` restoring files owned by `1000` gets a Calibre that starts, shows the library, and then cannot write to it. SQLite reports that as **"attempt to write a readonly database"** or **"database is locked"**, which reads like corruption and is nothing of the kind.

**DockBack now fixes this automatically.** It records the ids the app runs as, and on restore reads the ids the *target* container will run as. If they differ, it re-owns the restored data to match — **before the container starts**, so the app never sees data it cannot write. The run log says exactly what it changed.

If the change can't be made, the restore still succeeds and the log gives you the one command that fixes it. Your data is never at risk from this either way.

## Never put the library on a network share

Calibre's own documentation is blunt about it, and DockBack enforces it: a library or config directory on **CIFS/NFS corrupts**. `metadata.db` depends on file locking that network filesystems do not honour, and the failure is silent for days before it surfaces as an unreadable library.

The restore refuses when it can measure that the target's `/config` or `/calibre-library` is on a network filesystem, and reminds you of the requirement when it can't.

A **media or ingest folder** on a NAS is completely fine — that is not a library and has no database in it. Only the library and the config directories have to be local.

## Moving to another machine

Works on any host — Synology, UGREEN, a Linux box, Windows Docker. Two things usually need your input, both ordinary restore options:

- **Host paths** — use **Remap path** for the new base directory. The paths *inside* the container stay the same.
- **Addresses** — use **Remap machine IP** if anything is pinned to the old host.

**Keep the container paths identical.** Both apps record where the library is *inside the container* (`/config/Calibre Library`, `/calibre-library`). The host path can change freely; the container path should not. If it must, re-point calibre-web's database location in its admin settings afterwards.

**Version: newer is fine, older is refused.** Both `metadata.db` and `app.db` upgrade on start and neither can be downgraded. Restoring into the same or a newer version is safe (the migration is one-way); restoring into an older one is blocked, because it would fail after the data had been written.

## Your archive holds real secrets

Beyond the books: **user password hashes** in `app.db`, the desktop **GUI password**, and — if you connected Drive — **Google account tokens** in `gdrive.db`. All of it lives only inside the AES-256-GCM encrypted archive; the manifest carries variable names, never values.

Because those Drive tokens grant access to a Google account, this is a good candidate for **write-only mode**, where the running instance cannot decrypt its own archives at all. If an archive is ever exposed, the containment steps are: revoke the Google authorisation, reconnect Drive in calibre-web, and rotate the calibre-web and desktop passwords.

## Proving a restore worked

Not "the page loads". These four:

1. **Log in to calibre-web with the original password** — proves `app.db` came back intact.
2. **Open or download a book** — proves the files and the metadata links agree.
3. **Edit a book's metadata and save it** — the decisive one. It proves the database is **writable**, which is the ownership check nothing else catches.
4. **Book, author, tag and series counts match** what you had.

## The general rule

Where two containers share a directory, they are one unit — back them up together and restore them together. Where an app declares `PUID`/`PGID`, ownership is part of the restore, not an afterthought. And where an app keeps an embedded database, keep it on local disk and check its integrity at backup time, while the original is still there to repair.
