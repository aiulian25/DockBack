# Logs & live streaming

DockBack streams its activity live so you can watch backups and restores as they happen.

## Where to see logs

- **Logs page** (sidebar → Logs) — a live, streamed feed of engine activity across the fleet.
- **Per-run console** — a container's detail page and the restore drawer show the log lines for **that specific run** (starting, dumping, compressing, encrypting, storing, mirroring, verifying — and for restores: restoring, importing, starting).

Logs stream over Server-Sent Events, so they update in real time without refreshing. On the Logs page you can filter by **level** (INFO/WARN/ERR) and by **node** — each backup's lines are tagged with the node they came from, so on a large fleet you can isolate one server's activity.

## Stored run logs (survive a reload)

The live stream is only in memory, so closing the tab used to lose a finished run's log. Now every backup/restore/mirror run's log lines are also **persisted, keyed by the backup**. Open a finished backup in the **Backups** drawer (or the **Log** button on a backup row on a container's page) to expand its full **Run log** — the complete record of the run, not just whatever streamed while you happened to be watching. This is invaluable when a nightly backup failed and nobody was there to see it.

Each stored log has a **Download log** button that saves it as a plain-text `.txt` file for sharing or archiving. Stored logs are trimmed to the newest ~2000 lines per run, and are removed automatically when you delete the backup.

## Metrics (Prometheus)

The `/metrics` endpoint exposes both fleet-wide counters (`dockback_backups_total`, `_failed_total`, `_backups_queued`, `_last_successful_backup_age_seconds`) and **per-node** gauges labeled `{node, node_id}`: `dockback_node_reachable`, `dockback_node_containers_running`, `dockback_node_events_total`, and `dockback_node_last_successful_backup_age_seconds`. The last is handy for alerting — e.g. page if any node hasn't had a successful backup in N hours.

## Reading a backup run

A healthy backup typically shows:

1. `Starting backup of "<name>" …`
2. capture (volumes and/or a consistent database dump; hooks if configured),
3. `Backup stored: <key> (<size>)`,
4. mirroring to each checked destination,
5. `Verifying backup (always-on)` → `Verification PASSED`,
6. retention enforcement (if auto-prune is on).

## Reading a restore run

A restore shows the lifecycle steps: stopping the container (or recreating it for disaster recovery), restoring volume data and/or importing the database dump, then starting the container, ending with a clear **complete** or **failed** result.

## Troubleshooting with logs

When something fails, the log line carries the reason (e.g. a destination unreachable during mirror, or a hook failure). Mirror failures are best-effort and never fail an otherwise-verified local backup — the log notes which destination had trouble so you can fix it and re-run.
