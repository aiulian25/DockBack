# Homepage dashboard widget

[Homepage](https://gethomepage.dev), the self-hosted start page, can show DockBack on a tile: how many nodes and backups it has, how many failed, and how many containers have no recent backup. It uses Homepage's built-in **Custom API** widget, so nothing needs installing on the Homepage side.

## 1. Create a widget token

Under **Settings → Security → API tokens**, choose **Create API token** and tick only **Homepage widget**. That token can read `/api/widget` — the summary below — and nothing else, so the config file it ends up in is not a key to the rest of DockBack. If Homepage always connects from the same address, enter it under **Allowed source addresses** too (see *API tokens for automation*).

## 2. Give the token to Homepage

Add it to Homepage's environment rather than writing it into `services.yaml`:

```yaml
environment:
  HOMEPAGE_VAR_DOCKBACK_TOKEN: dback_…
```

## 3. Add the tile to services.yaml

```yaml
- Backups:
    - DockBack:
        icon: https://dockback.example.com/dockback-mark.png
        href: https://dockback.example.com
        description: Docker backups
        siteMonitor: http://dockback:28734/healthz
        widget:
          type: customapi
          url: http://dockback:28734/api/widget
          refreshInterval: 60000
          headers:
            Authorization: Bearer {{HOMEPAGE_VAR_DOCKBACK_TOKEN}}
          mappings:
            - field: nodes
              label: Nodes
              format: number
            - field: backups
              label: Backups
              format: number
            - field: failed_24h
              label: Failed (24h)
              format: number
            - field: unprotected
              label: Unprotected
              format: number
```

Two kinds of address appear here:

- `url` and `siteMonitor` are fetched by **the Homepage server**. Use the address Homepage reaches DockBack at — the container name when both are on one Docker network, otherwise the host and port.
- `href` and `icon` are opened by **your browser**. Use the address you browse DockBack at.

Homepage makes the widget's request from its own server, so the token never reaches your browser. `siteMonitor` adds a green or red dot from DockBack's health check, which needs no token.

## What the summary holds

| Field | Meaning |
|---|---|
| `nodes` | Nodes DockBack manages. |
| `nodes_offline` | Of those, how many it cannot reach right now. |
| `backups` | Every entry the Backups page lists. |
| `backups_bytes` | Their combined size — use `format: bytes`. |
| `failed_24h` | Backups that failed in the last 24 hours — the same count as the daily digest. |
| `failing` | Containers whose most recent backup failed. It goes back to 0 once a backup of each succeeds; containers since removed or ignored are left out. |
| `unprotected` | Running containers with no recent backup — never backed up, or older than their schedule allows. The dashboard's number. |
| `running` | Backups running right now. |
| `last_backup` | When the newest successful backup started — use `format: relativeDate`. Absent until there is one. |
| `failing_targets` | Up to 10 failing containers, each with `name`, `node` and `node_id`. |

Any field can take the place of one of the four tiles. For example:

```yaml
            - field: last_backup
              label: Last backup
              format: relativeDate
            - field: backups_bytes
              label: Stored
              format: bytes
```

## A list of what is failing

A second entry lists the failing containers by name, each linking to its node's backups in DockBack:

```yaml
    - DockBack failures:
        icon: https://dockback.example.com/dockback-mark.png
        widget:
          type: customapi
          url: http://dockback:28734/api/widget
          refreshInterval: 60000
          headers:
            Authorization: Bearer {{HOMEPAGE_VAR_DOCKBACK_TOKEN}}
          display: dynamic-list
          mappings:
            items: failing_targets
            name: name
            label: node
            limit: 10
            target: https://dockback.example.com/backups?node={node_id}
```

## Check it by hand

```
curl -H "Authorization: Bearer dback_…" http://dockback:28734/api/widget
```

It answers with the fields above. The same token on any other path gets **403**. The summary is read from DockBack's database and the inventory it already keeps — no node is asked anything — so even Homepage's default refresh of every 10 seconds costs only a few queries. Once a minute (`refreshInterval: 60000`) is plenty.
