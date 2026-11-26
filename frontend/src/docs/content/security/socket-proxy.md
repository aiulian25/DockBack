# Socket-proxy permissions explained

DockBack never talks to the raw Docker socket. All Docker access goes through a `tecnativa/docker-socket-proxy` that whitelists only the API groups DockBack needs. This is true for the bundled local sidecar and for any remote proxy you set up.

## The whitelist DockBack uses

```yaml
CONTAINERS: 1   # list / inspect containers
IMAGES: 1       # inspect images; pull-by-digest for the volume sidecar & restore
VOLUMES: 1      # inventory volumes
NETWORKS: 1     # inventory networks (and recreate)
EXEC: 1         # run database dumps, hooks, and restore imports inside containers
VERSION: 1      # Docker API version negotiation
INFO: 1         # health / ping
POST: 1         # allow mutating calls: create / start / stop / exec / wait
```

## Why each one

- **CONTAINERS / IMAGES / VOLUMES / NETWORKS** — discovery and the configuration captured in the manifest, plus image re-pull on restore.
- **EXEC + POST** — the powerful pair. They let DockBack run commands *inside* containers (consistent DB dumps, quiesce hooks) and perform restores (create/start/stop/wait). **Without them you get read-only inventory and all database backups + restores fail.**
- **VERSION / INFO** — negotiation and health checks.

## What's deliberately *not* enabled

Anything outside this list is refused, for example swarm, secrets, plugins and system-wide operations. That narrows what a mistake or a bug in DockBack can reach.

It does **not** stop the proxy from being host root. `CONTAINERS` and `POST` together let a caller create a container that mounts the host's root filesystem, or runs privileged, and start it. Anyone who can reach a proxy with this whitelist can take over that host. Treat its port exactly like the raw Docker socket: reachable from DockBack and nothing else.

## Hardening the deployment

- The raw socket is mounted **read-only** into the proxy only, never into DockBack.
- **Keep Docker Engine patched on every node: 29.5.1 or later.** The proxy chooses which API calls get through, but it passes those calls to the daemon unchanged, so a daemon bug reachable through them is not stopped by it. 29.5.1 fixes the `docker cp` archive flaws (CVE-2026-41567, CVE-2026-41568, CVE-2026-42306), and 29.3.1 the AuthZ and plugin-privilege ones (CVE-2026-34040, CVE-2026-33997). Check a node with `docker version --format '{{.Server.Version}}'`.
- DockBack talks to the local proxy over a **private, internal-only** Docker network.
- For remote proxies, **bind the port to a private overlay address** (WireGuard or Tailscale), never to all interfaces, and check that other machines are refused (*Connecting Servers → A remote Linux server*). A host firewall is not enough on its own: Docker's rules for published ports skip UFW's usual ones.
- The DockBack app container itself runs **non-root, read-only rootfs, all Linux capabilities dropped, `no-new-privileges`**, and under a **custom seccomp allow-list** (`deploy/seccomp.json`) that permits only the syscalls a static Go/SQLite server needs — stricter than Docker's default. Remove that one `security_opt` line to fall back to the default profile.

## Ready-to-use compose for a remote node

Run this on any host (or NAS) you want DockBack to manage, then add it as a *Remote socket-proxy (tcp)* node pointing at `tcp://<overlay-ip>:2375`. Replace `<overlay-ip>` with the host's WireGuard or Tailscale address; Compose refuses to start until you do. Project: [tecnativa/docker-socket-proxy](https://github.com/tecnativa/docker-socket-proxy).

```yaml
services:
  socket-proxy:
    image: tecnativa/docker-socket-proxy:0.3.0
    restart: unless-stopped
    ports:
      - "<overlay-ip>:2375:2375"
    environment:
      CONTAINERS: 1
      IMAGES: 1
      VOLUMES: 1
      NETWORKS: 1
      EXEC: 1
      VERSION: 1
      INFO: 1
      POST: 1
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    networks:
      - dback_internal
    security_opt:
      - "no-new-privileges:true"

networks:
  dback_internal:
    driver: bridge
```

> This port is root on that host. Before adding the node, check it from a machine other than the DockBack host and make sure the connection is refused (*A remote Linux server → Check who can reach it*). No overlay? Use SSH or daemon mTLS instead.

## Volume sidecar image pinning

Backups run a tiny helper container — the **volume sidecar** — on each managed server, attached to the target with `--volumes-from …:ro`. That means it runs **with read access to the data being backed up**, so which image it is matters.

DockBack records the image's **digest** the first time it runs a sidecar on a server, and **refuses to run a different one** afterwards. This is the same trust-on-first-use posture SSH host keys already get: the first use establishes what is trusted, and any later change is refused rather than accepted silently.

Without it, a repushed or hijacked `alpine:3.20` tag would simply be pulled and executed on every one of your servers, with no visible symptom.

Two more things limit what a wrong image could do:

- **Changing the image, or re-pinning it, asks for your password again.** A browser left signed in cannot swap it.
- **Helper containers run with no network** and with only the capabilities that copying files needs (no raw sockets, no changing user, no new privileges). They hand their result back over the Docker connection, so they have nowhere else to send it.

### What you see

Each server's page shows **Volume sidecar image** with its reference and pinned digest. Before the first backup it reads *"Not pinned yet — pins on the first backup"*.

If the image changes, the backup **fails and the image is not run**, and you get a **critical alert**. The message distinguishes two very different situations:

- *"the volume sidecar image changed since it was pinned"* — the same reference now resolves to different content. If you didn't cause that, treat it as a possible supply-chain compromise.
- *"the volume sidecar image was changed from X to Y"* — the configured reference itself changed, which is usually something you did on purpose.

### Re-pinning

After deliberately changing the image, use **Re-pin** on the server's page. That clears the pin so the next backup records what is present.

Re-pin does **not** accept a digest you supply — the pin must always be something DockBack observed itself, or the refusal could be waved through with an attacker's value. The action is confirmed, and recorded in the audit trail as `node.sidecar.repin`.

### Pinning the reference too

You can go further and pin the reference itself, so the tag can never be re-resolved at all:

```
docker inspect --format='{{index .RepoDigests 0}}' alpine:3.20
# then in .env:
DOCKBACK_SIDECAR_IMAGE=alpine@sha256:…
```

The per-node pin still applies on top, so a change is caught either way.
