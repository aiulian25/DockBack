# Label-driven policy (dockback.* labels)

If your homelab is defined in Compose files (GitOps), you can declare backup intent **right where the container is defined** — with `dockback.*` labels — instead of clicking in the UI. DockBack reads these labels on every inventory refresh and applies them automatically. This makes your backup policy part of your infrastructure-as-code: it travels with the compose file, survives a recreate, and is reviewable in version control.

## The labels

| Label | Value | Effect |
|---|---|---|
| `dockback.enable` | `true` \| `false` | `true` adds the container to backup schedule coverage; `false` opts it out. |
| `dockback.schedule` | a schedule **id** | Adds the container to that named schedule. An unknown value falls back to default coverage. |
| `dockback.pause-mode` | `pause` \| `stop` \| `none` | Consistency mode during the volume copy (see *Creating Backups → What a backup captures*). |
| `dockback.mounts.exclude` | `/path1,/path2` | Leaves those mount destinations **out** of the capture (everything else is included). |
| `dockback.retention` | `daily:7,weekly:4,monthly:6,yearly:1` | Per-container GFS retention. Also accepts `generations:N` and `autoprune:true`. |

### Example (Compose)

```yaml
services:
  sonarr:
    image: lscr.io/linuxserver/sonarr
    volumes:
      - sonarr_config:/config
      - /mnt/media:/media
    labels:
      dockback.enable: "true"
      dockback.schedule: "nightly"
      dockback.pause-mode: "pause"
      dockback.mounts.exclude: "/media"          # back up /config, skip the media library
      dockback.retention: "daily:7,weekly:4,monthly:6,yearly:1"
```

## How it behaves

- **Labels win.** For every field a label sets, the container detail page shows a read-only **"Managed by labels"** banner and disables that control — so the UI and the labels can't drift. Fields the labels *don't* set stay editable and inherit the global policy as usual.
- **Idempotent.** DockBack only writes stored policy when the labels actually change, so a busy fleet doesn't churn the database on every refresh.
- **Removing a label reverts it.** Delete a `dockback.*` label (and let the inventory refresh), and DockBack undoes exactly what that label had set — the container returns to inheriting defaults. Removing `dockback.enable=true` also removes the container from label-managed schedule coverage.
- **Garbage is ignored.** A typo'd value (e.g. `dockback.enable: yes-please`) is skipped rather than breaking discovery — that one field is simply left unset.

## Notes & limits

- Labels are read from the running container, so a change takes effect on the next inventory refresh (within ~30s) after the container is recreated with the new labels.
- `dockback.schedule` targets an existing **named schedule** by id (create schedules under *Scheduling → Scheduled backups*). Ad-hoc cron strings are intentionally not turned into new schedules, to keep the schedule list predictable.
- Destinations remain a global/node control and are **not** settable per-container by label.
