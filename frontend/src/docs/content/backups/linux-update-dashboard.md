# Linux Update Dashboard & backups that collapse a key separation

This dashboard holds the SSH passwords, private keys and certificates for every Linux server it patches — and the ability to reboot them and change their installed packages. A decrypted archive is privileged remote code execution across your whole fleet, in the same tier as Dockhand, Termix and Guacamole.

It also does something the others don't, which is worth understanding before you back it up.

## Your deployment splits the key. Your backup can't.

On disk, this app is well designed. The vault is field-level AES-256-GCM, and decrypting it needs **two** things:

- `.encryption_salt` — on the data disk, beside the database
- `LUDASH_ENCRYPTION_KEY` — in the container's **environment**, deliberately *not* on the data disk

So someone who copies the data directory off your NAS gets ciphertext and a salt, and gets nowhere. That separation is real protection and it is doing work every day.

**A backup necessarily puts both halves in one archive.** It has to — a restore that can't decrypt the vault isn't a restore. But it means the archive is a single object that unlocks everything, where your running deployment was two.

That is exactly why **write-only encryption matters more here than the 7 MB archive size suggests**. It seals the archive to an offline key that never touches your servers, restoring the "two things are needed" property at the backup layer: the archive, and a key you keep elsewhere. Until you turn it on, these backups are capped at grade **C** with the reason stated on the backup.

> **Losing `LUDASH_ENCRYPTION_KEY` is unrecoverable.** It cannot be regenerated to match existing ciphertext. It is in your compose file today; make sure it is somewhere that survives losing the server.

## Nothing is ever decrypted

DockBack never unlocks the vault. The database is captured as a **consistent snapshot** — folding in its write-ahead log, which matters here because scheduled update checks write continuously and a plain copy can catch it mid-transaction. The encrypted columns pass through as ciphertext. The salt and session key are captured as opaque blobs.

No SSH password, private key or token exists in plaintext anywhere in the pipeline, in a temporary file, or in a log.

**Verification doesn't decrypt either.** Each snapshot's checksum is recorded, and a restore hashes what landed on disk and compares — proving the database is **byte-identical** to what was captured, without reading a single field. That's a stronger guarantee than any count, and it costs nothing in exposure.

## Moving to another machine

Works on any host — Synology, UGREEN, a Linux box, Windows Docker. **Remap path** if `/data` lives elsewhere; **Remap machine IP** if anything is pinned to the old address.

**Keep `/data` on local disk.** WAL-mode SQLite on CIFS or NFS loses the file locking it depends on.

Two address concerns, and they are not the same:

- **`LUDASH_BASE_URL`** — DockBack sets this for you when you supply a new address in the restore panel.
- **Your identity provider's redirect URI** — if you sign in through SSO, that URI still points at the old address, and **DockBack cannot see or change it**. It lives in another system entirely.

The second one is worth dwelling on, because its symptom is misleading: the app comes up perfectly healthy, everything works, and only **login** fails with a redirect mismatch. That reads as a broken restore. It isn't — it's a setting left behind at the provider.

**Connections to the servers you manage are unaffected by a move.** Those target the managed machines, not this app, so nothing on them needs touching.

**Version: newer is fine, older is refused.** The app migrates its database on start with no way back, so restoring into an older image is blocked. This project releases often — **pin the tag** rather than tracking `latest`, so a disaster-recovery target can always run the matching version.

## Proving a restore worked, without exposing anything

1. **Log in with the original credentials** — whichever method you use; password hashes, passkeys and SSO config all live in the database and come back verbatim.
2. Your managed hosts, schedules and notification channels are all present.
3. **Run an update check against a spare or test host** — it succeeds.

Step 3 is the decisive one, and note what it does *not* involve: the app uses the restored credential to open the SSH connection itself and reports success or failure. **The password or key never leaves the container and is never printed.** Use a test host so no production server is touched.

## Two things to confirm yourself

**Check no default administrator credential is in use.** This couldn't be verified without reading the credential database, which is exactly the thing that shouldn't be done casually. Confirm it in the UI. If a quick-start default is still active, that matters far more than any of the above — your archive's encryption is irrelevant when the front door is open.

**Confirm which sign-in methods are enabled**, since SSO is what makes the redirect-URI step apply to you.

## If an archive is ever exposed

Assume every credential in it is known. Containment is **rotate the SSH credential or key for every managed server, and regenerate `LUDASH_ENCRYPTION_KEY`** — treat it as a privileged-access compromise of your whole fleet.

That is a long afternoon. It is much shorter than not knowing whether you needed to.

## The general rule

When an app deliberately splits its key material, notice what a backup does to that design. Collapsing the split is usually unavoidable — the alternative is an archive you cannot restore from — so the honest response is to move the separation up a layer: seal the archive with a key that lives somewhere the archive doesn't.
