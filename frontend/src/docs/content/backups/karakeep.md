# Karakeep & backing up AI-generated data

**Karakeep** (formerly Hoarder) is three containers of which exactly one holds anything you can't rebuild — and inside that one is a category of data worth thinking about carefully: **things a model produced that no model will produce again the same way.**

## The AI data is the irreplaceable part

Karakeep's database records which tags a **model** attached and which a **person** did — a single column, `attachedBy`. One real library held **6,273** AI-attached tags against **1,832** human ones, plus **397** generated summaries.

Lose that distinction and nothing looks broken. The tags are all still there. But re-running tagging would now treat your hand-curated tags as fair game, and re-generating summaries against a different model version produces different text. It is unrecoverable in the way a deleted file is not: you cannot get *those* back, only new ones.

All of it lives in `db.db`, so the question is simply whether that file comes back exactly.

### How DockBack proves it did

Every SQLite database is captured as a **consistent snapshot** — never a copy of the live file, which matters here because Karakeep's AI worker writes asynchronously and a plain copy can catch it mid-transaction. The snapshot's **SHA-256 is recorded**.

After a restore, DockBack hashes the database that landed on disk and compares:

> Verified /data/db.db: byte-identical to the captured database — every row and value is exactly as backed up

That is the strongest claim available, and the only one that actually covers this case. A row count would report 8,105 tags whether the AI/human attribution survived or not — the distinction is a *column value*, invisible to counting. A checksum sees everything.

If it doesn't match, **the restore fails** and names the database.

## What not to back up

**Meilisearch is derived data.** The search index is built *from* the database, not alongside it. Backing it up costs space and, worse, ties your archive to one Meilisearch version — a restored index can fail against a different build. Leave that service's data out; after a restore, run **Reindex all bookmarks** in Karakeep. Search is incomplete until that finishes while everything else is already correct.

**Reindexing does not touch your AI tags — worth saying plainly if you paid for them.** Karakeep's admin panel has three bulk actions, and they are not the same thing. *Reindex all bookmarks* rebuilds the search index from data already in the database: no AI runs, no pages are refetched, and it is the cheap one. Your AI-generated tags and summaries are **rows in the database**, captured with the backup and already back after the restore — reindexing just makes them searchable again. The expensive buttons are the other two: *Regenerate AI tags* re-runs inference on every bookmark, and *Recrawl all links* refetches every page. A restore needs **neither**. (Verified against the application's own code: the three actions feed three separate work queues.)

**Chrome is stateless.** The crawler container holds nothing. It comes back from your compose file with no data at all, which is exactly right.

DockBack says both of these on the container's page and in the restore panel, so a Meilisearch that comes back empty reads as correct rather than as something that went wrong.

## What to back up

Just the web service: the database and the assets tree — screenshots and archived pages, which in a real library ran to **7.6 GB**. Those are append-mostly, so **turn on incremental backups**: after the first run each backup captures only new archives.

Set the quiesce mode to **pause** for this container. The database is small and the pause is brief, and it removes any question about the background AI worker.

## Moving to another machine

Works on any host — Synology, UGREEN, a Linux box, Windows Docker. **Remap path** if the data directory lives elsewhere; **Remap machine IP** if anything is pinned to the old address.

**Nobody gets logged out.** `NEXTAUTH_SECRET` is a container environment value, captured inside the encrypted archive and reproduced verbatim, so existing sessions keep working. DockBack never regenerates it.

**In-flight jobs resume.** The job queue is captured alongside the database, so anything mid-tagging picks up after the restore. That is safe by construction: an item still pending has no AI data yet, so re-running it cannot overwrite anything curated. Items already tagged are committed in the database and are never re-run.

**Version: newer is fine, older is refused.** Karakeep migrates its database when it starts and offers no way back, so restoring into an older image is blocked before anything is written.

## Your archive is a reading history

It holds private bookmarks, personal notes, AI summaries that describe what you've been reading, and gigabytes of archived web pages. Encrypted always — but keep an **immutable copy off the machine**, and consider **write-only mode**, where the archive can only be opened with an offline key.

## Proving a restore worked

1. **Log in with the original credentials** — proves the session secret came across.
2. Open an AI-tagged bookmark: its tags still show as **AI-attached**, and the summary text is **identical**.
3. A manually-tagged bookmark still shows its **human** tags.
4. An **archived page or screenshot** renders — assets and their links agree.
5. After the reindex, **search finds AI-tagged content**.

Points 2 and 3 are the ones that matter, and the byte-identical check above proves them before you even open a browser.

## The general rule

Sort your data by what could be recreated and what could only be *replaced*. Model output is the second kind — it can be regenerated, but not regenerated *the same*. Anything in that category deserves a byte-level guarantee rather than a plausibility check, and anything derived from it — a search index, a cache — deserves to be left out entirely.
