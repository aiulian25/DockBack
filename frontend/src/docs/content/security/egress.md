# Egress allow-list (outbound connection control)

DockBack makes **outbound** connections in exactly two situations, and only ever to endpoints **you** configured:

- **Remote storage destinations** — S3/B2/Wasabi/MinIO (HTTPS), Nextcloud/WebDAV (HTTPS), Synology/Samba (SMB).
- **Notifications** — Gotify (HTTPS), a generic webhook (HTTPS), and email (SMTP).

There is no telemetry, no update check, and no implicit egress. The Docker socket-proxy network is `internal:` with no route off-host. So in the default configuration the app already only talks to destinations you entered.

## When to lock egress down further

In hardened or regulated environments you may want a belt-and-suspenders **default-deny** control, so that even a misconfiguration, a malicious redirect, or a DNS rebind cannot cause the app to connect somewhere unexpected. Set an **egress allow-list** and every outbound host must match it or the connection is refused.

This is an infrastructure-level control set once by the operator (like the trusted-proxy list), not a per-destination toggle — so it never changes the day-to-day workflow of adding or running backups.

## How to enable it

There are two ways, and they compose: an **environment default** and an **in-app override**.

### In the app (live, no restart)

**Settings → Egress allow-list** shows whether the policy is active, lists the effective entries, and lets you edit them — **applied live**, no container recreate. Enter one host per line (or comma-separated), click **Apply allow-list**, and the change takes effect immediately (an atomic policy swap). A chip shows **Active** (outbound hosts not listed are refused) or **Disabled** (any operator-configured host is allowed). Use **Test a host** to check whether a host would be permitted by the currently applied policy *before* you save a destination.

Rather than typing hosts by hand (and risking a typo that locks out a working destination), click **Suggest from current config**. DockBack lists the hosts it is *already configured to reach* — backup and app-backup destinations, notification endpoints (Gotify, email, webhook, heartbeat), and remote nodes — each marked **already allowed** or **would be blocked by the current list**, with an **Add** button that drops it into the box. Only bare hostnames are shown; no credentials, ports, or URL paths are ever exposed. As always, nothing is saved until you **Apply**.

An in-app value **overrides** the environment variable. Clear the field and apply to fall back to the environment default.

### Audit mode — see what would break, before it does

Turning a default-deny list on is all-or-nothing, and getting it wrong fails in the worst possible way: silently, hours later, when a scheduled backup cannot reach a destination nobody remembered to list.

Tick **Audit mode — observe, don't block**. The allow-list is still evaluated against every outbound connection, but a host it *would* refuse is **recorded and let through** instead of blocked. Then use the app normally — run a backup, send a test notification, let a mirror upload run — and come back to the **Would be blocked** list.

Each observation shows the host, how many times it was reached, and what it is:

- **A named source** (`destination: Nextcloud`, `notification: Gotify`, `node: NUC`) means this is a genuine gap in your list. **Add** drops it into the box above.
- **not a configured endpoint** means nothing you set up uses this host. It arrived some other way — an HTTP redirect, a CDN, a DNS change — which is exactly what the dial-time check exists to catch. Look at it before you allow it.
- **now allowed** means you have already fixed it; the observation is just history.

**Add all configured** takes only the hosts that belong to something you set up. Anything unrecognised has to be added on its own, deliberately — a one-click "allow everything that was observed" would let a redirect write itself into the very list that is supposed to stop it. As everywhere else here, adding only edits the box; nothing is saved until you **Apply**.

**Test a host** and **Suggest from current config** keep reporting the real verdict while audit mode is on. They answer *"does the policy allow this?"*, which does not change; audit mode changes only whether a denial is acted on.

When the list is empty or every entry is accounted for, untick audit mode. Enforcement resumes immediately. **Clear and start again** wipes the observations so you can re-run your workloads and confirm from a clean slate.

> While audit mode is on, the allow-list is **not protecting anything** — the chip in the section header says **Audit mode — observed, not enforced** rather than *Active*, and turning it on and off is recorded in the audit trail. It is a commissioning tool, not a setting to leave on.

### As an environment default

Set `DOCKBACK_EGRESS_ALLOW` to a comma-separated list of permitted hosts. **Leave it empty (the default) to keep egress unrestricted.** This is the boot default; an in-app value takes precedence over it.

```env
# Allow only these outbound hosts. Empty = unrestricted (default).
# This is the BOOT DEFAULT — the in-app "Egress allow-list" setting overrides it.
DOCKBACK_EGRESS_ALLOW=s3.us-west-002.backblazeb2.com,*.my-nextcloud.example,nas.lan,gotify.lan
```

Each entry may be:

| Form | Example | Matches |
| --- | --- | --- |
| Exact host | `s3.amazonaws.com` | only that host |
| Wildcard / domain | `*.example.com` | `example.com` **and** any subdomain |
| IP address | `203.0.113.10` | that IP |
| CIDR network | `203.0.113.0/24`, `2001:db8::/32` | any IP in the range |

Matching is case-insensitive and ignores the port. A pasted URL or `host:port` is tolerated — only the host part is used.

## What it protects

When the allow-list is set, the policy is enforced in two places:

1. **When you add or test a destination** (and when notifications send) — you get an immediate, clear error (`egress denied: "host" is not in the egress allow-list`) instead of a silent failure mid-backup.
2. **At the actual TCP dial** for every storage and notification connection — so an HTTP redirect or a DNS rebind to a host that is *not* on the list is also refused, not just the host you typed.

The **Local** destination makes no outbound connection and is always available regardless of the allow-list.

## Nodes are NOT affected — you never list them here

The allow-list gates **only storage destinations and notifications**. It does **not** gate connections to your Docker nodes (local socket-proxy, remote socket-proxy/TCP, SSH, or daemon mTLS). So:

- **Adding a node never requires editing `DOCKBACK_EGRESS_ALLOW`.** Add as many nodes as you like without touching it.
- Only add a host here when you add an **offsite storage destination** or a **notification endpoint** whose host isn't already covered by an existing entry (exact host, `*.domain`, IP, or CIDR).
- Tip: a single **CIDR** such as `10.168.1.0/24` covers every current and future host on that LAN subnet (NAS, Gotify, etc.) in one entry, so you rarely need to edit it again.

## Applying a change

Editing the list **in the app** applies immediately — no restart. Editing `DOCKBACK_EGRESS_ALLOW` in your `.env` is a boot default read at startup; apply it with `docker compose up -d` (recreates the container). Once you've saved an in-app value it takes precedence, so the env var only applies when the in-app field is empty.

## Trade-offs

- If you enable the allow-list and forget to add a destination's host, that destination's backups and tests will fail with the egress-denied error until you add the host. This is the intended default-deny behavior — **audit mode** (above) exists so you can find those gaps without living through them.
- Some S3 providers use **virtual-host-style** bucket URLs (`bucket.s3.region.example`). If your provider does, allow the whole domain with `*.s3.region.example` rather than the bare endpoint.

## Related controls

- **Destination path confinement** restricts *where within* a destination the app may write.
- **Trusted proxies** restricts inbound `X-Forwarded-*` handling.
- **Socket-proxy** keeps the Docker control plane off the network entirely.
