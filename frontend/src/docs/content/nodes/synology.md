# A Synology NAS (no SSH)

You can manage the containers on a Synology NAS **without ever turning on SSH**. The approach is the same as for any remote host: run the `docker-socket-proxy` on the Synology with **Container Manager**, then connect DockBack to it as a *Remote socket-proxy* node.

The proxy passes on only the Docker API calls DockBack needs, never a shell. Those calls are still enough to create a container that mounts the NAS's root filesystem, so **the proxy port is root on the NAS**, with no password and no encryption. It must be reachable from the DockBack host and nothing else.

That takes a private overlay. Install **Tailscale** from Package Center (or set up WireGuard; see *Reach nodes over Tailscale / WireGuard*), then bind the proxy to that address only. DSM's firewall is not a substitute: Docker manages its own rules for published ports, so don't assume a DSM firewall rule blocks one. Step 2 checks the result instead.

## 1. Create the proxy: Container Manager → Project

On DSM 7.2+, in **Container Manager → Project → Create**, give it a name and paste this. Replace `<overlay-ip>` with the NAS's Tailscale or WireGuard address; Compose refuses to start until you do. Project: [tecnativa/docker-socket-proxy](https://github.com/tecnativa/docker-socket-proxy).

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

> `EXEC` and `POST` are what let DockBack take consistent database dumps and perform restores. Leave them off and you'll only get read-only inventory.

Prefer the Project route to the container wizard: here the address the port is published on is written down explicitly. If you build the container in the wizard instead, publish port `2375` on the overlay address only, never on all interfaces, and check it with step 2.

## 2. Check who can reach it

Before adding the node:

1. From the DockBack host, this must print a version:

   ```bash
   curl -s -m 5 http://<overlay-ip>:2375/version
   ```

2. From another device on your LAN, aimed at the NAS's ordinary LAN address, the same request must fail (time out or be refused):

   ```bash
   curl -s -m 5 http://<synology-lan-ip>:2375/version
   ```

If the second one answers, the port is open to your whole network: stop the project and fix the binding before going on. With Tailscale, also limit port `2375` to the DockBack host in your ACLs.

## 3. Add the node in DockBack

1. **Servers → Add Node**.
2. **Name** — e.g. `synology-nas01`.
3. **Transport** — *Remote socket-proxy (tcp)*.
4. **Address** — `tcp://<overlay-ip>:2375`.
5. **Test Connection** → **Add Node**.

The NAS now appears as a node; its containers can be backed up and restored like any other server.

## Notes

- This is **separate** from using the Synology as a *backup destination* over SMB. A node = "containers DockBack manages"; a destination = "a place backups are stored". You can use the same NAS for both. See *External Backup Destinations → Synology NAS (SMB)*.
- No overlay possible? Configure dockerd with TLS and use the *Daemon mTLS* transport, which authenticates DockBack and encrypts the connection.
