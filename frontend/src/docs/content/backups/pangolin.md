# Pangolin & an application that is several containers

Some applications aren't a single container — they're a small cooperating stack where the pieces only make sense together. **Pangolin** is the sharpest example in the fleet, because it is a tunnel endpoint and a reverse proxy at the same time, and its state is spread across its services:

- **the application** — the database of organisations, sites, resources, users, and the per-site secrets remote agents authenticate with, plus its configuration file
- **the tunnel service** — a 44-byte **WireGuard private key** that *is* the endpoint's identity
- **the proxy** — routing configuration and the certificate store
- **the intrusion-detection service**, where present — its decision database and bouncer keys
- **remote agents** live on *other* machines, so there is nothing here to back up; they simply reconnect

Back these up one container at a time and every archive verifies perfectly. Then restore one, and the service comes back healthy while the deployment does not work: agents authenticate against an identity the database no longer matches, and nothing anywhere says so.

So DockBack treats the stack as **one unit**.

## One snapshot, or none

Use the stack's **app-consistent snapshot** rather than per-service backups. It captures every service inside a single window and tags them with one snapshot id.

- **A member that cannot be captured fails the whole run.** For an ordinary stack DockBack skips a service it could not prepare and warns. Here it stops — a snapshot missing one of its members is exactly the archive this exists to prevent, and restore time is too late to find out.
- **Restoring one service on its own is refused.** That is the case that looks most reasonable and is most damaging. The refusal names the other services, and happens before anything is stopped or overwritten.
- **A stack restore must come from one complete snapshot.** "Latest backup of each service" is a fair convenience for an ordinary stack; here it silently mixes moments in time. A tunnel key from Tuesday and a database from Thursday are both real backups, and together they are not a deployment. The restore dialog says so — and disables the button — before you commit.

**Inspecting a backup is always allowed.** Restore any member as an **isolated copy** and it comes up under a new name on a throwaway network, touching nothing. Looking inside an archive should never need permission.

Per-service backups of this stack are graded **F** with the reason spelled out. Not "partial" — partial means a restore missing some data, and these do not restore.

### The stack backup panel

**Back up stack** opens a panel with the stack-wide choices (destinations, app-consistent snapshot, pause override, per-run compression) and a **per-service table**: every container's effective saved options — compression, *Also save the container image*, incremental capture, app-native export, and how many of its mounts are selected — with a database chip on services that are dumped live. The toggles edit the **same remembered settings** as each container's own page, so nothing can disagree, and changes save immediately and apply to scheduled runs too.

## Which service stops, and for how long

The database is the only thing here under real write load, so it is the only service DockBack **stops** for the copy — a few seconds, for a snapshot the database does not have to recover on next open. Its siblings hold static files and are only briefly frozen.

Worth knowing before you schedule it: for those seconds, traffic through the proxy and the tunnels behind it pauses. Schedule it when that costs least. Every mode is still selectable per service on its container page, with the default and its reason shown.

Capture the **whole config directory**, never a hand-picked file: a SQLite database is written alongside its `-wal` and `-shm` sidecars, and all of them must travel together.

## Moving to another machine

Works on any host — Synology, UGREEN, a Linux box, Windows Docker. The good news first: **nothing inside the stack needs changing, and no remote agent needs touching.** Agents dial the endpoint by domain name, and both the tunnel identity and the per-site secrets travel inside the backup, so once the stack is up the sites re-pair with nothing done to them.

What does not travel is everything outside the container.

**DNS is the whole job.** The records still point at the old machine. Update the endpoint domain's record, and the records for every exposed subdomain — or the wildcard covering them — to the new address. If a proxy service sits in front, it is the **origin** record that changes.

**Certificates hide the problem for two months.** Certificates already issued keep working for the rest of their life, so a move with the DNS left alone looks like it worked. If yours are issued by HTTP challenge, the authority proves each domain by fetching a file from whatever address the DNS record points at — so **every renewal fails silently** until you repoint, and you find out when the first one expires.

Force one renewal after the move. It is the only way to learn this in a minute instead of in sixty days.

**Ports the new host must open:** inbound TCP **443**; inbound TCP **80**, which is not optional if certificates are issued by HTTP challenge, because that *is* the challenge; and the **UDP port your WireGuard listener uses**. The new host also has to reach whatever your resources proxy to.

**Only if the endpoint *domain* changes** — not just the machine behind it — does every remote agent need its endpoint updated.

**Use a domain, never a bare public IP**, and this stays a one-line DNS change forever. If a raw address is written into the configuration anywhere, that single value is the one thing you must edit by hand on the new host.

**Remap path** if the config tree lives somewhere else. **Version:** restore recreates by image digest and the whole set comes from one snapshot, so the services come back as the combination that was tested together. Downgrading the application is blocked — it migrates its database forward on start and offers no way back.

## The private key nobody looks at

This stack's setup commonly leaves the WireGuard private key **world-readable**. It works perfectly that way, which is exactly why it survives: nothing ever complains.

DockBack checks the permissions of files like that one — the tunnel key, the certificate store — and tells you when they are wider than they should be, both when the backup is taken and again after a restore, with the command to fix it.

**It does not fix them.** A restore reproduces the permissions it captured and never changes your security posture without being asked — and tightening is a change too. That rule is worth more than the convenience, and the fix is one command:

```
chmod 600 <the file named in the warning>
```

Only the permission bits are read. The files themselves are never opened.

## This is the most valuable archive on the machine

A decrypted archive of this stack is, at once:

- the **identity of your VPN endpoint** — enough to impersonate the concentrator every remote site connects to
- the **private keys for every certificate** the proxy serves
- the **signing secret of the control plane**, plus the database of users, sites and per-site secrets

Three things follow:

1. **Turn on write-only encryption.** Without it, the key that opens the archive lives on a running server — frequently one the archive grants access to. Write-only seals it to a key you keep offline. DockBack will not refuse a backup without it, because a stack this important with no backup is worse than one encrypted to the master key — but it grades the backup down and says why, every time.
2. **Keep a retention-locked offsite copy**, and never let the only copy live on the machine it protects.
3. **Know the containment steps before you need them.** A leaked, decrypted archive is a full tunnel, certificate and administrative compromise: regenerate the WireGuard key and re-pair every site, reissue every certificate, and rotate the server secret, any bouncer keys, and any credentials stored in the configuration file.

Custody cuts both ways: with write-only on, **losing the offline key means losing the backups.** That is the right trade for this data and it is not a small one — keep it where you would keep a recovery seed.

## The application's own database copies

Pangolin writes periodic copies of its own database inside the captured tree. Keep them if you like — they are a portable extra you can open by hand — but they are not what a restore uses: DockBack captures the live database directly, which is exact and version-matched.

On a large deployment they roughly double the archive, so there is a toggle on the container's page to leave them out. Nothing is lost for a restore either way.

## Proving a restore worked

1. **Log in with the original credentials**, and the organisation, site and resource counts match.
2. **The tunnel is up** — the WireGuard interface exists and the endpoint service is healthy.
3. **One resource is reachable through the proxy with a valid certificate.** The decisive one: it proves the database, the tunnel key and the certificate store came back as a matched set.
4. **One remote agent reconnects** after you repoint DNS — its site's last-seen freshens on its own, with nothing done on the agent.
5. **Force a certificate renewal** and watch it succeed from the new host.

Points 3 and 5 are the two that a partial restore, or a forgotten DNS record, will fail — and nothing else will.

## The general rule for stacks

Any app whose state lives in an **embedded SQLite** file is safest quiesced during the snapshot with its whole data directory captured. Any app that stores an address in its own data is safest relocated via a **domain plus DNS**, rather than expecting a backup tool to rewrite its internals.

And when an application is several containers, ask what one of them is worth alone. Usually the answer is "a working service". Occasionally it is "a healthy container attached to a deployment that no longer functions" — and where that is the answer, the unit of a backup has to be the application, not the container.
