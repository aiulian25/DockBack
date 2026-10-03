# Guacamole & all-in-one images with a database inside

**Apache Guacamole** in its all-in-one form is one container holding three things: the web app, the `guacd` proxy, and a **complete PostgreSQL server**. Nothing in the image name says so — and that one fact caused DockBack to back it up the wrong way.

## What used to happen, and what happens now

DockBack decides whether to dump a database by looking at the **image name**. `jwetzell/guacamole` doesn't contain "postgres", so it saw an ordinary application and copied `/config` wholesale — **including the live PostgreSQL data directory**, while the server was running. A hot file copy of a running database is exactly the torn-copy risk the dump machinery exists to prevent, and the result was a 176 MB archive with a questionable database inside it.

Now DockBack knows which images bundle their own database. For Guacamole it runs a proper **`pg_dump` inside the container**, and **leaves the raw data directory out** of the file capture — the dump replaces it. The archive shrinks to a few megabytes and, more importantly, the database in it is consistent.

The same applies to any image shaped this way, such as Uptime Kuma's embedded MariaDB.

> If the dump can't be taken for any reason, the raw directory is captured as before. A torn copy beats no copy, so the exclusion only ever engages once there is a good dump to replace it.

## The dump *is* the credentials

This is the part that makes Guacamole different from every other credential store, and it is worth being blunt about.

Termix and Dockhand encrypt their secrets at the application layer, so their database dumps contain ciphertext. **Guacamole does not.** It has to replay your RDP, SSH and VNC passwords to open a session, so it stores them in a **recoverable form**. The dump therefore contains working passwords for every system Guacamole reaches — not encrypted versions of them, the passwords.

There is no app-layer encryption to fall back on. **The archive's own encryption is the only barrier**, which makes two things non-negotiable:

- **Turn on write-only encryption.** The archive is sealed to an offline key that never touches your servers, so even a fully compromised DockBack cannot open it. Until you do, these backups are capped at grade **C**, with the reason stated on the backup.
- **Keep an immutable copy off the machine that made it.** A Guacamole archive stored only on a host Guacamole can reach is one compromise away from being both target and loot.

The dump is streamed straight into the encrypted archive — never a temporary file, never a log line, never printed.

## Restoring: the order is backwards from a database container

For a normal database container, DockBack stops the engine, wipes its data directory, and imports into a freshly-initialised server. **Doing that here would erase your configuration**, because for an all-in-one image the database lives *inside* the config directory.

So the order is inverted:

1. Restore the files and configuration — which starts the container.
2. Let the application bring up its own database, exactly as it would on a fresh install.
3. Wait for that database to accept connections.
4. Import the dump **over the top**, into the running server.
5. Restart the app so it drops the connections it opened against the old, empty database.

If the archive turns out to contain no dump, the restore **fails loudly** rather than leaving you with an application running happily on an empty database — which looks completely healthy and is the worst possible outcome.

## Moving to another machine

Works on any host — Synology, UGREEN, a Linux box, Windows Docker. **Remap path** for a different host directory; **Remap machine IP** if anything is pinned to the old address.

**Keep `/config` on local disk.** The embedded PostgreSQL lives there, and a database on CIFS or NFS loses the file locking it depends on.

**Version: newer is fine, older is refused.** Guacamole needs explicit schema-upgrade SQL between versions and offers no way back down, so restoring into an older image is blocked. Pin the image tag rather than tracking `latest` if you want to choose when that one-way step happens.

If you use **only database authentication** — the default, and what most single-admin installs run — a move needs no other reconciliation. There are no SSO redirect URIs to update, because there is no SSO.

## Check your admin account

Guacamole ships with a `guacadmin` / `guacadmin` account, and it is one of the most actively exploited defaults on the internet: the standard attack is to log in with it and read out every stored connection credential through the API.

Rename the account and change the password. If yours is still `guacadmin` with the stock password, treat that as urgent and independent of anything to do with backups — your archive's encryption is irrelevant when the front door is open.

## Proving a restore worked, without exposing anything

1. **Log in with the original admin credentials.**
2. The connections and connection groups are all present, with the same counts.
3. **Open one connection against a spare or test target** — the RDP/SSH/VNC session establishes.

Step 3 is the decisive one, and note what it does *not* involve: `guacd` uses the restored credential to open the session and reports success or failure. **The password never leaves the container and is never printed.** Use a test target so no production system is touched.

## If an archive is ever exposed

Assume every credential in it is known and **rotate every stored connection password and key**. Because Guacamole stores them recoverably, there is no partial containment here — an exposed archive is an exposed credential set for every system it reached.

## The general rule

An image's name tells you what the maintainer called it, not what runs inside it. When an application bundles its own database, it needs a real dump and its data directory needs leaving out of the file copy — and when that database stores credentials it must be able to replay, the archive deserves protection matched to the systems it unlocks, not to its size.
