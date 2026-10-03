# Uptime Kuma & one image, two databases

**Uptime Kuma** version 2 can keep its state two completely different ways: a plain SQLite file, or a full MariaDB server running inside the application's own container. You choose at setup, and **the image is identical either way** — the MariaDB client is present whichever you picked.

That matters because the right way to back up the two is not the same, and being wrong is not a near miss.

## What was going wrong

A real deployment here was running the embedded MariaDB, and being backed up by copying its 322 MB data directory — **while the server was writing heartbeats into it every twenty seconds.** A hot file copy of a running database is the one thing a backup should never be, and it is exactly what the logical-dump path exists to prevent.

Now DockBack reads the application's own configuration to see which way it is running, and:

- **Embedded MariaDB** → a **logical dump**, transactional. It is consistent, it takes **no downtime at all** (the application keeps running and keeps recording throughout), and the archive is a fraction of the size, because the raw data directory is left out once the dump has succeeded. Verified against a live instance: the container is not stopped, not paused, and not restarted.
- **SQLite** → the ordinary file path, which finds the database and takes a consistent snapshot of it.

The check is against the application's configuration file rather than the image, so a deployment that switched between the two is followed rather than assumed. And if the answer is ever unclear, DockBack takes the file path — the safe direction, since a file backup is never *wrong*, only sometimes less good.

## What a restore proves

Uptime Kuma's value is rows: a list of monitors and a long history of checks. A dump that declares every table and holds almost none of them is exactly the failure worth catching, and a table count cannot catch it.

So the counts of the application's own key tables — monitors, heartbeats, notifications, users, status pages, API keys — are recorded at capture and **compared after the import**. A shortfall fails the restore and names the table:

```
the imported database is INCOMPLETE — heartbeat has 3 of 34671 row(s)
```

More rows than were captured is not a shortfall — the application has been running and recording since. Anything that could not be counted produces no verdict at all, rather than a guess.

## Restoring an embedded database

The sequence matters, because the database lives inside the application's own directory:

1. The files are restored — configuration, uploads, status-page images — and the container starts.
2. The application initialises its own database server, exactly as it does normally. **The old raw data directory is not put back**; it was superseded by the dump.
3. Once that server accepts connections, the dump is imported over the top.
4. The application is restarted so it sees the imported data rather than the empty database it just made.
5. The counts are checked, and the health gate decides.

If the container being restored into is **not** configured to run the embedded server, that is caught before the import rather than surfacing as a timeout — restoring an embedded-database dump into a deployment set up the other way would leave the application running on an empty database of a different kind, looking completely healthy.

## Keep it on local disk

Uptime Kuma's own documentation is explicit that its database on a network share corrupts. DockBack treats that as a requirement: if a restore target's storage is measured to be CIFS or NFS, it is **blocked**, not warned about. The failure it prevents is silent and arrives days later.

## Moving to another machine

Works on any host — Synology, UGREEN, a Linux box, Windows Docker.

**Every monitor keeps working.** Monitors check absolute addresses, so moving Uptime Kuma changes none of them, and nothing needs re-entering. What the new host needs is the ability to reach the same things: each monitored target, and whatever the notification channels send to.

**Two things carry the old address and neither complains.** The status page's public URL, and any notification that includes a link back. Both are set in the application's own settings, and DockBack has no reach into them — update them after a move, because nothing will tell you.

**Remap path** if the data directory lives somewhere else (still local disk).

**Version: newer is fine, older is refused.** Uptime Kuma runs its migrations on start, forward only, so restoring a newer backup into an older image is blocked before anything is written.

**If the container also mounts the Docker socket** — some deployments do, to monitor Docker hosts — that grants it control of the host's Docker daemon. It has nothing to do with the backup, and DockBack reproduces it exactly as configured rather than widening it, but it is worth knowing what you are moving.

## The archive is a secrets store

The database holds the credentials for every notification channel, the API keys, and the sign-in and two-factor secrets of every user. Those travel — they must, or a restored instance could not alert anyone or let anyone in — and they exist only inside the encrypted archive. Nothing about them reaches a log: DockBack records sizes and row counts.

Turn on **write-only** encryption and keep a retention-locked offsite copy. If an archive ever leaks, rotate the notification credentials and API keys inside Uptime Kuma, and the database password in its configuration.

## Proving a restore worked

1. The monitor, heartbeat, notification, user, status-page and API-key counts match — DockBack checks these and fails the restore if they fall short.
2. **Log in with the original credentials**, and with the original two-factor code if it is enabled — that proves the secret came back byte-identical.
3. The monitors are present and **actively checking**, with fresh heartbeats appended to the restored history rather than starting from nothing.
4. The status page renders with its configuration.
5. **Send a test notification** from a restored channel. Use a spare one for a drill so live alerting is not disturbed.

Point 5 is the decisive one: it proves the notification credential survived intact, which nothing else quite does.

## The general rule

When one image can run more than one way, the question "how do I back this up?" has more than one answer — and the image cannot tell you which. Ask the application's own configuration, and be willing to take the safer, duller method when the answer is unclear.
