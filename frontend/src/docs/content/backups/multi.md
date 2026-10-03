# Back up many containers at once

You don't have to back up containers one by one.

## From a node's container list

Open a server to see all its containers on a single page. Select multiple containers with their checkboxes, then use **Backup Now** to start a backup of every selected container with your current policy destinations. You can **deselect** containers at any time, including after an initial selection.

This is the fastest way to get an immediate baseline of a whole server before you rely on the schedule.

## Full Server Backup & Backup Stack

On a server's detail page, **Full Server Backup** (header) backs up every running container, and **Backup Stack** opens that compose project's own page — services, their mounts, its snapshots, its live console, and the run controls. Both let you choose where that run's copies go instead of always using the policy default:

- **Local is always kept** — it's listed but can't be unticked.
- External destinations (Synology / Nextcloud / S3 / B2) are **opt-in per run**, each shown with a reachability dot and free space.
- The picker is **pre-selected** from your global policy destinations, so confirming without changes matches your usual setup — uncheck any you want to skip for this run.
- **Include stopped containers** is offered on the full-server run and is off by default, mirroring the same choice on a schedule: a stopped container is usually off on purpose, but an app you run occasionally still holds data you would miss.

**A full-server run captures what the schedule would.** Every container is backed up with its **own remembered options** — compression, app-native export, save-image, mount selection, pause mode — exactly as a scheduled run of that container resolves them, rather than a single blanket setting for the whole node. A host folder that two services of the same compose project both mount is captured **once**, by one of them, for that run only; neither service's saved mount selection is changed. A container that already has a backup queued or running is not queued a second time, so pressing the button twice costs nothing — the result says how many started and how many were already under way.

The run is queued, not fired all at once: it obeys the same fleet-wide and per-node concurrency limits as everything else, so a forty-container node does not overwhelm itself. You can leave the page; the run continues on the server.

**From the dashboard**, a node card's manage menu has **Back up node now** for the same thing without opening the node first — the action you want before touching a host.

## Stack runs use each service's saved backup options

**Backup Stack** produces, for every service, the same archive a manual **Run Manual Backup** on that container would: its remembered **compression**, **"Also save the container image"**, **app-native export**, incremental setting, mount selection, pause mode, and hooks all apply — exactly as they do for scheduled runs. A service that never had options saved uses the defaults (balanced compression, image/export off).

The stack page offers one per-run override: a **Compression** selector, defaulting to **"Each service's saved setting"**. Pick a specific mode to force it for every service in this run only — the saved per-container settings are not changed.

**The stack has a page of its own.** Reachable at *Servers → node → Stacks → Backup Stack*, it shows every service with its remembered options and its mounts (editable inline — the same settings that container's own page writes), the project's app-consistent snapshots, and a console that follows the run. You can leave it; the backup carries on and the console re-attaches when you come back.

## Choosing what each service captures

Every service row shows how many of its mounts are selected and how much data that adds up to — for example **3 of 5 · 12.4 GB**. Click it to open that service's mounts and tick exactly what you want, without leaving the dialog:

- Each mount shows its **path, type and measured size**, plus the same badges as the container's own page — `covered` for a folder another service already captures, `permission` where the reader can't read every file, and the filesystem name where the volume supports snapshots.
- A **database's data directory** is captured by its dump, not by copying files, so it stays unticked with that reason shown. Copying a live database's files can produce a torn backup, which is the whole reason the dump exists.
- A **large bind** (a media library, say) is skipped by default and says so. Tick it if you really want it inside the archive.

At the bottom, **This run will capture** totals the selected mounts across the whole project, so the effect of including a big folder is visible before you start rather than after.

> **Ticking a mount saves immediately.** It writes the same setting the container's own page writes, so scheduled runs and manual runs pick it up too — and that page will already agree when you open it. Cancelling the dialog does not undo a mount change.

Sizes are measured in the background, a few services at a time, because measuring a large folder can take a while. The dialog opens straight away and rows fill in as their measurements arrive; you never have to wait for them to start a backup. Stacks with one or two services open with their mounts already showing; larger ones start collapsed so the dialog stays manageable.

## App-consistent stack snapshot

By default **Backup Stack** captures each service independently and in parallel — fast, and each database is dumped live with its native tools. For most stacks that is exactly right. But when an app and its database must be captured at the *same* instant — an app that writes files and rows that have to agree — a parallel run can catch them a few seconds apart.

Tick **App-consistent snapshot (quiesce during capture)** in the stack picker to capture the whole project as one coherent point-in-time instead:

- The stack's **app services are quiesced** (paused, or stopped if you choose that mode) and, while they're frozen, every service's **database dump and volumes are captured together**.
- **Databases are still dumped live** and never paused — they must be running.
- The apps are brought back up **as soon as the data is captured**; the slower compress/encrypt/store/verify work runs afterward, so downtime is just the copy, not the whole run.
- Every service's backup is tagged with a shared **consistency group** so you can see they belong to the same snapshot.

The trade-off is **brief app downtime** during capture — that's the cost of a guaranteed-coherent snapshot. Leave it **off** for the normal concurrent behavior, and prefer it for tightly-coupled app+database stacks where a torn point-in-time would matter. Because it coordinates the whole project, it runs as one operation and won't overlap a stack restore.

### Restoring a snapshot group

By default **Restore Stack** rebuilds each service from its *latest* backup — which, if services were also backed up individually at different times, can mix members of different points in time (e.g. Tuesday's database with Wednesday's app volume). When a stack has app-consistent snapshots, **Restore Stack** first asks **which point in time** to restore:

- **Latest backup of each service** — the previous behavior; fastest to the newest data, but may span different capture times.
- **App-consistent snapshot — captured together at *{time}*** — restores every service from that one quiesce window, so the whole stack returns to a single coherent moment. Only snapshots that cover **every** service in the stack are offered.

The engine restores the chosen group's members in the usual dependency order (databases first). If a snapshot is missing a service the stack has, that restore is refused with a clear error rather than silently falling back to a different point in time. Stacks that were never captured with an app-consistent snapshot skip the prompt entirely and restore exactly as before.

## Whole-node / scheduled selection

For recurring coverage, use the **schedule** instead of manual multi-select. In **Settings → Scheduled Backups** you pick whole nodes (all running containers) or expand a node to choose specific containers, and the schedule backs them all up automatically on your chosen cadence. See *Scheduling & Retention → Scheduled (automatic) backups*.

## Tips

- Manual multi-select is best for a **starting point ASAP**; the schedule keeps it current afterwards.
- Bulk actions default to your **policy destinations** but let you change targets per run; **Full Server Backup** and **Backup Stack** always keep a Local copy and let you add/remove offsite targets before starting. Set your defaults up first (*Scheduling & Retention → The global backup policy*).
