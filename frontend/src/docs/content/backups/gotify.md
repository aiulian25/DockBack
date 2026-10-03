# Gotify & apps whose data *is* the credentials

**Gotify** is a small app with a sharp edge. Its entire state is one SQLite file, and most of what's in that file is **live tokens**: the application tokens your scripts and services push with, and the client tokens your phones and desktops receive with. Nothing else about the app matters much; those do.

## What a restore has to get exactly right

Restore `gotify.db` byte-faithfully and **every token stays valid** — no integration needs re-issuing, no client needs re-pairing. That is the whole job, and it happens automatically because DockBack never rewrites an application's data.

The counts in a real instance make the point: one user, nine application tokens, thirty-two client tokens, and a hundred thousand messages. Every one of those forty-one tokens is a working credential.

## Consistency: a snapshot, not a copy

Gotify writes to its database continuously, so copying the file while it runs can catch it mid-transaction. DockBack finds it **by its file header, not its name**, and takes a transactionally consistent snapshot. Any stale write-ahead log is folded in and removed, so the restored database opens clean.

Set this container's **quiesce mode to pause** as well. The database is small, so the pause is measured in seconds, and it removes any remaining doubt for an app that is ingesting messages the whole time.

## Verification names what's missing

Every snapshot records **how many rows each table held**, and every restore checks them back. That matters more here than almost anywhere else, because Gotify's row count is overwhelmingly message history:

> Losing all 32 rows of `clients` — every device that receives notifications — is a **0.03% shortfall** against 115,000 rows. Against a total, that is noise. Worse, if the message table grew in the meantime, the total comes out *higher* and the loss disappears completely.

So the check is per table. A short restore now says **`clients has 0 of 32 rows`** and fails, instead of quietly succeeding. Row counts and table names only — never a token, never a message.

## Moving to another machine

Works on any host — Synology, UGREEN, a Linux box, Windows Docker. Two ordinary restore options usually cover it: **Remap path** for a different host directory, **Remap machine IP** if anything is pinned to the old address.

### Tokens survive. Client apps may still need pointing.

This is the one thing worth understanding before you move Gotify, because getting it wrong wastes an afternoon.

Tokens are opaque values in the database. They are **not** tied to a hostname, so a move leaves every one of them valid. But each receiving app on your phone or desktop stores the **server URL** it calls. If the address people actually reach Gotify at changes, those apps are still calling the old one.

**The symptom looks exactly like a broken token** — notifications stop arriving — and the instinct is to re-issue credentials. Don't. Update the address in the client apps instead.

Keeping the same address (re-pointing DNS or your reverse proxy at the new host) needs nothing at all.

**Version: newer is fine, older is refused.** Gotify migrates its schema on start, and an older binary cannot open a newer database. Restoring into the same or a newer version is safe; restoring into an older one is blocked before anything is written.

## Treat the archive as the credential store it is

For most apps, secrets are incidental to the data. Here they *are* the data. A decrypted Gotify archive gives someone the ability to push notifications as any of your services **and read your entire alert history** — which, for most people, is a running commentary on everything else they run.

So: keep it **encrypted** (always on), keep an **immutable offsite copy**, and consider **write-only mode**, where the archive can only be opened with an offline key held away from your servers.

> If an archive is ever exposed, containment is to **rotate the affected tokens in Gotify**. That does mean re-issuing to each integration and re-pairing each client — which is precisely why it is worth protecting the archive properly in the first place.

### If you ever turn on Gotify's built-in TLS

This deployment terminates TLS upstream, so there is no certificate to back up. If you enable Gotify's own Let's Encrypt support, note that the certificate is bound to a specific domain: restoring it onto an instance reachable at a **different** hostname makes it invalid. Let Gotify reissue for the new domain rather than restoring the old certificate.

## Proving a restore worked

Not "the page loads". The decisive test uses tokens that existed **before** the backup:

1. **Push with an existing application token** — a `200` proves your integrations keep working untouched.
2. **Read with an existing client token** — proves receivers keep working, including seeing the message you just pushed.
3. Log in with the **original credentials**; the applications and clients are all listed by name.
4. The message history is present.

Steps 1 and 2 are the ones that matter. Do them against a spare test application and client so no production integration is disturbed.

## The general rule

When an app's database *is* a set of credentials for things outside it, two things follow: the restore has to be byte-faithful or you have invalidated working secrets, and the archive deserves protection in proportion to what it unlocks — not in proportion to its size.
