# Samba / SMB share

Store backups on any SMB/CIFS server (Linux Samba, Windows share, other NAS).

## On the server

1. Create an SMB share dedicated to backups.
2. Create a user with **write access to only that share**.
3. Note the server IP and (if non-standard) the SMB port.

## In DockBack

**Settings → External Backup Destinations → Add Storage Provider → Samba / SMB share:**

| Field | Value |
|-------|-------|
| **Display name** | A label you choose |
| **Host** | Server IP, add `:445` if the port isn't default — e.g. `10.168.1.50` or `10.168.1.50:445` |
| **Share** | The share name |
| **Sub-path** | Optional subfolder inside the share |
| **Username / Password** | The dedicated user |
| **Domain** | Only for Active Directory / Windows domains — leave blank otherwise |

**Test Connection**, then **Add Destination**.

## Notes

- DockBack operates only under `share` + `sub-path`; pair that with share-level permissions for least privilege.
- SMB is implemented in userspace (no kernel mount), so it works in the hardened container and on networks where you can't mount shares on the host.
