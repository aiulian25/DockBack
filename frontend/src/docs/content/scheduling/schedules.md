# Scheduled (automatic) backups

Automatic backups run on **named schedules**. Configure them in **Settings → Schedules**. You can define as many as you like — for example *"Databases hourly"*, *"Media weekly"*, or *"NUC nightly at 02:00"* and *"Synology nightly at 04:00"* — each with its own frequency, time, and targets, instead of forcing the whole fleet into one window.

## Setting it up

1. **Add schedule** — click to create a new schedule.
2. **Schedule name** — give it a memorable name (e.g. `Databases hourly`).
3. **Enabled** — toggle this schedule on.
4. **Frequency** — choose **Daily**, **Weekly**, **Monthly**, or **Custom** (a standard 5-field cron expression for full control).
5. **Time** — the time of day the run fires (e.g. `03:00`).
6. **What to back up** — pick whole nodes (all running containers), or expand a node to select specific containers or a whole **compose stack** (see below).
7. **Save.**

Each schedule shows its own **Next run** time, and (once it has run) its **Last run** time so you can confirm it is healthy. Every schedule is evaluated independently on each tick, so they fire at their own times without affecting one another.

## Protecting a whole stack in one click

On a node's page, any stack with services still unbacked-up shows **Protect**. One click does what the rest of this section describes, without the form: it adds **one** target for the whole project — app-consistent whenever there is more than one service — pins each database member to a live dump, and starts a first app-consistent backup immediately.

If some of that stack's services were already on the schedule as individual container targets, those are **replaced** by the single stack target, and the result says how many. That is deliberate: leaving them would back every member up twice on every run. Targets for containers outside the stack, and for the same names on other nodes, are untouched. Running Protect again changes nothing, and a node already covered by a **whole-node** target gains no stack target either — it is already backing up every member, and a second target would double every capture.

## Scheduling a whole stack

Expand a node in the target picker and you'll see **Stacks on this node** — each compose project can be selected as a single target. On every run it backs up **every current member** of that project, so services added to the stack later are picked up automatically (no need to edit the schedule).

Tick **App-consistent (quiesce during capture)** on a selected stack to schedule the coherent point-in-time snapshot that was previously only available from the manual *Backup Stack* dialog: the whole project is captured under **one quiesce window** (databases dumped and volumes copied together), so a restore returns the app **and** its database to the same instant. This is the capture mode that matters most for tightly-coupled app+DB stacks — and now it can run unattended on your schedule. It runs as a single stack-exclusive operation (like a stack restore), so it won't overlap another backup or restore of the same stack; the trade-off is brief app downtime during capture. Leave the checkbox off for the normal concurrent per-service backup.

A stack target honors the schedule's **destination override** and per-node policy just like any other target, and — like a removed container — a stack that no longer has any members is flagged so you can remove it (and is auto-cleaned after the grace period).

> Upgrading from an earlier version? Your existing single schedule appears automatically as a schedule named **Default** — nothing to reconfigure.

## How it behaves

- By default all schedules share the same **policy default destinations** and **retention** settings (see *The global backup policy*) — a schedule chooses *what* to back up and *when*. Retention stays global; **destinations can be overridden per schedule** (below).
- **Per-schedule destinations.** Each schedule has a **Destinations for this schedule** control. Left on *Use policy default*, its runs go to the effective policy destinations (unchanged). Turn on **Override where this schedule's runs go** to pick a per-schedule set — so a fast **nightly** can stay **local-only** while a **weekly** pushes **offsite**, without touching the global default or any per-container policy. **Local is always written;** unchecking every external destination makes that schedule's runs local-only (note: those backups then aren't 3-2-1 protected). The override applies to both scheduled runs and this schedule's **Backup Now**.
- The scheduler fires when the next scheduled time has passed; it **does not backfill** missed windows when you first *enable* it (so turning it on doesn't trigger an immediate flood).
- **Missed-run catch-up.** If DockBack was **down** when a scheduled window was due, it runs that backup **once** as soon as it's back up (each schedule's last-run time is persisted in its database). It catches up a single run per schedule, not every window you missed during a long outage — so a week of downtime won't unleash seven backups at once. A catch-up run is logged ("Missed scheduled window… running catch-up") naming the schedule.
- A custom cron is validated when you save, so an invalid expression is rejected with a clear message.

## At fleet scale (many nodes / thousands of containers)

A nightly window is drained through a **priority queue** so it scales without overwhelming your network, storage, or the Docker daemons:

- **Concurrency caps.** At most `DOCKBACK_MAX_CONCURRENT_BACKUPS` run at once across the whole fleet (default 3), and at most `DOCKBACK_MAX_CONCURRENT_PER_NODE` on any single node (default 2), so one busy node can't starve the rest. The remaining backups wait in the queue and start as slots free up — the scheduler never spawns thousands of jobs at once.
- **Stagger / jitter.** Scheduled backups start at a random offset within `DOCKBACK_SCHEDULE_JITTER` seconds (default 30) instead of all at the same instant, so a 20-node window doesn't thundering-herd shared storage. Set it to 0 to disable.
- **Interactive priority.** A manual **Backup Now** (or a stack action) jumps **ahead** of queued scheduled work and is never jittered, so you're never stuck behind a running nightly window.
- **Node-aware dispatch.** If a node is already at its per-node cap, the queue skips it and starts an eligible backup on a different node instead of wasting a global slot.
- **Bandwidth cap.** `DOCKBACK_MAX_UPLOAD_MBPS` limits the **total** offsite upload rate across all concurrent backups (megabits/sec, 0 = unlimited), so a nightly window can't saturate your uplink. Only offsite copies are throttled — the local archive write is always full-speed.
- **Resumable on restart.** Queued backups are persisted, so if DockBack restarts mid-window the jobs that hadn't finished are re-queued and run when it comes back (you'll see a "Resumed N queued backup(s)" log line) — a restart no longer silently drops a nightly window.

The number of backups currently waiting for a slot is exposed as `dockback_backups_queued` on the metrics endpoint.

## Backup Now vs schedule

Use **Backup Now** (manual multi-select, or a container's detail page) to get an **immediate** baseline. Use the schedule to keep everything current automatically afterward. The two work together.

> A restart does **not** trigger a backup. Schedules fire only at their configured times.

## Back up before changes (event-triggered)

Separately from any schedule, you can have a container snapshot itself **right before it changes**. On a container's detail page, tick **Back up before changes**. DockBack already watches each node's Docker event stream; with this on, when that container is about to be **recreated, updated, or removed** — a `docker compose up -d` that replaces it, a `docker rm -f`, or an image **pull** by an auto-updater like Watchtower — DockBack first enqueues an immediate, high-priority snapshot **labeled `auto: pre-change`**, so a bad update always leaves a fresh, verified rollback point from the state *just before* the change.

- It fires at the **stop / pull** moment, while the container and its named volumes still exist — not after it's already gone.
- A burst of events from a single recreate (kill → die → destroy) is **coalesced into one** snapshot by a short cooldown, so you get one pre-change backup, not a pile.
- The snapshot uses the container's normal policy destinations and is fully verified like any other backup. Find it on the **Backups** page by its `auto: pre-change` label (the search box matches labels).
- It's **opt-in per container** and off by default; untick it to stop.

## Define protection in your compose file (labels)

Prefer to declare backup intent as code instead of clicking? A container can opt into protection with `dockback.*` labels in its compose file, and DockBack applies them automatically on discovery — enable/disable, schedule membership, retention, pause mode, and mount exclusions. See **Scheduling & Retention → Label-driven policy (dockback.* labels)** in the sidebar.
