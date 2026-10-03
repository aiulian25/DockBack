# CommaFeed & apps with two storage backends

**CommaFeed** is a small app that teaches a big lesson: the same image name can ship two completely different storage backends, and only the **tag** tells you which one you are running. Get that wrong and you back up the wrong thing.

## Which one are you running?

| Image | Where the data lives | How it's backed up |
|---|---|---|
| `athou/commafeed:latest-postgresql` | a separate **PostgreSQL** container | a logical `pg_dump` — consistent by design |
| `athou/commafeed:latest` | an **embedded H2 database** inside `/commafeed/data` | captured as a file, while the container is quiesced |

Same application, same web interface, same container name. Check the tag.

### If you run the Postgres build

There is nothing special to do. All of your state — feeds, subscriptions, categories, read and starred status, accounts, password hashes and per-user API keys — is in the database, and DockBack dumps it logically inside the backup window with the app briefly paused. `/commafeed/data` is empty; it is captured anyway, harmlessly, in case a future version starts using it.

Back up **both services together** so the dump and any app-side state come from one moment.

### If you run the default (H2) build

DockBack has no dump tool for H2, so the database file is captured **as a file**. That is perfectly sound *while the container is quiesced for the copy* — which is the default — and a torn copy waiting to happen if it isn't.

So: **leave "Consistency during volume backup" set to pause or stop for this container.** DockBack now says so on the container's own page, right above that setting, because the risk is otherwise invisible — nothing about the running app hints that its database is a file rather than a service.

Keep `/commafeed/data` on **local disk** too. An embedded database on CIFS or NFS loses the file locking it depends on and corrupts silently.

> Switching to the `-postgresql` image is worth considering if you rely on this instance: a logical dump is consistent without pausing anything.

## Your API keys keep working

CommaFeed issues each user an **API key** for external clients and scripts. Those keys live in the database, so a restore brings them back **verbatim** — anything using them keeps working with no re-issue and no reconfiguration.

That is the intended behaviour, and it is worth knowing it cuts both ways: restoring onto a second machine duplicates the keys. If you are keeping a restored copy around long-term and would rather it not share credentials with the original, rotate the key there afterwards — which will, of course, break clients still pointing at it.

Password hashes are preserved the same way, so everyone logs in with the credentials they already have.

## Moving to another machine

Works on any host — Synology, UGREEN, a Linux box, Windows Docker. CommaFeed stores no address of its own (feed URLs are absolute and external), so a move needs no URL rewriting at all. Two ordinary restore options usually cover it:

- **Remap path** for a different host directory. Paths *inside* the container stay the same.
- **Remap machine IP** if anything is pinned to the old host's address.

Then re-point your reverse proxy or tunnel at the new host, and you're done.

**Version: newer is fine, older is refused.** CommaFeed migrates its schema forward when it starts, whichever database is behind it, and there is no way back down. Restoring into the same or a newer version is safe; restoring into an older one is blocked. Pin the tag if you'd rather choose when that one-way step happens.

## What the verification tells you

Every backup is verified by re-importing the dump into a throwaway, network-isolated database and counting what arrives. For a Postgres stack you should see something like **"13 tables in 1 database"**.

> **If you have older Postgres backups reporting `0 tables`, that was a bug in the check, not in your backup.** The count was being taken against the wrong database — PostgreSQL keeps `information_schema` per-database, and a restore puts your tables in their own. The dumps were always fine. Re-verify or re-run any affected backup and it will report its real count.

## Proving a restore worked

Not "the page loads". These four:

1. **Log in with the original credentials** — proves the accounts and hashes came back.
2. Feeds and categories are all present, with the same **read and starred** state.
3. **Refresh a feed** — it fetches new entries, proving the subscriptions are live and not just rows in a table.
4. **Call the API with the stored key** — a `200` proves external clients keep working.

## The general rule

When an image ships variants, the tag is part of your backup design, not a detail. An app backed by a real database server gets a logical dump and needs no downtime; an app with an embedded database file needs to be **quiesced while it is copied** — and if nothing tells you which one you have, that is exactly when it goes wrong.
