# Dashboard tour

The **Dashboard** is the at-a-glance view of your whole fleet.

## Node cards

Each connected server appears as a card showing, at a glance:

- A **container distribution** hero — the total container count, a health chip ("All containers healthy" or "N stopped"), and a colored bar + legend breaking the total into running / stopped / paused / restarting.
- **CPU** and **Memory** ring gauges (live load and used memory).
- An **inventory** grid: images, stacks (with a running/stopped/errored breakdown), volumes, and networks.
- An **events** footer — a real running count of container/stack/volume/image/network activity DockBack has observed on that node (today, and total since it started watching). This is counted live from the node's Docker event stream, not the daemon's capped in-memory log.

A green/red Wi-Fi indicator shows whether the node is currently reachable, and a status dot turns amber if any containers are unhealthy. Stats refresh in the background so the page stays responsive across many servers. The header shows an **All systems nominal** badge (or how many nodes are reachable) and a **Refresh** button.

Click a node to drill into its **container list**. From there, click any container to open its **detail page**, where you back it up and view its backup history.

## Backup coverage at a glance

Each node card flags containers that could be lost:

- **Unprotected** (amber `N/total`) — **running** containers with no successful backup that aren't covered by the schedule. This is the number to drive to zero.
- **Stopped, no backup** (a quieter, muted pill) — **stopped** containers that still hold a **named data volume** and have no backup. A stopped container is usually intentionally off, so it's kept out of the "unprotected running" count and never nags — but an app you run occasionally, or one that crashed, is exactly what you can lose. Containers with only throwaway mounts (tmpfs or anonymous volumes) are not counted.

Open the node to protect either kind: the node page's coverage banner lists them with a one-click **Protect** (smart defaults + a first backup), and a **Protect all (N)** that clears the whole list in one action.

**Protect all counts actions, not containers.** A compose project is protected **once, as a stack** — one app-consistent schedule target for the whole project — rather than service by service, so a six-service stack is one of the N, not six. That is not just tidier: six separate container targets capture six separate moments, and an application whose database and files must agree cannot be restored from that. Standalone containers are protected individually as before. Anything that fails is named in the result; it is never reported as a clean sweep.

## The sidebar

- **Dashboard** — fleet overview.
- **Servers** — connect and manage nodes.
- **Backups** — browse backups grouped by server; restore, download, or delete.
- **Logs** — live, streamed activity.
- **Audit Trail** — a record of every administrative action.
- **Settings** — backup policy, schedule, encryption, and external destinations.
- **Docs** — these guides.
- **Backup Now** — opens your **favorite backup targets** and fires any of them, or all of them, in one click.

### Favorite backups

**Backup Now → Edit** lets you tick the stacks and containers you back up most often. Each one runs with that target's own saved settings — the same archive its own page would produce — and the menu shows a live result per target with a link to the logs.

Two things about the list are worth knowing:

- **A container favorite follows the container's name, not its id.** A `docker compose up -d` gives a container a brand-new id; a favorite that remembered the id used to break silently at that moment and fail with a generic error. It now resolves the name to whatever id the container has at the time you press it, so redeploying changes nothing. Favorites saved before this are rewritten automatically the first time you open the menu; one whose container can no longer be found is **shown in the editor** rather than dropped, since only you can tell a renamed container from an offline node.
- **The list lives on the server, not in the browser.** Set it up on your laptop and it is there on your desktop. It holds nothing but names — no credentials, no policy — so it is stored plainly alongside your other preferences.

## Clusters

Group related servers into failure domains (e.g. `production`, `homelab`, `synology`). A server's cluster is picked on its **Edit** form; clusters themselves are created, renamed, coloured and removed under **Settings → Clusters**.

Once you have more than one cluster, a filter bar appears on the **Dashboard** and **Servers** pages: pick a cluster to narrow the view, or leave it on **All**. On the Dashboard, "All" groups the node cards under a per-cluster **rollup band** showing how many of that cluster's servers are reachable, how many of its running containers are protected, and the age of its **stalest** server's newest backup — the weakest link in that group. With a single cluster (the default) the bar and bands stay hidden, so nothing changes for small setups.

Each cluster can also carry its own **backup policy** (destinations and retention), inherited by every server in it. See **Connecting Servers → Clusters** for the full picture, including exactly what removing a cluster does and does not delete.

## Themes

Pick a theme under **Settings → Appearance**: **Midnight** (the default deep blue-black with ring gauges), **Classic** (the original high-contrast navy), or **Light** (for bright environments). The change applies instantly and is remembered in your browser.

## Alerts inbox

The **bell** in the top bar shows the number of **unacknowledged** warnings and criticals. DockBack now *persists* every alert-worthy event — a missed offsite copy, a destination filling up, a failed verification, the master key not confirmed backed up — so a warning is never lost, even when you have **no notification channel configured** or the app restarts.

Click the bell (or open **Logs → Alerts**) to see them. Each row shows a severity chip, the title and message, and how long ago it fired, with an **Acknowledge** button (or **Acknowledge all**). Filter by severity or show only unacknowledged. A recurring condition that you've acknowledged **resurfaces** if it's still happening — so clearing the inbox never hides an ongoing problem — but it won't pile up duplicate rows while it stays unacknowledged.

The inbox complements your notification channels (Gotify, email, webhook, heartbeat): channels push alerts *out*, the inbox keeps a durable record *in* the app. Only warnings and criticals are stored; routine successes stay in the daily digest.

### Activity history that survives restarts

The **Logs → Live log** tab streams events as they happen. Its **Activity** selector also lets you read the persisted history of the fleet-wide sources — **Scheduler**, **Queue**, **Critical**, **Notifications** — so you can answer "what happened last night" even after a restart wrapped the live buffer.

## Carrying the node name

A container's identity always carries the **node name you gave it**, because the same container name can exist on multiple machines. Whenever you back up or restore, the server it belongs to is shown alongside it so there's never ambiguity about *which* copy you're acting on.
