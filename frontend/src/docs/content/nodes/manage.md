# Edit, test, remove & troubleshoot

## Test Connection

Every Add/Edit Node form has a **Test Connection** button. It performs a real reachability check against the chosen transport and address before you save, so you catch credential or firewall problems immediately.

## Editing a node

Open the node and choose **Edit**. You can change the name, cluster, transport, and address. For SSH/mTLS nodes, **leave the credential fields blank to keep what's stored** — only fill them to replace the key/bundle/passphrase.

The **Cluster** field is a picker over your existing clusters; typing a name that doesn't exist yet offers to create it. See **Clusters — grouping your servers** for what a cluster does and how backup policy inherits through it.

## Removing a node

Removing a node disconnects DockBack from that server. Backups already taken from it remain in your catalog and on their destinations — removing the node does **not** delete its backups.

Removing a node is not the same as removing a **cluster**: removing a cluster only removes a grouping and never disconnects a server.

## Reachability indicator

Each node shows a green/red Wi-Fi indicator. Red means the last health check failed. DockBack re-checks in the background and reconnects automatically when the server comes back.

Each node keeps **one pooled, long-lived connection** with a bounded dial timeout and TCP keep-alive, so a slow or dead server fails fast instead of hanging — it never blocks the dashboard or other nodes' backups, which run concurrently and isolated per node. A node that goes offline is re-probed on an **automatic back-off** (it backs off from re-dialing every cycle up to a few minutes between attempts), so one dead host in a large fleet doesn't waste connection attempts; the first successful probe after it recovers flips the indicator back to green immediately. Clicking **Test Connection** always forces an immediate live attempt, bypassing the back-off.

## Connection history & uptime

The green/red indicator only shows the node's state *right now*. The node page also keeps a **Connection history** strip: every time a node flips reachable↔unreachable, DockBack records the transition (with a timestamp and the error), so a flaky NUC or a Wi-Fi node that drops overnight becomes visible as a **pattern**, not just a momentary red dot. The strip is a green/red timeline over the last 30 days, and the node's reliability is shown as an **Uptime (7d)** and **Uptime (30d)** percentage.

This is also how you answer *"was my node actually down during its backup window?"* — line up a `missed schedule` alert with the strip to confirm the node was unreachable at the time rather than a backup bug. Only **transitions** are stored (not one row per health check), and history older than 90 days is pruned automatically, so the log stays small even for a chronically-flapping host.

## Cached, event-driven inventory

DockBack does **not** re-list every node's containers on each page load. Instead it subscribes to each node's **Docker event stream** and refreshes that node's cached inventory the moment something changes (a container starts/stops, a stack comes up, a volume or image is created) — coalescing bursts so a `compose up` triggers a single refresh. A slow periodic reconcile (about every 20 seconds) re-samples CPU/memory and catches anything missed. The dashboard and container lists read this cache, so they stay responsive across **many nodes and thousands of containers**.

The cache is **persisted**, so after a restart the dashboard shows your fleet immediately while fresh data loads in the background. CPU/memory figures are sampled on the reconcile interval (not on every request), so they may lag a few seconds behind a container that just started — counts and state update instantly from events.

## Searching large fleets

On a node with hundreds of containers, the container table supports **server-side search, status filtering, and pagination** — type in the search box (matches name, image, stack, or service) or pick a status, and only the matching page is sent to your browser. "Back up all running" still covers every running container on the node, not just the visible page. The **Backups** history view offers the same: search by target or stack and filter by status/verification, paged on the server so it stays fast across thousands of past backups.

## Copying values from the tables

Rows in the Servers and container tables navigate on click, but they never steal a text selection: drag across any value (a name, an image reference, an endpoint) and release — the selection is kept instead of opening the row. The endpoint and image reference are also click-safe zones, so double-clicking a word there selects it without navigating, and each shows a **copy button** on hover for one-click copying — handy when pasting an image reference into a compose file. Copying works over plain-HTTP LAN access too.

## Troubleshooting

**`context deadline exceeded`**
The request to the server's Docker API timed out. Common causes:
- The server or its Docker daemon is slow or under load.
- A socket-proxy/mTLS port is firewalled or unreachable from the DockBack host.
- For SSH nodes, the first request after a restart pays a one-time handshake; if it consistently times out, the host is likely unreachable or the daemon is overloaded.

Check that the address/port is correct and reachable from the DockBack host (e.g. `nc -vz <host> <port>`), and that any firewall allows the DockBack host.

**`401` / authentication errors (SSH/mTLS)**
The key, passphrase, or TLS bundle is wrong. Re-enter the credential when editing the node.

**Socket-proxy: backups of databases or restores fail**
The proxy is probably missing `EXEC` and/or `POST`. Both are required for dumps and restores — see *A remote Linux server* and *Security & Operations → Socket-proxy permissions*.
