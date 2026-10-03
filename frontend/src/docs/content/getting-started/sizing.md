# Requirements, sizing & performance

DockBack is deliberately small. This page covers what it needs, how it behaves under load, and how to tune it for maximum efficiency on your hardware.

## Footprint at a glance

- **Image:** a single ~21 MB distroless binary (no shell, no package manager).
- **Idle:** roughly **18 MB RAM total** (the app plus its socket-proxy sidecar) and near‑zero CPU — it's just watching for changes and waiting for the schedule.

## What uses resources during a backup

A backup is **orchestrate → compress → encrypt → store**. Knowing where the cost goes makes tuning obvious:

- **CPU** — the main consumer is **compression** (zstd). It scales with how many backups run at once and which preset you choose. `Balanced` is light; `Max` and `Max + long-range` trade CPU (and some memory) for smaller archives.
- **RAM** — stays low and **bounded**. The pipeline is streaming, encryption is chunked, and large volume archives **spool to disk, not memory**, so even multi‑GB volumes don't blow up RAM. In practice a few hundred MB under load. Signing in or key operations cause a brief, small spike (password hashing).
- **Disk I/O** — usually the real bottleneck: reading the source volumes, writing the temporary work spool, and writing the compressed archive.
- **Network** — only when mirroring to offsite destinations; you can cap it (below).
- **Remote nodes carry their own weight.** The heavy volume reads and database dumps run in short‑lived sidecars **on the node being backed up** (databases are dumped inside their own container). So backing up a remote fleet does **not** load the DockBack host.

## Recommended machine specs

| Tier | vCPU | RAM | Good for |
|---|---|---|---|
| Minimum | 1 | 512 MB – 1 GB | a few containers on a single host |
| **Recommended** | 2 | 2 GB | typical home lab / small fleet, no hiccups |
| Larger fleet | 4 | 4 GB | many nodes, big volumes, high concurrency |

A Raspberry Pi 4 (4 GB) or any small VPS runs it comfortably. The default container limits are 2 vCPU / 1 GiB for the app and 1 vCPU / 128 MiB for the socket-proxy — raise or lower them in your compose file to fit the host.

## Sizing your disk (the real driver)

RAM and CPU are modest; **disk is what you actually plan for**:

- **Backup storage** — sized to *your data × how many copies you keep* (retention). zstd usually compresses well, so this is smaller than the raw data.
- **Work scratch** (`DOCKBACK_WORK_DIR`, default `/app/backups/.work`) — needs room for the **largest single uncompressed payload** being processed at once (one volume or database dump), multiplied by how many backups run concurrently. Point it at a roomy, fast disk (SSD) if you back up large volumes:

```
DOCKBACK_WORK_DIR=/mnt/fast-disk/dockback-work
```

## Store backups on a bigger disk (a mounted SSD)

By default DockBack keeps local backups in a managed Docker volume (`dback-backups`), which lives on the host's system disk. To send **all** local backups to a bigger disk — say a **2 TB SSD** mounted at `/mnt/ssd` — point the container's `/app/backups` at a folder on that disk. The bundled `docker-compose.yml` makes this a one-line change in your `.env`.

1. **Make a folder on the SSD and give it to DockBack's non-root user.** The container runs as UID **65532** with a read-only root filesystem, so the folder must be owned by that user or every write fails:

   ```
   sudo mkdir -p /mnt/ssd/dockback
   sudo chown -R 65532:65532 /mnt/ssd/dockback
   ```

2. **Set the host path in `.env`** (leave it blank to keep the managed volume):

   ```
   DOCKBACK_BACKUPS_DIR_HOST=/mnt/ssd/dockback
   ```

3. **Apply it:**

   ```
   docker compose up -d
   ```

That's it — every local backup (and, since the work spool defaults under it, the temporary spool too) now lands on the SSD. Confirm it: the first backup's log opens with `Destination free space: …` showing the SSD's capacity, and *Insights → Destinations* tracks the **Local backups volume** free space and fill-up forecast over time. The app also alerts when it's nearly full.

> **Only backups move — app data stays on the machine.** This redirects `/app/backups` **only**. DockBack's configuration, the SQLite catalog, and the sealed node/destination/notification secrets live in a separate managed volume (`dback-data`, mounted at `/app/data`) that always stays on the host's system disk — they are never written to the mounted path. So the SSD holds nothing but the (already-encrypted) backup archives; the encryption key and the catalog needed to make sense of them stay local. Nothing extra to set up for `dback-data`: it's a Docker-managed volume, created and correctly owned on first run.

> **Moving existing backups.** Switching this path changes *where new backups are written*; it does not copy your old ones. If you want to keep existing backups, copy them from the old volume to the SSD folder first — e.g. `docker run --rm -v dback-backups:/from -v /mnt/ssd/dockback:/to alpine cp -a /from/. /to/` (then `chown -R 65532:65532 /mnt/ssd/dockback` again) — before setting the variable.

> Prefer to keep backups where they are and just add *offsite* copies (Synology, Nextcloud, S3/B2)? Add those under *Settings → Destinations* instead — see *Destinations*.

## Tuning for maximum efficiency

Work down this list on a constrained host — each step trades a little of one resource for another:

1. **Compression preset (per container).** Keep **Balanced** as the default. Use **Max** or **Max + long-range** only for large, internally‑redundant payloads (big Nextcloud, many similar files) — they cost more CPU and memory. `Max + long-range` runs single‑threaded on purpose to keep memory predictable.
2. **Only back up state, not bulk.** In a container's backup panel, leave giant bind mounts (media libraries, downloads) **unticked** — they're skipped by default. Your selection is remembered for scheduled runs. This is the single biggest win for time and storage.
3. **Concurrency caps.** Lower these on a small host so backups run more sequentially. Set them in *Settings → Performance & tuning* (**Max concurrent backups** / **Max concurrent per node**) — they apply **immediately**, no restart, and a running backup is never interrupted — or as env defaults:
   - `DOCKBACK_MAX_CONCURRENT_BACKUPS` (default 3) — total at once across the fleet.
   - `DOCKBACK_MAX_CONCURRENT_PER_NODE` (default 2) — at once on any one node.
4. **Schedule off‑peak, and stagger it.** Run the schedule when the host is quiet. `DOCKBACK_SCHEDULE_JITTER` (seconds) spreads a fleet‑wide window so every backup doesn't start at the same instant.
5. **Cap upload bandwidth** so offsite mirroring never saturates your uplink:
   - `DOCKBACK_MAX_UPLOAD_MBPS` (0 = unlimited).
6. **Consistency mode.** *Live copy (no pause)* is cheapest and fine for most apps. Use *Pause* or *Stop* only where you need a perfectly clean snapshot — databases are always dumped consistently regardless.
7. **Low‑RPO / critical databases cost more.** Marking a database *critical* takes frequent verified dumps to shrink the data‑loss window — reserve it for data that truly can't lose a day, and pick the longest RPO you're comfortable with.
8. **Retention.** Keep only the copies you need — fewer generations means less storage and faster pruning.
9. **Container limits.** In your compose file, the app service's `mem_limit` and `cpus` are the ceilings; adjust them to leave headroom for the workloads the host also runs.

## Air-gapped / private-registry nodes

The short-lived sidecar that copies and measures volume data is a tiny **`alpine:3.20`** image, pulled on the node being backed up. On an air-gapped node — one that can only pull from a private mirror — that default pull fails, and with it **every volume backup**. Point the sidecar at your mirror instead:

- In the app: *Settings → Performance & tuning → **Volume sidecar image*** (e.g. `registry.internal/alpine:3.20`). Applies to the next backup, no restart.
- Or by environment: `DOCKBACK_SIDECAR_IMAGE=registry.internal/alpine:3.20` (an in-app value overrides the env).

The image only needs a shell and `tar`/`du`, so any small Alpine mirror works; pre-pull it on each air-gapped node so the first backup doesn't wait on a pull.

## Where to set these

- **Environment variables** (`DOCKBACK_*`, container `mem_limit`/`cpus`, `DOCKBACK_WORK_DIR`) live in your `.env` / `docker-compose` file and apply at startup.
- **Per‑backup and per‑container choices** (compression, which volumes, consistency mode, critical/low‑RPO) are set in the app: a container's backup panel and *Settings*.
- **Schedule, retention, destinations, and bandwidth** are in *Settings*.

## Giving DockBack more memory or CPU

Both are safe to raise. Container limits are **resource-exhaustion guardrails, not a security boundary** — the boundary is non-root, a read-only root filesystem, all capabilities dropped, `no-new-privileges`, the seccomp profile and the socket-proxy allow-list, and none of those change when you raise a limit.

Set them in your `.env`:

```
DOCKBACK_MEM_LIMIT=2g     # default 1g
#DOCKBACK_CPUS=4.0        # uncapped by default
```

then `docker compose up -d`.

**DockBack sizes itself to whatever you give it.** The Go runtime does not read cgroup limits on its own, so raising a container's memory would previously have been *permitted but unused* — worse, the garbage collector had no idea a ceiling existed and could grow the heap past it, letting the kernel kill DockBack mid-backup. At startup it now reads the cgroup and sets its heap target and thread count to match, and logs what it chose:

```
runtime: heap target 1740 MiB of 2048 MiB container memory
```

### Which one to raise

**CPU is the bigger lever for speed.** Compression is the bottleneck in a backup and parallelises well, so cores translate almost directly into throughput. In production CPU is uncapped by default — there is usually nothing to change.

**Memory matters for breadth, not size.** The backup spool is written to disk, so a 500 GB volume does not need 500 GB of RAM. What wants headroom is:

- the **volume file index** — a volume with millions of files builds a large in-memory index;
- **compression buffers** — one window per concurrent backup;
- **concurrent backups** — each one carries its own set of both.

So raise memory if you back up file-heavy volumes or run several backups at once. 1 GiB suits a small fleet; 2-4 GiB is a better fit once volumes get large.

### Also worth tuning

Concurrency is a separate lever, in **Settings → Advanced**: how many backups run at once fleet-wide, and how many per node. More concurrency uses more memory and more CPU, so raise it alongside the limits above rather than on its own.

Keep *some* limit in place. A ceiling is what stops a runaway backup taking the whole host down with it.
