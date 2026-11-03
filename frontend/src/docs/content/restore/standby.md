# Pilot-light standby (prove failover to another node)

A backup that verifies and even drills clean still leaves one question unanswered: **if this host dies, does the app actually come up on another machine?** Restore drills prove *archive → sandbox on the same node*. **Standby rehearsal** proves *archive → a different node → healthy*, continuously — the thing homelab disaster recovery actually hinges on.

## What it does

Turn it on per container (**Standby rehearsal** on the container's page) and pick a **fallback node** and a **cadence** (weekly or monthly). On that schedule DockBack:

1. Takes the container's **newest verified backup**.
2. Restores it as an **isolated clone** (`<name>-standby-<random>`) onto the chosen node — fresh empty volumes, **no published ports**, **no network** and **no host powers** — so it can never clash with or read anything running there.
3. Waits for the clone to come up (the same health gate a real restore uses).
4. Records **pass/fail + boot time**.
5. **Tears the clone down** — the exact container this rehearsal created and its throwaway volumes — leaving zero residue. Nothing else on the node is removed, whatever it is called. If DockBack itself stops mid-rehearsal, the clone's expiry label gets it swept within a day.

The **Recovery** page then shows a per-service standby status: *proven* on its fallback node (and how fresh that proof is), *failing*, or *not configured*. A configured-but-failing standby also adds a warning line to the runbook and raises an alert at drill severity — a silent failover gap is exactly what you don't want to discover mid-incident.

Standby readiness is surfaced everywhere the rest of your DR proof already is:

- **Prometheus**: `dockback_standby_configured`, `dockback_standby_failing`, and `dockback_oldest_standby_age_seconds` (age of the least-recently rehearsed standby; -1 until one has run) let an external monitor alert on a failing or stale standby.
- **Daily digest**: when any standbys are configured, the DR-confidence line appends "Standby: N of M proven."
- **Backups page**: a backup whose target has a failing standby mentions it in the confidence tooltip. The grade itself is untouched — the backup's contents are fine; it's the fallback path that needs attention.

Use **Rehearse now** on the container's page to run one immediately instead of waiting for the schedule.

## What "healthy" means here

The clone runs **isolated**: no network, no devices or GPU, no capabilities beyond Docker's defaults. It can't reach its dependencies, and an app that needs hardware or the internet to start will not pass here, whatever state its backup is in. A **self-contained** service (a database, or an app whose state is entirely in its own volumes) will come up healthy and prove out cleanly. A **multi-service app** that needs its database to become healthy may not reach "healthy" as a lone clone — the rehearsal records that honestly with an explanatory detail rather than a false pass. Standby rehearsal is therefore most valuable for databases and self-contained services; for a full multi-container app, pair it with the one-click stack restore story.

## Load & safety

- **A full restore crosses the network each rehearsal.** The whole data volume is pulled to the fallback node every time, so keep the cadence **weekly** (the default) and, ideally, **off-hours**.
- **Bounded, never a spike.** DockBack rehearses **one** container at a time across the whole fleet, on an hourly check that picks the most-overdue one — so rehearsals never pile up.
- **Never collides with real work.** A rehearsal takes the same **restore lock** as a real restore (on the throwaway clone's name), so it can't overlap a backup or restore; if the target is busy it simply waits for the next cycle.
- **No residue.** The clone is always removed afterward — on success and on failure — including its throwaway volumes.

## Security

The clone is isolated, with no network and no host powers, so it can't shadow the live stack, expose anything on the fallback node, or use the original's credentials to reach anything. Configuring, removing, or triggering a rehearsal are authenticated, CSRF-protected, and written to the audit trail. The rehearsal reads the same encrypted backup a normal restore does — nothing new is stored in the clear.
