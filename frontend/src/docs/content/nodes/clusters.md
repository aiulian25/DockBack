# Clusters — grouping your servers

A **cluster** is a group of servers that belong together: *production*, *homelab*, *the office site*. It is a label with consequences — DockBack uses it to roll health and backup coverage up per group, and to apply a backup policy to every server in the group at once.

Clusters are optional. A fleet that never touches them keeps working exactly as it always has: every server lands in a cluster called `default`, and the grouping stays invisible until you have a second cluster.

## What a cluster is — and is not

A cluster **is** a grouping of servers, plus a backup policy that its servers inherit.

A cluster **is not** a container for your data. It does not own your servers, your containers, your volumes or your backups. Every one of those belongs to a server, and every backup belongs to your catalogue and its destinations. This distinction is the reason removing a cluster is safe — see [Removing a cluster](#removing-a-cluster) below.

## Adding a cluster

There are two ways, and they do the same thing.

**From Settings** — go to **Settings → Clusters → New cluster**. Give it a name, optionally a description ("customer-facing, change window Sun 02:00") and a colour. Creating a cluster changes nothing on its own: it starts empty, and no server moves until you assign one — expand the new cluster and use **Move servers here**.

**From the server form** — open any server's **Edit** (or **Connect New Node**) and type a new name into the **Cluster** field. The field is a picker: existing clusters are listed with their server counts, and typing a name that doesn't exist yet offers **Create "…"**. Saving the server creates the cluster and assigns the server in one step.

### Names are matched without case

`prod` and `Prod` are the **same cluster**. If you type a spelling that differs only in case from one that already exists, DockBack quietly uses the existing spelling rather than creating a second cluster. This is deliberate: a fleet silently split in two by a capital letter is a real failure, and it is exactly what the old free-text field allowed.

A name can be up to 64 characters and may contain spaces and non-English characters. It cannot contain `/` or `\`.

## Assigning servers to a cluster

> **A cluster has no address of its own.** You do not connect *to* a cluster by IP or SSH. You connect to each Docker host individually — **Dashboard → Connect New Node** — and a cluster then groups the hosts you have already connected. DockBack talks to each host's own Docker API; there is no cluster-level endpoint to point it at. If you are looking for Docker Swarm or Kubernetes support (connect to one manager, discover its nodes automatically), that is a different feature and DockBack does not do it today.

There are two ways to put a server in a cluster:

**From the cluster** — **Settings → Clusters →** expand a cluster. It lists **Servers in this cluster** and offers **Move servers here**, which shows every other server in your fleet with its current cluster. Tick the ones to move and confirm.

**From the server** — open a server's **Edit** form and pick its cluster from the dropdown.

Both do exactly the same thing, and both change only which cluster the server belongs to. A move never touches the server's transport, address, stored credentials, pinned host key or backups.

A server always belongs to exactly one cluster; leaving the field blank puts it in `default`.

## Seeing a cluster's servers and containers

- **Settings → Clusters** — expand a cluster to see its servers with live reachability.
- **Dashboard** — with more than one cluster, server cards are grouped under a per-cluster band. Each card shows that server's container counts; click through for its containers.
- **Servers** page — the cluster filter chips narrow the table to one cluster.
- **Search / command palette** — a node's cluster is a search keyword.

There is no single "all containers in this cluster" list. Containers belong to a server, and DockBack's container views are per server — a cluster narrows *which servers* you are looking at, not the container list itself.

## Renaming a cluster

**Settings → Clusters →** the pencil icon on a cluster row. Change the name and save.

A rename carries everything with it, in a single atomic step:

- every server in the cluster follows the new name,
- the cluster's **backup policy** moves with it.

Nothing is lost and nothing reverts to the global default. Renaming a cluster to fix a typo is safe.

You cannot rename a cluster onto a name another cluster already has — merge is not a rename, and DockBack refuses rather than guessing. To merge two clusters, edit the servers of one to point at the other, then remove the empty cluster.

## Removing a cluster

**Settings → Clusters →** the trash icon on a cluster row.

### What is *not* deleted

Removing a cluster **never deletes data**. Specifically, it does not:

- delete or disconnect any **server** — every server stays connected, with its credentials, its pinned host key and its history intact;
- touch any **container** or **volume** on those servers;
- delete a single **backup**, on local storage or on any destination — your whole catalogue and every offsite copy are untouched;
- affect **schedules**, **destinations**, **retention already applied**, or the **audit trail**.

There is no "delete a cluster and everything in it" operation in DockBack, because a cluster does not contain anything. If you want to remove a server and its backups, that is **Servers → Forget node**, which is a separate, explicitly destructive action.

### What *does* change

- The cluster disappears from the dashboard grouping and from the cluster filter.
- **The cluster's backup policy is removed.** Its servers stop inheriting from it and fall back to the **global** policy. A server or container with its own override keeps that override — only the cluster tier goes away.
- Its servers **move to a cluster you choose**.

That second point is the one worth pausing on. If a cluster's policy was keeping 30 generations and mirroring to two offsite destinations, and the global policy keeps 7 and is local-only, then removing the cluster changes those servers' effective policy — future backups follow the global rules, and a future prune applies the shorter retention. Set the same values on the destination cluster (or per server) **before** removing, if that matters to you.

### A cluster with servers in it

You cannot remove a cluster that still has servers without saying where they go. The remove dialog asks you to pick a destination cluster, and the move and the removal happen **together** — there is never a moment where a server points at a cluster that no longer exists.

If the cluster you are removing is your **only** cluster and it still has servers, DockBack refuses: create another cluster first. Servers always belong to a cluster.

Removing an **empty** cluster asks nothing extra — there is nothing to move.

## Cluster backup policy

This is why clusters are worth using. Each cluster can override the global backup policy for every server in it, under **Settings → Clusters →** expand a cluster.

You can override:

- **Destinations** — which offsite destinations this cluster's backups are mirrored to. Local is always written.
- **Retention** — generations, GFS (daily/weekly/monthly/yearly) and auto-prune.

Either group can be overridden independently: a cluster can change destinations while inheriting the global retention, or the reverse.

### Where a setting actually comes from

DockBack resolves a setting most-specific-first:

```
container  →  server  →  cluster  →  global
```

The first tier that overrides a group of settings wins for that group. So:

- A cluster keeping 30 generations applies to all its servers…
- …unless a **server** in it sets its own retention, which wins…
- …unless a **container** on that server sets its own, which wins over everything.

A tier that overrides only destinations leaves retention alone, and vice versa — the two groups are resolved independently at every level.

Nothing changes for an existing setup: a cluster with no override is skipped entirely, and every server resolves to exactly the policy it did before clusters existed.

## The dashboard rollup

With more than one cluster, the **Dashboard** groups server cards under a per-cluster band showing:

- **Reachable** — how many of the cluster's servers are up.
- **Protected** — running containers with a backup or schedule coverage, out of the total running.
- **Oldest backup** — the age of the **stalest** server's newest backup, i.e. the weakest link in that group. If any server has never been backed up at all, the band shows that count instead, because "never" is not an age.

Selecting a single cluster from the filter chips keeps its band visible above the filtered grid.

The rollup costs nothing extra: it is computed in your browser from data the dashboard already loads. Adding clusters does not add requests, queries or page-load time.

Cluster colours are identity only, and they are shown as a **thin rule under the cluster's name** rather than as a coloured dot or badge. The palette is deliberately muted and kept clear of the green/amber/red DockBack uses for health, so a cluster's colour can never be misread as a status.

## Clusters in Prometheus metrics

Per-server metrics carry a `cluster` label:

```
dockback_node_reachable{node="Razer",node_id="a1b2c3",cluster="production"} 1
dockback_node_last_successful_backup_age_seconds{node="NUC",node_id="d4e5f6",cluster="homelab"} 10800
```

This is an **added label on the existing series**, not a new metric, so a dashboard that already sums these keeps working and gains a free `by (cluster)` split.

## Upgrading from an earlier version

Nothing to do. On first start after the upgrade, DockBack adopts the cluster names your servers were already using — if every server was in `default`, you get one cluster called `default` and the UI looks unchanged. Whatever was typed before is kept exactly as it was, even a name the new rules would reject.
