# Plex & backing up a server that lives in the cloud's address book

**Plex** is two awkward things at once: a large data directory that is mostly disposable, and a server whose identity is registered with Plex's cloud. Both shape how DockBack backs it up.

## What is worth keeping, and what is not

A Plex data directory is easy to back up badly. Copy it wholesale and you get an archive several gigabytes larger than it needs to be, most of that being files the server re-downloads on its next start.

DockBack always leaves out the parts that restore nothing:

| Left out | Why |
|---|---|
| `Cache` | regenerated |
| `Logs`, `Crash Reports`, `Diagnostics` | nothing to restore |
| `Codecs` | re-downloaded per version and architecture |
| `Drivers` | re-downloaded on start |
| `Updates` | downloaded installers |
| `Plug-in Support/Caches` | plug-in caches |

There is no toggle for these, because there is no reason to keep them: they cost archive space and none of them changes what a restored server can do.

What is **kept** matters more, and it is deliberate. `Metadata` and `Media` hold the artwork, chapter thumbnails and agent matches that took hours to build. They are regenerable in theory; in practice re-matching a large library is an evening's work and can quietly change matches. They travel. Turn on **incremental backups** for this container and the size stops mattering — both trees are append-mostly, so after the first run each backup is small.

**Your media files are not in the backup.** They are usually terabytes on separate storage, and that storage is their system of record. What DockBack protects is the server: its identity, database, artwork and settings.

## The database, and a default that quietly undermines it

Plex keeps everything — libraries, users, watch history, sharing — in a SQLite database that it writes to constantly. Alongside it sits a write-ahead log that can be as large as the database itself. **Copying those two files as files, at slightly different moments, is the definition of a torn copy.**

DockBack's answer is a consistent snapshot: the database is checkpointed into a sidecar copy, and on restore that copy is laid down and the stale write-ahead log removed, so Plex opens something coherent.

**That requires `sqlite3` in the volume sidecar image, and the shipped default does not have one.** When it is missing, DockBack falls back to copying the raw files — which for a busy Plex is exactly the risk the snapshot exists to remove. It now says so: the backup is graded down, the reason names the fix, and the list shows a **raw DB files** chip. Set a sidecar image that includes `sqlite3` under **Settings → Backups** and take the backup again.

This is worth checking even if you do not run Plex. Every application with an embedded database — and that is most self-hosted software — is affected by the same default.

Once the snapshot is engaging, a restore proves itself: the database is re-read on the target, its integrity checked, its per-table row counts compared against what was captured, and its checksum compared byte for byte. A restored Plex with **no libraries** or **no items** fails the restore before the container is started, rather than coming up looking like a fresh install.

## Identity: the reason a copy is dangerous

Plex clients do not find your server by address. They find it by an identity that is registered with Plex's cloud, and that identity — with the account token that publishes it — lives in `Preferences.xml`.

For a **real restore this is the best news in this page**: restore the file verbatim and the server that comes back *is* the same server. Every device that was already paired reconnects on its own, watch history and sharing are intact, remote access resumes, and there is nothing to re-link. DockBack never rewrites that file and never sets a claim token — a claim would mint a *new* identity, which is the opposite of what a restore is for.

It also means **only one server may carry the identity at a time.** For a migration: stop the old one before the restored one starts. Two servers publishing one identity is a fight neither wins.

### Restoring a copy is safe, because DockBack makes it safe

"Restore as a copy" is how you rehearse a restore without touching production — an isolated container, fresh volumes, no published ports. That isolation is about your network, and it says nothing about what the copy can reach *outbound*. A copy of Plex needs nothing more than internet access to announce itself as the server you are still running.

So before an isolated copy of Plex is started, DockBack clears the account token and publishing key **from the copy's own throwaway data**. The copy comes up, you can check its database and its artwork, and it cannot reach your Plex account or contend with the real server.

Two details worth knowing:

- **The server identifier itself is not cleared.** It is what makes a real restore work, and it is the token that does the publishing.
- **If the token cannot be cleared, the copy is not started.** A rehearsal is never worth disturbing production for, so DockBack stops and tells you rather than starting something that might publish.

Nothing about a real restore changes: there, the file is restored exactly as captured.

## Moving to another machine

Works on any host — Synology, UGREEN, a Linux box, Windows Docker.

- **Mount the media at the same container paths first.** Plex records each library's location as an absolute path inside the container. If the target does not provide them, the server comes back perfectly and every library reads as unavailable. The pre-restore panel lists the paths the target has to provide, including the media mount that is deliberately *not* in the backup.
- **Remap path** if the config directory lives elsewhere on the new host.
- **Ownership self-heals**: the restore writes numeric ownership, and Plex's own user mapping travels in its environment.
- **Host networking is reproduced** as captured — Plex needs it for discovery on the LAN.
- **Hardware transcoding is specific to the machine it was set up on.** The device path is recorded in Plex's own settings, so on a target without the same hardware Plex falls back to transcoding in software: it works, it costs far more processor time, and you reconfigure it in Settings afterwards. The pre-restore panel warns; it does not block.
- **Give the restored server internet access on first start.** Codecs and drivers are deliberately not in the backup and are re-downloaded.

**Version: newer is fine, older is refused.** Plex migrates its database forward when it starts, and an older server cannot open a migrated one — so restoring a newer backup into an older Plex is blocked before anything is written. Restore recreates by image digest; pinning the tag is worth doing if you would rather choose when that one-way step happens.

## The token in the archive

The archive holds the token that links this server to your Plex account. Treat it accordingly: keep encryption on (it is by default), consider **write-only** mode so only an offline key can open the archive, and keep a retention-locked offsite copy.

If an archive ever leaks, containment is unusually clean: sign that token out from your Plex account's authorised devices. It stops working, and nothing else is affected.

## Proving a restore worked

1. The server reports the **same identity** it had before.
2. The database passes its integrity check and the library and item counts match — DockBack checks both, and fails the restore if they fall short.
3. Artwork is present: open a show and see its poster, rather than a placeholder being fetched.
4. **A device that was already paired connects without re-linking**, and a show you had partly watched still remembers where you were.
5. A shared user still sees only the libraries they were given.
6. Libraries did **not** start a full re-scan.

Point 4 is the decisive one. Everything else can be true of a server that came back as a stranger; only that proves the identity travelled.

## The general rule

When an application registers itself somewhere outside the machine, its backup has two different jobs depending on why you are restoring. A recovery wants the identity back exactly. A rehearsal must not have it at all. A backup tool that only knows how to do the first one makes the second one dangerous — so it has to know the difference.
