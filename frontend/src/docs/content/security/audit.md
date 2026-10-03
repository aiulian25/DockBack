# Audit trail

The **Audit Trail** (sidebar → Audit Trail) records administrative actions taken in DockBack, so there's an accountable history of what happened and when.

## What's recorded

Actions such as:

- policy updates (destinations, retention, schedule changes),
- backups started and canceled,
- restores started,
- backups deleted,
- destination and node changes.

Each entry carries who performed it, the action, the target, and a short detail. The page is server-side searchable, date-rangeable, and exportable to CSV/JSON.

## Tamper evidence (hash-chained entries)

A plain audit log protects you from forgetting — not from an attacker with file access quietly rewriting history. Every new entry is therefore **cryptographically chained**: it stores an HMAC (keyed by your master encryption key) over its own fields *plus the previous entry's chain value*. Editing, deleting, or reordering any past entry breaks every later link.

- Press **Verify integrity** on the Audit Trail page (or call `GET /api/audit/verify`) to walk the chain. You'll see *"Checked N rows — chain intact"* or a red callout naming the **first broken entry**.
- DockBack also verifies the chain **automatically once a day** and raises an integrity-failure alert if it's broken.
- Entries written before the feature existed (and entries chained under a previous master key, after a key rotation) are grandfathered behind a recorded chain anchor — they're listed as before but not chain-verified.

One inherent limit: deleting the *newest* entries (truncating the tail) is indistinguishable from them never having been written — export the trail periodically if you need an external copy to compare against.

## The limit that matters more, and the fix

The chain above proves the log is **self-consistent**. It does not protect you from the one attacker who most wants to edit an audit trail: somebody who reached this container. They hold the master key that signs each link, the anchor that decides where verification starts, and the database itself — so they can rewrite the whole history, re-sign every entry, and *pass the check*. The evidence is defeated by exactly the compromise it exists to detect.

More cryptography cannot fix that, because they have the key. What fixes it is keeping the answer somewhere they cannot reach.

Turn on **Audit checkpoint** in *Settings → Notifications*. On a schedule, DockBack sends the trail's current head — an entry number and its chain value — to your channels. That message lands in your inbox, your webhook receiver, your phone. It is now outside this machine, and nothing running here can change it.

To use it, open **Audit Trail → Verify against a checkpoint**, paste the entry number and value from any checkpoint message you kept, and press *Check*. Three answers:

- **It matches** — everything recorded up to that entry is exactly as it was when the checkpoint was taken.
- **It no longer matches** — entries at or before that point were rewritten *after* the checkpoint. This is the case the built-in check cannot see.
- **The entry is gone** — the trail was truncated past your checkpoint, which also answers the tail-deletion limit above.

Two things worth knowing. **Keep the messages** — the oldest one you still have sets how far back you can prove, and each attests only to the entries up to its own head; activity afterwards is normal and doesn't invalidate it. And the *"last sent"* line shown in that panel comes from this database, so it is only there to help you find the right message — it is not itself evidence, for the same reason the chain isn't.

A key rotation publishes a checkpoint immediately, because rotation is exactly when older ones stop covering new entries.

## Why it's separate

The audit trail is its own page (not mixed into operational logs) because it answers a different question: *who changed what*, rather than *what is the engine doing right now*. For live operational output, see *Logs & live streaming*.

## Using it

Review the audit trail after configuration changes, when investigating an unexpected state, or as part of routine operational hygiene. Combined with the encryption-key discipline and least-privilege destinations, it rounds out a defensible backup posture.
