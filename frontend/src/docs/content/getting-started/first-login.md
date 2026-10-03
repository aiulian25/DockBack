# First login & changing your password

## Signing in

Open DockBack in your browser at the address and port you mapped in `docker-compose.yml` (by default `http://<host>:28734`). Sign in with the admin credentials configured for your deployment.

> **Change the password immediately** on any deployment that is reachable by anyone but you.

If you set `DOCKBACK_ADMIN_PASSWORD` for the first run, it has to meet the minimum password length (12 characters by default). A shorter one is refused and a strong password is generated in its place, printed in the container logs alongside the reason. That variable is only read when there is no account yet — changing it later does not change your password.

## Sessions & automatic sign-out

To limit the window an unattended or stolen session stays usable, sign-in is time-bounded two ways:

- **Inactivity (idle) timeout — 30 minutes by default.** If there's no real interaction (mouse, keyboard, touch), you're signed out. A warning appears shortly before, with **Stay signed in**. Background dashboards refreshing on their own do *not* count as activity.
- **Absolute lifetime — 12 hours by default.** Every session ends this long after sign-in regardless of activity. A countdown warning lets you **Extend 30 min** — but only **once**; after that you must sign in again.
- **App restart / update.** Sessions never survive a restart — after DockBack is restarted, rebuilt or updated, everyone must sign in again.

Either way, **running backups keep going in the background** — signing out never interrupts them.

### Seeing what is signed in

*Settings → Security → **Signed-in devices*** lists every browser and device currently holding a session for this account, with where it signed in from, when it was last used, and how long it has left. It sits at the top of that tab, above the password form, because it is what you reach for when something feels wrong.

- **One device** is the ordinary state, and the page says so plainly.
- **More than one** is called out, because that is the state worth a second look. If you do not recognise one, **Sign out** ends that session on its own, immediately — whatever it was doing needs your password again. **Sign out the other N** ends them all at once. This browser stays signed in either way.
- **Changing your password** also ends every other session, and the password form says how many that is right now.

### Changing those limits

Both timeouts, and the minimum password length, are set under *Settings → Security → Sign-in policy*. The right values depend on where DockBack is reachable from: an instance behind a VPN can reasonably hold a session for a day, one on a shared workstation wants two hours.

- **Session lifetime** — 1 to 720 hours (30 days).
- **Inactivity timeout** — 1 minute to 24 hours. Shortening it applies immediately, to sessions that are already open.
- **Minimum password length** — 12 to 128 characters. It can be **raised but never lowered**: 12 is the shortest DockBack accepts in any configuration. Raising it doesn't invalidate your current password; it applies the next time one is set.

Saving this section asks you to confirm your password (and two-factor code, if enabled), the same as minting an API token — lengthening a session is a change to who can reach your backups, so it isn't something an unattended browser should be able to do.

The environment variables `DOCKBACK_SESSION_TTL_HOURS`, `DOCKBACK_SESSION_IDLE_MINUTES` and `DOCKBACK_MIN_PASSWORD_LEN` set the boot defaults; a value saved in the app overrides them and survives a restart. Values outside the ranges above are clamped rather than rejected.

**Signed-in devices.** *Settings → Account Security* lists every browser and device currently signed in to your account: which browser and platform it is, the address it was last used from, and when. The one you are reading this on is marked **this device**.

Each row has its own **Sign out**, which is usually what you want — if you see five sessions and recognise four, ending the fifth beats ending all of them. **Sign out other sessions** is still there for "end everything now", and changing your password does it automatically.

If a device you do not recognise appears, sign it out *and* change your password: whoever held that session was already signed in, so ending it without changing the credential leaves them able to sign in again. The address shown updates as a session is used, so a laptop that moved networks shows where it is now rather than where it started.

## The encryption key comes first

Before you create a single backup, make sure you understand the encryption key. It is set with the `DOCKBACK_ENCRYPTION_KEY` environment variable (in your `.env` file) and it is what makes every backup readable.

> **If the key is lost, every backup becomes permanently unrecoverable.** There is no recovery path — that's the point of encryption.

Store a copy of the key somewhere safe and offline. See *Security & Operations → Back up your encryption key* for the full procedure.

## What to do next

1. **Connect a server** — *Connecting Servers (Nodes)*.
2. **Add an offsite destination** — *External Backup Destinations*.
3. **Set your policy & schedule** — *Scheduling & Retention*.
4. **Run your first backup and test a restore** — *Creating Backups* and *Restoring*.
