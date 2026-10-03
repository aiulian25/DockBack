# How restore works

Restore brings a backup's state back into a container. It's designed to be safe, resilient, and predictable.

## What the confidence grade means

Every successful backup shows a single **Restore confidence** grade — one letter that answers *"will this restore?"* instead of making you assemble six separate signals by hand. Open a backup to see exactly **what's missing** and one-click actions to fix each gap.

| Grade | Meaning |
|---|---|
| **A** | Verified, **drill-proven**, has a healthy **offsite** copy, and complete. Nothing to improve. |
| **B** | Verified, offsite, and complete — but **never drilled**, so a real restore hasn't been proven yet. Run a drill to reach A. |
| **C** | Verified and healthy, but **incomplete**: it's **local-only** (no offsite copy) or a **PARTIAL** capture (some data mounts were skipped). A PARTIAL backup can't score higher than C. |
| **D** | Verified, but a copy or check **failed**: an intended **offsite copy didn't upload**, or the **last restore drill failed**. |
| **F** | **Do not rely on it**: **unverified**, **failed verification/scrub**, or **encrypted with a key you no longer have** (key mismatch). A key mismatch is always F, no matter how good the other signals are. |

The grade is computed instantly from data already on the backup row — verification, the latest drill, offsite copy status, partial-capture flags, and key match — so browsing the Backups list stays fast (no restore is actually run to compute it). The drawer's **Restore confidence** section lists each gap with a button: **Run drill**, **Send offsite**, or **Verify now**; a PARTIAL capture is raised by re-backing up with the skipped mount included.

## An unverified backup is not a dead end

A backup that has never been verified — or whose last check **failed** — used to leave the Restore button greyed out with nothing to click and no explanation. The way out existed (press **Verify now**, wait, come back) but nothing said so.

Now the button reads **Verify & restore**. One confirmation covers both steps: DockBack re-reads and re-checks the archive, and starts the restore by itself only if it passes. If verification fails, **nothing is restored** and the report says what went wrong.

The refusal is enforced by the server, not only the screen. A backup whose **last verification failed** is refused by the restore API itself, and proceeding takes a separate, explicit acknowledgement — recorded in the audit trail as an override, because "why is this container full of corrupt data" is a question answered there or nowhere. A merely **unverified** backup is not refused: an imported backup is unverified by definition, and a recovery that turns away the only copy on the shelf is worse than one that checks it first.

## Safe by default (confirm + reversible)

Restore is destructive, so it has three guards:

- **Explicit confirmation** — the action always requires a deliberate confirm (the server rejects any restore that isn't explicitly confirmed).
- **Snapshot current state before overwrite** — on by default. Before overwriting, DockBack takes a **local, verified backup of the current state**, so if the restore turns out to be wrong you can **roll back** by restoring that snapshot. If the snapshot can't be created, the restore is **aborted** rather than destroying data without a recovery point. You can untick it for a faster restore when you don't need the rollback. This applies to a **standalone volume** restore as well as a container one — see *Backups → Concepts → Orphaned volumes* for what a volume rollback puts back.
- **Your password, for a protected container** — see below.

### Confirm your password before overwriting

Downloading a decrypted backup asks for your password. Revealing the encryption key asks for your password. Overwriting the live data those backups exist to protect did not — it was a confirm checkbox and a wipe.

For a **protected** container, an in-place restore now asks for your password (and two-factor code, if enabled) before anything is touched, the same step-up used everywhere else key material is involved. The check happens **after** every other gate, so a restore that would be refused as incompatible is refused without asking you for anything.

It is **on by default** for containers where an accident is unrecoverable:

- one whose data is marked **critical** (low-RPO protection), and
- one marked **never back this up without write-only encryption**.

It is **off by default everywhere else**, deliberately. Restoring is a routine operation, and a prompt on every restore is a prompt nobody reads by the time it matters.

Either way you decide: the checkbox **Confirm my password before overwriting this container**, in the container's *Backup options*, turns it on for anything you like and off for a container that defaults on. Both directions are recorded in the audit trail.

**It applies however you reach the overwrite.** Restoring a whole **stack**, or **every service on a node** from the DR runbook, overwrites exactly the same live data — so if any member of that set is protected, those restores ask for your password too, before the first service is touched and before any lock is taken. Otherwise the wider button would have been the way around the narrower one's guard, which is the wrong way round: a stack restore destroys more, not less.

You are asked **once** for the whole operation, not per service, and the prompt names the members that required it. The audit row records the same *(protected: step-up ok)* note the single-container restore does, naming them again. A stack or node with nothing protected is unchanged — no prompt, one click, exactly as before.

The check resolves against the node the restore **lands on**, because only what is there can be overwritten. A container marked critical on its original machine and restored onto a fresh one needs no password: there is nothing on the target to destroy.

> **Restore as a copy is never gated.** It creates a new container beside the original and touches nothing that exists, so the worst outcome of a mistaken click is a container to delete. The safer path is deliberately the faster one.

> Restores are **in-place** (same container/volumes, same or a chosen different host). DockBack doesn't restore into a parallel namespace; the pre-restore snapshot is what makes a bad restore reversible.

## Automatic rollback if it doesn't come up healthy

After the data is back in place and the container is started, DockBack **waits for it to come up** — healthy according to its healthcheck, or simply running if it has none. If it never becomes healthy within the wait (a crash-loop), the restore is **not** reported as success:

- **With the safety snapshot on** (the default), DockBack **automatically rolls back** to the pre-restore snapshot, returning the container to its prior state, marks the restore **failed** with *"restored data did not come up healthy — rolled back to the pre-restore snapshot"*, and sends a **critical notification**.
- **With the snapshot off**, there's nothing to roll back to, so the container is left as-is and the restore is reported as **not healthy** (rather than a silent success) so you can investigate.

A container with no healthcheck that stays running is treated as healthy — a good restore is never rolled back just for lacking a healthcheck. Because the outcome now waits for the container to actually come up, a restore of a slow-starting app may take a little longer to report **complete**.

**How long it waits (5 minutes by default) is configurable.** Set the global wait in *Settings → Performance & tuning* (**Restore health timeout**), and a **per-container override** on the container's page — so a heavy app whose first boot runs a long database migration isn't force-rolled-back mid-migration. The per-container value wins over the global one.

## Cancel a running restore

A restore you no longer want to wait for — most often one sitting on *"Verifying the restored container comes up healthy…"* for a container that clearly won't come up — can be stopped: press **Cancel restore** in the restore progress panel. It works the same for a single service, a whole stack, and a whole-node restore.

Cancelling is deliberately **not** an undo:

- DockBack stops at the **next safe point** — it never interrupts a half-written file or a database import mid-statement, and it never starts another service.
- **Nothing is rolled back automatically.** Data already restored stays restored; that is almost always what you want when you cancel a health wait (the restore itself succeeded — it's the app that isn't coming up).
- The log states exactly what was left where, and **names the pre-restore safety snapshot** when one was taken, so you can undo deliberately by restoring it.
- For a stack or node restore, the services already restored stay up and the remaining ones are **not touched** — the log says how many of how many were done.

The outcome is reported as **canceled**, not failed, and the cancel is recorded in the audit trail.

### Closing the tab doesn't lose the restore

A restore runs on the server, not in your browser, so it keeps going if you reload, navigate away, or close the tab. Reopening the **Backups** page finds it again: an *"Restore in progress"* banner names what is running and how long it has been going, and **Open progress** takes you to the live panel — with its log so far and a working **Cancel restore** button.

Nothing has to be reconnected by hand, and there is no window where a destructive operation is running with no way to stop it.

## Integrity-checked with fallback

Before any destructive step, DockBack reads the chosen copy and confirms its ciphertext **SHA-256 matches the manifest**. If the chosen copy is unreachable or corrupt, it automatically **falls back** to the other locations (local first, then offsite) until it finds a good one. A restore never begins from a copy it couldn't verify.

## Stack-exclusive while it runs

A restore takes the target's **stack exclusively** for its duration, so it can never overlap a backup of the same stack (which would otherwise read half-overwritten volumes) or a second restore. If you start a restore while a backup of that stack is still running — or another restore of it is in progress — DockBack refuses with *"a backup or restore of this stack is already in progress"*; wait for it to finish and try again. Backups of a stack's services still run **concurrently** with each other; only a restore is exclusive. The same container is also never backed up twice at once, so overlapping schedules never duplicate work.

## Lifecycle-aware

Restore handles the container lifecycle for you:

- For **app/volume** restores, the container is stopped for a consistent write, the data is restored, then it's started again.
- For **database** restores, the engine is stopped, its data directory is re-initialized fresh, and the **consistent dump** is imported over a local connection — so you get healthy data, not raw files.

### One thing a database container needs before it can be re-initialized

Re-initializing means the data directory is emptied and the engine builds a new one — and a database image **refuses to do that without a root password in its environment**. MariaDB and MySQL want one of `MYSQL_ROOT_PASSWORD` / `MARIADB_ROOT_PASSWORD` (or the `_HASH`, `ALLOW_EMPTY_` or `RANDOM_` variants); PostgreSQL wants `POSTGRES_PASSWORD` or `POSTGRES_HOST_AUTH_METHOD`.

Plenty of working containers do not have one. If the data directory was created years ago, the engine never asks again, so a compose file that sets only the application's own user runs perfectly — right up until the moment something empties that directory.

DockBack checks for this **before** it empties anything, and refuses the restore if the variable is missing, naming what to add. You will also see it in the restore drawer and in the stack restore plan, marked against the service it belongs to, before you start.

The value is yours to choose and your application never uses it: it sets root on the rebuilt engine, while the application's own user and password are recreated from the container's environment exactly as they were. Add it to the container, recreate it, and restore again.

## Who owns the restored files

A restore writes files with the **numeric** ids they had when they were captured, which is what makes a restore work across machines that have entirely different user accounts. Numbers mean the same thing everywhere; names do not.

That is right until the numbers themselves change — and moving off a NAS is exactly when they do. A Synology share is owned by an account like `1026:100`; on an ordinary Linux host the same application usually runs as `1000:1000`. Restore the data unchanged and every file belongs to a user that does not exist there, and the application cannot write to its own directory.

**DockBack aligns it automatically when the image says which user it runs as.** It reads the ids from the **target** container — not from the backup — because editing them is the whole point of the move, and it recognises the conventions images use to announce them: `PUID`/`PGID`, `USERMAP_UID`/`USERMAP_GID`, `UID`/`GID`, `USER_ID`/`GROUP_ID`. When those differ from what the data carries, the restored paths are chowned to match and the run log says so.

**When the image announces nothing, set it yourself.** Plenty of images run as a user baked into them — `www-data`, or a numeric `USER` in the Dockerfile — and announce no ids at all. There is nothing for DockBack to read, so nothing is aligned.

On the container's page, **Restore data owned by** takes a `uid:gid` and pins it for that container. It overrides what the image declares when both are present, and it is the only thing that applies when nothing is declared.

**It changes the container too, not just the files.** If the image declares a user-mapping pair — `USERMAP_UID`/`USERMAP_GID`, `PUID`/`PGID` — the recreated container gets the pinned values, because the alternative is worse than doing nothing: the application would drop to the id it was carrying from the old machine and then be unable to write the files that had just been handed to the new one. Both halves move together. An image with no such pair keeps the user built into it, and only its files are aligned.

- **Numeric only.** Names are resolved against a passwd file *inside* the container, which differs on every image and does not exist at all on a distroless one.
- **Per container, which is what a stack needs.** A database running as `999` and the application in front of it running as `1000` need different answers; one setting for a whole stack would be wrong for at least one of them. A stack restore reads each service's own pin.
- **Nothing happens by default.** Leave it blank and DockBack behaves exactly as before.

If the ownership change fails, the restore still succeeds and the log prints the `chown` to run by hand — the data is back either way.

## Revert a bad update

A normal restore puts a backup's **data** back into the container as it is now — it doesn't change the running image. When an image update itself is what broke things, tick **Revert update** in the restore drawer. DockBack then **recreates the container from the backup's saved image** (pinned by digest when available), rolling the container back to the exact version that was running when the backup was taken, and restores its data on top. Combined with the default safety snapshot, a bad upgrade becomes a one-click, reversible roll-back.

## Restore as a copy (bring a backup up alongside the original)

Sometimes you don't want to overwrite the running app — you want to **look at** a backup: inspect old data, test whether an upgrade migrates cleanly, or pull one record out. Tick **Restore as a copy** in the restore drawer and give it a **new container name** (e.g. `paperless-restored`). DockBack brings the backup up as a **separate, isolated container**:

- It gets its **own fresh volumes** filled from the backup — it never mounts the live container's data, so the original is **completely untouched**.
- It **publishes no host ports** and joins a throwaway **`dockback-restore`** network, so it can't clash with the running app on a port or interfere with the live stack.
- Because it's isolated from its usual dependencies (its database, other services), it may not go fully "healthy" — that's expected for a copy, so DockBack doesn't treat it as a failed restore.

Reach the copy with `docker exec`, or attach it to a network to browse it, then remove it when you're done. This is the safe way to migrate data or trial a restore without any risk to what's running.

### Test restore — the one-click version that cleans up after itself

**Test restore**, beside the Restore button, is the same isolated copy without the paperwork. DockBack names it `<container>-test-<MMDD>` (with `-2`, `-3` when you test the same backup twice in a day), isolates it exactly as above, and — the part that matters — **removes it automatically**, together with the volumes Docker created for it. The default lifetime is **24 hours**, adjustable in *Settings → Performance & tuning*.

The expiry is written onto the container itself, not into DockBack's database. So the clock keeps running if DockBack is restarted, restored from its own backup, or pointed at that node for the first time — the host can always answer "what did you leave here, and when does it go". Nothing accumulates because a record was lost.

The drawer lists the test clones currently on that node with the time left on each, and a **Delete now** if you're finished early. Because these copies are DockBack's own and temporary, the rest of the app ignores them: a whole-node backup schedule skips them, and they never appear as unprotected containers to go and protect.

A copy you name yourself with **Restore as a copy** is yours — it has no expiry and nothing sweeps it.

> A **write-only** backup works here too: paste the offline private key in the drawer first, and Test restore uses it exactly as a normal restore would.

For a whole compose project, the same option turns **Restore stack** into **Revert stack update**: every service is recreated from its latest backup's image, in dependency order (database first, then the app), so an update that broke the stack is undone in one action.

### Which version a revert lands on

A revert restores the **image that was running when the backup was taken**, resolved as precisely as the backup allows. The drawer shows this up front as a **Revert pin** and confirms it when you tick the option:

- **Digest-pinned** — the backup recorded the image's `sha256:…`, so revert re-pulls that **exact** image even if the tag has since moved. This is the normal case for any image pulled from a registry.
- **Image bundled** — you enabled *Also save the container image*, so the exact image is inside the archive and restores with **no registry at all** (works fully offline).
- **Tag only (may drift)** — the backup has no pinned digest (typically a locally-built image that was never pushed). Revert can only re-pull the recorded **tag**, which may now point to a different version — so it may not truly roll the update back. For images on a floating tag like `:latest` or `:release`, this is flagged with a warning; enable *Also save the container image* for an exact, offline-proof revert.

Because a revert targets the version captured at backup time, keep a backup from **before** an upgrade if you want to be able to roll that upgrade back.

## Disaster recovery built in

If the target container no longer exists, DockBack **recreates it from the manifest** (image by digest, env, ports, mounts, networks, restart policy, healthcheck) before restoring its data. See *Disaster recovery*.

## Live feedback

Every restore streams progress to the drawer with a clear success or failure result, so you always know what happened.

## One click, from the container's own page

The common restore is not a decision — it's *put yesterday back*. The container page has a **Restore…** button beside its list of backups for exactly that: the newest **verified** backup, back onto the same container, on the same host, with the safety snapshot taken first. Two clicks, no page to leave.

The card shows what it is about to do before you confirm it: which backup (its age, size, and verified chip), that the restore is **in place** and overwrites live data, and the **restore-readiness** verdict — whether the image is still obtainable, checked when the card opens rather than assumed. A protected container says so up front, then asks for your password inline; a compatibility warning from the engine still stops and asks, exactly as it does in the drawer.

It is deliberately narrow. When the restore needs a **decision**, the card says which one and sends you to the full view instead of guessing:

- the newest backup is **unverified** or **failed verification** — a one-click restore would be an unproven one, and the full view offers **Verify & restore** instead,
- it's **write-only encrypted** — it opens only with the offline private key from its recovery sheet,
- it was encrypted with a **key you no longer have** (key mismatch),
- the container's own configuration would **stop** the restore (a database whose environment can't initialize an empty data directory),
- a **backup of that container is still running**.

Anything the card doesn't offer — an older version, another host, a different source copy, an address remap, a throwaway copy — is one click away under **More options**, which opens that backup in the full drawer with nothing lost.

## Where to start

For the newest backup of one container, the fastest path is **Restore…** on the container's own page (above). For anything else, open **Backups**, pick the server, then the backup. The drawer lets you choose the **version** and **source location**, what to restore (volumes/database), and shows the verification report and manifest. See *Restore by version & source location*.

## When a restore doesn't come up: the container's own logs

If the post-restore health gate fails, DockBack no longer just says *"the restored container did not become healthy"* and stop. It pulls the **last 40 lines of the container's own stdout/stderr** and writes them into the run log, so the reason is in DockBack instead of requiring shell access to the host.

This came from a real recovery. A Paperless restore came back unhealthy and the run log said only that it was unhealthy; the actual cause — `ImproperlyConfigured: PAPERLESS_SECRET_KEY is not set` — was visible solely via `docker logs` on the NAS. That excerpt is now captured automatically.

It runs on every non-healthy outcome:

- the container was left as-is (no safety snapshot existed),
- the container was **rolled back** to the pre-restore snapshot — captured *before* the rollback, because the rollback replaces the container's state and takes the evidence with it,
- the restore was **canceled** while waiting for health.

A healthy restore captures nothing; there is no change at all to the success path.

A **failed restore drill** gets the same treatment: instead of only *"container exited on boot (code 1)"*, the result carries the app's own final output.

### What goes where, and why they differ

The **run log** gets the full excerpt, unredacted. It stays on your own instance and is the surface you diagnose from — masking it could remove the very line that explains the failure.

The **notification** (Gotify, email, webhook) gets a shorter excerpt with **credential-looking values masked**, because it leaves the machine to a third-party service and container startup output routinely contains connection strings:

```
DATABASE_URL=postgres://appuser:«redacted»@db:5432/app
ImproperlyConfigured: PAPERLESS_SECRET_KEY is not set
```

The error message itself survives — only values that look like secrets are replaced. The masking is a heuristic and deliberately guards the *outbound* copy rather than being relied on as a security boundary, so treat alert bodies as you would any other notification content.

Reading the logs is best-effort: a container that produced no output, or whose logs can't be read, results in a single note rather than changing the restore's verdict.
