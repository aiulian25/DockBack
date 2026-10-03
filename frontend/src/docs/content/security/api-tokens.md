# API tokens for automation

The DockBack UI authenticates with a session cookie, which is deliberately wiped on every restart — great for a browser, useless for a script. **API tokens** give cron jobs, CI pipelines, and external monitors a non-interactive way to talk to the API without a cookie.

Create them under **Settings → Security → API tokens**. Minting a token asks you to **confirm your account password** (and two-factor code when 2FA is on) — it creates a new credential, so it gets the same "sudo mode" protection as revealing the encryption key.

## How they work

- A token is a bearer secret of the form `dback_…`. Send it on every request:

  ```
  Authorization: Bearer dback_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
  ```

- The value is shown **once**, at creation — copy it then. DockBack stores only its SHA-256 hash, so it can never be recovered or displayed again (lose it, revoke it, make a new one).
- Every action a token takes is attributed to **`token:<name>`** in the *Audit trail*, and each token tracks a **last-used** time.
- Tokens carry no cookies, so they are exempt from CSRF — but they are still restricted to their **scope** (below) and can never manage tokens, change settings, rotate the key, restore, or download decrypted backup data.
- A token can be given an optional **expiry** at creation (30 days, 90 days, 1 year — or never). Once the date passes, every request with it returns **401 "API token expired"**; the row stays listed in Settings with an **expired** chip until you revoke it, so you can see what died and mint a replacement. The expiry cannot be edited afterwards — mint a new token instead.

- A token can also be **pinned to the addresses it may be used from** — see below. This is the only control that limits a *stolen* token: an expiry bounds how long it works, a scope bounds what it does, but neither stops it being used from somewhere else the moment it leaks.

## Pin a token to its source address

At creation, **Allowed source addresses** takes one IP or CIDR range per line — `10.0.0.5`, `10.168.1.0/24`, `2001:db8::/32`. Leave it blank and the token works from anywhere, exactly as tokens always have.

A pinned token presented from any other address is refused with **403**, *before* its scope is even considered, and the attempt is written to the *Audit trail* as `token.denied_ip`. That row is the point: it is what a leaked credential being exercised looks like.

The pin covers `/metrics` too, which is where it earns its keep — a scrape token pinned to your Prometheus host is useless to anyone who copies it out of a config file.

> **One prerequisite, and DockBack enforces it.** If `DOCKBACK_TRUST_PROXY` is on with no `DOCKBACK_TRUSTED_PROXIES` set, DockBack believes the `X-Forwarded-For` header from *anyone* — so any caller could claim an allowed address and the pin would keep out nobody. Creating a pinned token in that configuration is **refused**, with the fix named: set `DOCKBACK_TRUSTED_PROXIES` to your reverse proxy's address. A pin that can be spoofed is worse than no pin, because you would stop guarding the token by other means.

The pin is fixed at creation, like the expiry. To change it, mint a new token and revoke the old one — the shown value can never be re-displayed anyway.

## Scopes

Pick the least privilege that does the job:

| Scope | Grants |
|---|---|
| **Read-only** | Any `GET` — backup lists, status, node/stack inventory, insights. **Not** decrypted archive downloads or the app-backup export. |
| **Trigger backups** | Everything Read-only can do, **plus** starting and re-verifying backups: `POST /api/backups`, `/api/backups/{id}/verify`, `/drill`, `/mirror`, and stack backups. No config changes, no restores. |
| **Metrics only** | Only the Prometheus `/metrics` endpoint. |

A token may hold more than one scope.

## Examples

List backups (read):

```
curl -H "Authorization: Bearer dback_…" https://dockback.example.com/api/backups
```

Trigger a backup before a risky deploy (backup scope):

```
curl -X POST -H "Authorization: Bearer dback_…" \
  -H "Content-Type: application/json" \
  -d '{"node_id":"<node>","container_id":"<cid>"}' \
  https://dockback.example.com/api/backups
```

A `read` token that tries to `POST /api/backups` gets **403**; revoking a token makes its next request **401**.

## Protecting /metrics

`/metrics` is open by default (it exposes only operational counters). The moment you create **any** token with the **Metrics only** scope, the endpoint locks: unauthenticated scrapes then return **401**, and Prometheus must present a metrics token:

```yaml
scrape_configs:
  - job_name: dockback
    authorization:
      credentials: dback_…      # a metrics-scoped token
    static_configs:
      - targets: ["dockback.example.com"]
```

Delete every metrics token to reopen it. The current state is shown right on the API tokens card: either "/metrics is locked" or a warning that it is open.

An **expired** metrics token no longer opens the gate — but its mere existence still keeps `/metrics` locked, so an expiry never silently re-exposes the endpoint. Revoke it (or mint a fresh one) to restore scraping.

## Good practice

- **One token per consumer** (this CI job, that monitor), so you can revoke one without disturbing the others.
- **Set an expiry** on tokens for anything short-lived (a migration script, a one-off audit) so a forgotten token dies on its own.
- Prefer **Read-only** or **Metrics only** unless a job truly needs to start backups.
- Put DockBack behind **HTTPS** (see *HTTPS & TLS*) so bearer tokens are never sent in clear text.
- **Pin the source address** whenever the consumer has a stable one — a CI runner, a monitoring host, a LAN scraper. It is the difference between a leaked token being a problem and a leaked token being useless.
- **Revoke** promptly when a token is no longer needed or may have leaked — revocation is instant.
