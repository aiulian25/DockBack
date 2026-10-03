# Testing & capacity

## Test Connection

Every Add Storage Provider form has a **Test Connection** button. It performs a **real round-trip**: it writes a small probe object into your configured sub-path, reads it back, and deletes it. This proves three things at once:

- the credentials are valid,
- the path is writable, and
- DockBack can read its own data back.

A green result means the destination is ready. For filesystem-style destinations (SMB), it also reports **free of total** capacity.

## Common test failures

| Symptom | Likely cause |
|---------|--------------|
| `401` on Nextcloud | Using the login password with 2FA on — create an **app password** (*Nextcloud (WebDAV)*). |
| `permission denied` / write fails (SMB) | The user lacks write on the share, or the sub-path doesn't exist and can't be created. |
| Cannot connect / timeout | Wrong host/endpoint/port, firewall, or the service is down. |
| S3 access denied | The key isn't scoped to the bucket, or the region/endpoint is wrong. |

## Live status dot

Each destination card in **Settings → External Backup Destinations** carries a small **status dot** that refreshes automatically (every ~30s):

- **green (pulsing)** — reachable right now,
- **red (pulsing)** + a **Down** label — the destination is currently unreachable (host offline, network/credentials problem, or an unmounted local volume).

This is a *live* reachability check (a cheap probe that writes nothing), separate from the **status chip**, which reflects the result of the **last backup** to that destination.

## Capacity on the card

After adding a destination, its card shows **used of total** capacity where the provider reports it, so you can watch remaining space:

- **SMB / Synology** — reports share capacity.
- **Nextcloud / WebDAV** — reports your account **quota** (used + available) when the server exposes it. Accounts with an *unlimited* quota show "Capacity not reported".
- **S3 / Backblaze B2** — object stores have no fixed size, so capacity isn't shown.

## Fill-up forecasting

Beyond the current-space check, DockBack records each destination's usage over time and fits the trend, so it can tell you **when a destination will fill up** — not just that it's full once it's too late. After a couple of days of history, the card shows the growth rate (e.g. *"Growing ≈ 4 GB/mo"*) and, where a quota is known, a projected fill date (*"Fills in ~18d (≈ 3 May) · +12 GB/mo"*, coloured as the date nears). The header sums this into an **Estimated storage overhead ≈ X/mo** figure.

Because the forecast trends *measured* usage, it already accounts for retention — pruning shows up as the curve flattening. If a destination is on track to fill within the next month, you get a **high-priority alert** ahead of time (throttled to once a day), and external monitors can watch `dockback_destination_days_to_full`. A destination whose usage is flat or shrinking shows *"Usage stable"* and raises no alarm.

## Editing a destination

Use **Edit** on a destination card to change its URL, path, username or password in place — no need to remove and re-add it. For example, point a Nextcloud destination at a new URL, or rotate a password. **Secret fields (password, secret key) show blank — leave them blank to keep the stored secret**, or type a new value to change it. The stored secret is never sent back to your browser. **Test Connection** in the edit dialog verifies the change (reusing the saved secret if you left it blank) before you save.

## After it's added

Check the destination in your **policy defaults** (*Scheduling & Retention → The global backup policy*) so new and scheduled backups mirror to it, then run a backup and confirm a copy lands there (its name appears as a location badge on the backup).
