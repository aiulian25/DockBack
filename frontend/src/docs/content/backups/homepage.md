# Homepage & the smallest backup that matters most

**Homepage** is the simplest app in this guide and one of the most consequential to protect. No database, nothing written while it runs, a handful of YAML files — a plain file copy is completely safe. The catch is what's *in* those files.

## Your dashboard config is a credential dump

Every widget on your dashboard authenticates to the service behind it, and the key is written into `services.yaml`. One real config held working credentials for **more than twenty services**: Sonarr, Radarr, Immich, Nextcloud, Plex, Proxmox, Portainer, Tailscale, Paperless, Pi-hole, qBittorrent, Dockhand and more.

So a decrypted Homepage backup isn't "some dashboard settings". It's **a combined credential dump for most of your homelab, in one file** — including, quite often, the very services you're backing up separately and carefully.

That's why Homepage is treated as a **credential store**, alongside Dockhand and Guacamole. It's not about the app; it's about reach. (Gotify, by contrast, isn't in that class — its tokens reach only Gotify.)

**Turn on write-only encryption.** The archive is sealed to an offline key that never touches your servers, so even a fully compromised DockBack can't open it. Until you do, these backups are capped at grade **C**, with the reason stated on the backup itself.

> Leak containment here is unusually painful: rotate the API key for **every** integrated service. That's the honest cost, and the reason to seal the archive properly the first time.

## No database, so no ceremony

Homepage reads its YAML at startup and on reload, and serves widget data by proxying live API calls. Nothing is written to disk while it runs. So the backup is a straight encrypted file copy of the config and your custom icons — **no pause, no snapshot, no downtime**. It's typically around 30 MB, almost all of it images.

## Moving to another machine

Works on any host — Synology, UGREEN, a Linux box, Windows Docker. **Remap path** for a different host directory; **Remap machine IP** if anything is pinned to the old address.

### Set the new address, or you get a blank 400

This is the one that catches people. Homepage rejects any request whose `Host` header isn't in `HOMEPAGE_ALLOWED_HOSTS`, with a bare **HTTP 400 Bad Request** and no explanation anywhere.

Fill in **New address** in the restore panel and DockBack adds it for you as the container is recreated. It **adds** rather than replaces, so the dashboard keeps answering at the addresses it already accepts — replacing the list would fix one address and break every other.

Keeping the same address needs nothing at all.

### Pin the image version

Homepage **does not migrate its configuration**. If a newer version renames a widget key or restructures a setting, the affected widgets just stop rendering — no error, no log line, a blank space where a panel used to be. An older image has the same problem in reverse.

So the warning fires on **any** version difference, in either direction, and blocks nothing — the config isn't wrong, it's written for a different version, and only you can judge that. The reliable answer is to pin the target image to the version the backup came from.

## The Docker socket is reproduced exactly, never widened

Homepage's Docker widget needs the socket, and a socket mount is **authority, not data** — read-write access to it is effectively root on the host.

DockBack records the mount and its mode with the backup, and the restore reproduces it **exactly as configured**. It will never add a socket that wasn't there, and never turn a read-only mount into read-write. After every restore it checks and says so in the log; if it ever found the container had been given more access than the backup recorded, it says so loudly. A restored clone gets no host mounts at all.

> **Worth doing separately:** the Docker widget only needs to *read*. If yours is mounted read-write, change it to `:ro` in your compose file. DockBack won't do it for you — narrowing your access is a change to your deployment, and it isn't the restore's business to make it. But it's the right change.

## Two things to fix that aren't about backups

**Homepage has no login.** It ships without authentication, so whatever protects it has to be in front of it. If yours answers on your LAN or a public hostname without a login prompt, anyone who reaches it sees your whole dashboard and its live data. The embedded API keys stay server-side — Homepage proxies the widget calls, so keys aren't sent to the browser — but the dashboard itself is wide open. Put Cloudflare Access, an authentication rule on your reverse proxy, or Homepage's own optional auth in front of it.

**Check your socket mount mode**, as above.

Neither is caused or fixed by backing up. Both are worth an evening.

## Proving a restore worked

1. The dashboard loads — with the **correct Host header**, which is the whole point of the address setting.
2. The same tiles, bookmarks and layout as before.
3. **At least one widget shows live data** — a real queue count, a real disk figure.

Step 3 is the decisive one. It proves a restored credential still authenticates to a real service, and it proves it *without printing the key* — you're reading the widget, not the config.

## The general rule

Judge a backup by what it would grant if it leaked, not by how large it is or how simple the app is. Homepage is a few kilobytes of YAML that unlock twenty other systems, and that — not its size — is what decides how it should be encrypted, where it should be stored, and how loudly its restore should talk to you.
