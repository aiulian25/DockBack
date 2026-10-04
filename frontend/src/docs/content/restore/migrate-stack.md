# Moving a stack to another server

A whole compose project can be restored onto a **different node** than the one it was backed up from — to **migrate** it to a new machine, or to **recover** it after the original host is gone. It reuses the same proven per-service machinery as an in-place stack restore, just pointed at another node.

## How to do it

1. Open the source node (**Servers → the node the stack was backed up from**) and click **Restore Stack** on the project.
2. In the dialog, set **Restore to** to the target node. The current node ("this node — original") is the default; every other **reachable** node is listed.
3. Optionally pick an **app-consistent snapshot** point-in-time (if the stack has them — see *One-click stack restore*).
4. Confirm.

DockBack reads each service's backups from the **source node's catalog** and recreates every service **on the target**, in dependency order (databases first, the app last), each proven healthy before its dependents start. Images are **re-pulled by digest** (or loaded from the bundled `image.tar` for a fully offline restore), and **volumes and networks are recreated** under their original names on the target. The **original node is not touched** — this is a copy-onto-the-target, not a move.

## Making it come up cleanly on the new host

Two options in the stack-restore / per-backup dialog are especially useful when the target is a fresh machine:

- **Reconstruct stack folder on host** — rebuild the project's on-host directory and drop the reconstructed `docker-compose.yml` back into place (see *Disaster recovery*).
- **Remap machine IP** — rewrite the old host's IP to the new host's IP in the recreated config, so a service pinned to the old address doesn't fail to bind.
- **Remap stack paths** — move every service's bind-mount folders, the reconstructed compose file, and the stack folder from the old machine's base directory (e.g. `/opt/docker`) to this machine's (e.g. `/opt/stacks`), so the stack lands where your other stacks live instead of recreating the old layout (see *Disaster recovery*).

  Leave **From** blank and the base is read from where the stack's **data** actually lives — the bind mounts that carry the project's name, like `/volume1/docker/bookstack` — not from the compose working directory. The distinction matters when a stack was deployed through a management tool: Portainer runs compose from its own internal `/data/compose/<number>`, and a base taken from there matches none of your data paths, so nothing would be remapped and the old machine's layout would quietly reappear on the new host. For the same reason, the reconstructed folder is named after the **project**, never after a tool's numeric id.

## Safety

The stack is locked **on both nodes** for the duration of a migration: while it runs, a backup or restore of that same project on **either** the source or the target is refused (409) until it finishes, so a migration can never race a backup on either side. As always, existing containers of the stack **on the target** are overwritten in place (a safety snapshot is taken first when enabled), while the source stays exactly as it was.

## Portability preflight — what the target host can't provide

Some containers need something from the machine they run on: a `/dev/dri` device for hardware transcoding, a reserved GPU, a non-default logging driver, extra capabilities, kernel parameters, or privileged mode. All of that is recorded at backup time and faithfully replayed on restore — which is exactly the problem when you restore somewhere else.

Previously a hardware-transcoding container restored onto a host with no GPU was created, started, and failed with a raw Docker error, *after* its data had been written. Now, choosing a different target node shows what that host cannot honor **before** you start:

```
Target host may not support this container
• requires device /dev/dri/renderD128 — not present on nuc
• reserves 1 GPU(s) — no GPU device is present on nuc
```

In a stack restore the same warnings appear in the plan panel, under the specific service they belong to, so a twelve-service stack points at the one with the problem.

### Two things stop the restore until you confirm

Most warnings only inform. Two stop the restore before anything is written, because each produces a container that's created and then broken:

- **A device the target verifiably lacks**, such as gluetun's `/dev/net/tun` or Plex's GPU. The container would be created and fail to start.
- **A shared network the target doesn't have**: one the stack joins but doesn't own, like a reverse proxy's `npm` network or a macvlan. DockBack would create it as a plain bridge, which silently cuts the service off from everything it reaches through that network. Networks the stack owns are recreated as recorded, as before.

The restore names what's missing and asks. Confirm if you know better (the device is about to be attached, or a plain bridge is fine), or fix the target and restore again. "Could not verify" never stops anything, because it isn't a fact.

### The image is checked on the target

**Check restore readiness** asks the node the restore lands on, not the one the backup came from, whether the image can be obtained: a target behind stricter egress rules is the one that matters. It also checks the image offers a build for the target's CPU architecture, so an amd64-only image is caught before a Raspberry Pi pulls nothing useful.

### After the move

A cross-host restore ends its log with what Docker can't see that may still point at the old machine: reverse-proxy hosts, tunnel targets, DNS records, cron jobs, monitors and bookmarks. It also lists the container's environment variables that hold addresses.

### Three kinds of statement, deliberately distinct

- **"not present on `<host>`"** — a fact. DockBack read that host and the device is not there.
- **"could not verify …"** — DockBack could not read that host (unreachable, or not yet probed). It never claims something is missing on this basis.
- **"`<host>` must allow …"** — for privileged mode, capabilities and kernel parameters. Whether a host permits these depends on its kernel and daemon policy, and the only reliable test is trying, so these are reported rather than probed.

Device and GPU checks read the target's **cached hardware probe** (the same data behind the Machine page), so opening a restore dialog does not start any work on a production host. If that host has never been probed, the check says "could not verify" and starts one in the background — the next time you open the dialog it has the answer.

A container that asks nothing special of its host produces no warnings at all, and backups taken before this feature recorded no requirements, so they are silent too.
