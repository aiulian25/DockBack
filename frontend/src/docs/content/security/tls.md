# HTTPS & TLS

DockBack carries your session cookie and the encryption-key-entry form, so those must not travel in cleartext. You have two options.

## Option 1 — Reverse proxy (recommended for public domains)

By default DockBack serves **plain HTTP** bound to `127.0.0.1:28734` and lets your ingress terminate TLS — Cloudflare Tunnel, Tailscale, Pangolin, nginx/Caddy/Traefik, etc. This is the recommended setup for anything reachable on a real domain (automatic, renewing, trusted certificates).

Tell DockBack it's behind a trusted proxy so it enables `Secure` + `__Host-` cookies, HSTS, and correct client IPs for the login lockout:

```env
DOCKBACK_TRUST_PROXY=true
DOCKBACK_TRUSTED_PROXIES=127.0.0.0/8,::1/128,172.16.0.0/12   # your proxy's source ranges
```

List **every** proxy in the chain, not just the one DockBack talks to: the client IP is taken from the last `X-Forwarded-For` entry that isn't one of your proxies, so a hop left out of the list would be reported as the caller.

See *Reach nodes over Tailscale / WireGuard* and the README for per-proxy patterns.

## Option 2 — Built-in TLS (direct HTTPS, no proxy)

For a direct LAN deployment with no proxy, DockBack can serve HTTPS itself.

**With your own certificate** (trusted chain, e.g. from your internal CA):

```env
DOCKBACK_TLS_CERT=/app/certs/tls.crt
DOCKBACK_TLS_KEY=/app/certs/tls.key
```

Mount the files read-only into the container. Cert and key must be provided **together**.

**Self-signed** (quickest; browsers will show a trust warning, but traffic is still encrypted):

```env
DOCKBACK_TLS_SELF_SIGNED=true
```

The self-signed key is generated **in memory** (nothing written to disk). To reach it from other machines, also bind beyond loopback (e.g. `HOST=0.0.0.0`) and map the port.

## What turns on automatically under TLS

However TLS is provided (proxy or built-in), DockBack detects the secure connection and:

- marks the session/CSRF cookies **`Secure`** with the **`__Host-`** prefix,
- sends **HSTS** (`Strict-Transport-Security`).

Over plain HTTP these are deliberately *not* set (a `__Host-`/`Secure` cookie would be rejected by the browser and break login), so an HTTP→HTTPS switch never forces a logout.

> For internet-facing deployments prefer Option 1 — a reverse proxy gives you trusted, auto-renewing certificates. Built-in TLS is ideal for isolated/LAN installs.
