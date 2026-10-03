# Nextcloud & the backup that has to be undone

**Nextcloud** is a four-container stack, and backing it up correctly requires one trick that then has to be reversed on the way back. Getting the second half wrong makes every restore fail — which is exactly what used to happen.

## Maintenance mode, both ways

To capture the files and the database as a matched set, DockBack puts Nextcloud into **maintenance mode** for the capture window, then takes it back out. That part has always worked, including when a backup fails or is cancelled — maintenance mode is switched off on every path.

But it means `config.php` is captured saying **`'maintenance' => true`**. Restore that faithfully and the instance comes back *in maintenance mode*: every page returns 503, the container's healthcheck fails, and DockBack correctly rolls the whole thing back. The backup was perfect and the restore could never succeed.

So the restore now runs the other half, **after the data is back and the container has started, before the health check**:

```
php occ maintenance:mode --off
php occ maintenance:data-fingerprint
```

The second one tells your desktop and mobile clients the server state came from a backup, so they re-sync instead of assuming their local copy is newer and helpfully restoring files you deleted.

You can add your own steps too, on the container's page under **After a restore** — they run once the built-in ones have finished, so they act on a live instance rather than a sealed one.

## Everything that makes it *your* Nextcloud travels

`config.php` holds `instanceid`, `passwordsalt` and `secret`, and DockBack restores it byte-identically without ever rewriting it. That's what makes the difference between a restored Nextcloud and a new one:

- **Two-factor authentication keeps working.** TOTP secrets live in the database encrypted with `secret` from `config.php`, and both are captured in the same window. Restore them together and your authenticator app still works.
- **Share links keep resolving.** Same tokens, same instance identity.
- **No file rescan.** The database and `data/` come from one moment, so the file cache is already correct.
- **The code itself travels**, because the whole webroot is a bind mount — the restored instance runs exactly the version that was backed up, whatever the tag says today.

## Moving to another machine

Works on any host — Synology, UGREEN, a Linux box, Windows Docker. Restore **all four services together**, in dependency order; DockBack recreates the networks first so the database and Redis hostnames resolve just as they did.

- **Remap path** if the stack lives under a different directory.
- **Remap machine IP** if anything is pinned to the old host address.

**Trusted domains are reconciled through Nextcloud's own tool, never by editing config.php.** Supply a new address and DockBack uses `occ` to replace the stale entry **in place** — it never appends, never writes a wildcard, and never touches your domain entries. `overwritehost` and `overwrite.cli.url` are set to match. The list ends up exactly as long as it was.

**And in the environment, which is what actually decides.** The official image reads `OVERWRITEHOST`, `OVERWRITECLIURL` and `NEXTCLOUD_TRUSTED_DOMAINS` from the container's own environment — `reverse-proxy.config.php` is loaded after `config.php`, so those variables beat anything `occ` writes. A Nextcloud moved to a new machine with the old `OVERWRITEHOST` still in its compose file redirects every visitor to a host that is no longer there, while `occ` reports success. DockBack now sets both halves when it recreates the container, and says so plainly if it finds the environment overriding what it just set.

**`trusted_proxies` depends on whether the machine changed.** It's your reverse proxy's IP, not your site's address. Restoring onto the same machine keeps it — the proxy is almost certainly still there. Restoring onto a different machine *with a new address* clears it: the recorded upstream is provably not the one in front of it any more, and trusting a machine that is no longer in the path lets whatever answers at that address claim to be any client. The old value is printed first, as the command that puts it back.

**Your `config.php` is copied aside before anything is changed**, to `config/config.php.dockback-<timestamp>.bak` inside the container. Nothing is deleted.

**Your domain doesn't follow the container.** If Nextcloud is behind a tunnel or reverse proxy, that still points at the old machine until you re-point it. The container restores correctly; the route is infrastructure DockBack has no reach into.

**Redis is expendable.** It holds cache and file locks, which Nextcloud rebuilds. It's captured because it costs nothing, and it never gates a restore.

**Check the database container for a root password before you start.** Restoring the database re-initializes it from the dump, and MariaDB will not initialize an empty data directory without `MARIADB_ROOT_PASSWORD` (or `MYSQL_ROOT_PASSWORD`) in its environment. Many Nextcloud compose files set only `MYSQL_DATABASE`, `MYSQL_USER` and `MYSQL_PASSWORD` — which works forever on the original host, because that directory was initialized once and never again.

DockBack tells you in the restore plan and refuses before it empties anything, so this costs you one variable rather than a database. Pick any value; Nextcloud doesn't use it — it connects as `MYSQL_USER`, which is recreated from the environment unchanged.

## If you enable server-side encryption

This matters enough to say in advance. With encryption enabled, the master and user keys live under `data/files_encryption/` and are captured automatically with the rest of the data — inside the same encrypted archive, so they get at least the protection of the files they unlock.

Two rules follow:

1. **Losing that archive means losing every encrypted file.** A retention-locked offsite copy stops being a good idea and becomes necessary.
2. **Never separate the keys from the data** into somewhere with different protection.

Your restore drill should then include opening one encrypted file on the restored copy, not just checking that the files are present.

## Version: newer is fine, older is not

Restore recreates by **image digest**, so tags that moved don't matter. The Nextcloud image also refuses to start when its code is older than the installed instance — its own guard against exactly this.

Pin the **cron** container to the same version as the web container. A floating tag there means a `docker compose pull` can leave cron running different code against the same webroot.

## Permissions are preserved, not corrected

Whatever ownership and modes the files had, they come back. If `config/` was world-writable it still is. That's deliberate — a restore must never quietly change your security posture in either direction. Tightening it is worth doing, separately and on purpose.

## Proving a restore worked

1. `occ status` — same version, and **`maintenance: false`**. That last one is the whole point of the restore hook.
2. `occ integrity:check-core` is clean, proving the code came back intact.
3. User files are byte-identical, and the file, share and user counts match.
4. **Open a share link created before the backup** — the same token still resolves.
5. **Log in as a user with two-factor enabled** — the original authenticator code is demanded and works.

Point 5 is the decisive one: it proves `config.php`'s `secret` and the database came back coherent with each other, which nothing else quite does.

## The general rule

When a backup deliberately changes an application's state to capture it cleanly, the restore owes you the reverse change. Look for that pair. A quiesce with no matching un-quiesce is a backup you can take forever and never restore from.
