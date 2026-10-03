# Ransomware tripwire (mass-change detection)

Ransomware has a distinctive backup-side signature: **one run in which most of a volume's files suddenly change** (mass encryption) or **vanish** (a wipe) — often with the changed files all gaining a single new extension. A naive backup system happily captures the encrypted garbage as a "successful backup" and, worse, its retention policy then **ages out the last clean copies** while the schedule keeps running. The tripwire closes exactly that gap.

## What it watches

Containers using **incremental volume backups** already produce an exact per-run file diff (changed + deleted lists against the previous run's index). The tripwire analyzes every delta:

- more than **60%** of the previously-indexed files changed in a single run, or
- more than **40%** were deleted in a single run,

with a **minimum of 200 files** either way, so a small volume (a config directory, a tiny database) can never false-positive on routine churn. When many of the changed files share **one previously-unseen extension**, the alert names it — the classic `.locked`/`.encrypted` smoking gun.

Both percentages and the minimum are tunable in *Settings → Performance & tuning*, where the whole tripwire can also be switched off.

## What happens on a trip

The backup itself **still succeeds** — capturing the evidence (and the deletion list) is valuable, and a backup system should never refuse to run mid-incident. Instead, three things happen:

1. The backup row is flagged **suspect** (a red chip on the Backups page, with the reason in its tooltip).
2. A **critical-path alert** fires through your notification channels: *"Possible mass-change/ransomware event"*, naming the container and the counts.
3. A **retention hold** is placed on that container: no pruning — scheduled, post-backup, or manual sweep — touches its backups until you clear the hold. Your clean pre-event generations stay exactly where they are.

## Reviewing and clearing

Open the container's page — a red banner explains the hold. Review the flagged backups (the incremental chain means the pre-event generations are intact); if in doubt, run a **restore drill** on a pre-event backup or restore it into a sandbox. Legitimate causes exist: a major app upgrade rewriting its data directory, a bulk re-import, a mass photo re-tag. Once satisfied, press **Clear hold** — pruning resumes, the suspect flags reset, and the action is recorded in the audit trail (`tripwire.clear`).

## Scope and limits

- Only containers using **incremental backups** are covered — the detection needs the per-file diff a delta produces. A container on full backups has no such diff; its size/duration is still watched by the ordinary backup-drift anomaly alert.
- The first backup after enabling incrementals (the baseline) and runs against an empty previous index never trip — there was nothing to mass-change.
- The tripwire is a **detector, not a preventer**: pair it with an **immutable (WORM) offsite copy** so even a compromised host can't delete history, and with restore drills so you know the clean generations actually restore.
