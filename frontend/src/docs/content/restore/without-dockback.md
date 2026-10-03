# Restore without DockBack (offline tool)

The whole point of a backup is that it survives the tool that made it. DockBack backups are deliberately built so that, in the worst case — DockBack is gone, the server is gone, all you have left is a `.dback` file and your encryption key — you can still get your data back. This page is that path.

You do **not** need DockBack, Docker, or a Go toolchain. You need three things:

1. The **`.dback`** archive (and, ideally, its `.manifest.json` sidecar next to it).
2. Your **64-hex master key** (`DOCKBACK_ENCRYPTION_KEY` — the one on your recovery sheet).
3. A machine with **Python 3**.

## Get the recovery script

`dockback-recover.py` is a single, dependency-free Python script. Get it either way:

- **From your running DockBack** — open the **encryption-key recovery** dialog (the "Back up your encryption key" panel) and click **Download dockback-recover.py**. The dialog also shows its **SHA-256**, and that same fingerprint is printed on your **recovery sheet**, so the sheet is a complete recovery kit.
- **From the release page** — every DockBack release attaches `dockback-recover.py` as an asset. The recovery sheet includes the exact URL and its SHA-256.

Verify what you downloaded matches the fingerprint on your sheet:

```bash
sha256sum dockback-recover.py
```

## Recover an archive

```bash
python3 dockback-recover.py \
  --key <your 64-hex key> \
  --in backup.dback \
  --manifest backup.dback.manifest.json \
  --verify \
  --out backup.tar
```

- `--key` takes the 64-hex key directly, or `@path` to a file that contains it (keeps the key out of your shell history).
- `--manifest` is optional — the script auto-detects `backup.dback.manifest.json` next to the archive. It's needed for `--verify` and for **envelope** backups (see below).
- `--verify` re-computes the archive's SHA-256 and checks it against the manifest before decrypting, so you catch a corrupt or truncated file early.
- `--out` is the recovered **plain tar** (defaults to `<input>.tar`).

The script prints a summary of the backup (target, image, volumes, databases), then writes the decrypted, decompressed tar. Unpack it with any tar tool:

```bash
tar -tf backup.tar                     # manifest.json, config/inspect.json, volumes.tar, db/…
tar -xf backup.tar                     # extract everything
tar -xf volumes.tar -C ./restored      # the volume data is a tar within the tar
```

## Incremental backups (chains)

A backup made with **incremental volume capture** stores only the files changed since its parent (in `volumes-delta.tar` instead of `volumes.tar`), so a single delta archive is **not** a complete restore on its own. The script tells you this in the summary and can reconstruct the whole chain for you. Put every `.dback` of the chain (baseline + deltas) in one folder and run:

```bash
dockback-recover.py --key @key.txt --in newest-delta.dback --apply-chain ./chain-folder --out ./restored
```

It walks the parent pointers to the full baseline (checking each link's cryptographic pin), decrypts every generation in order, and writes numbered tars plus an **`APPLY_ORDER.txt`** describing exactly how to extract the baseline and apply each delta over it — including which paths each delta removed. A full backup (or any backup made with the toggle off) is self-contained and needs none of this.

## How it protects you

- **Authenticated decryption.** Every 1 MiB frame is AES-256-GCM. A wrong key, a corrupt archive, or a tampered byte fails the authentication tag and the script exits non-zero **without writing partial or garbage output** — it never hands you plausible-looking wrong data.
- **Envelope and legacy backups both work.** Newer backups wrap a per-backup data key with your master key (recorded as `wrapped_key` in the manifest); the script unwraps it with your master key automatically. Older backups encrypted directly with the master key work too, with no manifest required.
- **No dependencies required.** The script prefers the `cryptography` package when it's installed (fast), and otherwise falls back to a built-in, pure-standard-library AES-256-GCM implementation, so it runs on a stock Python 3. For **zstd** archives (the default) it uses the `zstandard`/`pyzstd` module or the `zstd` command if present; for **gzip**/**xz** it uses the Python standard library. If it can't decompress zstd on that machine, it decrypts to a `.tar.zst` and prints the one command to finish (`zstd -d …` / `tar --zstd -xf …`).
- **App-backups too.** The same script also recovers a DockBack **application backup** (its own control-plane database — the `dockback-config-*.dback` files): pass it to `--in` and the script detects it, unwraps its key, and writes a plain tar containing `manifest.json` + `dockback.db`. So even DockBack's own configuration is recoverable without DockBack.

> The pure-Python fallback is correct but slow on large archives. For a big recovery, install `cryptography` (`pip install cryptography`) or run it on a machine that has the `zstd` command — both make it fast.

## Keep the kit together

Store the recovery script (or its release URL + SHA-256), your recovery sheet, and your key **offline and together** — but separate from the backups themselves. That trio is everything a future you needs to recover, even if this product no longer exists.
