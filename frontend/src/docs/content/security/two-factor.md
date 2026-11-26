# Two-factor authentication (2FA)

Add a second factor to the admin login: a time-based one-time code (TOTP) from an authenticator app, on top of your password. It's **optional and off by default** — enabling it doesn't change anything for anyone until you turn it on.

## Enable it

1. **Settings → Two-Factor Authentication → Enable 2FA.**
2. Scan the QR code with an authenticator app — Google Authenticator, Microsoft Authenticator, Aegis, 1Password, Bitwarden, etc. (or type the manual key).
3. Enter the current 6-digit code to confirm and activate.
4. **Save your recovery codes.** Ten one-time codes are shown **once** — store them somewhere safe (password manager). Each works a single time if you lose your authenticator.

After this, signing in asks for your password **and** a code.

## Sign in with 2FA

Enter your username + password as usual; you'll then be asked for the **authentication code**. Enter the 6-digit code from your app, **or** one of your recovery codes (each recovery code works once).

## Recovery codes

- Shown once at enrolment; each is single-use.
- Settings shows how many remain.
- Lost your authenticator *and* your recovery codes? See *Locked out* below.

## Disable it

**Settings → Two-Factor Authentication → Disable 2FA**, then confirm with your password **and** a current code or a recovery code. This clears the secret and all recovery codes.

## Security notes

- The TOTP secret is **encrypted at rest** with your master key; recovery codes are stored only as one-time hashes.
- Wrong codes count toward the **login lockout** (the same per-IP/account throttle as passwords), so the 6-digit code can't be brute-forced. A correct password doesn't clear those failures: they clear only after a sign-in or a 2FA disable that also passes the code.
- Behind a reverse proxy, set `DOCKBACK_TRUST_PROXY` (and `DOCKBACK_TRUSTED_PROXIES`) so the lockout sees real client IPs — see *Socket-proxy permissions* / deployment notes. With no list set, DockBack believes the forwarded address only from a proxy on this machine or a private network; a proxy on a public address must be listed.

## Actions that ask for your password again

Being signed in lets you look around and run backups. A few actions ask you to confirm your password first (and your 2FA code, if you use one), so that a browser left signed in, or a copied session, cannot be used for them. After you confirm, you are not asked again for five minutes.

- **Running commands in a container:** saving new or changed backup hooks or an export profile.
- **Choosing what runs against your data:** changing the volume sidecar image, and re-pinning it on a server.
- **Taking protection off a restore:** switching off "ask for the password before restoring" for a container, or removing the mark (critical database, write-only required) that turned it on.
- **Reading or replacing secrets and data:** revealing or rotating the encryption key, downloading a decrypted backup, creating an API token, adding a passkey, changing the sign-in policy, restoring DockBack itself, and restoring a protected container or a whole server.

## Locked out (no app, no recovery codes)

2FA state lives in the SQLite catalog (`/app/data`). As a last resort, an operator with host access can clear it for the admin user by blanking the `totp_secret`, `totp_pending` and `totp_recovery` columns in the `users` table, then restarting. Treat this as break-glass and re-enable 2FA afterward.
