# Notifications

The original problem this tool exists to solve is *silence* — a backup quietly breaking and you only finding out months later. DockBack pushes you an alert when a backup **fails**, and most importantly when a backup completes but **fails verification** (so you never trust a backup that can't actually be restored).

Configure channels in **Settings → Notifications**. Each channel independently subscribes to:

- **On failure & alerts** — recommended, on by default. Fires for anything that puts your data at risk or needs attention (see *Severity & alert types* below), sent at high priority.
- **On success** — optional, for a per-backup "all good" ping.

### Minimum severity (route by importance)

Each channel also has a **Minimum severity** selector so channels can subscribe *independently* by importance — for example, send **Critical only** to email while your **Gotify** takes **Warning and up**:

- **Match success/failure toggles** (default) — keeps the two toggles above; nothing changes for existing setups.
- **Info and up** — everything, including per-backup successes.
- **Warning and up** — warnings and critical alerts (no success pings).
- **Critical only** — just the data-at-risk events.

When you pick a severity floor it **overrides** that channel's success/failure toggles. Leaving it on *Match toggles* preserves today's behavior exactly.

## Severity & alert types

Events carry a **severity** so the important ones are never buried in a success digest. Success is *info* (the "On success" ping); everything below is an **alert** delivered on the "On failure & alerts" subscription, at a priority that scales with severity (webhooks also receive a `severity` field for routing on your side):

- **Critical** — *backup failed*, *verification failed*, *scrub found corruption* (a stored backup that no longer verifies — bit-rot or a destination gone bad), *restore failed*, *restore rolled back*, *host key changed*, *sidecar image changed*, *sign-in locked out* (repeated failures locked an address — someone guessed at your console until it stopped them) and *master key revealed* (the encryption key was displayed to someone; legitimate when it was you making a recovery sheet, an incident when it was not). Act now. A *backup failed* alert is sent only once a run has **exhausted its automatic retries** (see below) — a blip that the retry recovers never pages you.
- **Warning** — *no offsite copy* (a backup verified locally but didn't reach a destination, so 3-2-1 isn't satisfied), *destination almost full* (**90%** used by default), *destination filling up* (projected to fill within **30 days** by default), *missed schedule* (the app was down at backup time), *unusual backup* (a run took or grew far more than the container's recent norm — an early sign of a runaway volume or a slow disk), *container crashed* / *out of memory* (a container you protect died with a non-zero exit code, or was OOM-killed by the kernel), *encryption key not backed up* (backups exist but you haven't confirmed the master key is saved — losing it destroys every backup), and *restore canceled* (you stopped a restore part-way — not a fault, but a destructive operation was interrupted and the record matters).

Two warnings watch **coverage**, checked hourly from the cached inventory and the catalog:

- *backups no longer recent* (`coverage.stale`): a container's newest backup is older than twice the interval of the schedule covering it (8 days if none does). A stack is named once, with the age of its oldest member's backup. Each one is announced once, and again only if it recovers and later lapses. The first check after an upgrade sends everything already stale in one message.
- *new stack with no backup schedule* (`coverage.new_project`): a compose project DockBack hasn't seen before appears, and no enabled schedule covers it. The first check only records what exists, so turning this on never floods.

Two more watch **DockBack's own backup** (see *Security → Back up & restore DockBack itself*): *DockBack's own backup is stale* (`appbackup.stale`, the newest app backup is older than 8 days or there is none) and *DockBack could not back itself up* (`appbackup.failed`, a scheduled app backup or its push off the machine failed).

Two further warnings watch the sign-in surface itself: *repeated failed sign-ins* (an address is grinding at the login — the early warning before it gets locked) and *re-authentication failed* (a session that is already signed in failed the password/two-factor check in front of a protected action such as revealing the key or an overwrite restore; if that was not you, that session is live — revoke it and change the password). Repeats from one address are throttled to one alert per lockout window, so an attacker retrying all night is one message, not hundreds.

The *unusual backup* check compares each successful run's **duration and archive size** against the median of that container's last 8 successful backups; if either is more than **3×** the usual (configurable in *Settings → Performance & tuning → Backup drift alert factor*), it warns once. A container with fewer than 3 prior backups is never flagged.

The *almost full* and *filling up* checks now cover the **local backups volume** (shown as *Local backups volume* in the alert and in **Insights → Capacity outlook**), not only offsite destinations — so the disk every backup lands on warns you days ahead of a nightly run failing on a full volume, instead of only at the moment it fails.

The *crashed* / *out of memory* alerts fire only for containers DockBack **watches** — one with a successful backup on record or with *Back up before changes* enabled — so a throwaway container's exit never pages you. A clean stop (exit 0) is silent, and an OOM kill raises a single *out of memory* alert rather than doubling up with the crash that follows it. If the crashed container also has *Back up before changes* on, a protective pre-change snapshot is taken from the same event, so you keep a fresh rollback point before it can crash-loop into a bad state.

**Automatic retry.** A backup that fails on a **transient** error (the node briefly unreachable, a database not yet ready, a registry hiccup pulling the volume sidecar) is re-queued automatically with a backing-off delay — **1 minute**, then **5 minutes** — up to **3 attempts total**, instead of waiting for the next scheduled run. Each attempt is its own entry in the backup history, so you can see the retries happened. A **permanent** failure (an invalid selection, no space left) is never retried and alerts immediately. Only the final, failed attempt (or a permanent failure) sends the *backup failed* notification.

Recurring conditions (a stuck-full destination, a dead offsite target, an un-backed-up key) are **throttled** — you're alerted once per condition per window, not on every backup — so alerts stay meaningful.

> **Tune the thresholds.** The "almost full" percentage and the "filling up" forecast horizon (days) are configurable in **Settings → Performance & tuning** — lower the percentage or shorten the horizon for a tiny destination that needs earlier warning, or relax them for a large one. The same card sets how many stored backups are re-verified per scrub cycle.

### Restore alerts are their own kinds

A restore failure used to arrive as *scrub found corruption*, which made two very different things indistinguishable: a stored backup rotting overnight is **hygiene**, while a restore failing right now is an **incident in progress**. They now have their own kinds:

| Kind | Severity | Means |
|---|---|---|
| `restore.failed` | Critical | A live restore did not complete — including a whole-node restore, and the case where the automatic rollback *also* failed |
| `restore.rolled_back` | Critical | The restored container never came up healthy, so DockBack rolled back to the pre-restore safety snapshot |
| `restore.canceled` | Warning | You stopped a restore mid-run. Data already written stays written — DockBack never rolls back behind your back |

Because severity routing is per channel, you can now push a live restore failure to Gotify while overnight scrub regressions go to email only.

In **Logs → Alerts** each alert shows its kind, and a **Kind** filter appears once more than one kind is present — so "show me everything that went wrong with restores" is a single selection. The list is built from the alerts themselves, so new kinds appear automatically.

## Daily summary (digest)

On a fleet that backs up dozens of containers in a nightly window, a *ping per backup* becomes noise — so people turn success alerts off and lose the "it ran" signal entirely. Enable **Daily summary** (Settings → Notifications) to get one message at a chosen time instead:

> DockBack daily summary — Last 24h: 42 backups, 42 verified, 0 failed. Destinations: 3 of 3 healthy. RPO: all 2 critical databases on target. Coverage: stale backup — arr on NUC (last backed up 12d ago). never backed up — searxng on NUC. DR confidence: 8 of 9 stacks drill-proven, 1 undrilled, 0 partial backups. App-backup proven 2d ago. Key backup: confirmed.

- **Off-site** — when every backup copy is on this machine, or DockBack's own backup is, the digest says so.
- **Coverage** — always included: every stack or container whose backup is stale and every one never backed up, by name, or "all N running containers have a recent backup".
- **Summary time** — the local time the digest is sent (once per day).
- **Success notifications** — choose **Per backup** (a ping for each, today's behavior), **Daily summary** (the digest *replaces* per-backup success pings), or **Both**.
- **Include DR confidence** (on by default) — appends a disaster-recovery line: how many stacks are **drill-proven** (a restore drill passed on their latest backup) vs. **undrilled**, how many latest backups are **partial** (some mounts skipped), whether the **app-backup** restore has been proven and when, whether the **master key** is confirmed backed up, and — when any pilot-light standbys are configured — how many are **proven** on their fallback node ("Standby: N of M proven."). It answers "if the building burned down tonight, how much of this is actually proven recoverable?" at a glance. Turn it off to keep the digest to the last-24h line only.

The DR-confidence line reads from the same catalog and drill records — no extra Docker or network probes. The digest is assembled from the same catalog the **Insights** page uses — no extra Docker or network probes. **Failures and critical alerts are never rolled into the digest** — they still notify immediately, so a problem never waits until tomorrow's summary. The digest is an *info*-severity message, so it follows the same per-channel routing as a success ping (a channel set to **Warning and up** won't receive it).

## Channels

- **Gotify** — self-hosted push. Enter your server URL and an application token. Failures are sent at high priority.
- **Email (SMTP)** — host, port (587 with STARTTLS is typical), optional username/password, from and to addresses. Multiple recipients can be comma-separated. Alert emails are **subject-tagged with severity** — `[CRITICAL] …` or `[WARNING] …` — so you can filter or route them with a mailbox rule; routine success emails are left untagged.
- **Webhook** — POSTs a small JSON payload (`{kind, severity, title, message, text, time}`) to any URL. The `text` field makes it drop-in compatible with Slack incoming webhooks and similar; `kind` and `severity` let you route or filter on your side.
- **Heartbeat (dead-man's-switch)** — the opposite of an alert. DockBack **pings** an external monitor (healthchecks.io, Uptime Kuma push, or any URL) on every **verified** backup; you configure that monitor to alert if the ping *stops*. This is the only way to catch DockBack dying entirely — in-app alerts can't fire if the app is down. A failed/unverified backup deliberately does **not** ping, so a run of failures also trips the monitor. Optionally set a **keep-alive interval** so a heartbeat is also sent between backups (good for "push" monitors that expect a regular ping); leave it at 0 to ping only on verified backups.

Use **Send test** on each channel to confirm it's wired up before you rely on it.

## Set up an Uptime Kuma heartbeat

Uptime Kuma has a **Push** monitor built for exactly this: it expects your service to ping a URL on a schedule and marks itself **down** if the ping stops. Wire it to DockBack in a few minutes.

**In Uptime Kuma:**

1. Open Uptime Kuma and click **`+ Add New Monitor`**.
2. Set **Monitor Type** to **`Push`** (not the default HTTP(s)). A **Push URL** field appears.
3. Give it a **Friendly Name** (e.g. *DockBack backups*).
4. Set the **Heartbeat Interval** (seconds) — how long Kuma waits for a ping before it alarms. Make it comfortably **longer** than how often DockBack pings (see the timing rule below).
5. Click **Save**. Kuma now shows the monitor's **Push URL** — copy the **whole** thing. It looks like:
   ```
   http://<kuma-host>:<port>/api/push/<token>?status=up&msg=OK&ping=
   ```
   To find it again later, open the monitor and click **Edit** — the Push URL is there.
6. Attach a **Notification** to the monitor (its edit page → *Notifications*) so a missing heartbeat actually alerts you. Without this, Kuma tracks the status but never tells you.

**In DockBack (Settings → Notifications → Heartbeat):**

7. Tick **Heartbeat (dead-man's-switch)** and paste the **full** Push URL into **Ping URL**.
8. Optionally set a **keep-alive interval** in minutes so DockBack also pings between backups; leave it at `0` to ping only on a verified backup.
9. Click **Send test** — the Kuma monitor should turn from grey **Pending** to green **Up**.

**Timing rule (avoid false alarms):** DockBack's ping cadence must be *shorter* than Kuma's Heartbeat Interval, with margin. If keep-alive is `0`, DockBack only pings on a verified backup, so set Kuma's interval a bit longer than your backup schedule (e.g. daily backups → a ~26–48h interval); if you set a keep-alive (say 60 min), make Kuma's interval ~2× that.

**Two things that quietly break it:**

- **Use the full Push URL, not Kuma's base address.** A bare `http://<kuma-host>:<port>` is the web UI — it redirects, so *Send test* can look like it passed while no monitor ever receives the ping. The URL must contain `/api/push/<token>`. Pasting the full Push URL into a browser should return `{"ok":true}`.
- **Egress allow-list.** If you've set `DOCKBACK_EGRESS_ALLOW` (default-deny outbound), the heartbeat is refused unless Kuma's host is covered — add its hostname, IP, or a CIDR (e.g. `10.168.1.0/24`) to that list and recreate the container. If `DOCKBACK_EGRESS_ALLOW` is unset, any host is allowed. See *Security & Operations → Egress allow-list (outbound control)*.

The `<token>` is effectively a secret — anyone who can reach the URL can send or spoof a heartbeat — so keep it private. DockBack stores it encrypted with the other notification settings.

## Security & behaviour

- **Secrets** (the Gotify token, SMTP password) are **encrypted at rest** with your master key and are never sent back to the browser — the field shows as stored; leave it blank to keep the existing value.
- Notifications are **best-effort and asynchronous** — a slow or down notifier never delays or fails a backup; a delivery error is logged.
- Notification endpoints are **outbound connections** you configure. Point them only at servers you trust.
