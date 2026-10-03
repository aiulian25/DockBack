# Write-only backups (DockBack can't read its own backups)

Normally DockBack holds one master key. That key encrypts every backup and seals every destination credential, which means **DockBack can read every backup it has ever made** — and so can anyone who breaks into its container. They get the key, the credentials, and plaintext access to your whole backup history on every destination. Immutable (WORM) storage does not help: object locks stop *deletion*, never *reading*.

**Write-only mode removes that capability from DockBack itself.**

You generate a keypair. DockBack keeps only the **public** half and seals each backup to it. The **private** half is shown to you exactly once and never stored. From then on, DockBack can create, upload, copy, prune and integrity-check backups — but it cannot open them. Restoring requires pasting your offline private key, for that one restore.

If someone owns your DockBack container tomorrow, they can still delete or corrupt future backups. What they cannot do is **read** them.

## What you give up

This is a genuine trade, not a free win, and it is worth understanding before you turn it on.

**A backup DockBack cannot read is a backup DockBack cannot test.** There is no way around that — a tool cannot both prove it can restore a backup and be unable to open it.

What still works on write-only backups:

- **Integrity checking** — the ciphertext SHA-256 and the signed manifest are verified exactly as before, so corruption, truncation and tampering are still caught.
- **Everything that doesn't need the contents** — scheduling, mirroring to destinations, retention and pruning, immutability, alerts, the audit trail.
- **Offline recovery** — fully, with the standalone Python tool (see below).

What stops working on write-only backups:

- **Deep verification** — the decrypt-and-walk pass that opens the archive and checks every entry.
- **Automatic restore drills** — DockBack cannot test-restore what it cannot open. These backups are *skipped*, not failed: you will not get "drill failed" alerts for them.
- **Standby rehearsals** on a fallback node.
- **Cross-backup file search** and **generation diffs** — both read the file index inside the archive.
- **Incremental backups** — a delta is built by diffing against the previous backup's index, which lives inside the previous ciphertext. Write-only backups therefore always capture **in full**. Expect more storage and longer runs.
- **Restore-confidence grade caps at B** — grade A requires a passed drill. This is honest rather than punitive: an untested backup really does carry less proof.

## When to use it

**Good fit** when the data is sensitive enough that disclosure is worse than the extra storage — customer data, financial records, anything where "an attacker read our entire backup history" is the worst outcome. Also a good fit when DockBack is exposed to more risk than the data it protects.

**Poor fit** for large media volumes where incrementals are doing the heavy lifting, or where you rely on automated drills for confidence.

You do not have to choose once and forever. The mode applies to **new** backups; existing backups keep whatever envelope they were made with, and both kinds live side by side in one catalogue.

## Turning it on

1. Go to **Settings → Security → Encryption key** and open **Write-only backups**.
2. Click **Enable write-only backups**. You will be asked to re-enter your password (and 2FA code, if enabled) — this is a step-up-protected action, because arming it changes what a break-in is worth.
3. **The private key is displayed once.** Copy it, or download the recovery sheet. DockBack does not keep a copy and **cannot reissue it**.
4. Store it somewhere separate from your master key — a different password manager entry, a printed sheet in a safe, a hardware token. Storing both in the same place gives back most of what the mode bought you.

From the next backup onwards, new backups are sealed to the new keypair. The Backups list marks them with a **write-only** chip.

> **If you lose the private key, every backup taken while the mode was armed is permanently unrecoverable.** Not by DockBack, not by us, not by anyone. That is the design. Treat the key as seriously as the master key — arguably more so, because there is no second copy anywhere.

## Check the key before you need it

The private key is now the single point of failure for every write-only backup you hold, and until you use it, nothing has ever confirmed it is the right one. A transposed character, a copy-paste that dropped the last line, or a sheet from a keypair you rotated months ago all look exactly like a good key — right up to the disaster restore where it matters.

**Verify recovery key** answers that question in seconds. Open any write-only backup, paste the key into the **Offline private key** box in the restore panel, and click **Verify this key**. DockBack unwraps that backup's data key with it and decrypts the first frame of the archive.

- **Pass** means the key opens this backup — and every other backup sealed to the same keypair, since they share it.
- **Fail** tells you which of two very different things went wrong: the key is from a *different keypair* (you are holding the wrong sheet), or the key is right but *this copy of the archive is damaged*.

Nothing is restored, stopped or changed. Only the first frame is read, so the check costs seconds even on a large backup stored offsite, and it stops without downloading the rest.

You will be asked to confirm your password first, the same as revealing the master key. The key you paste is used for that one request and is then gone: never stored, never logged, and never written to the audit trail — which records only the *pass or fail* and the keypair fingerprint.

Worth doing when you first set up write-only mode, whenever you rotate the keypair, and on whatever cadence you already use to test restores. A recovery key you have never tested is a backup you have never tested.

> If the fingerprint shown on a failure is not the one on your recovery sheet, you are holding a key for a different keypair. Find the right sheet before concluding anything is wrong with the backup.

## Restoring a write-only backup

**In DockBack:** open the backup and start a restore. The restore panel shows an **Offline private key** box. Paste the key and continue.

The key is checked against the backup's recorded keypair fingerprint **before** anything is stopped, snapshotted or overwritten — so a wrong or missing key costs you nothing and leaves the running container untouched. It is held in memory for that one restore, then discarded: never written to the database, never to a log line, and never to the audit trail (the audit records only that a key *was* supplied).

**A whole stack.** *Restore stack* takes the key too, once for the project. Any member sealed to the offline keypair is marked **write-only** in the restore plan, and the stack restore page asks for the key before the confirm is enabled — one key for the stack, because write-only mode seals every backup to the same instance keypair. If you reached that page from a single backup's drawer, the key is asked for again there rather than carried across: moving key material between screens means writing it somewhere it can be read, and one paste is cheaper than that.

It is checked against **every** sealed member up front, so a missing or wrong key stops the restore before the first service is touched. That matters more here than anywhere else: without it, a stack whose database was write-only restored all the application services and *then* stopped dead at the database, leaving a half-restored stack and no way to supply the key at all. If one member disagrees with the key, the refusal names that service, so you know which recovery sheet to look for.

The key travels in the request body, never in the URL — a web address is written to the access log of every proxy in front of DockBack, kept in browser history, and sent in `Referer` headers, and this key opens every backup you hold.

**Without DockBack:** the standalone recovery script handles it too — that promise is not weakened by write-only mode.

```
python3 dockback-recover.py \
  --private-key @privkey.txt \
  --manifest myapp.dback.manifest.json \
  --in myapp.dback --out restored.tar
```

The master key is not needed for the archive itself. You will still need `--key` as well if your manifest sidecars are **sealed** (the `.manifest.json.enc` form), because the sidecar is encrypted with the master key.

The script depends on nothing but the Python standard library — no `pip install`, no network — because a recovery tool with dependencies is a recovery tool that fails on the day you need it.

## Turning it off

**Settings → Security → Encryption key → Write-only backups → Turn off write-only backups** (also step-up protected).

New backups go back to the master-key envelope. **Existing write-only backups are not converted** and never can be — they stay sealed to the offline key. DockBack tells you how many are still affected when you turn it off.

**Keep the recovery sheet as long as any write-only backup exists**, including copies on remote destinations, even long after the mode is off.

## How it works

Each backup gets a fresh random data key (DEK) that encrypts the archive with AES-256-GCM. Only the way that DEK is *wrapped* changes:

- **Normally:** the DEK is encrypted with the master key DockBack holds. DockBack can unwrap it any time.
- **Write-only:** a fresh ephemeral X25519 keypair is generated per backup, combined with your public key to derive a one-time wrapping key (HKDF-SHA256), and the DEK is sealed under it. The ephemeral private half is discarded immediately, so **the wrap is irreversible even for the process that performed it**.

There is deliberately **no** master-key-openable copy of the DEK anywhere. If there were, the mode would be theatre.

Master-key rotation skips write-only backups, correctly and completely: nothing of theirs was ever wrapped with the master key, so they stay restorable with the same offline key before and after a rotation.

## Related

- **Encryption & keys** — the master key, recovery sheet, and passphrase-protected keyfile.
- **Restore without DockBack** — the standalone recovery script in full.
