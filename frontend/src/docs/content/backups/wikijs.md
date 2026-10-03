# Wiki.js & a backup that is almost entirely one dump

**Wiki.js** keeps nearly all of itself in PostgreSQL: pages, the full edit history, users, groups, permissions, comments, navigation, the theme, the site settings, the authentication configuration — and its own signing certificate, which is what keeps sessions and logins coherent after a restore.

It also keeps the **uploaded attachments in the database**, under its default storage module. That is unusual and it is the thing to understand about this application: the two small directories on disk hold a rendered cache and a scratch folder, and the database holds everything else.

So the dump is not one artifact among several. It is the backup.

## "0 tables" — the red flag that was the check, not the backup

A real Wiki.js dump here — 17.9 MB, everything about it green — verified as **restoring to zero tables**. That is exactly the shape of a backup you cannot trust, and it was the blocking question for this application: was the *capture* empty, or was the *check* wrong?

It was the check, and it is fixed. PostgreSQL's `information_schema` is **per-database**: it lists only the tables of whatever database you are connected to. The old verification connected to the `postgres` maintenance database and counted there — where a Wiki.js schema has never existed and never will. The dumps were sound the whole time.

Measured against a real Wiki.js 2.5 database: the old query returns **0**, the current one returns **30**, which is the real schema.

> MySQL was never affected and reported real numbers throughout, because *its* `information_schema` is server-wide. That the two engines disagreed was the clue.

The verification now walks every database on the server and reports **tables and databases together** — so a genuine zero reads as "the probe found nothing", not as a confident fact about your backup.

## What a backup contains

Run it as a **stack** backup, so the database and the application are captured in one window and are a matched set.

- **The database** — a full logical dump with a completeness contract: the primary and foreign keys it declares are recorded at capture and checked after the import. A dump that was cut short fails loudly instead of importing quietly.
- **The configuration file** — small, and here it is the stock template whose values all come from environment variables. Worth keeping for a faithful rebuild.
- **The data directory** — a scratch folder and a rendered cache.

The raw PostgreSQL data directory is **not** captured: the logical dump supersedes it, and shipping both would mean shipping a torn copy alongside a good one.

**The rendered cache is an opt-in exclusion.** Wiki.js re-renders each page from the database the first time it is asked for, so nothing is lost — the cost is a slightly slower first few page loads after a restore. On a large wiki it is most of what is on disk.

## Two things a restore genuinely cannot bring back

With the default settings there are none: importing the database restores the whole wiki, attachments included. But Wiki.js can be configured to keep part of itself elsewhere, and when it is, the restore looks finished and is not.

So after a stack restore DockBack asks the restored database which optional subsystems are switched on, and says what each one needs:

- **An external search engine.** Its index lives in that engine, not in your backup. Every page is back; searching finds nothing until you rebuild the index from **Administration → Search Engine**.
- **A Git mirror.** It is about to resume syncing using the credentials that were just restored. Check the remote and its deploy key first — the database is the source of truth, and a mirror pointed somewhere unexpected is worth catching before it pushes.

Both are read-only questions and only a count comes back — the query runs against a database holding user accounts and secrets, and a note needs to know only whether something is on. Neither can fail a restore, and a deployment using the defaults hears nothing at all.

## Moving to another machine

Works on any host — Synology, UGREEN, a Linux box, Windows Docker. Restore both services together; DockBack recreates the network first, so the database hostname resolves exactly as it did.

**Internal links are relative**, so the wiki works at a new address with nothing changed. Logins keep working too: the signing certificate is in the database and comes back with it.

The one value that carries the old address is the **Site URL**, used for absolute links in emails and login redirects. DockBack leaves it exactly as captured and does not edit it. That is deliberate: it lives in a JSON column inside the application's own settings row, there is no supported command-line tool for it, and a backup tool reaching into an application's database to rewrite configuration is a good way to turn a working restore into a broken one. Set it in **Administration → General → Site URL**, where the application validates it.

**Remap path** if the directories live somewhere else. **Ownership** is restored numerically, so it does not matter which users exist on the new host.

**Your domain does not follow the container.** If the wiki is reached through a tunnel or reverse proxy, that still points at the old machine until you re-point it.

## Version: newer migrates, older is refused

Wiki.js runs its database migrations at startup, **forward only**. Restoring into a newer version is safe — it migrates once, and that is one-way. Restoring into an **older** version than the database was written by is refused before anything is written, because there are no down-migrations to get back.

Restores recreate by image digest, so a `:latest` tag that has moved on since the backup does not decide what you get. Pinning the tag is still worth doing if you would rather choose when that one-way step happens.

## The archive is sensitive

The dump holds every user's password hash, the signing certificate, the full page history, and every uploaded attachment. It exists only inside the encrypted archive; the manifest carries environment variable **names** only, and the database password is among the values that never leave it.

Keep a retention-locked offsite copy. **Write-only** encryption is worth considering for a wiki with private content.

## Proving a restore worked

1. Table, page, page-history, user, group and asset counts match — and the verification's table count is a real number, not zero.
2. **Log in as the original admin with the original password** — that proves the hashes and the signing certificate came back.
3. Open a page and check its **history** shows the earlier edits.
4. Open an **uploaded image**. It came out of the database, which is the whole point of this application's shape.
5. Log in as a second user and confirm their **group permissions** still apply — allowed pages allowed, denied pages denied.
6. **Search for a term from a page written before the backup**, and get a hit *without* rebuilding anything. Unless you use an external search engine — in which case DockBack has already told you.

Point 4 is the one that catches a wrong mental model: if you assumed the attachments were on disk, this is where you would find out otherwise.

## The general rule

When a check and a backup disagree, find out which one is lying before you fix anything. A verification that reports zero is making a claim about your data, and a claim that turns out to be about the query is still worth chasing down — because for as long as it stands, nobody can tell the difference between it and the real thing.
