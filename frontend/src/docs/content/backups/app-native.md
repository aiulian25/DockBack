# App-native exports (Paperless, Gitea, …)

For some applications the gold standard for backup and recovery is the app's **own** export tooling, because it produces a portable archive that survives version changes and engine upgrades. DockBack supports this as an alternative capture strategy.

## What it is

Instead of (or in addition to) raw volumes and database dumps, DockBack runs the application's documented export command inside the container and stores the resulting export (`appexport.tar`) in the encrypted backup. On restore, it uses the app's matching **import** command to bring the data back — the most robust path across upgrades.

## Recognized applications (no setup needed)

DockBack ships built-in presets for apps with first-party dump tools, so their app-native option appears **automatically** — no profile to research or type. When a container's image is recognized, the container detail page shows an **App-native export (<tool>)** checkbox out of the box:

- **Paperless-ngx** — `document_exporter` / `document_importer`. Fully symmetric: exports and restores in one command each. See **Paperless-ngx & two backups that do different jobs** for what its export leaves behind and what its importer refuses.
- **Gitea** and **Forgejo** — `gitea dump` / `forgejo dump`. Produces a **complete, portable dump** (database + repositories + config + attachments) in one archive.

Recognized presets are conservative **starting points** — you can review and edit them, and a custom profile you save always overrides the built-in one.

> **Restore differs by app.** Paperless has a matching one-command importer, so its app-native backup restores automatically. Gitea/Forgejo have **no one-shot restore** — their dump is a complete portable archive, but bringing it back is a manual, documented procedure (stop the server, load the database, unpack `custom/`, `data/`, `repos/`). DockBack won't pretend an import succeeded when it can't: an attempted restore stops with clear guidance, and a restore drill will flag it. Because app-native export **replaces** the raw volume/database capture, for export-only apps consider keeping a standard volume backup (leave the app-native checkbox off, or run both on different schedules) for a guaranteed one-click restore, and verify any app-native backup with a **restore drill** before relying on it.

## How to use it

App-native export is driven by an **export profile** for the container (the command, working directory, and how files are laid out) — either a built-in preset above or one you enter. When a profile is available, DockBack can produce a portable export rather than raw data, and the container detail page reflects that the app-native option is available. It is opt-in **per backup** (tick *App-native export* when backing up).

## Reuse a profile on another container (Copy / Paste)

A hand-crafted profile (a Vaultwarden dump, a Nextcloud `occ` export, a Home Assistant snapshot…) shouldn't have to be re-typed for every instance. On the container detail page, next to **Save export profile**, use:

- **Copy profile** — serializes the current directory + export/import commands + run-as user to a small JSON on your clipboard.
- **Paste profile** — fills those fields from a profile JSON on your clipboard (or one you paste when prompted). **Review the commands, then Save** — nothing is applied until you save.

So you can define a working profile once and reuse it on the same app running on another node or a second instance, building a personal library without re-typing shell. (The same JSON is available over the API — `GET`/`POST /api/nodes/{id}/containers/{cid}/export-profile` — for file-based or scripted import.)

> **Security:** A custom export/import command runs inside the container with its privileges — treat it like operator-level shell access and use only commands you trust, **including a profile you paste from elsewhere** — read it before saving. Only a signed-in administrator can define or import an export profile, and the change is protected against cross-site requests.

## A preset library (define once, apply anywhere)

Copy/Paste solves one profile at a time. When you run the same app on several containers or several nodes, promote the recipe to a **named preset** in *Settings → Backups → App-native export presets*:

- **Add preset** — give it a name ("Nextcloud occ export"), the export directory, the export and import commands, and optionally a run-as user.
- **Image match** (optional) — a substring of the image reference, e.g. `nextcloud`. Any container whose image contains it is offered your preset automatically. Leave it blank to keep the preset library-only: applied by hand, never claiming an image on its own.
- On a container's page, **Use preset…** above the export fields fills them in. It only fills — you review the commands and press *Save export profile* yourself.

The commands are always shown in full in the list. What will run inside your containers is never hidden behind a friendly name.

### Which recipe wins

Most specific first:

1. **The container's own saved profile** — an explicit decision about that container.
2. **Your matching preset** — your library beats our starting point, so a recipe you worked out for your image doesn't have to fight a built-in.
3. **The built-in table** — Paperless, Gitea, Forgejo.

The built-ins are listed in the library too, marked read-only, with a **Copy to my presets** button so you can start from one rather than retyping it.

### Sharing a library between deployments

**Export presets** downloads your own presets (not the built-ins) as JSON. **Import presets** loads such a file on another DockBack.

> **Import replaces your whole library, and it contains commands that will run inside your containers.** Treat a preset file exactly like a shell script someone sent you: read it first. DockBack asks you to confirm, names how many presets are being replaced, and lists every command in full afterwards. One malformed entry rejects the entire import, so you never end up with half of a library you reviewed as a whole.
>
> Nothing runs merely because a preset matches an image — app-native export stays opt-in **per backup**. A preset changes what *would* run when you tick that box, not whether it gets ticked.

Deleting a preset does not touch any container: a container configured from it keeps its own copy, so removal only stops it being offered for new ones.

## When to prefer it

- You expect to restore onto a **different version** of the app or a fresh deployment.
- The application explicitly recommends its own exporter for backups.

For most containers, the default volume + consistent-database-dump strategy is ideal; app-native export is the extra-portable option for apps that provide first-class tooling.
