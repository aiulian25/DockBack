# Quiesce hooks (pre/post)

Quiesce hooks let you put an application into a consistent state **just before** its data is captured, and restore normal operation **right after** — even if the backup fails.

## Why

Some apps need a moment of quiet to guarantee a consistent snapshot: flush caches, enter maintenance mode, or briefly pause writes. Hooks run commands inside the container at the right points so the captured files and database represent a single, coherent instant.

## Pre vs post

- **Pre-hooks** run before data capture. If a pre-hook fails, the backup is aborted (so you never store an inconsistent snapshot).
- **Post-hooks** always run afterward — including on failure — so the app is never left in maintenance mode. Example: turn Nextcloud maintenance mode **on** in pre, **off** in post.

## Configuring hooks

On a container's detail page, expand **Advanced — Backup Hooks**. Enter the pre and post commands (one per line) and, if needed, the user to run them as. Hooks execute inside the container via the node's `EXEC`/`POST` permissions.

> **Security:** Hook commands run inside the target container with that container's privileges and user — treat them like operator-level shell access and enter only commands you trust. Only a signed-in administrator can set hooks, and the change is protected against cross-site requests.

> Hooks require a shell in the target container. Containers built `FROM scratch`/distroless (no `/bin/sh`) can't run shell hooks — capture their data with the standard volume/dump strategy or an app-native export instead.

## Auto-detected hooks

For some well-known images, sensible hooks are suggested automatically so you don't have to write them from scratch. Review and adjust them to your needs before relying on them.
