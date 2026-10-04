# Disaster recovery (recreate a container)

DockBack can restore a container **even if it no longer exists** — deleted, lost with its host, or being rebuilt on a fresh server. This is true disaster recovery, not just data restore.

> **Recovery runbook.** The **Recovery** page auto-generates a step-by-step disaster-recovery runbook from your real backup metadata — the correct restore order (databases first, extensions to reinstall, then app volumes), where every copy lives, key-recovery steps, and the last proven-restore date per service. Print it to PDF or download it as Markdown and keep a copy **off** this machine, so you have the plan even if DockBack is down.

## Starting from nothing: the recovery guide on the dashboard

The hardest part of a total loss is not any one step — it is knowing the **order**, at the worst possible moment. A brand-new DockBack that has no nodes you added and no backups now says so, and lays the sequence out on the dashboard:

1. **Restore DockBack's own backup.** This is first because it brings back everything else at once: every node and its credentials, the entire backup catalog, your settings, destinations and admin account. Upload the archive you kept off-box, or add the destination it was pushed to and fetch the newest one from there. DockBack restarts into the restored state.
2. **Check the destinations still resolve.** The restored catalog *describes* archives that live on your destinations. Confirm each one still connects and holds what the catalog expects — a destination that moved, or whose credentials were rotated, is the difference between having a catalog and having a recovery.
3. **Rebuild each machine you lost.** The Recovery page now lists every node from the restored catalog with the order to bring its services back, and a node whose hardware is gone can be rebuilt onto a different host from the same backups.

The guide **remembers where you are across the restart** that step 1 causes — which is the point at which a note in the database would have been thrown away, since the restore replaces that database. When DockBack comes back it picks up at step 2 by itself.

It appears only on an instance that holds nothing. Connecting a node or taking a backup ends it, and the **×** dismisses it for good if you are simply setting up something new rather than rebuilding something old. Nothing in it runs on its own: every step opens an existing screen and waits for you.

## Sharing the runbook

In a real incident you often need to hand the restore plan to a helper — a colleague, a family member, or future-you on a phone — who has **no DockBack login**. The **Share link** button on the Recovery page mints a link that opens a **redacted, read-only** copy of the runbook without any credentials.

- **Choose how long it lives** — **1 hour**, **24 hours**, or **7 days**. The link stops working the instant it expires.
- **It's signed, not a secret store.** The token is an HMAC over the link id + expiry using your master key, verified statelessly — a tampered or expired token simply returns *Not found*. Nothing about the runbook is embedded in the link itself.
- **Revoke any time.** The dialog lists your active links; **Revoke** kills one immediately (it then returns *Not found* on the next open).
- **What's redacted.** The shared page shows the **restore order, service and stack names, image references, copy locations (names/types), and the recovery notes** — everything a helper needs to act. It **omits**: the **master-key fingerprint** and key posture, **node addresses**, internal node ids, and per-copy **detail** strings. The shared page also states plainly that the actual restore still needs the master encryption key, which only you hold.
- **Not indexed, not cached.** The page is served with `no-store` and `noindex`, and the endpoint is rate-limited per IP.

Every **mint**, first **view**, and **revoke** is written to the *Audit trail*.

## How it works

Every backup stores the container's full configuration in `config/inspect.json`. When you restore and the target container is missing, DockBack:

1. **Provides the exact image that was running**: from this host when it is still here (or was just loaded from the bundled `image.tar`), otherwise pulled by its recorded digest.
2. **Recreates the container** with its saved configuration — env, ports, mounts/volumes, networks, restart policy, healthcheck.
3. **Restores the data** (volumes and/or the consistent database dump) into the recreated container.
4. **Starts it.**

The result is the container running again with its original configuration and data.

## Is the image still available?

A recreate uses the exact image the backup ran: the one still on this host, or the one pulled by its recorded digest. It falls back to the tag only when the tag still names that same image. When the recorded image is gone and the tag now names a **different** one, the recreate **stops before removing anything** and says so. An application can migrate older data to a newer version beyond going back; Uptime Kuma 2 did exactly that to a backed-up version 1. To go ahead anyway, tick **Allow a newer image** in the restore drawer. Otherwise, load the image (or re-back up with the image bundled) and restore the recorded version, then upgrade later. DockBack surfaces all of this **before** you rely on the backup:

- **On a backup** (Backups → open a backup → Manifest), **Check restore readiness** verifies the image is obtainable right now — present locally, still pullable by digest/tag, or bundled — without pulling anything. A green *Image ready* or an amber warning tells you where you stand.
- **In the runbook** (Recovery), any service whose image is **only referenced by a tag and isn't bundled** is flagged *image not bundled*, because a restore then depends on that tag still existing in its registry.

The fix is the existing **Also save the container image** option: re-back up with it enabled and the image travels inside the archive (`image.tar`), so the restore needs no registry at all and works fully offline. A backup made with that option is always ready.

## Using it

Just restore the backup as usual (*Restore by version & source location*). If the target doesn't exist, DockBack detects that and recreates it automatically — the live log shows "recreating from backup (disaster recovery)".

## First: freeze the evidence

Before restoring anything after a disaster, press **Freeze evidence** on the node's page. It downloads one JSON file describing how every container there is put together: the full `docker inspect` record for each, with environment variables reduced to their **names** and logging-option values removed, plus the node's networks and volumes. It asks for your password like an export, because commands and labels can still carry secrets.

It's the record you'd otherwise rebuild by hand. On the night that prompted it, DockBack's own compose file was gone with everything else, and DockBack was rebuilt from `docker inspect` while its container still ran.

## Files only: the folder is gone, the containers still run

The common disaster isn't a dead machine. It's a deleted folder: a stack manager or a stray `rm -rf` removes `~/docker/<stack>`, and every container keeps running on files that no longer have a path. Tick **Files only** in the restore drawer, for one container or a whole stack, to put back exactly what the folder held:

- your **compose file** and **`.env`**, with the reconstruction beside them (see below);
- the project's **other files**: scripts, READMEs, extra env files (see *What a backup holds* below);
- every **single-file bind** that is missing, such as a key or a licence, with its recorded owner and mode.

No container is stopped, restarted or changed, and **nothing that exists is overwritten**. A file still there is kept as it is, and the log says so.

Files only brings back **no data**. A bind-mounted data folder that was deleted is still held open by its running container, so it looks fine until that container restarts onto an empty folder. The restore names every such folder in its log. Run a full restore for those before the container restarts.

### What a backup holds of the project folder

Besides your compose file(s) and `.env`, every backup of a compose container carries the rest of its project folder (`config/project-folder.tar`). It leaves out what doesn't belong:

- every **bind-mounted path** inside it, because the volume data has its own capture (or was deliberately left out);
- `.git`, `node_modules`, `__pycache__` and `.cache`, at any depth;
- anything listed in a **`.dockbackignore`** in the project folder. It works like `.gitignore`: `*.log` matches at any depth, and `/build` or `cache/data` match from the folder's root.

The capture is capped at 64 MB; a bigger folder is skipped, and the backup log says so. A restore that rebuilds the stack folder (full or files only) writes back only the files that are missing.

## Rebuild the on-host stack folder (compose file + directory)

If you keep each stack in its own directory — e.g. `/opt/docker/<stack>/` holding a `docker-compose.yml` and the app's config folders — a plain recreate brings the **container** back but doesn't reproduce that tidy on-host layout: Docker only makes the exact bind-mount folders it needs, and the compose file stays inside the backup.

Tick **Reconstruct stack folder on host** in the restore panel to fix that. When DockBack recreates the container, it also:

- **creates the stack directory** on the target host,
- **writes your own compose file and `.env` from the backup** into it — or, when the backup holds no original compose file, the one DockBack rebuilds from the container's configuration — and
- owns them to **match the parent directory**, so they sit alongside your other stacks.

**When the parent is brand new, it doesn't match root.** On a fresh host there is no `/opt/stacks` yet, so Docker creates it a moment before DockBack writes — as root, because that is what Docker does. Matching that parent would hand you a stack folder you need `sudo` to edit, on the one machine where you are most likely to be editing it. So when the parent reads as root, DockBack instead uses **the ids this restore is running the container as** — your pinned `uid:gid` if you set one, otherwise the ids the image declares. The parent Docker made is left exactly as it is; only what DockBack itself writes is owned this way. If neither is known, it falls back to matching the parent as before.

It's **opt-in** and deliberately safe:

- It writes **the filename `docker compose` will actually read** — the name this stack was deployed from, or the one already in the folder if that differs. A file whose name has to be passed with `-f` every time is a file that quietly stops being the one anybody edits.
- **Your own file is the one Compose runs.** When the backup holds your original compose file, it's written under that name, and DockBack's reconstruction goes beside it as `docker-compose.dockback.yml`, which Compose ignores unless you pass it with `-f`. Only when the backup has no original does the reconstruction take the name. Then read it before relying on it: it's built from the container's runtime configuration, so `env_file`, profiles and comments aren't in it, and its secrets are moved to the `.env` as `${VAR}`.
- It **never destroys** an existing file. If a different one is already there, it is **renamed aside** to `<name>.pre-dockback-restore-<timestamp>` first, and the log names both paths. If the file already matches, nothing is written or renamed at all — running a restore twice converges instead of leaving a backup behind each time.
- Every file it writes is **readable by its owner only** (mode 600) from the moment it's created, because a compose file or `.env` can hold passwords.
- The target directory comes from the container's own compose metadata (the project's working directory), so it lands exactly where it lived. For a container started **without** compose (a plain `docker run`, so there's no recorded directory), give a **base directory** in the panel and DockBack uses `‹base›/‹container-name›/`. The base is remembered for next time.
- Writes are strictly validated — DockBack refuses system paths (`/etc`, `/usr`, `/var/lib`, …) and anything too shallow to be a real stack directory.

The live log names the exact path it wrote and the ownership it applied. This works per-container, for a whole stack, and for a whole-node restore.

## Remap the machine IP (cross-host)

A service that pins itself to the **old** host's IP address often won't come up on a new machine. The classic case is a published port bound to a specific address — e.g. `10.168.1.10:8080` — which **fails to start** on a host that doesn't own that IP ("cannot assign requested address"). Old IPs also hide in environment variables (advertise/URL settings) and `extra_hosts` entries.

Tick **Remap machine IP** in the restore panel to fix this as part of the restore. When the container is recreated, DockBack rewrites the **source** machine's IP to the **target** machine's IP in:

- published-port host IPs,
- environment variables,
- `extra_hosts` entries, and
- the reconstructed compose file (when you're also reconstructing the host layout).

The two IP fields are **prefilled** from the node addresses (when a node is addressed by IP) and you can edit either — useful when a node is reached by hostname, so its IP isn't known automatically. It's **opt-in** and precise:

- Only **exact** matches of the source IP are changed — remapping `10.168.1.1` never touches `10.168.1.10`.
- A port stays bound to a **specific interface** (the target's IP); DockBack never quietly widens a binding to all interfaces.
- Every substitution is written to the restore log, so you can see exactly what changed.

> Set the **target** IP to an address that actually exists on the new host (the prefilled value is the target node's own IP). If you point it at an IP the host doesn't have, the bind will still fail — now with your chosen address.

## Remap stack paths (cross-host)

The same class of problem exists for **folders**: a recreated container keeps its bind mounts' original **host paths**, so restoring onto a machine organized differently silently reproduces the *old* machine's layout — Docker auto-creates directories like `/home/olduser/docker/<stack>/data` on a host whose stacks live under `/opt/stacks`.

Tick **Remap stack paths** in the restore panel to move everything to the new machine's layout in one step. When the container is recreated, DockBack rewrites the old base directory to the new one in:

- every **bind mount's host source** (`/opt/docker/app1/data` → `/opt/stacks/app1/data`),
- the **reconstructed compose file's** volume paths (when you're also reconstructing the host layout), and
- the **stack folder location** itself, so the compose file and the data land together under the new base.

The **From** base is prefilled with the parent of the stack's recorded compose folder; the **To** base with your reconstruction base directory. It's **opt-in** and precise:

- Only **exact prefix** matches move — remapping `/opt/docker` never touches `/opt/dockerx`, a container-side path after a bind's `:` separator, or a path merely containing the base (like `/mnt/opt/docker`).
- **Named volumes are never touched** — they're recreated by name on the target as always.
- The target base is validated against the same **protected system roots** the host reconstruction refuses (`/etc`, `/usr`, `/var/lib`, …) — a remap can never point a stack into a system directory.
- Every substitution is written to the restore log.

With the toggle off, restores behave exactly as before.

## The stack's `.env` moves with it

A compose file that reads `${DOMAIN}` holds no domain — the value is in the `.env` beside it. DockBack captures that file too (on every kind of node, alongside the compose file, into the encrypted archive) and writes it back on a restore **through the same remaps**: the IP, the domain and the path rewrites all apply to it.

This matters more than it sounds. Without it, a cross-host restore recreated the containers with a correctly remapped environment — and then the first `docker compose up` re-read the old `.env` and put the previous address straight back. The pair you run has to agree with the containers, or the containers lose.

Where both files define the same key, **yours wins.** DockBack appends only the values its own reconstructed compose file references and cannot find, under a comment saying it added them.

When a remap is switched on and finds **nothing** to change, the log says so as a **warning**, not a quiet note. On a machine that has just moved, "the old address does not appear" almost never means the address was already right — it means the value reaches the container through a `${VAR}`, so the message names the file to look in.

One limit worth knowing: an archive can only bring back what it captured. Backups taken **before** a stack's `.env` was part of the capture do not carry one, so a restore from an older archive still writes only the generated file. Take a fresh backup of the stack and the pair travels together from then on.

## Restoring onto a new server

1. Connect the new server as a **node** (*Connecting Servers*).
2. Make sure the backup's destination is reachable from DockBack (it usually already is — your offsite copy).
3. Restore the backup targeting the new node; DockBack recreates the container there. Tick **Reconstruct stack folder on host** to also rebuild the `‹stack›/docker-compose.yml` layout on the new machine, and **Remap machine IP** (above) if any service is pinned to the old host's address.

## Whole stacks

To bring back a multi-container application (app + database + sidecars) in the correct order, use *One-click stack restore*.

## Whole node (run the runbook)

When you've lost or are rebuilding an entire host, the **Recovery** page has a **Restore entire node** button per node. It executes the runbook's computed order in one action: it recreates and restores **every** service on that node from its latest backup — **databases first** (each proven healthy before anything that depends on it starts), then the applications — snapshotting each service's current state first so a bad restore can roll back. Progress streams live on the page.

**Rebuilding onto a different machine.** *Restore entire node* recreates everything back onto the node it belongs to — which is not much help when that node is the thing you lost. **Rebuild onto another node…** beside it opens the same plan with a target picker: choose any reachable node and the whole runbook executes there instead, in the same proven order, while the original node is left untouched.

It carries the cross-host options the stack dialog has always had, and for the same reasons: **reconstruct the stack folders** on the new host, **remap the machine IP** (both ends filled in from the two nodes' addresses when you leave them blank — if neither can be derived, the rebuild refuses rather than silently remapping nothing), and **remap the stack paths** to the new machine's layout. Each project's old base directory is read from *its own* recorded layout, because one machine's projects rarely share one; a project that records none keeps its paths rather than failing the rebuild.

A **compose project is restored as one step**, through the same stack path the stack dialog uses — so it keeps its own dependency order, its handling of applications whose services are only meaningful together, and the single merged compose file written at the end. The trade-off is worth stating: ordering is stack-at-a-time rather than every database on the machine before every application. Within a project nothing changes, and each service still passes its health gate before the next starts.

**You see the plan first.** Clicking the button opens the exact sequence the run will execute — every service numbered in restore order, which backup it comes from, how old it is, and whether that backup is verified. Nothing is confirmed until you have read it.

**Anything it cannot restore is named there, not discovered part-way.** A service the whole-node run has no way to handle is marked in the plan with the reason, and the run **will not start** while one is listed. Either restore that service on its own first, or tick **Skip these and restore the rest** — an explicit choice, with the affected services named, and the skipped ones stated in the first line of the run log.

The usual reason is a **write-only** backup: it is sealed to your offline keypair. Paste that key into the **Offline private key** box the plan shows and those services stop being blocked and join the run; leave it blank and they are simply left out. The key is used for that one run and is never saved, logged, or written to the audit trail — which records only that a key was supplied. The other is a database whose recorded environment could not re-initialise an empty data directory — the same check the single-container restore makes before it wipes anything.

Because a restore is destructive it takes each affected stack's exclusive lock for the duration, so it can't overlap a backup or another restore of the same stack (you'll get a clear "already in progress" message if one is). If any container on the node is marked **protected**, the run asks for your password before it starts. If a service fails to come up healthy, the run **stops** at that service — the ones already restored stay up, the rest are left untouched — and raises a high-priority alert, so you never end up with apps started against a database that didn't come back.

## Network topology is restored, not reinvented

A recreate used to put the container on a **bare bridge network**: whatever custom networks it was attached to were created with a default subnet and nothing else. Three things went missing, none of them visibly:

- the **subnet**, so an address a service had pinned in its own configuration no longer existed on the network it came back on;
- the container's **static IP**, so anything referring to it by address broke;
- the **`internal`** flag, so a network deliberately cut off from the outside came back with internet access — a silent loss of isolation.

DockBack now records each attached network's full definition at backup time — driver, subnet, gateway, IP range, `internal`/`attachable`/IPv6 flags, driver options and labels — along with **this container's own address and DNS aliases**. On a disaster-recovery restore those networks are recreated with their recorded properties, and the container is reattached at the address it had.

You can see what was recorded on any backup: open it and look for **Networks** in the Manifest panel.

### What is deliberately not done

**An existing network is never modified.** If the network already exists on the target host but differs — a different subnet, say — DockBack leaves it exactly as it is and writes a warning naming each difference. Changing a live network's subnet would disrupt every other service on it, and restoring one container has no business doing that. Recreate it by hand if the difference matters.

**A pinned address that cannot be honored falls back rather than failing.** If the recorded IP is already taken, or outside the subnet this host's network uses, the container is attached with a dynamic address and the log says so:

```
Network: could not reattach stack_backend at 172.20.0.5 (…) — connected with a dynamic address instead
```

A container that is *reachable but at the wrong address* is recoverable; one that never came back is not. The warning is there so you know to correct it.

**Only pinned addresses are restored.** A container that took whatever address Docker handed it stays dynamic — recording the lease it happened to hold would make it fail to restore the moment that address was in use.

**Isolated clones get none of this.** A clone deliberately stays on its throwaway network and never claims the original's addresses.

### A logging driver the target host does not have

A container is recreated with the logging driver it was captured with. If that driver is a plugin the target host has not installed — or a name that daemon simply does not know — the create is refused outright, and in a stack restore that ends the whole run at whichever service hits it first:

```
Service "db" failed: recreate container: creating container "Wiki.js-DB":
error looking up logging plugin db: plugin "db" not found
```

That trade is wrong in an obvious direction. A logging driver decides where the container's stdout is written; it has nothing to do with its data, and the daemon's default works everywhere. So DockBack drops it, recreates the container with default logging, and says exactly what it dropped:

```
this container was captured using the "db" logging driver, which this host cannot
provide — it was recreated with the daemon's default logging instead. Its data is
unaffected and nothing else changed.
```

Install the driver on the new host and recreate the container if its logs need to keep going to the same place. The restore plan also flags a non-default logging driver **before** you start, under the service that uses it.

### Your compose file, and the reconstruction beside it

At backup time DockBack reads the compose file(s) the stack was deployed from, and the `.env` beside them, straight from the host, on every kind of node. Both go into the encrypted archive. When the restore rebuilds the stack folder:

- **`docker-compose.yml`** (or whatever name the stack was deployed from) is **your own file**, byte for byte, with only the remaps you asked for applied. It's the file `docker compose` runs.
- **`.env`**, mode `600`, is **your own `.env`**, your lines first and unchanged. DockBack appends only values the reconstruction needs that you don't define, under a labelled comment. Usually that's nothing.
- **`docker-compose.dockback.yml`** is DockBack's reconstruction from the containers **as restored**, for comparison. Compose ignores it unless you pass it with `-f`.

Every file follows the same rules. An identical file is left untouched, so re-running a restore converges. A different one is renamed aside with a timestamp, never deleted. Every file is created mode `600`.

Every compose file the restore writes is then **checked with Docker Compose** itself, in a short-lived helper container on that machine, with the folder exactly as it now is. The log gives one of three answers:

- **valid, and `docker compose up -d` will change nothing.** Every service's configuration matches what its running container was created from.
- **valid, but `up -d` would recreate** the named services (their configuration differs), or create ones with no container yet.
- **not valid**, in Compose's own words, such as a missing `env_file`. Your own file is still written as it was backed up, so you can fix it.

Anything Compose warns about, such as a variable that isn't set and would become blank, is quoted in the same line. The helper is the official Docker CLI image (a 67 MB download), pinned to one version for every machine. DockBack fetches it when it's needed and removes it again afterwards, unless the machine already had it. If it can't be fetched, the log says the files went unchecked, and the restore carries on.

When the backup holds **no** original compose file (it couldn't be read at backup time), the reconstruction takes the compose file's name instead, but only if Docker Compose accepts it. One that Compose rejects goes beside the folder as `docker-compose.dockback.yml` for you to fix, and the compose file already there (if any) is left alone. A restore that covers only part of a project, because services were left out or have no backup, writes your own files and never a reconstruction of the part.

### What the reconstruction is

The reconstruction is synthesized from `docker inspect`: a faithful record of what was **running**, not of what you wrote.

- **Values are resolved.** Secrets are moved to the `.env` as `${VAR}`. Every other `$` is written as `$$`, so Compose reads it literally rather than blanking it.
- **The image is pinned to what actually ran.** Image defaults (`PATH` and the like) are left out unless the container overrode them.
- **The stack's own networks** are declared under the names Compose gave them, with their recorded subnets. **Networks it only joins** (a proxy network another stack owns) are `external: true`, so Compose joins them instead of creating copies.
- **Named volumes** are declared `external: true` by their real names. After a restore the data is already in them.
- **Not reproducible from a running container:** `pull_policy`, `env_file`, profiles, `build` and comments. Anything else the container runs with that Compose can't express, such as a non-default logging driver, is listed on its service under `x-dockback-not-written`.

It's a valid Compose file for the cases DockBack's own end-to-end drill checks. Still, read it before relying on it, especially on another host.

**Updating a side container** (tika, gotenberg, a database) is the ordinary compose workflow either way: change the tag, run `docker compose up -d`, and only that service is recreated.

## NFS- and CIFS-backed volumes keep their backing store

A named volume created with the `local` driver and NFS options — `type=nfs,o=addr=…,device=:/export` — used to be recreated on restore as a **plain empty local volume**. The restored data then went to local disk instead of the NAS, with nothing to indicate it.

DockBack now records each named volume's **driver options and labels**, and creates the volumes with those options **before** recreating the container. That ordering is the fix: Docker auto-creates a missing named volume as a plain local one the instant a container references it, so anything done afterwards is too late.

The Manifest panel shows the driver type beside each volume — `app_data (nfs)` — so you can see what a restore will need before you start one.

### Credentials are deliberately not stored

CIFS/SMB volumes usually carry a password in their options (`o=username=backup,password=…`). DockBack **does not record it**.

The reason is where the manifest goes: the copy written beside every archive is **readable by default**, so it travels in the clear to S3, SMB, WebDAV and SFTP. A password recorded there would be exposed on every destination the backup reaches. Everything else in the mount specification *is* kept — the share, the NFS version, the user id — so only the secret itself is missing.

When a volume's options included a credential, the backup lists it under **Volume credentials** and the restore **refuses to create that volume**, with a message naming it. Create it by hand with its full options first, then restore — DockBack will then use it. Half-creating it would produce a volume that fails to mount, or quietly falls back to local disk, which is the failure this whole feature exists to prevent.

### An existing volume is never modified

If the volume already exists but with different options, the restore reports the difference and leaves it alone:

```
Volume: volume app_data exists with different driver options
(device recorded as ":/export/app", host has ":/export/other")
— restoring into it would write to the wrong backing store
```

Docker offers no way to change a volume's options, and silently reusing one backed by different storage is exactly the wrong-place failure being fixed. The restore continues so the rest of the container comes back; that warning is what tells you which volume needs attention.

Backups taken before this feature recorded no options and restore exactly as they did before.
