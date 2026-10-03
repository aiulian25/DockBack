# Termix & the archive that is your whole fleet

**Termix** stores SSH credentials and private keys for the machines you manage. Backing it up is simple — one directory, a few hundred kilobytes. Understanding what that archive *is* takes longer, and it changes how you should protect it.

## Its encryption does not protect a backup

Termix encrypts its database, and keeps the key that decrypts it **in the same directory**.

That is a sound design for the threat it is aimed at: a disk pulled out of a machine, without the running system around it. It means something very specific for a backup, though — **copying the directory copies both halves.** Termix's at-rest encryption offers no protection at all to anyone holding a copy of its data directory, and a copy of its data directory is exactly what a backup is.

So for this application, **your backup's own encryption is not one layer among several. It is the only one.**

## DockBack never opens the database

The database is captured as the opaque encrypted blob it already is. That is not a policy DockBack promises to keep — it is structural: DockBack finds databases by file *header*, and an encrypted file has none. Nothing in the pipeline can see a SQLite database there, so no plaintext credential exists in a temporary file, a log, a report, or anywhere else, at any stage.

The consequence is that there is no consistent-snapshot path available either. So **the container is stopped for the copy** rather than paused. Pausing freezes the process with its state still in memory; stopping asks the application to flush and close its database cleanly first, which is the difference between a consistent encrypted file and one that merely looks like it. The database is small — this is seconds — and it is the default for this application.

## Write-only encryption, and a way to insist on it

An archive sealed with DockBack's master key can be opened by the DockBack server. For most applications that is a reasonable default. For this one, that server is frequently *one of the machines whose credentials are in the archive*.

**Write-only mode** seals each backup to an offline key instead — the private half never touches any of your machines, so a full compromise of the DockBack instance still cannot open a Termix backup. Turn it on.

It has one weakness worth naming: it is global, and it can be turned off. Legitimately, too — you switch it off to run a restore drill, because an instance that cannot read its own backups cannot test them. And then the next scheduled run of everything writes archives the server can read.

So a container can be marked **"never back this up without write-only encryption"**. When it is, a run that would produce a master-key-openable archive is **refused** before anything is captured, with the reason and both ways out.

That is off by default, deliberately. Refusing by default would mean somebody who has not yet set up an offline key gets no backup at all of the thing they can least afford to lose — a worse outcome, not a safer one. Turning it on is a decision to make once, on purpose.

**The custody trade is real and it cuts both ways.** With write-only on, losing the offline key means losing the backups. For a store of SSH credentials, "unrecoverable if I lose the key I chose to keep" is the right side of that trade against "readable by anyone who gets onto the server". Keep it where you would keep a recovery seed.

## The key file is world-readable, and that is worth two seconds

Termix creates `.env` — the file holding the keys that decrypt everything — with permissions that let **any user on the host, and any process in any container sharing that directory, read it**. Verified on a fresh install.

DockBack checks that file's permissions when it backs up, and again after a restore, and tells you with the command:

```
chmod 600 <the file named in the warning>
```

As everywhere else, it reports and never changes: a restore reproduces the permissions it captured, in both directions. Only the permission bits are read; the file itself is never opened.

## Restoring

Everything in the data directory travels, and none of it is optional. The database is useless without the key file, the key file is meaningless without the database, and nothing in there regenerates. There are no exclusions to consider.

**Version is locked in both directions.** Termix reworked how it stores and encrypts its data recently, and neither restoring forward nor restoring back has a demonstrated compatibility story for that format. So DockBack refuses to restore across a version difference either way — a database that opens but decrypts wrong is a far worse outcome for a credential store than a refused restore. The way out is to pin the target to the recorded version, which restoring by image digest — the normal path — does for you.

**Restore each instance from its own backup.** Two Termix deployments have separate auto-generated keys and separate databases. An archive from one cannot open in the other, and crossing them is not a recoverable mistake.

**Moving Termix changes nothing about what it can reach.** The hosts it connects to are recorded against *their* addresses, which have not moved. No connection needs re-entering, and nothing needs re-authenticating.

**Remap path** if the data directory lives somewhere else on the new host. Ownership is restored numerically, so the application reads its files regardless of what users exist there.

## If it ever leaks

Containment is severe and worth knowing before you need it. An exposed archive **plus** its offline key is a full compromise of every machine Termix manages: rotate every credential and key it holds, on every host, and check those hosts for access that predates the rotation.

That severity is the reason for everything above. It is also worth confirming, separately from any of this, that the Termix admin account is not on a default or quick-start password — a check to do by logging in, not by looking at the database.

## Proving a restore worked

1. **Log in with the original credentials.** The key file came back, so the database opens.
2. The hosts, tunnels and snippets you had are all present.
3. **Ask Termix to test a connection to one host.** It uses a restored credential and reports success — and the credential never leaves Termix, so nothing is printed. Use a spare or test host for a drill rather than a production one.

Point 3 is the decisive one: it proves a stored credential survived intact, without anything ever reading it.

## The general rule

When an application encrypts its own data, ask where it keeps the key. If the answer is "beside the data", then its encryption protects a stolen disk and nothing else — and every copy you make of that directory is a copy of the plaintext, however it looks. That is not a flaw to fix in the application; it is a fact to encrypt around.
