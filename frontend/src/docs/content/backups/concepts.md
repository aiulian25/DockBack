# What a backup captures & verification

A DockBack backup is a single, self-describing, encrypted archive of everything needed to bring a container back.

## What's inside

- **`config/inspect.json`** — the container's full configuration (image + digest, env, ports, mounts, networks, restart policy, healthcheck). This is what lets DockBack **recreate** a container that no longer exists.
- **`config/docker-compose.yml`** — a **reconstructed**, human-readable Compose file built from the container's runtime configuration. It's a *functional* equivalent for **restore-by-hand** with standard tools — review image tags, secrets and `external` networks before reuse on a new host.
- **`config/original-compose/…`** — the **genuine** compose file(s) from the host: your actual, commented, hand-tuned original, plus the stack's **`.env`** (stored as `dockback-stack.env`). DockBack reads the exact files named in the container's compose `config_files` label, and the `.env` compose actually interpolates from, and stores them here beside the reconstruction. If a compose file is present, the backup shows an **original compose included** chip, the runbook points you at these files, and you should **prefer them** over the reconstruction when rebuilding by hand. The `.env` matters most on a move: a compose file that says `${DOMAIN}` has no literal to rewrite, so without it a `docker compose up` on the new host puts the old address straight back.

  Capture works on **every transport** and is strictly bounded either way: on an **SSH** node it is a read-only `cat` over the same pinned, authenticated connection used for Docker; on the socket-proxy transports (local/TCP/mTLS) it is a short-lived, unprivileged sidecar with the host filesystem mounted **read-only**. Both take the path **only** from the container's own label and validate it identically (absolute, no `..`, no shell metacharacters), both are **size-capped** (1 MiB), and both are best-effort — a failure just falls back to the reconstruction. These files land **inside the encrypted archive**, never in the manifest that sits beside it, because the `.env` is precisely where the secrets the compose file doesn't carry are kept.
- **Volume & bind data** (`volumes.tar`) — the files in the selected volumes and bind mounts, captured via a sidecar so it works even in a hardened environment. By default named volumes are included and **large bind mounts (e.g. media libraries) are skipped**; you choose exactly what to capture per container (see *Run a manual backup*). **Read-only** binds are ordinary candidates too — a mount being read-only never stopped DockBack reading it, and it is often exactly where a key or a licence file lives.
- **`config/bind-files/…`** — a bind mount whose source is a **single file** rather than a directory, held as its own member. It cannot travel in `volumes.tar`: that archive is extracted in a sidecar where every bind is a live mount, and replacing a bind-mounted file needs an unlink the kernel refuses — which would fail the whole extraction. So its bytes are kept separately and written to the host **before** the container is created.
- **Database dumps** (`db/<service>.dump`) — for database containers, a **consistent logical dump**, not raw data files. See *Database-aware consistent dumps*.
- **App-native export** (`appexport.tar`) — for supported apps, a portable export produced by the app's own tooling. See *App-native exports*.
- **Image tarball** (`image.tar`) — *optional, off by default*. Tick **Also save the container image** to bundle a `docker save` of the image so a restore works **fully air-gapped** (no registry needed) — DockBack `docker load`s it automatically on restore. It makes the backup substantially larger, so it's opt-in per backup.
- **`manifest.json`** — the **versioned contract** that restore is driven from: stack & service, image + digest, the volume list and a **volume-payload checksum**, database engine **and version**, encryption key fingerprint, size, timestamps, and (on the catalog record) the **verification report**. A copy lives inside the archive and beside it. A copy lives **inside** the encrypted archive (confidential), and a readable copy sits **beside** it for hand-restore — the beside copy is **HMAC-signed** (`.manifest.json.sig`) so any tampering with the stored metadata is detected on verification. Prefer not to expose volume/DB names and digests in that beside-copy? Turn on **Encrypt backup manifests** (*Settings → Encryption*): new backups then store a sealed `.manifest.json.enc` instead — confidential *and* tamper-evident.

## Shared bind mounts

One host folder is often mounted into **several** containers — think of a photo app whose `upload/` directory is mounted into both the main server and its machine-learning sidecar. DockBack understands this:

- The folder is **captured once, by its owner** — deterministically, the container that has it ticked in its backup selection (then the read-write mounter, then first by name). A **stack backup** automatically skips it in every other service for that run and logs which service captured it.
- Every other container's backup records the skip as **captured via the owner** — it shows a "backed up via `<owner>`" note instead of the large-bind warning, and it is **not** counted as a Partial backup in the grade, runbook, or digest. It stays listed in the backup's details, with a pointer to restore that data from the owner's backups.
- A skipped folder that **no** container captures is still honestly flagged Partial, exactly as before.

The mount picker shows "also mounted in `<names>`" on any bind shared with other containers on the node, so you always know a folder does double duty.

## Encryption & compression

The archive is compressed with **zstd** (you choose the level) and encrypted with **AES-256-GCM** (`DBACKv1`, chunked) using your master key. Nothing is ever written in the clear.

## Always-on verification

After a backup is stored, DockBack **verifies** it automatically — confirming the manifest is present, the archive is complete (every chunk's GCM tag authenticates), and the ciphertext's SHA-256 matches what was recorded. A backup is only marked **Verified** once it passes. Retention only ever prunes in favour of verified copies, so you never end up trusting a bad backup.

### Deep verification (test-restore)

For the strongest guarantee, enable **Deep verification** in *Settings → Encryption*. After the integrity checks, DockBack **re-imports each database dump into a throwaway, network-isolated container and runs a sanity query** — proving the dump genuinely restores and returns data, not just that it decompresses. This applies to **database containers**; an app or file/volume backup has no dump to test-restore, so its report simply shows *nothing to test-restore* (you'll see this in the live log and the verification report). The throwaway container publishes no ports, mounts no host data, uses ephemeral credentials, and is removed automatically, so it never touches your real container. It's heavier (a DB container is spun up per dump), so it's opt-in; when on, a failed test-restore marks the backup **Unverified**. Each result is recorded as a `db-restore:<service>` line in the verification report.

### Periodic re-verification (scrubs)

Verifying at creation can't catch **bit-rot months later** or a destination that quietly went bad. Turn on **scrubs** in *Settings → Encryption* by setting a re-verify cadence in days (0 = off). On that cadence DockBack re-reads stored backups (oldest-verified first, a few per cycle so it never hammers your disk) and re-runs the full integrity check; a backup that **no longer verifies is flagged Unverified and alerts you** via your notification channels (and trips the heartbeat). Every backup shows **"Last verified <age>"**, and you can press **Verify now** on any backup to re-check it immediately. The metric `dockback_oldest_verified_age_seconds` lets an external monitor watch staleness.

Scrubs are **per copy**. A backup can live in several places — the local archive and one or more offsite destinations — and each copy is re-verified on its own, so a destination that silently rotted is caught even when the local copy is still perfect. In a backup's drawer each copy shows its own **copy verified <age>** or a red **copy failed verification**, with a **Verify this copy** button to re-check just that one on demand. Scrub cycles rotate across every copy (bounded by the same per-cycle limit), and the alert names the exact copy that failed. A backup **adopted** from a destination — with no local copy at all — scrubs its real offsite copy, so it no longer reports a spurious failure.

### Restore drills

Integrity checks prove the *bytes* are intact; a **restore drill** proves the backup actually **restores**. Drills are **per backup** — the same granularity as verification — and live right on the **Backups** page: each backup row shows its last drill result (**Passed / Failed / Never**, with age) after *Created*, with a **Run** button to drill that exact backup on demand. Because a row is already specific to one node and container, there's no ambiguity when the same container runs on several nodes.

A drill test-restores the backup into an **isolated sandbox** — for a **database** backup it spins up a throwaway engine and re-imports the dump; for a **volume/app** backup it extracts the captured volumes into a **fresh throwaway volume** and **boots an isolated copy of the container** from the backup's image and config to prove the app actually comes back. Either way it also checks the archive extracts and its recorded volume checksum matches. It **never touches your live stack**: the throwaway container runs with **no network** (`none`), no host mounts, no inherited privileges, and is removed — along with its throwaway volumes — automatically. Set a **Restore drills** cadence in *Settings → Encryption* (days; 0 = off) to auto-drill on a schedule. Two knobs tune what and how much is drilled: **Coverage** — *Newest backup only* (default) drills each container's latest backup, while *Newest of each week* also drills the newest of each ISO week so **older generations** are proven restorable too; and **Drills per cycle** (1–5) — how many overdue backups run each cycle, to tune throughput to your hardware. How long a drill waits for the throwaway app to come up before judging it (45s by default) is set by **Drill boot wait** in *Settings → Performance & tuning* — raise it for a slow-starting image. Runs are still spread across cycles so there's no load spike. A failed drill raises a high-priority alert, and external monitors can watch `dockback_oldest_restore_drill_age_seconds` and `dockback_restore_drills_failing`.

> A restore drill proves your backup **reconstitutes**: a database re-imports and returns data; a volume/app backup extracts and its container **boots in isolation**. Because the sandbox is fully isolated, an app that needs other services (a database, another container) can't reach them — so a container that **starts and stays running** counts as a pass even if its healthcheck can't turn green without its dependencies. A container that **crashes on boot** fails the drill. Treat a passing drill as "this backup restores and the app starts", which is exactly the proof the original silent-failure problem needed.

## Configuration drift ("changes since last backup")

Every backup captures the container's full configuration — so a backup taken *before* you changed an env var, remapped a port, or moved a mount will restore the **old** configuration. DockBack compares the live container against the newest successful backup's captured config and, when they materially differ, shows an amber **"Configuration changed since the last backup"** banner on the container's page naming what changed (image, environment, mounts, ports, restart policy, command) with a one-click **Back up now** to capture it. The DR runbook adds a matching note for a drifted service.

Only *material* fields are compared — runtime noise (timestamps, container id, network sandbox paths, the injected `PATH`/`HOSTNAME`) never triggers it. Backups taken before this feature carry no comparison fingerprint and never show drift; the indicator starts working from the first new backup. On the runbook (which never probes nodes live), drift detection covers the fields the fleet inventory tracks — image and mounts; the container page checks the full set.

## Orphaned volumes (data with no container)

Deleting a container can leave its **named volume** behind, still full of data but attached to nothing — a backup blind spot, since only container-attached mounts are normally backable. DockBack surfaces these on the **node page** under **Orphaned volumes**: each named volume that no container references, with its size. Click **Back up** to capture one directly — DockBack tars the volume's contents through the sidecar into a normal encrypted, verified backup recorded as **`target_name: volume:<name>`**.

Restoring such a backup **re-creates the named volume** (by its original name) and unpacks the contents back into it — so a volume you'd otherwise have forgotten stays protected and recoverable, even with no container to hang it on. (A volume still in use by a container isn't listed here — back that up through its container instead, so its config and databases are captured too.)

**Restoring over a volume that still has data.** The extract writes *over* what is already in the volume: every file the backup contains replaces the live file at that path, and files added since the backup that aren't in it are left where they are. So a volume restore is destructive in place, exactly like a container restore — and it takes the same precaution. With **Snapshot current state before restoring** ticked (the default), DockBack first captures the volume's current contents as its own local `volume:<name>` backup, and restoring *that* puts back every file the restore is about to overwrite. If the snapshot can't be taken, the restore is **aborted** rather than overwriting data with no way back.

If the volume doesn't exist yet — the usual case, since these backups exist for data whose container is long gone — there is nothing to overwrite and no snapshot is taken. The snapshot is labelled `auto: pre-restore`, so it lives in the separate retention budget for automatic snapshots and never uses up one of that volume's scheduled generations.

A standalone volume backup can't be **restored as a copy** under a different name: it restores into its own volume. Use the container restore for that.

## Locations

Every backup records all the **locations** it lives in (local + each destination it was mirrored to). This is what powers offsite redundancy, restore fallback, and retention pruning across every copy. See *External Backup Destinations → Overview*.
