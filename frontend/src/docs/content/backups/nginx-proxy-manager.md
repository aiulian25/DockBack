# Nginx Proxy Manager & two volumes that are one thing

**Nginx Proxy Manager** keeps its configuration in two places that only make sense together. `/data` holds the proxy hosts, the users and the JWT signing keys. `/etc/letsencrypt` holds the certificates those hosts are configured to serve, the ACME account, and the DNS credentials renewals need.

Neither half is a smaller version of the whole. Restore only `/data` and nginx refuses to start — it is told to serve a certificate that is not there. Restore only `/etc/letsencrypt` and the proxy comes up **factory-fresh**: every route gone, and an unauthenticated setup wizard waiting for whoever reaches the admin port first.

So DockBack treats the two as **one unit**.

## Both, or neither

**Capture keeps them together.** If one of them is deselected in the mount picker — or falls out of the size-based default because its size could not be measured — DockBack puts it back and says so in the run log. That is a deliberate override of your selection, and the only one DockBack makes: these are configuration directories measured in hundreds of kilobytes, so the cost of including one you did not ask for is nothing, and the cost of leaving it out is the configuration.

**Restore refuses half.** An archive holding only one of them is rejected *before* anything is stopped, snapshotted or overwritten. Nothing on the target changes; the live proxy keeps running. The archive is still perfectly readable — open its file list and put individual files back by hand if that is what you want.

Such an archive is also graded **F** in the backups list, with the reason spelled out. Not "partial" — partial describes a backup that restores an application missing some of its data. This one does not restore at all.

## It is never paused

Every other application is briefly frozen during the volume copy, because its files could otherwise be written mid-copy. An ingress proxy is the exception, and DockBack defaults it to a **live copy**.

Pausing it would stall every request to every service behind it. Nothing is gained: the only thing in `/data` that could tear is `database.sqlite`, and that is captured by a consistent online snapshot which does not need the container held still. Everything else in both volumes is static files.

You can still choose Pause or Stop on the container's page. The default is shown there with its reason, so it is a decision you can disagree with rather than one made behind your back.

## The factory-reset guard

The failure worth designing against is not a restore that errors. It is a restore that *succeeds* into an empty database, because Nginx Proxy Manager will happily start on one and offer to set itself up.

So after the database is written back and **before the container is started**, DockBack checks that the tables that make it *your* proxy still have rows in them:

- **no accounts** — the state a brand-new install creates
- **no proxy hosts** — nothing behind the proxy would be reachable

If the backup recorded rows there and the restored copy has none, the restore **fails and the container is never started**. Your existing deployment is untouched and rolls back.

If the backup itself recorded none — you backed up an install that was genuinely empty — that is not a defect and does not fail. It is said out loud instead, because restoring nothing is worth knowing about.

## Certificates travel, and their expiry travels with them

Certificates are bound to **domain names, not to a machine**, so they stay valid wherever you restore them. Nothing needs reissuing, and no route needs editing: proxy hosts route by domain to an upstream address, never by the proxy's own address.

DockBack records what is in the archive — each certificate's path, issuer, validity dates, how many names it covers, and whether its private key travelled beside it — and shows it in the restore panel. The point is the one thing a faithful restore cannot fix for you: a certificate that expired while the archive sat in storage restores perfectly and every browser still refuses it.

- **Expired** is reported, never blocked. The restore is correct; the certificate aged. Blocking would refuse exactly the disaster-recovery restore this product exists for, and renewing needs the deployment running.
- **Missing** is fatal, and is caught before the container starts. A recorded certificate that did not come back — or came back without its key — means "cannot load certificate", and a stopped restore is a much better way to learn that than a container log.

**What is never read is the private key.** DockBack establishes that it sits beside its certificate by testing that the file exists. Its bytes are never read, never parsed, never recorded. The same goes for the DNS-provider credential your renewals use — captured with the rest of the data, inside the encrypted archive, and never opened.

The names a certificate covers are **counted, not listed**. The record that describes an archive travels in plaintext to every destination you copy it to, and your internal hostnames are not something a backup should publish in order to describe itself.

## This is the most sensitive archive you have

Treat it that way. It holds:

- the **private keys** for every certificate the proxy serves — a wildcard key impersonates every service behind it
- the **DNS-provider API credentials** used to renew them, which can create further certificates for the whole domain
- the proxy's admin accounts and its JWT signing keys

Three things follow, and DockBack will say so on the backup page:

1. **Turn on write-only encryption.** Without it, the key that opens the archive lives on a running server — often one of the machines the archive grants access to. Write-only seals it to a key you keep offline.
2. **Keep a retention-locked offsite copy**, and give that destination the most restrictive access of anything you have.
3. **Know the containment steps in advance.** If a decrypted archive ever leaks: rotate the DNS-provider token, reissue every certificate it covers, and rotate the admin credentials and signing keys.

## Moving to another machine

Works on any host — Synology, UGREEN, a Linux box, Windows Docker. Restore both volumes together, which is the only thing DockBack will let you do.

- **Remap path** if the volumes live under a different directory on the new host. The container-side paths do not change either way.
- **Ports 80, 443 and 81 must be free** on the target. The restore panel names whatever is already holding them, so a conflict is something you plan around rather than discover from a container that will not start.
- **The new host must reach every upstream** your routes point at, and reach your certificate authority — plus your DNS provider's API if you use DNS-01 validation.

**What does not follow the container is what points clients at it.** The proxy restores identically, and then nothing arrives, because the DNS record, tunnel or router forward still names the old machine. That looks like a failed restore and is not one. Re-point it and traffic returns; keep the same address and there is nothing to do at all.

## Version: newer is fine, older is refused

Nginx Proxy Manager migrates its schema on start, forward only — and its documented behaviour on a schema it does not understand is to fall back to a fresh configuration, which is the same wipe this whole page is about. So restoring a **newer backup into an older image is blocked** before anything is written.

Restore recreates by image digest, so a tag that moved does not matter. Pinning the tag is still worth doing here: it puts you in charge of when the one-way step happens.

## Proving a restore worked

1. **Log in with the original credentials** — not a fresh setup wizard. This is the decisive one: it proves no factory reset.
2. Proxy-host, certificate and user counts match what you had.
3. **Open one proxied service over HTTPS** and check the certificate is the same one and still valid. That proves both volumes came back as a matched pair and nginx loaded the certificate.
4. The rehearsal to use is **Restore as a copy** on different ports. It brings the whole thing up beside the live proxy without contending for 80, 443 or 81, so a restore that is disruptive to get wrong can be tested without any risk at all.

## The general rule

When an application's state is split across volumes, ask what each half is worth alone. Usually the answer is "a smaller backup". Occasionally it is "a broken application, or a factory reset" — and where that is the answer, the selection is not a preference and should not be offered as one.
