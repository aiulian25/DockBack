# Transports compared — which to pick

A **node** is a Docker server DockBack controls. When you add one (**Servers → Add Node**) you choose a *transport* — how DockBack reaches that server's Docker engine. There are four:

| Transport | Address example | Credential | Best for |
|-----------|-----------------|------------|----------|
| **Local socket-proxy (tcp)** | `tcp://socket-proxy:2375` | none | The host DockBack runs on (bundled sidecar). |
| **Remote socket-proxy (tcp)** | `tcp://10.168.1.172:2375` | none | Remote servers & NAS **without SSH** — recommended. |
| **SSH** | `ssh://user@10.168.1.172:22` | private key (+ passphrase) **or** password | Servers where you already have SSH and prefer it. |
| **Daemon mTLS** | `tcp://10.168.1.172:2376` | CA + cert + key bundle | An encrypted, mutually-authenticated TCP connection to dockerd. |

## The honest baseline

**Any** Docker endpoint — proxy or SSH — is effectively **root on the target host** (DockBack must create sidecars and `exec` into containers, and a container-create can mount the host filesystem). So the real question isn't "root vs not-root"; it's *attack surface, transit security, and operational fit*.

## socket-proxy vs SSH at a glance

| Dimension | Remote socket-proxy (tcp) | SSH |
|-----------|---------------------------|-----|
| **Permission granularity** | **Better** — endpoint allow-list; everything else denied | Full Docker API (user in `docker` group) |
| **Transit encryption** | **None by itself** — plaintext :2375 | **Encrypted by default** |
| **Authentication** | **None** — relies on network isolation | **Strong** — key-based |
| **Speed / overhead** | Slightly lower (raw TCP→HTTP) | Adds crypto; with AES-NI still saturates gigabit |
| **Setup** | Run a proxy sidecar per node | **Easiest** — nothing to install |
| **Safe to expose on untrusted network?** | **No** (unless over an overlay) | Yes (brings its own auth+crypto) |

## How to choose

- **The machine DockBack runs on** → *Local socket-proxy*. Already wired up in `docker-compose.yml`.
- **A node on a private encrypted overlay (Tailscale / WireGuard)** → *Remote socket-proxy*. Tightest surface **and** fast — this is the recommended fleet setup. See *Reach nodes over Tailscale / WireGuard*.
- **A node with no overlay, or you want it working in 2 minutes** → *SSH*. It brings its own encryption + authentication, so it's the safe default over a LAN. DockBack tunnels the Docker API over one persistent SSH connection (no `ssh` binary needed; passphrase keys supported).
- **You already expose dockerd over TLS** → *Daemon mTLS*. Encrypted+authenticated, but the most setup and no endpoint filtering.
- **Avoid:** a raw plaintext `2375` proxy on a LAN/internet (unauthenticated host root in the clear), and mTLS unless you specifically need it.

### Decision rule

Have a private encrypted overlay between DockBack and the node? → **socket-proxy** (tighter surface, faster). No overlay / minimal setup / untrusted path? → **SSH**. The ideal hardened combination is **socket-proxy *over* WireGuard/Tailscale** — see *Reach nodes over Tailscale / WireGuard*.

## Changing a node's transport later

You're not locked into how a node was first added. Open **Servers → (node) → Edit** and pick a different **Transport** — e.g. move a node from a `tecnativa/docker-socket-proxy` (tcp) connection to **SSH**, or the reverse, or to **mTLS**. When you change the method, enter fresh credentials for the new one; the old, now-unused credential is **dropped** (not silently kept). Everything else about the node — its name, backups, schedules, and history — is preserved. Use **Test Connection** to confirm the new method before saving.

## Security note

Whichever transport you pick, keep the endpoint on a trusted path. A socket-proxy or mTLS port must be firewalled to **only** the DockBack host (or bound to a private overlay interface), and never exposed to the internet.
