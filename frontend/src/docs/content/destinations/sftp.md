# SSH (SFTP)

Any Linux box, Raspberry Pi, or NAS you can reach over SSH can be an offsite destination — no agent, no extra software, just `sshd`. This is the commonest homelab offsite, and it gets full parity: mirroring, scrub, backfill, Scan & adopt, runbook locations, and capacity forecasting (when the server supports the SFTP `statvfs` extension; otherwise capacity simply shows as unknown).

## Setting it up

1. On the target machine, create a **dedicated user** just for backups:
   ```
   sudo useradd -m -s /usr/sbin/nologin dockback
   sudo mkdir -p /srv/backups/dockback
   sudo chown dockback:dockback /srv/backups/dockback
   ```
   Give it write access to **only** that directory — DockBack never touches anything outside the remote directory you configure anyway (path traversal is rejected at the storage layer), but a least-privilege user makes that a property of the *server*, not just the client.
2. Auth with a **private key** (recommended): generate a key pair (`ssh-keygen -t ed25519`), put the public key in the user's `~/.ssh/authorized_keys`, and paste the **private** key into the Add Destination form. A **password** works too. Either credential is encrypted with your master key before it is stored.
3. Host = the machine's IP or name, Port = 22 unless changed, Remote directory = the folder from step 1.

## Host-key pinning (the same protection your nodes get)

When you add the destination, DockBack connects once and **pins the server's SSH host key** — the host must be reachable at add time. From then on, every upload verifies the pin: a **changed key refuses all transfers** with a loud error instead of silently trusting a possibly-impersonated server. If the change is legitimate (you re-installed the box), press **Reset pinned key** in the destination's edit dialog — the fresh key is pinned immediately.

## Atomic uploads

Archives are uploaded under a temporary name and **renamed into place** only when complete — a connection drop mid-transfer can never leave a half-written file that looks like a finished backup.

## Hardening ideas

- Restrict the `dockback` user to SFTP only in `sshd_config`:
  ```
  Match User dockback
    ForceCommand internal-sftp
    ChrootDirectory /srv/backups
  ```
- Pair it with a second destination of a different kind (S3 with Object Lock, a NAS share) for real 3-2-1 coverage — an SSH box on the same power circuit is offsite for disk failure, not for lightning.
