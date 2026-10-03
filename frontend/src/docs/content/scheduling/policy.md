# The global backup policy

The **backup policy** is one place that governs *where* backups go and *how many* copies are kept. Find it in **Settings → Backup Policy**.

> **Per-cluster and per-node overrides (configure the fleet once).** The Settings policy is the **global default** for every node. A whole **cluster** can override **destinations** and/or **retention** for all its servers at once (**Settings → Clusters →** expand a cluster), and a single node can override them for just itself (**Servers → a node → Backup Policy — this node**) — everything else keeps inheriting. So you set sane defaults once and only adjust the exceptions (e.g. send one noisy node's backups to a bigger destination, or keep more generations for a critical host). A node with no override always tracks the global policy as it changes. Scheduled backups, auto-prune, and the retention preview all respect each node's effective policy.

## Settings

**Default destinations**
The destinations new and scheduled backups are written to by default. Check the ones you want (Local is always included). This is the default selection you'll see pre-ticked when running a manual backup, and it's what scheduled backups use.

**Copies to keep (per container)**
A simple retention count — how many of the newest backups to keep **per container**, regardless of age.

**GFS retention (daily / weekly / monthly / yearly)**
Grandfather-Father-Son retention: keep the **newest backup of each of the most recent N days, weeks, months, and years** (0 = off for that period). It combines with "copies to keep" — a backup is kept if *any* rule keeps it — and the **newest backup is always kept**. This lets you hold, say, 7 dailies + 4 weeklies + 12 monthlies + 3 yearlies without storing every run forever — so a tiny once-a-year archive survives long after the dailies age out.

**Dry-run preview before anything is deleted**
Click **Preview pruning (dry run)** to see exactly which backups the current policy would delete (per container, with the space freed) — *without* removing anything. Only then does **Prune now** apply it. Pruning removes copies from **every** location they live in (local and all destinations).

**Auto-prune after each backup**
When enabled, the same policy is enforced automatically after each successful, verified backup — so old copies don't pile up (locally or offsite). Leave it off if you prefer to prune manually after reviewing the preview.

**Prune on a schedule**
Auto-prune only runs *when a backup runs*, so a container that stops being backed up never reclaims its space. Enable **Prune on a schedule** to run the retention policy **fleet-wide** on its own cadence (e.g. weekly), independent of any backup — reclaiming space even from targets that aren't being actively backed up. It applies the exact same policy as **Prune now**, honors each node's effective policy, and never deletes a copy whose immutable (WORM) lock hasn't expired. A newly-enabled schedule fires at its next window (it doesn't prune the moment you save), and each run is recorded in the audit trail.

**Pin a backup (keep forever)**
Any single backup can be **pinned** from its row (the pin icon) or its detail drawer. A pinned backup is **exempt from all pruning** — auto-prune, scheduled prune, and "Prune now" all skip it — so a pre-upgrade snapshot survives no matter how the retention counts age out. Unpin it to let the policy manage it again. You can also give a backup a free-text **label** (e.g. `pre-Immich-upgrade`); labels show on the row and are matched by the Backups search box. Pinning never protects against a manual delete — it only exempts the backup from *automatic* retention.

**Automatic snapshots have their own budget**
DockBack takes **protective snapshots automatically** before a risky change — when a watched container is stopped, killed, or updated (a new image is pulled), and before a **standalone volume** is overwritten by a restore. These are labelled `auto:` and, without a separate rule, a flappy container, a busy update night, or a few repeated restores could produce enough of them to age your **scheduled** history out of the GFS buckets. So they get their **own** retention class: **Keep newest auto-snapshots** (Settings → Backup Policy, default **3**) keeps that many per target and prunes the rest **before** the GFS rules run — so automatic snapshots can never crowd out the scheduled generations you rely on. Set it to **0** to treat them like any other backup. Pinned auto-snapshots are always kept and don't count against the budget. The **retention preview** tags rows whose pruning includes automatic snapshots with an `auto` badge.

## How retention chooses what to keep

- Only **verified** backups count toward the kept set, so a failed/corrupt run never displaces a good one.
- Pruning is **per container** (node + container name), so each app keeps its own N most-recent copies.
- Each backup shows its retention state in the UI: **Keep (N gen)** for copies within the limit, **Prunable** for ones that will be removed.

Save with **Save policy**.

## Deleting backups by hand

Automatic pruning aside, you can remove specific backups yourself from **Backups → a node**. Tick the checkboxes on the rows you want gone (or the header checkbox to select the whole page) and click **Delete selected**; a single backup can also be deleted from its detail drawer. Deletion removes the encrypted archive from **every** location it lives in and is **permanent** — it asks for confirmation first, and each removal is recorded in the Audit Trail. A copy held on a destination under an **immutable / WORM object-lock** is left in place until its lock expires — that's the whole point of the lock — so it can survive an accidental delete.
