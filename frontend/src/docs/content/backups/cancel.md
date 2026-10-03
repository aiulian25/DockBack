# Cancel a running backup

A backup that's still in progress can be stopped — useful if you started the wrong one, or it's running at a bad time.

## How to cancel

- **From a container's detail page:** in the **Available Backups** table, a running backup shows a **Cancel** button in the Retention column. Click it.
- **From the Backups page:** open the running backup and click **Cancel Backup** in its drawer.

The run stops and the record is marked **Canceled** (distinct from Failed). A queued backup that hasn't started yet can be canceled too.

## What happens to partial data

Canceling unwinds the in-flight run; the catalog records it as canceled rather than leaving it stuck. No verified/usable backup is produced by a canceled run.

## Interrupted by a restart

If the DockBack container itself is restarted or rebuilt while a backup is running, that run can't finish. On the next startup DockBack **discards** any backup left mid-run — it never produced a usable archive, so it's removed rather than kept as a confusing *failed* entry — and you never see a backup stuck *In Progress* forever. The next scheduled (or manual) run simply backs the container up again.
