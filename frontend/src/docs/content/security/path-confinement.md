# Destination path confinement

DockBack is built so it can **never** read, overwrite, or delete files outside the backup folder you give it on a destination — protecting the rest of your NAS, Nextcloud account, or bucket.

## The guarantee

Every storage backend (SMB, WebDAV, S3, local) confines all operations to the configured location:

- **SMB** — under `share` + `sub-path`.
- **WebDAV (Nextcloud)** — under the WebDAV URL's `sub-path`.
- **S3 / B2** — under `bucket` + `sub-path` (key prefix).
- **Local** — under the backup root volume.

On top of that, a validation layer rejects:

- **empty keys** — which could otherwise resolve to the folder root, and
- **path-traversal segments** (`.` / `..`) — so no input can climb out of the backup folder.

This means a bug, a malformed name, or unexpected input can't escape the backup folder. Retention pruning and deletes operate only on DockBack's own backup objects within that folder.

## Defence in depth: scope the credential too

Application-level confinement protects against DockBack touching other files. For the strongest posture, also limit what the **credential** can see:

- **Synology/SMB** — a user with access to only the backup share/folder.
- **Nextcloud** — a dedicated user whose only files are the backup folder; use its app password.
- **S3/B2** — a key scoped to just the backup bucket.

With both layers in place, even a fully compromised credential can only reach your backups, nothing else.
