# Dockhand & apps that hold the keys to everything else

A few applications are not "an app that happens to have some secrets in it". Their entire purpose is holding credentials to **other** systems — and that inverts how you should think about their backup.

For an ordinary app, the backup protects the data. For these, **the backup is the more dangerous object.** A decrypted archive is worth more than the running instance, because it hands over everything the app can reach, all at once, quietly.

DockBack now names them:

| App | What a decrypted archive grants |
|---|---|
| **Dockhand** | Docker-daemon control of every host it manages — any container, as root, across the fleet |
| **Termix** | SSH access to every machine it connects to |
| **Guacamole** | The remote desktops and servers it reaches — it stores those passwords **recoverably**, because it has to replay them |
| **Linux Update Dashboard** | Root-capable SSH access to every machine it patches |

## Turn on write-only encryption for these

Normally DockBack encrypts each archive with a key it holds, so it can verify, drill and restore on its own. Sensible for almost everything.

It is the wrong trade here. The unwrapping key sits on a running server — frequently **one of the very machines the archive grants access to**. Compromise that server and the attacker gets both halves.

**Write-only mode** seals each backup to an offline public key whose private half never touches any of your servers. The running instance can still create, verify and store backups; it simply cannot open them. Restoring needs the offline key, supplied by you at the time and held only in memory.

Until you do this, backups of these apps are **capped at grade C**, with the reason spelled out on the backup itself. That is not a defect in the backup — it is a statement about what is protecting it.

> **Before you enable it:** generate the keypair and store the private half **off the fleet** — a password manager, a hardware token, printed in a safe. If you lose it, those backups are unrecoverable by anyone, including you. For a credential store that is the right trade; it should still be a decision you make deliberately.

## Nothing is ever decrypted

DockBack never unlocks an application's own secrets. Dockhand encrypts sensitive database columns with its own `.encryption_key`; the dump captures that **ciphertext as-is**, and the key is captured as an opaque file. No token, no password, no private key is ever in plaintext in the pipeline, in a temporary file, or in a log.

That also means the key and the database are an **atomic pair** — neither is any use alone, and both must be in the same backup for a restore to work. DockBack captures them together in one consistency window; you do not need to arrange it.

## Where to store these archives

- **Never only on the machine that made them.** A Dockhand backup living solely on a host Dockhand manages is one compromise away from being both the target and the loot.
- **Retention-locked or immutable**, so ransomware or a mistaken prune cannot remove them.
- **Most restrictive access** the destination supports.

## Restoring Dockhand specifically

Dockhand is a control plane, so a restore has an unusual shape worth knowing:

**Your managed containers keep running.** The agents on every other host are independent of Dockhand. While it is stopped and restored, everything it manages carries on untouched — only Dockhand's own interface is briefly unavailable. There is no need to schedule downtime for the fleet, and no agent needs touching.

**Moving Dockhand needs no change on any managed host.** Dockhand **dials out** to its agents, so it keeps calling the same addresses from wherever it runs. What the new machine *does* need is the ability to **reach every one of those addresses** — including any agent on Tailscale or another private network. Join the new host to that network first, or those environments come back unreachable.

**Check the local socket environment after a move.** It points at `/var/run/docker.sock`, so on a new machine it manages **that** host's Docker, not the old one's. Re-point or remove it if that is not what you meant.

**Version: newer is fine, older is refused.** Dockhand migrates its schema on start with no way back down, so restoring into an older image is blocked.

## Proving a restore worked, without exposing anything

The verification never reads a credential. Let the application itself do the talking:

1. **Log in with the original credentials.**
2. The environments, registries and stacks are all present, with the same counts.
3. **Ping an environment** — a connected result proves the restored token still works, and returns nothing but a boolean.
4. **Test a registry credential** — same idea, same boolean.

Points 3 and 4 are the decisive ones: the app decrypts internally and reports success or failure, and **no token is ever printed**. Rehearse against a spare or test environment so no production agent is disturbed.

## If an archive is ever exposed

Assume full compromise of everything it reaches and rotate accordingly:

- **Dockhand** — every managed host's agent token, the registry credential, and the database password.
- **Termix** / **Linux Update Dashboard** — every SSH key and credential they hold.
- **Guacamole** — every stored connection password.

Rotating is tedious. It is far less tedious than not knowing whether you needed to.

## The general rule

Ask what a decrypted archive would **grant**, not what it contains. When the answer is "access to other systems", the archive outranks the app: seal it to a key that lives nowhere on the fleet, keep it off the machine that made it, and make it immutable.
