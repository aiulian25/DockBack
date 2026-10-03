# Paperless-ngx & two backups that do different jobs

**Paperless-ngx** is a stack — the application, a PostgreSQL database, a Redis broker, and sometimes converters alongside them. Everything that matters lives across the first three, so back up the **whole stack**, not the web container on its own.

Two layers, and they are not alternatives.

## Layer A — the nightly stack backup (the one you restore from)

One **app-consistent stack snapshot** captures, inside a single window:

- the **database**, as a logical dump with a completeness contract — primary and foreign key counts recorded at capture and checked after the import, so a partial restore fails loudly instead of leaving a database with rows and no constraints
- the **data** volume — the search index, the trained classifier, the schedule
- the **media** volume — originals, archived PDFs and thumbnails
- **Redis**, as a point-in-time snapshot
- every service's configuration, networks and image digest

Because the index and the classifier are inside the captured data volume and the database comes from the same window, a restore needs **no re-indexing, no re-OCR and no retraining**. That is the point of doing it this way, and it is why this layer is the primary one.

The database's raw data directory is deliberately **not** captured: the logical dump supersedes it, and shipping both would mean shipping a torn copy alongside a good one.

## Layer B — the app-native export (portability insurance)

Paperless ships a symmetric exporter and importer, and DockBack has a built-in preset for them:

```
document_exporter /usr/src/paperless/export --no-progress-bar
document_importer /usr/src/paperless/export --no-progress-bar
```

Both were re-checked against the current image and are still exactly right. This layer survives a jump across major versions where a raw database restore could be refused, at the cost of rebuilding the index on import. Run it weekly if you like; restore from Layer A.

**An app-native restore needs an instance that does not already hold those documents.** The importer refuses to overwrite existing files — it stops with `FileExistsError` and a non-zero exit, and DockBack fails the restore rather than reporting a success that imported nothing. So restore it as an **isolated copy**, or into a container recreated with empty volumes. Restoring it over a live, populated Paperless is the one thing that will not work.

### The plaintext copy the exporter leaves behind

This is worth knowing before you enable Layer B. `document_exporter` writes **every document, decrypted and renamed**, into the export directory — and leaves it there. DockBack's archive of that directory is encrypted; the directory itself is not, and it persists between backups.

So there is a toggle on the container's page: **Empty the export directory after each backup**. It is off by default and it runs only after the archive has been written successfully — a failed backup never deletes the thing it was made from. DockBack builds the deletion itself from the profile's own directory, after validating the path; it is not a command you can type, precisely because "run this `rm` as root inside the container" should not be a text field.

The trade-off: the exporter is incremental, skipping files whose size and timestamp still match. Emptying the directory makes every run a full re-export. On a few hundred documents that is nothing; on a very large archive it is real time. Only you know which you have.

## What DockBack tells you about Redis, and why it is unusual

Redis here is a task broker: queue entries and cache, most of them carrying an expiry.

**Redis deletes already-expired keys as it loads a snapshot.** So a backup taken yesterday legitimately comes back with fewer keys than were captured — every time, from a perfectly good backup. Treating that as a shortfall would make a day-old backup of a broker unrestorable, which is exactly backwards.

So DockBack records, at capture, how many keys carried an expiry, and does the arithmetic:

- fewer keys than were captured **without** an expiry → **the restore fails.** Those keys could not have expired on their own, so something really was lost.
- somewhere between that and the full count → reported with both numbers, and the restore continues. A time-to-live did its job.
- an **empty** Redis where keys were captured → the restore fails. That is the snapshot not loading at all, which is the silent failure this check exists for.

Nothing in Paperless depends on Redis surviving a move: it rebuilds its queue. It is captured because it costs almost nothing and the stack comes up without a hiccup.

### If your Redis has a password

DockBack dumps Redis through `redis-cli`, which has to authenticate like anything else. It looks for the password in every place one is normally kept: `REDIS_PASSWORD`, `REDIS_PASS`, `REDIS_AUTH`, their `_FILE` variants for a mounted secret, `REDIS_ARGS`, and — the common one — `--requirepass` on the **server's own command line** in your compose file.

That last one has to be read from the container's recorded configuration rather than from inside it, because Redis overwrites its own command line with a process title (`redis-server *:6379`) the moment it starts. The value is handed to `redis-cli` through an environment variable and never on a command line, so it cannot be read out of the process list.

A `redis.conf` is checked too — the file named on the server's own command line, and the paths the images use — for both `requirepass` and the ACL spelling, `user default >password`.

**If none of them holds a working password, the backup does not fail.** Redis writes a complete RDB to `/data` on its own save schedule, and that file is exactly what a restore replays — so DockBack captures the directory as files instead and says so plainly:

```
Could not authenticate to Redis, so no consistent snapshot was taken — capturing
its /data directory as files instead. That holds the RDB Redis last wrote on its
own save schedule, so a restore works from a slightly older point in time.
```

The archive records that fallback on its face, so the grade and the runbook say a consistent snapshot was not taken and why — it is an older backup, not a silently weaker one.

This holds in a **stack backup with an app-consistent snapshot** too: the Redis member falls back the same way, its `/data` directory rejoins the capture for that run, and the rest of the group still forms one coherent point in time. One broker nobody can authenticate to does not cost the stack its snapshot.

This is the one engine allowed to do that. A PostgreSQL or MySQL data directory copied out from under a running server is torn, and a torn copy that grades like a backup is worse than none, so those still fail loudly.

Set `REDIS_PASSWORD` on the container for a real point-in-time snapshot. A password set at runtime with `CONFIG SET requirepass` is the one case nothing can discover — it exists only inside the running server.

## Moving to another machine

Works on any host — Synology, UGREEN, a Linux box, Windows Docker. Restore all the services together, in dependency order; DockBack recreates the networks first, so the database and broker hostnames resolve exactly as they did.

- **Remap path** if the stack lives under a different directory — `/volume1/docker/...` on a NAS versus `/home/you/docker` elsewhere. The container-side paths do not change.
- **Remap machine IP** if anything is pinned to the old host address.
- **Sessions, passwords and tokens keep working**, because the secret key is part of the captured configuration and is restored byte-identically.

### If you change USERMAP_UID / USERMAP_GID

This is the normal thing to do when moving off a Synology: its shares are owned by an account like `1026:100`, and on an ordinary Linux host you want `1000:1000`.

The restore writes files with the ids the archive recorded — so on its own, that change would leave every document owned by a user that does not exist on the new machine, and Paperless unable to write to its own media directory.

DockBack compares the ids the **target** container declares against the ids the data actually carries, and aligns the restored paths when they differ:

```
Paperless on this host runs as USERMAP_UID/USERMAP_GID=1000:1000, but the restored
data is owned by 1026:100 — aligning ownership of 3 restored path(s)
```

It reads the target's environment, not the backup's, precisely because the edit is the point. If it cannot change ownership it says so and prints the `chown` to run by hand — the data is restored either way.

### The address

Paperless records where it lives in **three** environment variables, and each wants a different form of the same address — `PAPERLESS_URL` a full URL, `PAPERLESS_CSRF_TRUSTED_ORIGINS` and `PAPERLESS_CORS_ALLOWED_HOSTS` a list of `scheme://host` origins, and `PAPERLESS_ALLOWED_HOSTS` a list of bare hostnames. Django rejects a trusted origin with no scheme, and matches nothing for an allowed host that has one.

Supply a **new address** in the restore dialog and DockBack writes each of them in the form it needs, as the container is recreated. Existing entries in the lists are kept, so the instance keeps answering everywhere it already answers.

Leave it blank and nothing is changed — the common move, where the domain follows the app, needs none of it.

**A bare host is not enough for `PAPERLESS_URL`.** Type `docs.example.com` and DockBack writes `https://docs.example.com` there, because Paperless parses that variable as a URL and appends it to its CSRF trusted origins — a missing scheme makes Django refuse to start at all, and the restore looks finished while the container never comes up. Supply a scheme yourself if it is not `https`.

**Your address does not follow the container.** If Paperless is behind a tunnel or reverse proxy, that still points at the old machine until you re-point it.

### Does mail fetching still work after a restore?

**With a password account, yes, and there is nothing to do.** The account, the server, and its password are rows in the database, restored with everything else.

**With an OAuth account — Gmail or Microsoft — check it.** The refresh token comes back with the database, but those are issued to an app registration at the provider and they expire. Nothing about a restore can walk through a consent screen, so this is the one part of a complete restore that can be quietly dead: every document is there, and no new ones arrive.

DockBack asks the restored database whether any mail account uses token authentication and says so at the end of a stack restore. A deployment using passwords, or none at all, hears nothing.

## The consume directory

If your consume folder is a shared directory, restoring files back into it will make Paperless consume them again. Paperless deduplicates by checksum, so this is noise rather than duplication — but if it is a large shared drop folder, consider leaving it out of the backup selection.

## Proving a restore worked

1. **Document, tag and user counts match** what you had.
2. **File bytes are identical** — checksum the originals and archive directories on both sides.
3. **Log in with the original password** and open a document.
4. **Search for a term that was OCR'd before the backup.** A hit proves the index came across rather than being rebuilt — the whole reason Layer A captures the data volume.
5. Run Paperless's own **`document_sanity_checker`** and read its output.

> Point 5 is a manual step on purpose. That checker prints its findings and **always exits 0**, even when it reports documents with errors — measured, not assumed. DockBack can attach a post-import check to an export profile and fail a restore when it exits non-zero, but wiring in a command that can never fail would look like proof and provide none. So this one is yours to read.

## The general rule

When an application has both a raw capture and its own exporter, they answer different questions. The raw capture restores *this* deployment exactly, index and all. The exporter carries the data across a version jump the raw path could not survive. Keep both, and be clear with yourself about which one you would actually restore from at three in the morning.
