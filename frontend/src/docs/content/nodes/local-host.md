# The local host (bundled proxy)

The server DockBack itself runs on is reached through the **`docker-socket-proxy` sidecar** that ships in `docker-compose.yml`. This is the safest possible setup: the raw Docker socket is mounted **only** into the proxy (read-only), and DockBack talks to the proxy over a private, internal-only Docker network.

## It's usually already connected

In a standard deployment the local node is configured for you. If you need to add or re-add it manually:

1. Go to **Servers → Add Node**.
2. **Name** — anything meaningful (e.g. the hostname).
3. **Transport** — *Local socket-proxy (tcp)*.
4. **Address** — `tcp://socket-proxy:2375` (the sidecar's service name and port).
5. Click **Test Connection**, then **Add Node**.

## Why a proxy instead of the socket directly

Mounting `/var/run/docker.sock` straight into an application is equivalent to giving it root on the host. The proxy sits in between and only forwards the specific API calls DockBack needs (list/inspect, exec for dumps, create/start/stop for restore). See *Security & Operations → Socket-proxy permissions* for the exact whitelist.
