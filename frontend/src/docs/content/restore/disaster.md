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

1. **Re-pulls the image by digest** (the exact image that was running, from the manifest).
2. **Recreates the container** with its saved configuration — env, ports, mounts/volumes, networks, restart policy, healthcheck.
3. **Restores the data** (volumes and/or the consistent database dump) into the recreated container.
4. **Starts it.**

The result is the container running again with its original configuration and data.

## Is the image still available?

A recreate re-pulls the container image by its recorded digest (or tag). If that image was deleted locally and its tag or registry is **gone** — and you didn't bundle the image — the recreate fails partway. DockBack surfaces this **before** you rely on the backup:

- **On a backup** (Backups → open a backup → Manifest), **Check restore readiness** verifies the image is obtainable right now — present locally, still pullable by digest/tag, or bundled — without pulling anything. A green *Image ready* or an amber warning tells you where you stand.
- **In the runbook** (Recovery), any service whose image is **only referenced by a tag and isn't bundled** is flagged *image not bundled*, because a restore then depends on that tag still existing in its registry.

The fix is the existing **Also save the container image** option: re-back up with it enabled and the image travels inside the archive (`image.tar`), so the restore needs no registry at all and works fully offline. A backup made with that option is always ready.

## Using it

Just restore the backup as usual (*Restore by version & source location*). If the target doesn't exist, DockBack detects that and recreates it automatically — the live log shows "recreating from backup (disaster recovery)".

## Rebuild the on-host stack folder (compose file + directory)

If you keep each stack in its own directory — e.g. `/opt/docker/<stack>/` holding a `docker-compose.yml` and the app's config folders — a plain recreate brings the **container** back but doesn't reproduce that tidy on-host layout: Docker only makes the exact bind-mount folders it needs, and the compose file stays inside the backup.

Tick **Reconstruct stack folder on host** in the restore panel to fix that. When DockBack recreates the container, it also:

- **creates the stack directory** on the target host, and
- **writes the reconstructed `docker-compose.yml`** into it (the same functional compose DockBack rebuilds from the container's configuration),
- owning both to **match the parent directory**, so they sit alongside your other stacks.

**When the parent is brand new, it doesn't match root.** On a fresh host there is no `/opt/stacks` yet, so Docker creates it a moment before DockBack writes — as root, because that is what Docker does. Matching that parent would hand you a stack folder you need `sudo` to edit, on the one machine where you are most likely to be editing it. So when the parent reads as root, DockBack instead uses **the ids this restore is running the container as** — your pinned `uid:gid` if you set one, otherwise the ids the image declares. The parent Docker made is left exactly as it is; only what DockBack itself writes is owned this way. If neither is known, it falls back to matching the parent as before.

It's **opt-in** and deliberately safe:

- It writes **the filename `docker compose` will actually read** — the name this stack was deployed from, or the one already in the folder if that differs. A file whose name has to be passed with `-f` every time is a file that quietly stops being the one anybody edits.
- It **never destroys** an existing compose file. If one is already there, it is **renamed aside** to `<name>.pre-dockback-restore-<timestamp>.yml` before the reconstruction takes the canonical name, and the log names both paths. If the file already matches, nothing is written or renamed at all — running a restore twice converges instead of leaving a backup behind each time.
- **Read the reconstruction before relying on it.** It is built from the container's runtime configuration, so its environment values are *resolved*: a password your own file referenced as `${VAR}` from a `.env` is written out in full, and `env_file`, profiles and comments are not carried over. The displaced file is right beside it to compare against.
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

A compose file that reads `${DOMAIN}` holds no domain — the value is in the `.env` beside it. DockBack captures that file too (over SSH, alongside the compose file, into the encrypted archive) and writes it back on a restore **through the same remaps**: the IP, the domain and the path rewrites all apply to it.

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

### Why it is reconstructed and not copied

The compose file DockBack writes is **synthesized from `docker inspect`**, not copied from the one you wrote. It is worth being plain about why, because the difference shows.

DockBack talks to the Docker API — and, in the recommended deployment, only through a socket proxy. Your compose file lives on the **host filesystem**, outside Docker entirely. Nothing DockBack sees at backup time includes it: what the API can describe is the container that compose produced, not the document that produced it.

So the reconstruction is a faithful record of what is *running*, which differs from what you wrote in ways that matter when you read it:

- **Values are resolved.** A `${PAPERLESS_DBPASS}` that came from your `.env` appears in full. The reconstruction carries real secrets and should be treated as such.
- **The image is pinned to what actually ran** — `:3.0.0`, not `:latest`.
- **Everything the image contributed is present**: its `PATH`, its build labels, its entrypoint. Compose never mentioned those; the container has them.
- **Comments, anchors, `env_file:`, profiles and `depends_on` conditions are gone.** They are instructions to compose, and compose consumed them.

It is a working file — `docker compose up` with it reproduces the container — but it is a *description of the result*, not your source document. Keep your own compose file in version control; treat this one as the record of what was running when the backup was taken.

### What lands at the stack's root

The reconstruction writes the layout you would keep by hand — everything at the stack directory's root, beside the data folders:

- **`docker-compose.yml`** (or the flavour of the name the folder already uses), built from the containers **as restored** — so the address you typed in the dialog, the remapped IP and paths, and the rewritten user-mapping ids are all in it. It carries **no secrets**.
- **`.env`**, mode `600`, holding the secret values the compose file references as `${VAR}` — the shape you would keep in version control (the compose) beside the file you would not (the `.env`). Which values count as secret is decided by name: passwords, keys, tokens, salts. A value the dotenv format could misread stays inline rather than round-trip broken.

Both follow the same rules: an existing different file is renamed aside with a timestamp, never deleted; an identical one is left untouched, so re-running a restore converges instead of accumulating backups.

### Living with it, or replacing it with yours

Both are fine, and the difference matters most at the next **image upgrade**.

**If you keep the reconstruction and edit it**, changing an address, a port or a bind path is safe — those are your values in the first place. The one to watch is the image's own environment, which the reconstruction pins because the container had it: `PATH`, `PYTHON_VERSION`, `GPG_KEY` and friends came from `paperless-ngx:3.0.0`, not from you. Change the tag to `3.1.0` and compose will hand the new image the **old image's** `PATH`. Usually harmless; occasionally the reason a working upgrade breaks for no visible reason. Before bumping a tag, delete the variables you did not set yourself — anything you would not have typed belongs to the image.

**If you put your original compose back**, that is the better long-term answer and it costs nothing structural. Drop it in, run `docker compose up -d`, and compose reconciles: containers whose configuration now differs are recreated, the rest are left alone. Your named volumes and bind mounts are matched by name and path, so **the restored data stays where it is** — a recreate replaces the container, never its volumes. Check three things first, because these are what the restore may have changed underneath it:

- the **image tag**, if you pinned `:latest` and the restore brought back a digest;
- **`USERMAP_UID` / `PUID`** and the like, if you changed the ids for this machine;
- the **address** variables, if you supplied a new one during the restore.

Restoring again afterwards will displace your file a second time — DockBack renames it aside rather than deleting it, so nothing is lost, but expect to put it back. A restore is not a routine operation; a compose file you maintain is.

**Updating a side container** — tika, gotenberg, a database — is the ordinary compose workflow either way. Change the tag, `docker compose up -d`, and only that service is recreated.

### Reconstructed compose files

The compose file DockBack reconstructs now declares real network definitions — subnet, `internal`, driver — instead of marking every network `external: true`. So `docker compose up` on a fresh host reproduces the topology rather than inventing a default bridge. Backups taken before this feature still emit `external: true`, which remains the only safe assumption when nothing about the network was recorded.

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
