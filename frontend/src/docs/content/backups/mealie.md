# Mealie & three pieces that must match

**Mealie** in PostgreSQL mode keeps its state in three places that only make sense together: the database (recipes, meal plans, shopping lists, users), the `/app/data` tree (recipe images and two small signing secrets), and the container's environment. Capture them at different moments and you get a recipe whose photo is missing, or a working app that logs everyone out.

DockBack captures them in one window, so they're a matched set.

## Which mode are you running?

Check `DB_ENGINE`. With **PostgreSQL** — a separate database container — the database is dumped logically and `/app/data` is captured as files. With the default **SQLite**, everything is in `/app/data` and gets a consistent snapshot instead. Same backup, different mechanics; DockBack works it out from the container.

## Two 64-byte files decide whether anyone stays logged in

`/app/data/.secret` and `.session_secret` sign Mealie's sessions and tokens. Lose them, or let them be regenerated, and everyone is logged out — the same class of thing as an app key.

They live inside the data directory, so capturing it preserves them, and DockBack never rewrites an application's own files. Nothing to configure; worth knowing so you don't go looking for a separate setting.

## Rotated logs are never backed up

One real Mealie backup was **93 MB, of which ~37 MB was rotated log files** — against about 10 MB of actual recipe images. Logs restore nothing.

So `mealie.log*` and the `.temp/` scratch directory are **always excluded**, with no toggle. That's deliberate: offering a choice would imply there's a reason to keep rotated logs inside an encrypted archive, and there isn't. They cost space, and logs routinely carry request paths and identifiers, so keeping them only widens what a leaked archive would expose.

The archive records what was skipped, so it's a stated decision rather than a silent gap.

> This is different from something like Jellyfin's preview thumbnails, which *do* have value and are merely expensive to rebuild. Those you're asked about. Logs you aren't.

## Mealie's own ZIP export is not the restore path

Mealie can write ZIP exports into `/app/data/backups`, and it's natural to assume those are the backup. They aren't the one DockBack restores from.

Inside each ZIP is `database.json` — a portable JSON dump whose import is **schema-version-sensitive** and has broken across major versions. It's genuinely useful for pulling out a single recipe or moving to a very different version, so keep it if you like. But DockBack captures the database and files **directly**, which is exact and version-matched, and that's what a restore uses.

Those ZIPs roughly double the size of the data half of your archive. Keeping only the newest one, or none, is a reasonable call.

## Moving to another machine

Works on any host — Synology, UGREEN, a Linux box, Windows Docker. Back up **both services together** so the database and the images come from one moment.

- **Remap path** if `/app/data` lives somewhere else. This one matters more than usual: the data directory is often on a **network share** rather than local disk, and a new machine may not have that share. Remap lets you restore it to local disk instead — the path inside the container doesn't change either way.
- **Remap machine IP** if anything is pinned to the old address.

**`BASE_URL` is an environment variable**, read at runtime. Set a new address in the restore panel and DockBack sets it on the recreated container — **nothing in the database is rewritten**, because there's nothing there to rewrite. It only affects the links Mealie puts in emails and shared pages.

> Worth checking yours matches how people actually reach Mealie. If it points at an internal address while everyone uses a public domain, emailed links go to the wrong place. DockBack restores it exactly as captured — correcting it is a change to your deployment, not the restore's business.

**Version: newer is fine, older is refused.** Mealie runs its migrations automatically at startup, forward only, so restoring into an older image is blocked before anything is written. Pin the tag if you'd rather choose when that one-way step happens.

**If you enable single sign-on later**, a move will also mean updating the redirect URI at your identity provider — DockBack can't reach that.

## Proving a restore worked

1. **Log in with the original password.**
2. Recipe, user, group, meal-plan and shopping-list counts match.
3. Open a recipe — **its image renders**. This is the matched-set check: a broken image means the database and the files came from different moments.
4. A second user still sees only their group's content.
5. If you kept a session cookie from before, it still works — proving the signing secrets came across.

## The general rule

When an app's state is split across a database, a file tree and an environment, the backup's job is not just to capture all three but to capture them at the *same instant*. And when one of those pieces is mostly logs, the job is to leave them out — a backup exists to restore an application, and no log has ever restored anything.
