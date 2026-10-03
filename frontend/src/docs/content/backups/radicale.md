# Radicale & noticing what nobody looks at

**Radicale** is the smallest thing in this documentation: a single container whose whole world is two small directories — a config file with a list of password hashes, and a tree of calendars and contacts measured in megabytes.

There is nothing difficult about backing it up. Both directories are captured, the container is briefly paused so the copy is taken between operations, and a restore puts them back exactly as they were. No database to dump, no engine to quiesce, no schema to migrate.

Which is precisely why it is worth a page. Small and sensitive is the combination that gets overlooked.

## What travels

- **`/config`** — the configuration and the file of **password hashes**. This is what makes a restored server accept the same logins.
- **`/data`** — the whole collection tree: every calendar, every contact, Radicale's own metadata and history, and the lock file.

**The lock file is nothing to worry about.** It is an advisory lock, held by a running process, so it dies with the process. There is no stale lock to clear after a restore, and no reason to exclude it from the backup — it is captured and restored like any other file, and means nothing at rest.

Radicale writes safely: every change is staged in a temporary directory beside its target and then renamed into place, so an interrupted write leaves either the old content or the new one and never a half of each. Pausing during the copy is belt and braces on top of that.

## The thing nobody looks at

That staging pattern has a consequence. If a write *is* interrupted — a crash, a container killed mid-import — the staging directory is simply left behind. Radicale ignores it forever, because nothing is looking for it.

So it stays. In one real deployment, **two of them sat in the tree for a year** after an initial calendar import, roughly doubling the item count of every backup taken since. Nothing was broken. Nobody noticed, because nothing ever tells you.

A backup walks the whole tree anyway, so DockBack now looks. When it finds a staging directory more than an hour old it says so: how many, how much space they cost in every backup that carries them, when the oldest one dates from, and where they are.

**It does not delete them,** and that is deliberate. A backup tool that removes things it judges to be debris is a backup tool that can be wrong about your data. Clearing them is yours to do:

1. Take a backup and check it verified.
2. Stop the container.
3. Remove the directories the warning named.
4. Start it again.

The one-hour threshold is what makes this a finding rather than noise — a staging directory created moments ago is a write in progress, not a leftover, and reporting those would train you to ignore the message.

## The password-hash file

`/config/users` holds bcrypt hashes. Bcrypt buys time against someone who has the file; it is not a reason for them to have it. DockBack checks that file's permissions when it backs up and again after a restore, and tells you — with the command — if it is readable by more than its owner. As everywhere else, it reports and never changes: a restore reproduces the permissions it captured, in both directions.

The archive holds your calendars, your contacts and those hashes together, so it is worth the usual care: encryption is on by default, a retention-locked offsite copy is worth having, and **write-only** mode is worth considering.

## Moving to another machine

Works on any host — Synology, UGREEN, a Linux box, Windows Docker. The restore reproduces the container as captured, including its hardening: a read-only root filesystem, dropped capabilities and no-new-privileges all come back, because they are part of the configuration the backup holds. A restore never softens them.

**Ownership sorts itself out twice over.** The files are restored with their original numeric ownership, and the image's own entrypoint re-takes ownership of the storage on every start. So it does not matter whether the user IDs mean anything on the new machine.

**Remap path** if the directories live somewhere else on the new host.

### Whether your clients follow depends on a decision you already made

Radicale addresses each collection by its path, and the paths come back verbatim with the tree. So a client reaches exactly the same collection on the new machine — *provided it can still find the server*.

- A client pointed at a **domain name** follows automatically. Point the domain at the new host and every device reconnects with nothing done to it.
- A client pointed at an **IP address** does not. Every device has to be re-configured by hand, one at a time.

Nothing in a backup can change that; it is decided when the client is first set up. If you have devices on an IP address, moving them onto a name — before you need to move the server — turns a device-by-device job into a single change.

## Versions

Radicale's collection format is **stable across the whole of version 3**, so a backup restores into any 3.x server and there is nothing to block in either direction. The pre-restore panel says so, rather than leaving a version difference looking like a risk it isn't.

Version 2 used a different storage layout. Do not restore a 3.x backup into a 2.x image.

Restores recreate by image digest, so a tag that has moved on since the backup does not change what you get.

## Proving a restore worked

1. An unauthenticated request still returns **401** — auth was not weakened.
2. Logging in works with the **original password**, which proves the hash file came back intact.
3. A client discovers the **same collections at the same paths**, and syncs every event and contact.
4. Change one event through the client and confirm the write lands — the store is healthy read-write and the lock is working.
5. Compare a checksum of the collection tree against the source; it should be identical.

Point 3 is the one that matters. Everything else can be true of a server that came back with its data in a slightly different place, and only a real client proves it did not.

## The general rule

Most of what goes wrong with a small, well-behaved application is not that the backup fails. It is that nothing ever looks at the data closely enough to notice something has been quietly wrong for a year. A backup reads every byte anyway — so it is the cheapest possible moment to look, and the one time somebody is paying attention.
