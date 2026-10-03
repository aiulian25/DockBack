# Nextcloud (WebDAV)

Store backups in Nextcloud over WebDAV. Works with 2FA/TOTP enabled — the key is to use an **app password**.

## Create an app password (not your login password)

1. In Nextcloud: **Settings → Security → "Devices & sessions"**.
2. Enter a name (e.g. `DockBack`) → **Create new app password**.
3. Copy the generated password — you'll paste it as the Password below.

> **Why an app password?** When 2FA/TOTP is enabled, Nextcloud **rejects your normal login password for WebDAV with a `401`**. App passwords authenticate WebDAV directly and can be revoked individually if ever leaked.

## In DockBack

**Settings → External Backup Destinations → Add Storage Provider → Nextcloud:**

| Field | Value |
|-------|-------|
| **Display name** | A label you choose |
| **WebDAV URL** | `https://YOUR-NEXTCLOUD/remote.php/dav/files/USERNAME/` (include the trailing slash) |
| **Sub-path** | A folder for backups, e.g. `DockBack` (created automatically) |
| **Username** | Your Nextcloud username |
| **Password** | The **app password** from above |

**Test Connection** (write/read/delete round-trip), then **Add Destination**. If the test returns `401`, you're using the login password instead of an app password, or 2FA is blocking it — create an app password and retry.

## Least privilege

DockBack only ever touches the **sub-path** you set. An app password technically still grants access to that account's files, so for strict isolation create a **dedicated Nextcloud user** whose only files are the backup folder and use *its* app password.

## Large backups (chunked upload)

Big archives (e.g. a Jellyfin/Plex `/config`) are uploaded to Nextcloud in **32 MiB chunks** via its chunked-upload API, then assembled server-side. This avoids the **HTTP 413 "Payload Too Large"** error a single multi-GB `PUT` triggers on Nextcloud's reverse-proxy / PHP upload limits — so large offsite copies actually complete. The remote size is verified after assembly, and a failed upload cleans up its temporary chunks (no junk left behind). Plain WebDAV / Synology targets and small files use a normal single upload.

## Performance

WebDAV to a remote Nextcloud is high-latency by nature. DockBack minimizes round-trips (preemptive auth, no redundant directory creation, streamed uploads) so backups stay quick despite that.
