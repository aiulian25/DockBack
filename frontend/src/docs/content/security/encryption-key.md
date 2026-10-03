# Back up your encryption key (critical)

This is the single most important operational task in DockBack.

## Why it matters

Every backup archive and every stored destination credential is encrypted with your master key (`DOCKBACK_ENCRYPTION_KEY`, set in your `.env`). The key never leaves your deployment.

> **How it's encrypted (envelope encryption).** Each backup gets its own random **data key** that encrypts the archive; that data key is then **wrapped (encrypted) with your master key** and stored in the backup's manifest. Your master key is therefore the one secret that unlocks everything — but because only the small wrapped keys reference it, the master key can be **rotated without re-encrypting your archives** (see *Rotation* below). Compromising one backup's data key never exposes any other backup.

> **If you lose the key, every backup is permanently unrecoverable.** There is no backdoor and no reset — that is what makes the encryption trustworthy. A backup you can't decrypt is no backup at all.

## The in-app recovery flow

DockBack nags you until this is done. A reminder banner stays at the top of every page until you confirm the key is backed up. Click **Back up now** (or **Settings → Encryption → Back up encryption key**) to:

1. **Reveal** the master key (a deliberate, audited action — you must **re-enter your account password**, plus your two-factor code if 2FA is on, even inside a valid session),
2. **Download a recovery sheet** — a plain-text file with the key, its fingerprint, and exact instructions to restore on a fresh install, and
3. tick **"I've saved it"** to clear the reminder.

> **Why the extra password prompt?** Revealing the key, generating a keyfile, rotating the key, and minting an API token are the highest-value actions in the app, so they require fresh proof of identity ("sudo mode") — a stolen browser session alone is not enough. One confirmation covers the whole flow for a few minutes, so you won't be asked at every step. Failed attempts count toward the same lockout as the login page.

If no persistent key is configured, DockBack runs on a **temporary key** and shows a red, non-dismissible warning — backups made then become unreadable after a restart. Fix it by setting `DOCKBACK_ENCRYPTION_KEY` (to the revealed value) and restarting.

## Encrypt the key at rest (passphrase-protected keyfile)

If you'd rather not keep the raw key in your `.env`, DockBack can store it as an **encrypted keyfile** unlocked by a **passphrase** (the recovery flow → *Advanced: passphrase-protected keyfile*):

1. Enter a strong passphrase and **download** `dockback.keyfile.json` — the master key sealed with an argon2id-derived key (AES-256-GCM). It is useless without the passphrase.
2. Mount the file (e.g. as a Docker secret) and set `DOCKBACK_ENCRYPTION_KEYFILE` to its path and `DOCKBACK_ENCRYPTION_PASSPHRASE` (or `…_PASSPHRASE_FILE`) to the passphrase.
3. **Remove `DOCKBACK_ENCRYPTION_KEY`** and restart. The key is now unwrapped in memory at boot — never on disk in plaintext.

Because the passphrase comes from the environment / a secret, the container still restarts unattended. **Back up the passphrase too** — like the key, it can't be recovered. (Tip: keep the keyfile and the passphrase in *different* places, so compromising one doesn't expose the key.)

## What to do

1. Find `DOCKBACK_ENCRYPTION_KEY` in your `.env` file (or use the in-app **Back up now** flow above).
2. Copy the value and store it **offline**, in a place independent of this server:
   - a password manager,
   - a printed copy in a safe, and/or
   - an encrypted USB/key vault kept off-site.
3. Keep it **with**, but not in the same basket as, knowledge of where your offsite backups live — you need both the key and a copy to recover.

## When restoring on a new server

A new DockBack deployment must use the **same** `DOCKBACK_ENCRYPTION_KEY` to read existing backups. Set it from your offline copy before pointing the new instance at your destinations, then restore as normal (*Restoring → Disaster recovery*).

## Rotation

You can rotate the master key from **Settings → Encryption → Rotate encryption key**. Thanks to envelope encryption, rotation **re-wraps each backup's small data key** from the old master key to the new one — the large archives are never re-encrypted, so they stay byte-for-byte identical and don't need re-uploading. This now covers **control-plane app-backups** (DockBack's own self-backups) too: they carry the same envelope, so rotation re-wraps them in place, and the dialog reports how many were re-wrapped. Rotation also **re-seals everything else the master key protects** — node connection secrets, destination credentials, the notification config, and your 2FA secret — so the app keeps working end to end.

The flow is deliberately guarded:

1. Enter the **current** key, a **new** 64-hex key (generate one in the dialog, or paste your own), and your **account password** to re-authenticate (with 2FA enabled, the dialog also asks for your **two-factor code**).
2. Tick the confirmation that you've **saved the new key** offline.
3. DockBack re-wraps and re-seals in place, then switches the running process to the new key. It reports how many backups were re-wrapped, skipped, or failed.
4. It finishes by taking **one fresh app-backup on the new key automatically**, so the control-plane is immediately recoverable under the new key even if every earlier app-backup predates envelope encryption. The dialog shows its filename; if that step ever fails it becomes a follow-up warning, not a rotation failure.

**After a successful rotation you must update `DOCKBACK_ENCRYPTION_KEY`** (in your `.env`, or your keyfile) to the **new** key before the next restart — the running process is already on the new key, but a restart reads the environment. **Keep the old key archived** until you've verified backups restore under the new key.

Two things rotation intentionally does **not** touch:

- **A backup wrapped by a different (third) key** — e.g. one from another deployment — is **skipped and left untouched**, not corrupted. It still needs its own key.
- **App-backups everywhere follow the rotation**: local app-backups are re-wrapped in place, and copies on **external app-backup destinations** are downloaded, re-wrapped, and re-uploaded — so every restorable app-backup opens with the **current** key and the old key can truly be retired. App-backup **destination credentials** are re-sealed too. An app-backup that turns out to be sealed under an *unrelated* key is reported as **failed** in the summary — never silently skipped — so you know before discarding the old key.
- The only remaining old-key case: **very old (pre-upgrade) app-backups** taken before envelope encryption existed — they were sealed whole, with no small wrapped key to re-wrap, and are reported as *skipped (pre-envelope)*. Keep the old key only if you need to restore one of those; rotation already took a **fresh app-backup on the new key** (step 4 above).

A backup whose master-key fingerprint no longer matches the current key is flagged **Key mismatch** in the Backups list — set the matching key (from your offline copy) to restore it, or re-run a rotation from that key.
