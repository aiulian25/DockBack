# A remote Linux server (no SSH)

The recommended way to add a remote Docker host **without enabling SSH** is to run the same hardened `docker-socket-proxy` on that server and connect DockBack to it over your LAN. DockBack only ever sees the whitelisted Docker API — never a shell.

## 1. Run the socket-proxy on the remote server

On the remote machine, create a `compose.yml` (copy-paste as-is):

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

Bring it up:

```bash
docker compose up -d
```

> Project: [tecnativa/docker-socket-proxy](https://github.com/tecnativa/docker-socket-proxy).
>
> `POST` and `EXEC` are required so DockBack can take consistent database dumps and perform restores. Without them you get read-only inventory but backups of databases and all restores will fail.

## 2. Firewall it to the DockBack host only

This port speaks the Docker API. Restrict it so **only** your DockBack server can reach it, for example:

```bash
# allow just the DockBack host, drop everyone else
sudo ufw allow from <dockback-ip> to any port 2375 proto tcp
sudo ufw deny 2375/tcp
```

Never expose port `2375` to the internet.

## 3. Add the node in DockBack

1. **Servers → Add Node**.
2. **Transport** — *Remote socket-proxy (tcp)*.
3. **Address** — `tcp://10.168.1.172:2375`.
4. **Test Connection** → **Add Node**.

For an encrypted, authenticated wire instead of a firewalled plaintext port, use *Connect over daemon mTLS*.
