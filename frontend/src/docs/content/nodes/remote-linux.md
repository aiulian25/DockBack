# A remote Linux server (no SSH)

You can add a remote Docker host **without enabling SSH** by running the same `docker-socket-proxy` on it. DockBack then sees only the Docker API calls the proxy allows, never a shell.

That API is still powerful. With `CONTAINERS` and `POST` allowed, anyone who reaches the proxy's port can create a container that mounts the server's root filesystem. **The port is root on that server**, with no password and no encryption. So it must be reachable from the DockBack host and nothing else:

- **Bind it to a private overlay address** (WireGuard or Tailscale), as below. See *Reach nodes over Tailscale / WireGuard* to set one up.
- **No overlay?** Use *Connect over SSH* or *Connect over daemon mTLS* instead. Both authenticate DockBack and encrypt the connection.

## 1. Run the socket-proxy, bound to the overlay address

On the remote machine, create a `compose.yml` and replace `<overlay-ip>` with the server's WireGuard or Tailscale address. Compose refuses to start until you do, so this file can't publish the port on every interface by accident.

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

Bring it up:

```bash
docker compose up -d
```

> Project: [tecnativa/docker-socket-proxy](https://github.com/tecnativa/docker-socket-proxy).
>
> `POST` and `EXEC` are required so DockBack can take consistent database dumps and perform restores. Without them you get read-only inventory but backups of databases and all restores will fail.

Never publish it as `"2375:2375"` or on `0.0.0.0`: that puts it on every interface, the LAN and the internet included. A host firewall won't save that setup. Docker adds its own rules for published ports, and traffic to them skips UFW's usual rules, so `ufw deny 2375` does not block a published port. See Docker's [Docker and ufw](https://docs.docker.com/engine/network/packet-filtering-firewalls/#docker-and-ufw).

## 2. Check who can reach it

Do this before adding the node. On the server, the port must be listening on the overlay address only:

```bash
sudo ss -ltnp | grep 2375
```

You should see `<overlay-ip>:2375`, never `0.0.0.0:2375` or `[::]:2375`.

From the DockBack host, it must answer:

```bash
curl -s -m 5 http://<overlay-ip>:2375/version
```

From another machine that is **not** the DockBack host, aimed at the server's ordinary LAN address, it must fail (time out or be refused):

```bash
curl -s -m 5 http://<server-lan-ip>:2375/version
```

If that second command prints a version, stop and fix the binding before going on. With Tailscale, also limit port `2375` to the DockBack host in your ACLs, so other devices on the tailnet are refused too.

## 3. Add the node in DockBack

1. **Servers → Add Node**.
2. **Transport** — *Remote socket-proxy (tcp)*.
3. **Address** — `tcp://<overlay-ip>:2375`.
4. **Test Connection** → **Add Node**.
