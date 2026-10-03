# Restore by version & source location

When you open a backup in **Backups**, the **Restore from** section lets you choose exactly *which point in time* and *which copy* to recover from.

## Choosing the version (point in time)

The **Version** dropdown lists every successful backup of that same container, newest first, with date and size (the most recent is marked *latest*). Pick the snapshot you want — selecting a different version re-points the whole drawer (manifest, verification report, copies) to that backup, so you can compare before committing.

This matters when the latest backup already captured a problem (e.g. bad data, a failed migration) and you need to roll back to an earlier known-good point.

## Recovery timeline

Above the dropdown, a **recovery timeline** lays out the same recovery points left-to-right (oldest to newest) so you can *see* the container's history instead of reading a list. Each point is colored by state:

- **Green** — verified by test-restore; a **ringed** green point is **drill-proven** (an actual sandbox restore passed).
- **Amber** — degraded: the backup is safe locally but an offsite copy is missing.
- **Red** — a restore drill failed, or the backup is encrypted with a key that isn't the current one.
- **Grey** — stored but not yet verified.

A dashed blue link with a flag marks where the **image version changed** (an upgrade), and a small ▲/▼ under each point shows how much the backup **grew or shrank**. Hover any point for its date, image, size change, and copies; **click** it to scrub the drawer to that version, ready to restore or *Revert update*. This makes "take me back to when it worked" a single glance and click.

The same timeline appears on a container's page (under **Available Backups**) — click a point there to jump straight into its restore drawer.

## Choosing the source copy

The **Source copy** dropdown controls *where the archive is read from*:

- **Auto — fastest available (local first)** — the default. DockBack reads the local copy if present (fastest), otherwise an offsite copy, always integrity-checking first.
- **Local (fastest)** — force the local copy.
- **Each offsite copy** — e.g. *nas01 (synology)*, *NextCloud-RO (webdav)* — restore directly from that destination, for example when the local disk is gone.

> A chosen copy that's unreachable or fails its integrity check **automatically falls back** to the others, so picking a source can never strand a restore.

## Choosing the target machine (cross-host restore)

When you manage more than one node, the drawer also shows **Restore to (node)** — defaulting to the backup's original machine. Pick a **different** node to restore onto **another machine**: DockBack recreates the container there, re-pulling the image **by digest** (or loading the bundled image tarball if the backup was saved air-gapped), and recreates the networks and volumes under their original names. This is how you rebuild a stack on a replacement host after a machine dies, or clone it to another server.

> Restore is **in-place by name**: if the target node already runs that same-named container, it is overwritten **in place** (with a safety snapshot first, if enabled) rather than started as a parallel copy. Stack restore (recreating a whole compose project at once) runs on the backup's original node.

## Image drift — when the image moved on since the backup

A restore replays the container's **old configuration** into whatever image will actually run. That is usually what you want — and occasionally exactly wrong, because the image itself has moved on.

The case that motivated this: a Paperless-ngx container backed up before an upgrade was restored after one. The restore succeeded, the container came back, and then died in a loop — the newer version required a setting the older configuration had no reason to set. DockBack reported a healthy restore while the application was broken.

Now, if the exact image a backup was taken from is no longer available, the drawer says so before you start:

```
Image changed since this backup — may need new configuration
• image changed since this backup (3.0.0 → 3.3.1) — a newer version may
  require configuration this backup does not have; check the project's
  release notes
• the new image declares 1 environment default(s) this backup does not set:
  PAPERLESS_SECRET_KEY — the newer version may require them
```

DockBack compares what the backed-up image **declared** against what the image about to run declares, and reports the differences that commonly need new configuration: new or removed environment defaults, a changed `ENTRYPOINT` or `CMD`, a new `VOLUME` the backup has no data for, and a gained or lost `HEALTHCHECK`. Keys the container already sets itself are not reported — otherwise every routine image bump would produce a wall nobody reads.

Version numbers come from the image's own version **label**, because with a floating tag like `:latest` the reference is identical across upgrades and only the digest moves.

### An honest limit worth knowing

This can only see what an image *declares*. A project that adds a hard requirement **without** declaring a default — which is what Paperless did — leaves no trace in the image configuration. That is why the change itself is reported as a signal, rather than treating silence as safety: when the exact image is gone, you are told so and pointed at the release notes even when nothing else differs.

Like the portability preflight, this **informs and never blocks**, and it is silent whenever the exact recorded image is still available — including for every restore that pulls by digest.

## Running it

1. Choose **Version** and **Source copy**.
2. Confirm the destructive-action prompt.
3. Watch live progress in the drawer; you'll get a clear **complete** or **failed** result.

Restore is only enabled for **verified** backups. To restore only part of a backup, see *Volumes-only or database-only*.
