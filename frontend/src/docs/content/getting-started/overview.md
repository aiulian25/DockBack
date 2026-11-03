# What DockBack is & how it's secured

DockBack backs up your Docker containers — their configuration, volumes/bind data, and databases — across one or many servers, integrity-checks every backup (with opt-in test restores on top), mirrors copies to offsite destinations, enforces retention, and restores on demand (including recreating a container that was deleted entirely).

## What it protects

- **Container configuration** — the full `inspect` (image, ports, env, mounts, networks, restart policy, healthcheck) so a container can be rebuilt from scratch.
- **Volume & bind data** — the irreplaceable files your apps write.
- **Databases** — captured as a **consistent logical dump** (not a raw file copy), so the restored database is always healthy.
- **App-native exports** — where supported (e.g. Paperless, Nextcloud), a portable, version-independent export via the application's own tooling.

## The security model

DockBack is built to hold powerful credentials safely:

- **Distroless, non-root, read-only container** — minimal attack surface, no shell.
- **Docker socket is never exposed directly.** All Docker access goes through a hardened `docker-socket-proxy` sidecar that whitelists only the API calls DockBack needs. See *Security & Operations → Socket-proxy permissions*.
- **Encryption at rest.** Every backup archive is encrypted with **AES-256-GCM** (chunked, `DBACKv1`) using your master key. Destination credentials are sealed with the same key.
- **Always-on verification.** A backup is only marked verified after it is read back end to end: its stored hash, a full decrypt and decompress, a walk of the archive and a sanity check of each database dump. That proves the archive is intact; it is not a test restore. **Deep verification** (each database imported into a throwaway container) and **restore drills** (a whole container brought up in a sandbox) are opt-in and off by default.
- **Least privilege on destinations.** DockBack only ever reads, writes, and deletes inside the backup folder you configure on each destination — never anywhere else.

## How the pieces fit together

1. **Nodes** are the Docker servers you connect (local host, remote Linux, Synology, …).
2. **Backups** capture a container's state, encrypt it, store it locally, and mirror it to your **destinations**.
3. A **policy** + **schedule** automate which containers are backed up, how many copies to keep, and when.
4. **Restore** brings any version back — to the same container, a recreated one, or a whole stack.

The rest of these guides walk through each step in detail.
