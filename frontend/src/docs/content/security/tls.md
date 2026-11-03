# HTTPS & TLS

DockBack carries your session cookie and the encryption-key-entry form, so those must not travel in cleartext. You have two options.

DockBack is a control plane: it can restore and recreate containers on every node it manages. Keep it on a private network, as the README explains. TLS protects the connection, but it doesn't make a public address safe.

## Option 1 — Reverse proxy on a private path (recommended)

By default DockBack serves **plain HTTP** bound to `127.0.0.1:28734` and lets a proxy in front of it terminate TLS. Use one only you can reach: Tailscale Serve, or nginx/Caddy/Traefik on a private interface or internal domain. A tunnel such as Cloudflare Tunnel or Pangolin is fine only with an identity or access policy in front of it, never as a plain public hostname. This gives you trusted, automatically renewed certificates.

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

> Prefer Option 1 when a proxy is already part of your private setup: it gives you trusted, auto-renewing certificates. Built-in TLS suits an isolated LAN install. Neither makes it safe to publish DockBack on the internet.
