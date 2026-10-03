# Reach nodes safely over Tailscale or WireGuard

A Docker endpoint is **root on that host**, so *how it travels the network matters as much as the transport you pick*. The strongest, simplest pattern is to put DockBack and every node on a **private encrypted overlay** — **Tailscale** or **WireGuard** — and connect over that.

This gives you the best of both worlds:

- The fast **socket-proxy (tcp)** transport — which is plaintext and unauthenticated on its own — now rides an **encrypted, authenticated** tunnel, so it's safe to use beyond a single trusted LAN.
- **SSH** gets a second layer (the overlay) on top of its own encryption — defense in depth, and you can stop exposing port 22 to the LAN/internet entirely.

> Rule of thumb: **socket-proxy *over* WireGuard/Tailscale** is the ideal hardened setup — endpoint filtering *and* encrypted transport at near-native speed. If you don't want to run an overlay, plain **SSH** is the safe default because it brings its own encryption and authentication.

---

## Option A — Tailscale (easiest)

Tailscale builds a WireGuard mesh for you with zero key juggling and gives each machine a stable `100.x.y.z` address (and a MagicDNS name).

### 1. Install on the DockBack host **and** every node

```bash
curl -fsSL https://tailscale.com/install.sh | sh
sudo tailscale up
```

Authenticate each machine in the browser link it prints. Confirm the addresses:

```bash
tailscale ip -4      # e.g. 100.101.102.103
tailscale status     # see all your machines
```

### 2. (Recommended) lock it down with ACLs

In the Tailscale admin console → **Access Controls**, restrict who can reach a node. Example: only the DockBack host may reach the Docker port on nodes tagged `tag:docker`:

```jsonc
{
  "tagOwners": { "tag:docker": ["your-login@example.com"] },
  "acls": [
    // DockBack host -> docker nodes, only on the proxy/ssh ports
    { "action": "accept", "src": ["100.101.102.103"], "dst": ["tag:docker:2375,22"] }
  ]
}
```

Tag a node from its shell: `sudo tailscale up --advertise-tags=tag:docker`.

### 3. Use it (see "Connect over the overlay" below)

The node's address is just its Tailscale IP or MagicDNS name — e.g. `ssh://deploy@100.101.102.104` or `tcp://100.101.102.104:2375`.

---

## Option B — WireGuard (self-hosted, no third party)

More manual, but nothing leaves your control.

### 1. Install on both ends

```bash
sudo apt install wireguard           # Debian/Ubuntu (or your distro's package)
```

### 2. Generate a key pair on each machine

```bash
umask 077
wg genkey | tee privatekey | wg pubkey > publickey
```

### 3. Write the configs

Pick an unused subnet (e.g. `10.10.0.0/24`). On the **DockBack host** (`/etc/wireguard/wg0.conf`):

```ini
[Interface]
Address = 10.10.0.1/24
PrivateKey = <DOCKBACK_HOST_PRIVATE_KEY>
ListenPort = 51820

[Peer]                                  # the node
PublicKey = <NODE_PUBLIC_KEY>
AllowedIPs = 10.10.0.2/32
Endpoint = <node-public-or-lan-ip>:51820
PersistentKeepalive = 25
```

On the **node** (`/etc/wireguard/wg0.conf`):

```ini
[Interface]
Address = 10.10.0.2/24
PrivateKey = <NODE_PRIVATE_KEY>
ListenPort = 51820

[Peer]                                  # the DockBack host
PublicKey = <DOCKBACK_HOST_PUBLIC_KEY>
AllowedIPs = 10.10.0.1/32
Endpoint = <dockback-host-ip>:51820
PersistentKeepalive = 25
```

### 4. Bring it up (and enable on boot)

```bash
sudo wg-quick up wg0
sudo systemctl enable wg-quick@wg0
sudo wg            # verify the handshake/peer
ping 10.10.0.2     # from the DockBack host
```

> Open UDP `51820` between the two machines (only). If a large backup throughput looks off, lower the MTU in `[Interface]` (e.g. `MTU = 1380`).

The node's overlay address is now `10.10.0.2`.

---

## Connect over the overlay

Whichever overlay you chose, you now have a private IP for the node (`100.x.y.z` for Tailscale, `10.10.0.2` for WireGuard). Use it as the node **Address**.

### Over SSH

Nothing special — point the SSH transport at the overlay IP and, ideally, firewall sshd to the overlay interface only:

1. **Servers → Add Node → Transport: SSH**.
2. **Address** — `ssh://user@10.10.0.2` (or the Tailscale IP/MagicDNS name). Add the port if not 22.
3. Paste the key (+ passphrase) → **Test Connection** → **Add Node**.

Optional hardening on the node — only accept SSH from the overlay:

```bash
sudo ufw allow in on wg0 to any port 22 proto tcp     # WireGuard interface
sudo ufw allow in on tailscale0 to any port 22 proto tcp   # Tailscale interface
sudo ufw deny 22/tcp                                   # block SSH elsewhere
```

### Over the socket-proxy (recommended hardened setup)

Run the proxy on the node but **bind it to the overlay IP only**, so the plaintext Docker API never touches any other network. Edit the proxy compose from *A remote Linux server (no SSH)* and pin the published port to the overlay address:

```yaml
    ports:
      - "10.10.0.2:2375:2375"        # WireGuard IP  (or "100.101.102.104:2375:2375" for Tailscale)
```

Then firewall it to the overlay interface as a belt-and-suspenders:

```bash
sudo ufw allow in on wg0 to any port 2375 proto tcp    # or tailscale0
sudo ufw deny 2375/tcp
```

Add the node:

1. **Servers → Add Node → Transport: Remote socket-proxy (tcp)**.
2. **Address** — `tcp://10.10.0.2:2375` (or the Tailscale IP).
3. **Test Connection** → **Add Node**.

Now you get the proxy's endpoint filtering **and** encrypted, authenticated transport — and the Docker port is unreachable from anywhere except the overlay.

---

## Do / don't

- **Do** bind the socket-proxy to the overlay IP (or `127.0.0.1` + overlay), never `0.0.0.0`.
- **Do** firewall the Docker/SSH port to the `wg0` / `tailscale0` interface.
- **Don't** publish a plaintext `2375` proxy on the LAN or internet — that's unauthenticated host root in the clear. The overlay exists precisely to avoid that.
- **Don't** forget the overlay is reachable only while it's up; `PersistentKeepalive` (WireGuard) and the Tailscale daemon keep it alive across reboots.
