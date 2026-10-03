# Per-container overrides

Most setups are happy with the global policy, but you can tailor behavior per container when needed — ideal for **large** apps (Jellyfin, Plex, Audiobookshelf, Bookstack) where keeping the fleet-wide number of copies would eat storage.

Overrides resolve most-specific-first: **container → node → cluster → global**. A container with no override simply inherits, and each group of settings (destinations, retention) resolves independently — so a cluster can set destinations while a container sets only retention. See **Connecting Servers → Clusters** for the cluster tier.

## Granular control per container

On a container's detail page, open **Advanced — backup schedule & retention** (below the backup button). Each section is independent — leave one off to inherit the global setting.

### How often to back up

Turn on **Override how often to back up** and choose **Back up at most**:

- **Every scheduled run** (default) — no throttle.
- **At most once a day / week / month.**

When the global schedule fires, the container is skipped if its last successful backup is newer than that interval. So you can keep small apps on the nightly schedule while a large media server backs up weekly or monthly — fewer copies created, less space used. (Manual "Initiate Backup Now" always runs; the throttle only affects scheduled runs.)

### How many copies to keep (and for how long)

Turn on **Override how many copies to keep** to set, for this container only:

- **Copies to keep (newest N).**
- **Daily / weekly / monthly** — additionally keep the newest backup in each recent day/week/month (0 = off). The newest backup is always kept.
- **Auto-prune older copies after each backup.**

This is the direct fix for "3 copies of a huge container fills the disk": give that one container a smaller count (or a shorter GFS window) without changing the fleet default.

## Choosing what's covered (the schedule)

Pick coverage per schedule in **Settings → Schedules**:

- **Whole node** — back up all running containers on that server.
- **Specific containers** — expand a node and tick individual containers.

Specific containers are tracked by **name**, so a target survives the container being recreated (a new ID, same name). If a targeted container no longer exists, the run skips it, alerts, and the picker flags it as **missing**. You can clear stale entries individually or with the **Remove all missing** button on the picker. To stop dead targets accumulating, set **Auto-remove targets missing for N days** on the Schedules card (0 = never): once a target's container has been gone that long, it is dropped from its schedule automatically and the removal is recorded in the audit trail.

## Destinations per node & per run

- **Per node:** a node can override where its backups go (**node page → Backup Policy — this node**) — e.g. keep a Synology *node's* own containers off the Synology *destination* to avoid a single point of failure.
- **Per run:** a manual backup's destination checklist starts from the effective defaults but you can change it for that one run.

> Looking for the fleet-wide knobs instead? See *The global backup policy*.
