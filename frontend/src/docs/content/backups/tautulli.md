# Tautulli & the address that breaks when something else moves

**Tautulli** watches a media server and keeps the history — who watched what, when, and for how long. Its whole state is one directory, so backing it up is straightforward. Two things about it are not.

## Two thirds of the old backup was nothing

A real archive of this container came out at **336 MB**. About 133 MB of that was `cache/` (artwork, refetched on demand) and `logs/` (rotated), and neither restores anything.

Both are now **always excluded**, no toggle — the same treatment rotated logs get everywhere in DockBack, for the same reason: offering a choice would imply there is a reason to keep them inside an encrypted archive, and there isn't. The archive drops to about 25 MB.

**Tautulli's own scheduled backup copies** in `/config/backups` are a different case and get a toggle. They are genuine consistent snapshots the application can restore itself from, which makes them a useful portable extra — and they carry the same credentials the live configuration does, which is worth knowing before deciding. Nothing is lost for a DockBack restore either way: the live database is captured directly.

## The database

`tautulli.db` is SQLite and it is being written to while you back it up. DockBack takes a **consistent snapshot** rather than copying the file, so a write in progress cannot leave you with a torn database.

DockBack takes that snapshot itself, with its own SQLite engine: while Tautulli is paused, the database and its write-ahead log are copied out together; once it is running again, that copy is written into one clean file, integrity-checked and counted, so none of it lengthens the pause. A copy taken while Tautulli was running can be caught mid-write, so the snapshot is skipped rather than trusted and the backup is graded down with the fix — pause the container during the copy.

With a volume sidecar image that includes `sqlite3` (Settings → Advanced → Performance & tuning), the restore also proves itself: the database is re-read on the target, its integrity checked, its per-table counts compared against capture, and its checksum compared byte for byte. A restored Tautulli with **no history** or **no users** fails the restore before the container starts, rather than coming back looking like a fresh install.

## The interesting half: moving Tautulli is free, moving Plex is not

Tautulli stores the address of the media server it reads from, and a token to authenticate with. The token is bound to that **server's identity**, not to any address.

That gives an asymmetry worth being precise about:

- **Move Tautulli.** Nothing to do. The token still works, the identity still matches, and the address it holds still points at a server that has not gone anywhere. No re-linking, no setup wizard, no re-authentication.
- **Move the media server.** Tautulli did not change at all — and it quietly stops collecting. It starts, its healthcheck passes, its interface loads, and the history simply stops growing, because the thing it reads from is no longer where it was told to look.

So the restore panel offers a second address field: **the new address of the service this depends on**, separate from the field asking where Tautulli itself is reached. Leave it blank — the normal case — and nothing is touched.

Fill it in and DockBack updates **only the two recorded address keys**, inside the container, before it starts. The edit is section-aware, so a key of the same name elsewhere in the configuration is not touched, and it is verified afterwards — an edit that silently matched nothing is reported rather than assumed. The token, the recorded server identity and every other setting are left exactly as restored, because none of them needed changing.

### And then it checks

Once the restore is healthy, DockBack asks the media server who it is, using the token that was just restored, and tells you one of three things:

- **it answered and it is the same server** — nothing needed re-authenticating, and the connection is proven end to end;
- **something answered, but it is a different server** — the case below;
- **nothing answered** — worth looking at, and not a fault in the backup.

The token never leaves the container. The check runs inside it, reads the credential from the application's own configuration, and prints only a verdict. Nothing about it reaches a log.

**The one case a restore cannot paper over** is a media server that was rebuilt from scratch rather than moved. It has a new identity, so the stored token and the history's linkage to it no longer refer to anything, and re-linking is unavoidable. DockBack names that specifically rather than reporting a vague failure.

## Moving to another machine

Works on any host — Synology, UGREEN, a Linux box, Windows Docker.

- **Remap path** if the configuration directory lives somewhere else. It is often on a network share; a restore can put it on local disk instead, and the container-side path does not change either way.
- **Ownership** is restored numerically and the container's own user mapping travels in its environment, so it does not matter whether those user IDs mean anything on the new host.
- **Web authentication stays on.** The username and the hashed password come back exactly as captured; no step in a restore weakens them.

> One thing worth knowing if your configuration directory is on a **network share**: SQLite depends on file locking that CIFS and NFS do not reliably provide. Plenty of people run it that way without trouble, and DockBack does not block it — the database traverses the share in normal operation regardless, so a backup neither creates nor worsens the risk. But local disk is the safer home for it, and a restore is a natural moment to move it.

**Version: newer is fine, older is refused.** Tautulli migrates its database when it starts, forward only, so restoring a newer backup into an older image is blocked before anything is written. Restores recreate by image digest, so a tag that has moved does not change what you get.

## The token in the archive

The archive holds a live token for the media server, Tautulli's own API key, the hashed web password, and the credentials of every notifier it sends through. Encryption is on by default; **write-only** mode and a retention-locked offsite copy are both worth it here.

If an archive ever leaks: sign that token out from the media account's authorised devices and rotate the API key. That is the containment step, and it is quick.

## Proving a restore worked

1. History, user, notifier and library counts match — DockBack checks them and fails the restore if they fall short.
2. **Log in with the original web credentials.**
3. The graphs render, which means the database came back whole.
4. Your notifier is still there and a test message sends — its credential lives in the database and survived.
5. **The media-server connection shows green without re-authenticating.** DockBack has already checked this and put the answer in the restore log.

Point 5 is the decisive one. Everything else can be true of an instance that came back with an intact history and no idea where to keep collecting it.

## The general rule

Most applications record where *they* are. Some also record where something *else* is — and those two break at opposite moments. Moving the application invalidates the first; moving its dependency invalidates the second, without the application changing at all. It is worth knowing which kind of address you are looking at, because only one of them is your problem when a restore comes back healthy and does nothing.
