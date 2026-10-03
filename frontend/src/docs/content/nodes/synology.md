# A Synology NAS (no SSH)

You can manage the containers on a Synology NAS **without ever turning on SSH**. The approach is the same as for any remote host: run the hardened `docker-socket-proxy` on the Synology with **Container Manager**, then connect DockBack to it over your LAN as a *Remote socket-proxy* node.

This exposes only the specific Docker API calls DockBack needs — never a shell, never the raw socket.

## Quick way: Container Manager → Project (compose)

The fastest path on DSM 7.2+ is to paste a compose file. In **Container Manager → Project → Create**, give it a name and paste this (project: [tecnativa/docker-socket-proxy](https://github.com/tecnativa/docker-socket-proxy)):

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

Then skip to **step 3** to firewall the port and **step 4** to add the node. Prefer the GUI? Follow steps 1–2 below instead.

## 1. Install the proxy image

1. Open **Container Manager** (older DSM: **Docker**).
2. Go to **Registry**, search for `tecnativa/docker-socket-proxy`, and **Download** the `0.3.0` tag (or latest).

## 2. Create the proxy container

In **Container Manager → Container → Create**, choose the image and set:

- **Volume / mount:** add a bind mount
  - File/Folder: `/var/run/docker.sock`
  - Mount path: `/var/run/docker.sock`
  - Mode: **Read-only**
- **Port settings:** map container port `2375` to a host port (e.g. `2375`).
- **Environment variables** (Advanced settings → Environment) — these grant exactly what DockBack needs:

| Variable | Value |
|----------|-------|
| `CONTAINERS` | `1` |
| `IMAGES` | `1` |
| `VOLUMES` | `1` |
| `NETWORKS` | `1` |
| `EXEC` | `1` |
| `VERSION` | `1` |
| `INFO` | `1` |
| `POST` | `1` |

Enable **auto-restart** and start the container.

> `EXEC` and `POST` are what let DockBack take consistent database dumps and perform restores. Leave them off and you'll only get read-only inventory.

## 3. Lock the port down

The proxy port speaks the Docker API, so restrict who can reach it:

1. **Control Panel → Security → Firewall** → enable the firewall.
2. Add a rule allowing the proxy port (e.g. `2375`) **only** from your DockBack host's IP.
3. Add a rule denying that port from everywhere else. Never expose it to the internet.

## 4. Add the node in DockBack

1. **Servers → Add Node**.
2. **Name** — e.g. `synology-nas01`.
3. **Transport** — *Remote socket-proxy (tcp)*.
4. **Address** — `tcp://<synology-ip>:2375`.
5. **Test Connection** → **Add Node**.

The NAS now appears as a node; its containers can be backed up and restored like any other server.

## Notes

- This is **separate** from using the Synology as a *backup destination* over SMB. A node = "containers DockBack manages"; a destination = "a place backups are stored". You can use the same NAS for both. See *External Backup Destinations → Synology NAS (SMB)*.
- Prefer an encrypted, authenticated channel? Configure dockerd with TLS and use the *Daemon mTLS* transport instead.
