# Connect over SSH

If you already manage a server over SSH and prefer it, DockBack can tunnel the Docker API over a single, persistent SSH connection. No `ssh` binary is required inside the container, and the connection is reused across requests (so multi-container pages stay fast).

> Prefer not to keep SSH enabled? Use *A remote Linux server (no SSH)* or *A Synology NAS (no SSH)* instead.

## Prerequisites

- An SSH user on the target host that can reach the Docker socket (typically a member of the `docker` group, or `root`).
- Either the user's **private key** (PEM/OpenSSH format) **or** the user's **password**.

## Steps

1. **Servers → Add Node**.
2. **Transport** — *SSH*.
3. **Address** — `ssh://user@host`. **Include the port if it isn't 22**, e.g. `ssh://deploy@10.168.1.172:2222`. The username here is the SSH user for both auth methods.
4. **Authentication** — choose **Private key** or **Password**:
   - **Private key** — paste the key (the leading `-----BEGIN OPENSSH PRIVATE KEY-----` line is expected). If it's passphrase-protected, enter the **passphrase** too.
   - **Password** — enter the SSH user's password. (A key is more resistant to brute force where you can use one, but password auth is fully supported for hosts that don't use keys.)
5. **Test Connection** — this also shows the host's **key fingerprint**. Verify it matches your server before trusting it — this is what protects the connection (and your password) from a man-in-the-middle. It's pinned on first connect; a later change is refused until you reset the pinned key.
6. **Add Node**.

Credentials (key, passphrase, or password) are **encrypted at rest** with your master key and reused across restarts/rebuilds, so you won't be prompted again.

## Custom socket path

By default DockBack tunnels to `/var/run/docker.sock` on the remote host. If your daemon uses a different socket path, append it to the address path, e.g. `ssh://user@host:22/run/user/1000/docker.sock`.

## Editing credentials later

Open **Servers → (node) → Edit**. The form opens on the node's current authentication method. Leave the credential field(s) blank to keep what's stored; fill them in only to replace them. You can also **switch methods** — e.g. from a key to a password (or the reverse) — and the previous credential is cleared. **Test Connection** re-uses the stored credential when a field is left blank, so you can re-test without re-pasting anything. This works the same regardless of how the node was originally added.
