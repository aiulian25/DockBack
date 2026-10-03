# Jellyfin & backing up only what you can't rebuild

**Jellyfin** keeps everything under one `/config` tree, and a real one measured **11.4 GB** — of which **11 GB was scrubbing-preview thumbnails** the server draws itself. The database that holds your users, watch history, playlists and collections was 54 MB.

So the interesting question isn't how to back Jellyfin up. It's what to leave out.

## Leave out the trickplay previews (if you want to)

`data/trickplay` holds the little images shown when you scrub a video's timeline. Jellyfin regenerates them by re-decoding every file — expensive, but automatic and in the background.

That directory lives *inside* the `/config` mount, so ticking or unticking mounts can't reach it. The container's page now offers it directly:

> **Leave out trickplay scrubbing previews** — Jellyfin redraws them in the background after a restore, which re-decodes every video and can take hours on a large library.

**It's off by default** — everything is captured. Turning it on takes a 12 GB backup down to roughly 1.3 GB and trades that regeneration. Whichever you choose, the archive **records what was left out and why**, so a restore is never a surprise. And because it's a deliberate choice about something the app rebuilds, it does **not** mark your backup as incomplete — that warning is reserved for data that is genuinely gone.

Also worth enabling: **incremental backups**. Artwork under `metadata/` and the trickplay images are written once and rarely change, so after the first run each backup captures only what's new.

## What's excluded automatically

`/cache` and `/logs` are separate mounts, so they're already outside a normal selection — transcodes live under cache, and logs are operational. Nothing to configure.

## The database gets a real snapshot

`jellyfin.db` is the whole picture: users, libraries, watch state, resume points, playlists, collections. Jellyfin holds a write-ahead log while running, so copying the file live can catch it torn.

DockBack finds it **by its file header, not its name**, and takes a transactionally consistent snapshot, folding in and removing the stale write-ahead log so the restored database opens clean. Set this container's **quiesce mode to stop** as well — the database is small and the pause is brief.

Every snapshot records **how many rows each table held**, and every restore checks them back. If `Users` or `BaseItems` came back short, the restore **fails and names the table** rather than handing you a server with a library full of holes.

> Jellyfin's own in-app backup feature isn't used as the restore path here. If you enable its scheduled database task, those files are captured too — as a complement, not the plan.

## Keep your media paths identical

This is the one that bites on a move.

Jellyfin stores **absolute paths** — in library definitions *and* inside playlist files. If your media lives at `/mnt/NAS` on one machine, it must be mounted at `/mnt/NAS` on the next. The **host** path can be anything; the path *inside the container* cannot change.

Get it wrong and Jellyfin comes up perfectly, libraries report "unavailable", and playlists point at nothing. Rewriting those paths afterwards means editing the database and the playlist files by hand — possible, fragile, and much worse than mounting things in the right place to begin with. The restore panel lists the recorded paths so you can check before you commit.

**Your media isn't in the backup.** Multi-terabyte libraries on a NAS are that NAS's job; this protects Jellyfin's own configuration. Confirm your media is covered separately — the worst outcome is assuming it was.

## Moving to another machine

Works on any host — Synology, UGREEN, a Linux box, Windows Docker. **Remap path** if `/config` lives elsewhere; **Remap machine IP** if anything is pinned to the old address. Mount the media at the same container paths *first*.

**Hardware transcoding is host-specific.** If the source used `/dev/dri` or an NVIDIA GPU, the restore panel warns when the target has neither — Jellyfin falls back to software transcoding, which works but costs a great deal more CPU.

**Ownership** is handled: DockBack records the user Jellyfin runs as and re-owns the restored files to match the target's configuration before the container starts.

**Version: newer is fine, older is refused.** Jellyfin migrates its database when it starts, and a migrated database will not open on an older server — which is why the project tells you to back up before every upgrade. Restoring into the same or a newer version is safe; restoring into an older one is blocked before anything is written.

## Your archive holds a working API key

`system.xml`, `network.xml` and the database contain Jellyfin's API keys — one of which is probably also sitting in your dashboard and monitoring widgets. Anyone who decrypts the archive has server API access.

Keep an immutable offsite copy, and consider **write-only mode**. If an archive is ever exposed, revoke the key in **Dashboard → API Keys** — and remember to update every widget that was using it.

## Proving a restore worked

Not "the page loads". These five:

1. **Log in as the original user with the original password.**
2. A half-watched item **resumes at exactly the right point** — playback state survived.
3. A **playlist** and a **collection** are intact and play — this is the absolute-path check.
4. A restricted user's **parental controls still apply**.
5. Libraries did **not** kick off a full re-scan and re-match.

Point 3 is the one that catches a path mismatch, and point 2 is the one that proves the database really came back rather than being rebuilt.

## The general rule

Sort an app's data into what you can't rebuild, what you can rebuild cheaply, and what belongs to something else. Back up the first, make the second an explicit choice with its cost stated, and be clear that the third is somebody else's job.
