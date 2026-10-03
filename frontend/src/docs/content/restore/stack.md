# One-click stack restore

Real applications are usually several containers — an app, its database, a cache, sidecars — defined together as a Compose **stack**. DockBack can restore the whole stack in one action, in the right order.

## What a stack is here

DockBack groups containers by their Compose project label, so the services that make up one application are recognized as a **stack**. Backups carry the stack name and each service's dependencies.

## Restoring a stack

Restoring a stack recreates/repairs every service and restores its data, respecting **dependency order** — for example the database comes up and is imported before the app that needs it starts. This avoids the classic failure where an app boots against an empty or not-yet-ready database.

You don't need to restore each container by hand (or use a terminal/Portainer to start them) — one click restores and starts the whole app.

## Where to find it

There are two places, both with live progress:

- **Backups page** → open any backup that belongs to a stack → **Restore stack (N services)**. Restoring just that one service is still available as **Restore this service only**. The choices you already made in the drawer — target node, point in time, source copy, remaps, addresses — come with you, so nothing is configured twice.
- **Servers** → open the node → **Stacks** section → **Restore Stack**.
- Or open the stack's own page and use **Restore stack** there.

Each service is restored from its **latest** backup. If a service's container no longer exists, it's recreated from the manifest (disaster recovery) before its data is restored.

Both places open the **same page**, with the full option set: target node (cross-host), point-in-time snapshot group, source copy, per-service selection, revert update, safety snapshot, host reconstruction, and the IP / domain / stack-path remaps.

**It is a page, not a dialog.** The restore plan — every service, in the order it will run, with what cannot run and why — is the thing you must read before agreeing to something destructive, so it gets the width and sits beside the options rather than below them. The page has its own address, which means you can link to it from a runbook, open it on a second screen, and reload it without losing your place. The confirm sits in a bar at the bottom that states what is about to happen in the same sentence as the button.

> A stack with a **write-only** member asks for the offline private key on this page, where it is used. It is deliberately not carried over from the Backups drawer: passing key material between screens would mean putting it somewhere it can be read, and one paste is a small price for it never being written down.

## The restore plan

Before you confirm, the dialog shows a **plan** — computed by the exact same selection and ordering the restore executes, so what you see is what will happen:

- every service in **restore order** (databases first), numbered;
- which backup each restores from (age, **Verified** state, **Partial**, **delta** and **write-only** chips, snapshot-group membership);
- with host reconstruction on, the exact **target folder** per service — including the effect of a stack-path remap;
- services that have **no backup**, listed as *will be skipped* — a restore can only recreate what was captured, so these would simply be absent afterwards.

That last list comes from DockBack's own cached inventory of the source machine, not from asking that machine live. It is the difference that matters in a real recovery: a node that is down answers nothing, and a dialog that asked it would have shown an empty gap list — looking complete in exactly the situation the warning exists for.

The confirm button stays disabled until the plan has loaded, and the plan refreshes as you change options. A point-in-time group that can't cover every service is refused **here**, before anything is touched, with the same error the restore itself would raise.

**It remembers the route.** Moving the same stack to the same machine twice used to mean typing the same domain, the same folders and the same address again — the IP remap could derive itself from the two nodes' addresses, but nothing could derive the rest. DockBack now records what a restore of *this project onto this machine* needed, and offers it the next time you open the dialog for that pair: a line under **Restore to** names what was used last time, and the fields are already filled in.

The switches are **not** ticked for you. Opening a dialog and clicking restore should not rewrite environment values because of something you did weeks ago, so the values are offered and you decide what still applies. Only empty fields are filled — anything you have already typed is left alone.

What is remembered is hostnames, IP addresses and folder paths, stored the same way the reconstruction base folder always has been. The **offline private key is never stored**, for this or anything else; it is used for one restore and discarded.

Choices made in a single backup's drawer carry across too: tick a domain remap there, click **Restore stack**, and the dialog opens with it already set rather than blank.

**Choosing which copy to read from.** Every backup can live in several places — the local disk and each destination it was mirrored to — and by default a restore reads the fastest good one, preferring local. **Source copy** in the dialog pins the whole stack to a specific copy instead, which is the point when the local disk is the thing you distrust: after a ransomware event, a failing controller, or on a host you are rebuilding, *"read every service from the offsite copy"* is the entire request.

Only copies the stack's members **actually hold** are offered, each saying how many services it covers. Members of one project do not always share a destination, so a picker listing every destination you have configured would happily offer one where half the stack has no copy — and choosing it would silently fall back to local for the rest, which is exactly the outcome you were trying to avoid. If the copy you pick does not cover everything, the dialog says so before you confirm, and the run log names the copy each service was read from.

A member with no copy at the chosen destination falls back to its best available one rather than failing, and every copy is integrity-checked against its recorded hash before a restore reads it.

**Restoring only some of the services.** Every plan row has a checkbox, all ticked. Untick the ones you do not want and the rest restore in the same dependency order — the services you left out are not touched at all. This is for the shape recovery actually takes: a stack restore that stopped part-way leaves some services back and some not, and putting the remainder right used to mean finding each of their individual backups on the Backups page one at a time.

Two things behave differently for a partial restore, both deliberately:

- **An application whose services are only meaningful together cannot be split.** Its rows are ticked and disabled, with the reason on hover — restoring half of one gives you healthy containers and a deployment that does not work. DockBack refuses such a subset on the server too, so the checkbox is a courtesy, not the guard.
- **The stack&rsquo;s compose file is not rewritten.** That file describes the whole project, and a restore covering three of its eight services has no business replacing it. Reconstruct the folders on a full restore instead.

A name the stack does not have is refused rather than quietly skipped, so "restore db and cache" never silently restores only `db`.

**Protected members.** If any service in the plan is marked *Confirm my password before overwriting this container* — on by default for containers holding critical data or requiring write-only encryption — the dialog asks for your password before the restore starts, once for the whole stack. See *Restoring → Concepts → Confirm your password before overwriting*.

**Write-only members.** If any service carries a **write-only** chip, its backup is sealed to your offline keypair and DockBack cannot open it. The dialog then shows an **Offline private key** box and keeps the confirm disabled until you paste the key — one key for the whole stack. It is validated against every sealed member before the first service is touched, so a wrong key costs you nothing; if one member disagrees with it, the refusal names that service. See *Security & Operations → Write-only backups*.

## Why order matters

- Databases are restored from their **consistent dumps** and brought to a ready state first.
- Dependent services start only after what they depend on is healthy.
- The result is a coherent, running application rather than a pile of containers that started in a random order.

## Tips

- Back up the **whole stack** (all its services) so every piece is available at restore time — use multi-select or schedule the node/stack (*Creating Backups → Back up many containers at once*).
- For a single service, a normal per-container restore (*Restore by version & source location*) is all you need.
