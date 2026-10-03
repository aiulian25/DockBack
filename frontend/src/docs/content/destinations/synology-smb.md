# Synology NAS (SMB)

Store backups on a Synology NAS over SMB. (This is a *destination* — for managing the NAS's containers, see *Connecting Servers → A Synology NAS*.)

## On the Synology

1. **Control Panel → Shared Folder → Create** a folder just for backups (e.g. `backups`).
2. **Control Panel → User → Create** a dedicated user and grant it **Read/Write on only that shared folder** — no access to anything else. This is your least-privilege scope.
3. **Control Panel → File Services → SMB → Enable SMB.**

## In DockBack

**Settings → External Backup Destinations → Add Storage Provider → Synology NAS:**

| Field | Value |
|-------|-------|
| **Display name** | A label you choose, e.g. `nas01` |
| **Host** | The NAS IP, e.g. `10.168.1.50` |
| **Share** | The shared folder name, e.g. `backups` |
| **Sub-path** | Optional subfolder inside the share, e.g. `dockback` |
| **Username / Password** | The dedicated backup user |

Click **Test Connection** (a real write/read/delete round-trip; it also reports free/total capacity), then **Add Destination**.

## Notes

- DockBack writes only within `share` + `sub-path`. Combined with a user limited to that shared folder, it can't reach any other data on the NAS.
- Capacity (free of total) is shown on the destination card so you can keep an eye on space.
