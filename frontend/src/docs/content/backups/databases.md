# Database-aware consistent dumps

Copying a database's raw data files while it's running produces a backup that may be **corrupt or inconsistent**. DockBack avoids this entirely: when it detects a database container, it captures a **consistent logical dump** using the engine's own tools, over a local connection inside the container.

## How it works

1. DockBack detects the database engine from the container's **image name** (e.g. `postgres`, `mariadb`/`mysql`, `mongo`, `redis`).
2. It runs the engine's dump tool inside the container (via the socket-proxy `EXEC`/`POST` permissions) to produce a consistent dump of your user databases.
3. The dump is included in the backup archive (`db/<service>.dump`), compressed and encrypted like everything else.

Application containers, by contrast, have their **volume/bind files** captured (optionally with the app stopped or quiesced for a clean snapshot).

## No redundant raw copy

When a consistent dump is taken, DockBack **does not also capture the engine's raw data directory** (`/var/lib/mysql`, `/var/lib/postgresql/data`, `/data/db`) — a live-file copy is exactly the frequently-corrupt artifact the dump exists to replace, and keeping it would only bloat the backup. The data dir shows in the mount picker as *"captured via consistent dump — raw copy skipped"*. If you really want the raw files too, you can still tick the data dir explicitly.

## Backing up one database of a shared engine

A single Postgres or MariaDB/MySQL server often hosts several apps' databases. By default DockBack dumps the **whole cluster** (all databases, plus roles/globals) so a restore is complete. When you need to protect or recover just one app's database independently, open the database container and, under **Backup Options → Databases to include**, untick the ones you don't want. The selector lists the engine's app databases (system schemas excluded) and defaults to **all selected**.

- With a **strict subset** selected, the dump contains only those databases — for Postgres, their **roles/globals** are included so ownership and grants restore correctly; for MySQL, each selected database is emitted with a clean drop-and-recreate.
- **Restoring a subset is surgical:** DockBack imports it into the **running** engine and replaces **only** the selected databases — the other databases on that shared server are left untouched (it does **not** wipe and re-initialize the whole cluster the way a full-cluster restore does). For Postgres, active sessions on the target database are closed first so it can be replaced.
- Leaving **all** selected (or unticking everything) keeps the default **whole-cluster** dump and restore — so nothing changes unless you deliberately narrow it.

The selector appears only when the engine hosts **more than one** app database, and only while it's running (the list is read live from the server).

## Detection is by image, not env vars

A database is recognised by its **image** (the container that *is* the database server), not by environment variables. Apps like **Mealie** or **Nextcloud** set `POSTGRES_*`/`MYSQL_*` to *connect* to their database — that doesn't make the app a database, so DockBack backs the app up as files and dumps the **separate** database container on its own.

> Because of this, back up the **whole stack** (app + its database container) so recovery has both the files and the consistent dump — see *Restoring → One-click stack restore*.

### When the dump tools are missing

If a container looks like a database by image but its dump tool is missing, DockBack logs a warning and **falls back to a file/volume backup** rather than failing the run. That fallback is now **recorded and graded honestly**, because it matters:

Copying a running database's files is exactly what the dump feature exists to avoid — the engine may be mid-write, so the copy can be **torn** and refuse to start (or start with silent corruption). Such a backup used to be indistinguishable from an ordinary application backup: its manifest carried no databases at all, so it could earn an **A** with no reasons listed.

Now that backup:

- carries `db_fallback` in its manifest, naming the engine that degraded;
- shows a **raw DB files** chip on the Backups list;
- **cannot grade better than C**, with the reason *"this database was captured as raw files (its dump tools were missing) — the copy may be torn"*;
- carries a note in the **runbook**, because the recovery procedure is different: restore is **file-level, not a dump import**, so expect the engine to run its own recovery on first start and verify the data before trusting it.

**The fix is in the image, not in DockBack:** install the engine's client tools (`pg_dumpall`, `mysqldump`/`mariadb-dump`, `mongodump`) in the container and back up again to get a consistent dump. Some slim images ship the server without the client tools — that is the usual cause.

> This is not the same as a container that merely *connects* to a database (see above): those are applications, are captured as files by design, and are not flagged.

## Supported engines

Common engines are detected automatically (PostgreSQL, MySQL/MariaDB, MongoDB, and Redis/Valkey/KeyDB). For MariaDB/MySQL, DockBack uses the engine's current client tooling and dumps user databases.

**Redis** (and its drop-in forks **Valkey** and **KeyDB**) keeps its dataset in memory and only periodically flushes it to disk, so a raw file copy can catch `dump.rdb` mid-write. DockBack instead streams a **consistent point-in-time RDB snapshot** with `redis-cli`, captured as `db/<service>.rdb`. If the server has a password, DockBack reads it from the container's own `REDIS_PASSWORD`/`REDISCLI_AUTH` environment and passes it out-of-band (never on the command line). On restore, the snapshot is placed back as the engine's data file and loaded on startup — a Redis container that only connects a raw file copy is no longer treated as "dumped live" when it isn't.

**Finding the password.** DockBack looks in every place a Redis password normally lives: `REDISCLI_AUTH` or `REDIS_PASSWORD`/`REDIS_PASS`/`REDIS_AUTH` on the container, their `_FILE` variants for a mounted secret, `REDIS_ARGS`, a `redis.conf`, and `--requirepass` on the server's own command line. When none of them has a working one, the backup falls back to copying the `/data` directory as files — which still holds the RDB Redis last wrote on its own save schedule, so a restore works from a slightly older point in time.

**When the password was set at runtime.** One arrangement defeats every one of those searches: a password set while Redis was already running, with `CONFIG SET requirepass`. It is in no file, no environment variable and no command line — it exists only inside the running server. Until now that meant the fallback was not an occasional degradation for that broker but *every run, forever*, each backup quietly losing point-in-time consistency.

Record it once instead: on the container's page, under **Backup options**, a **Redis password** field appears for any Redis container. Save the password there and every subsequent backup takes a real snapshot.

- It is stored **encrypted with your master key**, like a node's connection credentials, and is **never shown again** — there is no way to read it back, so replace it rather than look it up. A master-key rotation carries it across with everything else.
- It reaches `redis-cli` through an environment variable, never a command line, so it is not visible in the container's process list.
- It applies to **both** an individual backup of that container and an app-consistent stack snapshot that includes it.
- Clearing the field returns the container to the ordinary search, and to the file fallback if that finds nothing.

The PostgreSQL family is detected broadly — beyond the official image, the vector-search and distribution builds are recognised too: **pgvector**, **pgvecto.rs**, **VectorChord**, **TimescaleDB**, **Citus**, and Supabase's Postgres (for example Immich's `tensorchord/pgvecto-rs` and `ghcr.io/immich-app/postgres`). These don't contain the word "postgres" in their image name, so recognising them explicitly is what ensures they get a **consistent logical dump** instead of a raw data-directory copy. For a Postgres backup, DockBack also records the database's **installed extensions and versions** (e.g. `vectors 0.2.0`) in the manifest, so a restore target's extension compatibility can be checked — the exact kind of mismatch behind vector-extension upgrade breakages.

**How the target is checked.** When you restore into a **running** Postgres container, DockBack reads the target's **actual installed extensions** from the live database rather than inferring them from the image name — so a privately-tagged or renamed image (e.g. `myreg/custom-pg`) is judged by what it truly provides, not what its name suggests. A restore into an image that genuinely has the dump's vector extension is allowed even if its name gives no hint; a restore into one that lacks it is blocked regardless of a reassuring-looking name. If the target **isn't running** (so its extensions can't be read), DockBack falls back to the image-name guess and the warning says so explicitly. As always, a blocking warning is overridable if you know what you're doing.

**Version-downgrade guard.** Every database backup records the engine's **version** (Postgres, MySQL/MariaDB, MongoDB, Redis). Before a restore, DockBack compares the dump's major version against the target container's live version and **blocks a downgrade** — e.g. restoring a **PostgreSQL 16** dump into a **PostgreSQL 15** image, which otherwise fails deep in the import *after* the data directory has already been wiped. You get an explicit, overridable warning naming both versions (restore anyway if you know what you're doing); an *upgrade* (15 → 16) only warns, and an equal version passes silently. If the target's version can't be read (e.g. it isn't running), the check is skipped rather than risk a false block.

The Postgres dump also **preflights the connection** and fails with a clear message if the container's `POSTGRES_USER`/`POSTGRES_PASSWORD` can't authenticate, rather than letting a bad credential degrade the backup silently.

DockBack works out how to authenticate on its own, so you don't have to configure credentials. For MariaDB/MySQL it tries, in order, the **root password** from the container's environment, then **root over the local socket** (MariaDB's default, where root logs in without a password), then the **application user** (`MYSQL_USER`/`MYSQL_PASSWORD`) against its own database. This matters because MariaDB's default `root` account refuses password logins — an older assumption that could produce a silent empty dump. If none of these can authenticate, the backup now **fails with a clear message** instead of writing an empty dump.

**MongoDB** authenticates the same way: DockBack tries the **root credentials** (`MONGO_INITDB_ROOT_USERNAME`/`PASSWORD`, or Bitnami's `MONGODB_ROOT_USER`/`PASSWORD`), then a **no-auth** connection (for a server with auth disabled), then the **application user** (`MONGODB_USERNAME`/`PASSWORD` against `MONGODB_DATABASE`/`MONGO_INITDB_DATABASE`). If none work, the backup **fails loudly** instead of writing a thin, unauthenticated archive, and the restore re-authenticates with the same root credentials so an auth-enabled Mongo restores cleanly. The server version is recorded in the manifest for restore-compatibility, the same as Postgres and MySQL.

## SQLite files inside a volume

Unlike the engines above, **SQLite** isn't a separate container — it's a file (or a few) sitting inside an app's own volume (Sonarr/Radarr, Miniflux, Vaultwarden, Paperless-SQLite and many others). A byte-for-byte copy of a live SQLite file can catch a half-written WAL/journal and produce a **corrupt** backup with no warning.

So when a captured volume/bind contains SQLite databases (detected by their `SQLite format 3` header, never by filename), DockBack takes a **consistent online snapshot** of each — the SQLite backup API, taken while the app keeps writing — and records it in the manifest (`sqlite_dumps`). The **source file is never touched**; the snapshot rides along in the archive under `sqlite/`. On restore, the consistent snapshot is laid back over the raw file and the stale `-wal`/`-shm` are removed, so the app opens a guaranteed-good database. The container detail page notes *"N SQLite database(s) — captured consistently"* based on your last backup.

The consistent snapshot uses `sqlite3` in the **volume sidecar** (see *Requirements → Settings → volume sidecar image*). The default sidecar image doesn't include `sqlite3`; when it's absent, DockBack safely falls back to capturing the raw file **together with its WAL** (still restorable — SQLite replays the WAL on open) and logs a note. For a guaranteed-consistent online snapshot across your fleet, point the sidecar at an image that includes `sqlite3`, or enable **pause/stop** on the container so the file is frozen during the copy.

## Requirements

- The node's socket-proxy (or transport) must allow **`EXEC`** and **`POST`** so the dump command can run. See *Connecting Servers → A remote Linux server* and *Security & Operations → Socket-proxy permissions*.
- The container must actually be a database (DockBack decides based on the image/env). If a database isn't detected, it's backed up as a normal app (files), which may not be consistent for that engine — open an issue/define a hook if you need dump behavior for an unusual image.
- **Verification** spins up a short-lived throwaway copy of the engine to test-restore the dump. On slow hardware (e.g. a NAS), a fresh database can take a few minutes to become ready; DockBack waits up to **5 minutes** by default. If you see *"throwaway db not ready"* on a very slow or heavily loaded host, raise the limit under **Settings → Performance & tuning → Database ready timeout** (applies immediately, no restart) — or set the `DOCKBACK_DB_READY_TIMEOUT` (seconds) environment default.

## Critical data (low-RPO protection)

A once-a-day dump can still lose up to a day of data. For a database where that's unacceptable, open the container and tick **Critical data (low-RPO protection)** and pick a target **RPO** (Recovery Point Objective) — 15 minutes, 1 hour, 6 hours, or 12 hours. DockBack then keeps a fresh **consistent, verified** dump within that window, so the worst-case data-loss window is the RPO you chose, not a full day.

This rides the **normal verified backup pipeline** — same consistent dump, same encryption, same automatic verification, same retention and offsite mirroring. It does **not** restart or reconfigure your database. The card shows your **newest verified recovery point** (only a backup that actually passed verification counts) and warns when it drifts outside the target; the `/metrics` endpoint exposes `dockback_rpo_breaches` so your existing monitoring can alert. The minimum RPO is **5 minutes** by default — a deliberate floor so frequent dumps can never overload the database they protect — but you can lower it (down to 1 minute) in *Settings → Performance & tuning* (**Critical-DB minimum RPO**) if your hardware can take it. The same card sets how often the low-RPO loop checks for due dumps (**Critical-DB check interval**).

If automatic low-RPO backups **keep failing verification** (e.g. the dump comes back empty), DockBack **pauses** them for that database after a few consecutive failures (**3** by default, configurable in *Settings → Performance & tuning*) instead of looping forever, alerts you once, and shows a paused notice on the card. Fix the underlying dump and run one backup manually — once it verifies, automatic protection **resumes on its own**.

> If a critical database isn't running, DockBack can't dump it — it logs a warning and the breach surfaces on `/metrics`. Keep critical databases running, or expect a gap.

### Why not continuous WAL archiving / PITR?

True point-in-time recovery (continuous WAL archiving with `pg_receivewal`/`archive_command`) would let you recover to *any* second, but it requires **restarting and persistently reconfiguring your database** (`wal_level`/`archive_mode` are restart-only settings), a **physical restore model** that can't be verified the way DockBack verifies every logical dump, and a long-lived replication connection DockBack's hardened, socket-proxy-only networking doesn't grant. DockBack is a backup tool that deliberately never reconfigures the apps it protects, so it delivers low RPO through frequent verified dumps instead.

### Point-in-time vs full-dump — the "PITR readiness" chip

So you always know your **real recovery granularity**, a running Postgres container's detail page shows a read-only **PITR readiness** chip, read live from the server with a settings-only query (`current_setting('wal_level')`/`archive_mode`/`max_wal_senders`) — it changes nothing and never restarts the database:

- **Green — "Ready"**: `wal_level` is `replica` (or `logical`) **and** `archive_mode` is `on`. Your server is configured such that external WAL-PITR tooling could attach for point-in-time recovery.
- **Amber — "Full-dump only"**: WAL archiving isn't enabled, with the exact one-line fix (`set wal_level=replica and archive_mode=on for PITR`).

Either way, remember that **DockBack's own recovery point is the last full dump**, not an arbitrary second — the chip reflects your *server's* PITR capability, which is separate from DockBack's frequent-verified-dump strategy above. The disaster-recovery **runbook** (*Recovery*) states this explicitly for every Postgres service, so the RPO limit is never a surprise mid-incident.

## Restoring

Databases are restored from the **dump**, never from raw files — the dump is imported over a local connection, guaranteeing healthy data. For a **full-cluster** dump the engine re-initializes fresh (its data directory is cleared) and the dump is imported. For a **per-database subset** (see *Backing up one database of a shared engine* above) the dump is imported into the running engine and replaces **only** those databases, leaving the rest of the server intact. See *Restoring → Volumes-only or database-only*.

## The dump integrity contract

Every database dump is fingerprinted **as it is captured**: what structure it declares (constraints for Postgres, tables for MySQL, collections for MongoDB, keys for Redis), whether it reached its completion marker, and a SHA-256 of the bytes exactly as written. All of it is recorded in the backup's manifest.

This matters because of a real failure. A Postgres dump that was genuinely complete — 246 constraints, correct trailer — restored as **27 of 72 primary keys and none of its 115 foreign keys**, and was reported a success. The rows were all there, so nothing looked wrong; the missing constraints only surfaced days later when an upgrade failed.

The contract closes that in three places:

**At capture.** A Postgres dump that starts but never reaches its completion marker is **not stored**. The backup fails immediately, while the source database is still there, with a message naming what was captured. Storing a dump that cannot restore is worse than failing loudly.

**In storage.** Routine verification re-hashes each stored dump and compares it with the checksum recorded at capture, as a `db-dump-integrity` check. Bit-rot, a truncated upload, or a tampered copy is caught by a scrub rather than at the moment you need to restore.

**At restore.** The recorded numbers are the contract the restored database is held to. Previously the expected values were re-derived from the dump as it streamed into the import — which meant a stored dump damaged after capture would be checked against *itself* and quietly pass. If the restore lands fewer than the dump declared, it fails loudly with both numbers.

Only a **shortfall** fails. Landing *more* is fine and expected — a target legitimately holds other databases in the same server, and a Redis may have taken new keys since the snapshot — so a restore that genuinely worked is never failed by this. A check that cannot be run at all (client missing, credentials refused) logs a warning naming the gap rather than either passing silently or failing a good restore.

You can see the recorded contract on any backup: open it and look for **Database dump** in the Manifest panel.

### Every engine is checked, each in its own terms

A restore is held to a did-it-actually-land check for **all four** engines. What is checked differs because each dump format offers a different honest invariant:

| Engine | What the dump declares | What the restore is asked |
|---|---|---|
| PostgreSQL | primary/foreign keys, completion marker | constraint counts across the cluster |
| MySQL / MariaDB | tables, completion marker | table count across user schemas |
| MongoDB | collection count (taken at capture) | collection count across user databases |
| Redis | key count (taken at capture) | key count across all databases |

MongoDB and Redis dump a **binary** stream that declares nothing readable, so their expected numbers are asked of the live database at capture time and carried in the manifest. Redis is worth calling out: it restores by writing an RDB file rather than through a client, so a snapshot that failed to load would otherwise leave an **empty but perfectly healthy** Redis and a restore reported as successful. The key count is what catches that.

MongoDB's import output is now read too: `mongorestore` exits 0 even when documents fail, so its own `Failed:` count is treated as the verdict.

### What is and isn't gated at CAPTURE

Only **PostgreSQL** capture is failed on a missing completion marker — it is the engine whose dumps carry a marker that can be relied on across every image. A truncated MySQL dump is still caught, but at restore rather than at capture.

A dump that is not a `pg_dump` at all is never failed by this: the check requires having seen pg_dump's own opening banner, so a binary archive makes no claim either way.

Backups taken before this existed have no recorded contract and fall back to the previous behaviour — they are not retroactively distrusted.
