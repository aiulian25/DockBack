# Run a manual backup (every option)

Open a container's **detail page** (Dashboard → node → container, or Servers → node → container). The **Run Manual Backup** panel on the right has every option.

## Options

**Compression** — pick the algorithm/level per backup (the choice is recorded in the manifest, so any backup stays restorable):
- **Fast (zstd)** — quickest, larger archives.
- **Balanced (zstd)** — the default; best speed/ratio balance.
- **Max (zstd)** — smaller archives, more CPU.
- **Max + long-range (zstd)** — adds **long-range matching** (a 32&nbsp;MiB match window) so zstd can deduplicate repeats that sit far apart in a large archive — repeated or near-identical files, big database dumps, log piles. Best for large, internally-redundant payloads (think a sizable Nextcloud or a multi-database container). It costs more CPU and memory and runs single-threaded, so reserve it for the big ones; small backups won't benefit. The archive stays a single self-contained, verifiable file and restores with no special handling.
- **gzip** — universal compatibility (any tool can open it); larger than zstd.
- **xz** — the smallest archive at a high, single-threaded CPU cost; for cold archival.

zstd is the recommended default. Existing backups (made before this choice existed) are zstd and restore unchanged.

**Automatic fast mode for incompressible data.** DockBack learns each container's *real* compression ratio from its past runs. When a selection of **1 GiB or more** has a learned ratio of **0.97 or higher** — photo, video, audiobook, and ebook libraries are already compressed, so re-compressing them burns CPU for nothing — a run that would use the default **Balanced** mode is executed as **Fast** instead, logged as such, with roughly the same archive size at a fraction of the CPU time. This never overrides a compression mode you chose explicitly (a saved non-default on the container, or a per-run choice in the stack dialog), and it can be turned off entirely in **Settings → Advanced → Performance & tuning**.

**Destinations**
A checklist of where copies are written. **Local** is always available; each external destination you've added (e.g. nas01, NextCloud-RO) appears with its type. Copies are written to **Local plus every checked destination** — pick more than one to follow the 3-2-1 rule and avoid a single point of failure. Each destination keeps the name you gave it, so you always know where a copy lives.

**Volumes / paths to back up**
A checklist of the container's mounts (named volumes and writable bind mounts) with their **sizes**. You choose exactly what to capture:

- **Named volumes** are selected by default (they hold app state).
- **Large bind mounts** (e.g. an *arr media library, downloads) are **skipped by default** and shown with a note — tick them only if you really want them in the backup. This prevents a backup from ballooning to terabytes or filling the disk.
- Small bind mounts (config/data dirs) are selected by default.
- **"Large" is configurable.** The cutoff defaults to **5 GiB** but you can change it globally in *Settings → Performance & tuning* (**Large bind-mount threshold**), and a **per-container override** in this picker wins over the global value — so a 3 GiB bind you *do* want, or an 8 GiB one you *don't*, is handled without re-ticking on every new container. Changing the cutoff re-evaluates the default selection immediately; named volumes are always included regardless.
- **Permission warning.** A bind mount whose files the backup reader can't fully access (a UID/GID mismatch, or an NFS share exported with `root_squash`) is flagged with a **permission** badge. Such files would be **missing** from the backup, so DockBack never skips them silently — fix the host ownership to match the container's user, then re-check. The same warning is logged during every backup run.
- **Backing-filesystem badge.** Each mount shows the filesystem its data sits on (e.g. `ext2/ext3`, `xfs`, `zfs`, `btrfs`). Volumes on **ZFS or btrfs** — which support atomic point-in-time snapshots — are highlighted as snapshot-capable. This is **informational** today: DockBack still captures every volume consistently through its unprivileged sidecar (plus pause/quiesce and consistent DB dumps below). Taking a true host-level filesystem snapshot would require privileges the hardened, socket-proxy-only app deliberately does not have, so it is not performed automatically.

Your selection is **remembered per container**, so scheduled backups capture the same set.

## Pre-flight checks

Before any expensive or destructive work, DockBack runs pre-flight checks and **fails fast with a clear message** rather than partway through:

- **Encryption key present** — refuses to start if no master key is configured (it would produce unencryptable data).
- **Storage reachable & writable** — a tiny write/delete probe confirms the backup target accepts writes.
- **Free disk space** — estimates the data size and checks both the scratch/work filesystem (which holds the raw, uncompressed spool) and the destination (which holds the **estimated compressed** archive, sized by your compression preset) have room, refusing up front rather than filling the disk mid-archive.
- **Database tools present** — for a database container, confirms the dump tool exists; if not, it **warns** and falls back to a file backup rather than failing.

**Target volume / path**
The local storage location for the archive (defaults to the app's backup volume, e.g. `local:/app/backups`). Use the edit control to change it.

**Backup options**
- **Consistency during volume backup** — how the app is quiesced while its volumes are copied. Choose one; the choice is **remembered per container**, so scheduled and bulk backups honor it too:
  - *Pause during copy* — **the default**. The container is frozen (`docker pause`) only while the tar runs, then unpaused. Near-zero downtime and a consistent snapshot — the right choice for almost every stateful app (including apps backed by an embedded **SQLite** file, where freezing the app is exactly what makes the copy consistent).
  - *Live copy (no pause)* — fastest, but a busy app may produce an inconsistent snapshot. Pick this only when even a brief freeze is unacceptable and the app tolerates live copies.
  - *Stop during copy* — the container is stopped, copied, then restarted. Brief downtime, maximum consistency.
  - **Databases are never paused or stopped** — a detected database server (PostgreSQL, MySQL/MariaDB, MongoDB) is dumped **live** with native tools and must stay running, so it is automatically excluded from pausing.
- **Notify on completion** — send a notification when the run finishes (shown as *not configured* until notifications are set up).

**Advanced — backup hooks**
Expand to define application-aware **pre/post hooks** (e.g. put an app into maintenance mode for a consistent snapshot). See *Quiesce hooks*.

## Start it

Click **Initiate Backup Now**. The button immediately switches to *Backup queued…* and stays disabled until the run is visible, then shows *Backup in progress…* — one click is always enough. If the run finishes before you can see it (small containers back up in under a second), a toast confirms the completion instead. Repeat clicks are harmless: while a backup of the container is already queued or running, the app reuses that run rather than starting another.

The run appears in **Available Backups** as *In Progress*, with live output in the console below. When it finishes it becomes **Success** and then **Verified** after the automatic verification pass. You can **Schedule Future Backup** from the same panel to hand off to the global schedule (see *Scheduling & Retention*).

## Watching progress

The **Console Output** streams the engine's log for the run (starting, dumping, compressing, encrypting, storing, mirroring, verifying). The **Available Backups** table shows each backup's status, location badges, size, and retention state.
