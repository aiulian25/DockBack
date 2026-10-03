# BookStack & apps configured entirely by environment variables

**BookStack** is a two-container stack — the app plus its own MariaDB — and it is a good example of a pattern worth understanding, because getting it right depends on one fact that is easy to miss: **BookStack has no `.env` file to back up.** Its entrypoint actively deletes one if it finds it. Every setting, including the encryption key, is a container environment variable.

## Back up the whole stack, not just the app

| Service | What matters | How DockBack captures it |
|---|---|---|
| **app** | `public/uploads` (images embedded in pages) and `storage/uploads` (attachments, avatars) | paused during the snapshot, archived with numeric owners |
| **db** | Everything else — books, chapters, pages, **full revision history**, users, roles, permissions, comments, tags, the audit log and settings | a consistent MariaDB dump; the raw data directory is excluded automatically |
| **both** | The container environment, including `APP_KEY` and the database and mail passwords | inside the encrypted archive only |

Back them up **together as a stack**, so the database dump and the uploaded files come from one pause window. That matters more than it sounds: page HTML references image files by path, so a database captured at one moment and files captured at another can give you pages with broken images.

## `APP_KEY` must be preserved — and it is, automatically

BookStack uses `APP_KEY` to **encrypt two-factor secrets** and to sign sessions and tokens. If it changes:

- every user with **MFA enabled is locked out** until an administrator resets their two-factor setup;
- sessions, "remember me" tokens and password-reset links are all invalidated.

Your pages and books are *not* encrypted with it, so nothing is lost — but "everyone has to re-enrol MFA" is not a successful restore.

Because `APP_KEY` is a container environment variable, DockBack preserves it with no effort on your part: a restore recreates the container with the identical environment, taken from inside the encrypted archive. **DockBack never rewrites an application's configuration or data**, so the key that comes back is the key that left.

> If you ever *want* to rotate it — a suspected key compromise, say — that is a deliberate action you take afterwards, with the MFA and session consequences above. It is never something a restore does quietly.

## Moving to a different machine

This works on any host — a Synology or UGREEN NAS, a Linux box, a Windows machine running Docker. The container is recreated from its **recorded image digest** with its original environment, so what comes back is the version you backed up, not whatever the tag points at today.

Two things typically need your input, and both are ordinary restore options:

- **Host paths.** Your bind mounts point somewhere that may not exist on the new machine (`/volume1/docker/...` on a Synology means nothing on a Linux server). Use **Remap path** in the restore panel to give the new base directory. The paths *inside* the container never change.
- **Addresses.** If anything is pinned to the old machine's IP, use **Remap machine IP** and DockBack rewrites it in the recreated configuration, the compose file and the environment.

### Keeping the same address needs no URL rewrite

If BookStack keeps serving at the same domain after the move — the usual case, where you just re-point your reverse proxy or tunnel at the new host — **there is nothing else to do**. Content links keep working.

### Changing the address is a separate, irreversible step

BookStack stores **absolute URLs inside page content**, so a genuine domain change needs its own rewrite. DockBack **will not do this for you**, because it edits your content in place and cannot be undone. Instead, after a successful restore, the run log prints the exact commands with your current address already filled in:

```
docker exec BookStack php artisan bookstack:update-url https://old.example.com https://NEW-ADDRESS-HERE
docker exec BookStack php artisan cache:clear
```

Before running them: take a backup, update `APP_URL` in your compose file to the new address, and recreate the container. The rewrite command asks for confirmation before changing anything.

## Version: newer is fine, older is refused

BookStack runs `php artisan migrate --force` **every time it starts**, so a restored stack migrates itself forward as soon as it comes up. That is convenient and it **ratchets**: once a newer image has advanced the schema, older images can no longer run that database.

- Restoring into the **same or a newer** version is fine — DockBack warns that the migration is one-way.
- Restoring into an **older** version is **blocked**, because it would fail at start after the data had already been written.

This is a good reason to **pin the image tag** rather than tracking `latest`. Each automatic update silently narrows the range of images your *older* backups can be restored into.

## Why restores of this app watch a little longer

The BookStack image ships **no healthcheck**, so Docker can only report "running". For an app that migrates its database at startup, that is reported a second or two after the container starts — while the migration is still going. A migration that then failed would take the container down *after* the restore had already been called healthy.

So for this specific combination — migrates at startup, no healthcheck — DockBack keeps watching for a short window and **fails the restore if the container stops**, then writes the startup output (which contains the migration log) into the run log either way.

**Adding a healthcheck to the service removes the guesswork entirely** and is worth doing:

```yaml
healthcheck:
  test: ["CMD-SHELL", "curl -f http://localhost:8080/status || exit 1"]
```

With that in place the ordinary health gate becomes a real verdict, and restores gate on the application genuinely answering rather than on the process merely existing.

## Proving a restore worked

The decisive check is not that the site loads. It is:

1. **Log in as a user who has MFA enabled**, using their existing authenticator code. This is the one test that proves `APP_KEY` travelled correctly — a regenerated key fails exactly here and nowhere else.
2. Open a page with an **embedded image** and download an **attachment** — proves the database and the uploads came from the same moment.
3. Check a page's **revision history** is complete.
4. Confirm a user with restricted permissions **still cannot** see what they could not see before.

## The general rule

For any app configured purely by environment variables, the environment **is** the configuration — and DockBack keeps it inside the encrypted archive, never in the plaintext manifest. For any app that migrates its schema at startup, **pin the image tag** and keep the restore target at the same version or newer. And for any app that stores absolute URLs in its content, treat a domain change as a separate, deliberate, one-way step — never part of the restore itself.
