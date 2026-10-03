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

Anything outside this list — for example arbitrary host-level operations the proxy can gate — is not granted. The proxy is the boundary that keeps a powerful tool from becoming host root.

## Hardening the deployment

- The raw socket is mounted **read-only** into the proxy only, never into DockBack.
- DockBack talks to the local proxy over a **private, internal-only** Docker network.
- For remote proxies, **firewall the port to the DockBack host** and never expose it publicly (*Connecting Servers → A remote Linux server*).
- The DockBack app container itself runs **non-root, read-only rootfs, all Linux capabilities dropped, `no-new-privileges`**, and under a **custom seccomp allow-list** (`deploy/seccomp.json`) that permits only the syscalls a static Go/SQLite server needs — stricter than Docker's default. Remove that one `security_opt` line to fall back to the default profile.

## Ready-to-use compose for a remote node

Run this on any host (or NAS) you want DockBack to manage, then add it as a *Remote socket-proxy (tcp)* node pointing at `tcp://<host-ip>:2375`. Project: [tecnativa/docker-socket-proxy](https://github.com/tecnativa/docker-socket-proxy).

```yaml
services:
  socket-proxy:
    image: tecnativa/docker-socket-proxy:0.3.0
    restart: unless-stopped
    ports:
      - "2375:2375"
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

> Keep `2375` firewalled to only the DockBack host — it speaks the Docker API.

## Volume sidecar image pinning

Backups run a tiny helper container — the **volume sidecar** — on each managed server, attached to the target with `--volumes-from …:ro`. That means it runs **with read access to the data being backed up**, so which image it is matters.

DockBack records the image's **digest** the first time it runs a sidecar on a server, and **refuses to run a different one** afterwards. This is the same trust-on-first-use posture SSH host keys already get: the first use establishes what is trusted, and any later change is refused rather than accepted silently.

Without it, a repushed or hijacked `alpine:3.20` tag would simply be pulled and executed on every one of your servers, with no visible symptom.

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
