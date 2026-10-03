# Immich & backing up a large photo library

**Immich** is four containers, tens of gigabytes of photos, and a database with vector search — and it has two failure modes that both produce a backup which *looks* fine.

## The one that catches everyone: the media isn't in it

DockBack skips bind mounts above a size threshold by default. That's the right call for a media library you already back up another way — and completely wrong for Immich, where the photo tree **is** the app.

A real Immich backup once captured **20.7 KB** of configuration and skipped **58 GB** of photos. It reported success. A restore from it would have brought back albums, faces and users pointing at nothing.

**Tick the upload mount** on the container's page, or the backup is metadata only.

DockBack now says how much is missing rather than noting it in passing. A partial backup's reason reads:

> PARTIAL — 58.0 GB of data was NOT captured (/usr/src/app/upload). This backup does not contain it; re-back up with it included

That's the same information as before, but at a scale you can act on.

**Turn on incremental backups for this container.** Immich's originals, thumbnails and transcoded videos are written once and never modified, so after the first full baseline every run captures only new photos. The 58 GB is a one-time cost.

## The one that's quieter: vector extension versions

Immich's database uses vector-search extensions — VectorChord, pgvecto.rs, pgvector — and the DB image tag names their exact versions, like `postgres:16-vectorchord0.4.2-pgvectors0.2.0`.

These extensions change their on-disk index format between versions. A dump restored into a **different** version restores **without a single error** and leaves search returning wrong results. Nothing fails; the answers are just wrong.

DockBack records the extension versions with every backup and checks them before restoring:

- A **different extension** (pgvecto.rs where the dump needs VectorChord) — **blocked**.
- A **different major or minor version** of the same extension (0.4.2 → 0.5.0) — **blocked**, with both versions named.
- A **patch** difference (0.4.2 → 0.4.3) — a note, not a block. Patch releases are fixes.

Restoring **by digest** — the default — reproduces the exact image and sidesteps all of this. The guard is there for when someone points a restore at a different image on purpose.

> Keep your Immich server and database images pinned **together** across your machines. The two versions are coupled, and a disaster-recovery target that can't run the matching database image can't take the restore.

## Everything else is already handled

**Faces and smart search survive.** Face embeddings and CLIP vectors live in the **database**, not the model cache. Restore the database and People stays intact with no reprocessing. The machine-learning cache is a few tens of megabytes of re-downloadable model weights — captured because it's small, never something to wait for.

**Sessions and API keys survive.** `JWT_SECRET` is a container environment value, captured inside the encrypted archive and reproduced verbatim, so nobody is logged out and no API key needs re-issuing.

**The database dump is the right kind.** DockBack uses `pg_dumpall`, which is what Immich's own scheduled backup uses and what these extensions require — a per-database dump can lose the extension wiring.

## Moving to another machine

Works on any host — Synology, UGREEN, a Linux box, Windows Docker. Back up and restore **all four services together** so the database and the photos come from one moment.

- **Remap path** if the photo tree lives somewhere else on the new machine. The path *inside* the container stays the same; Immich records asset locations relative to it.
- **Remap machine IP** if anything is pinned to the old address.

**Immich itself stores no server address**, so a move needs no change inside it and every login, API key and shared link stays valid. What holds an address is **each installed app** — if the address people reach Immich at genuinely changes, update the server URL in the phone and desktop apps. That failure looks like a broken login and isn't.

## Your archive is the family photo album

Encrypted always, but be deliberate about the rest. The archive holds the photos, their **GPS locations**, face-recognition data, and user accounts. Whoever holds the key holds all of it.

- **Consider write-only mode**, where the archive can only be opened with an offline key kept away from your servers.
- **Keep an immutable copy off the machine that made it.**

Key custody is the real security boundary here — more than anything in the pipeline.

## Proving a restore worked

1. Asset, album, person and shared-link counts match.
2. Open the timeline — **thumbnails and video previews render immediately**, proving the derived files came back and nothing is reprocessing.
3. Open **People** — the face groupings are intact **without re-running machine learning**. This is the decisive one: those groupings come from vectors in the database, so it proves the vector extensions restored correctly.
4. A **shared link** still resolves and an **API key** still authenticates — proving `JWT_SECRET` came across.

If People is empty and the timeline is reprocessing, the vector data didn't survive — stop and check the extension versions before letting it rebuild over the top.

## The general rule

For an app whose data is mostly one enormous directory, the two questions are "is that directory actually in the backup?" and "will the database still understand it?" Everything else — accounts, sessions, albums — takes care of itself.
