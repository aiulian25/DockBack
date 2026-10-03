package backup

import (
	"context"
	"fmt"
	"strings"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
)

// preflight runs the pre-backup checks (PLAN §4.4): encryption key present,
// backup storage reachable + writable, and DB dump tools present. Key/storage
// problems fail fast (before any destructive or expensive work); missing DB
// tools only warn, since the engine falls back to a file backup. The disk-space
// estimate is a separate check (guardFreeSpace), run once the mount selection is
// known so it can size the selection.
// It returns dbFallback (F103): non-empty when the container was detected as a
// database engine but its dump tools are absent, so the capture will degrade to
// raw files. The caller records it in the manifest, because a file-level copy of
// a LIVE database can be torn — the exact failure the dump feature exists to
// prevent — and must not be graded as an ordinary application backup.
func (e *Engine) preflight(ctx context.Context, cli *client.Client, opts Options, engineKind, id string) (dbFallback string, err error) {
	// 1) Encryption key present (PLAN §4.4 / §2.7) — never write unencryptable data.
	if len(e.MasterKey()) != 32 {
		return "", fmt.Errorf("encryption key not configured (set DOCKBACK_ENCRYPTION_KEY) — refusing to back up unencryptable data")
	}

	// 2) Storage backend reachable + writable (a tiny write/delete probe).
	if err := e.probeStorageWritable(ctx); err != nil {
		return "", fmt.Errorf("backup storage %q not writable: %w", e.Storage.Name(), err)
	}
	e.logf(id, "INFO", "Pre-flight: encryption key present; storage %q reachable and writable", e.Storage.Name())

	// 3) DB dump tools present (warn-only — engine falls back to a file backup).
	if engineKind != "" && cli != nil {
		if e.probeDBTools(ctx, cli, opts.ContainerID, engineKind) {
			e.logf(id, "INFO", "Pre-flight: %s dump tools present in the container", engineKind)
		} else {
			e.logf(id, "WARN", "Pre-flight: %s dump tools not found in the container — will fall back to a file backup", engineKind)
			dbFallback = dbFallbackNote(engineKind)
		}
	}
	return dbFallback, nil
}

// dbFallbackNote is the single wording for a degraded database capture, so the
// manifest, the grade and the runbook cannot drift apart.
func dbFallbackNote(engineKind string) string {
	if engineKind == "" {
		return ""
	}
	return engineKind + ": dump tools not found — captured as raw files"
}

// redisAuthFallbackNote is what a Redis backup records when nothing could
// authenticate to the server (F185). Distinct wording from dbFallbackNote,
// because the cause and the remedy are different: the tools were there, the
// password was not.
const redisAuthFallbackNote = "redis: no working password found — no consistent snapshot was taken; " +
	"the /data directory was captured as files instead, which holds the RDB Redis last wrote on its own schedule"

// probeStorageWritable confirms the backup storage accepts a write by putting a
// tiny object and deleting it (PLAN §4.4 — "confirm storage backend writable").
func (e *Engine) probeStorageWritable(ctx context.Context) error {
	key := ".dback-preflight-" + short(newID())
	if _, err := e.Storage.Put(ctx, key, strings.NewReader("ok")); err != nil {
		return err
	}
	_ = e.Storage.Delete(ctx, key)
	return nil
}

// probeDBTools checks the native dump tool exists in the target container, so a
// "looks like a DB but has no tools" case is surfaced up front rather than only
// at dump time (PLAN §4.4).
func (e *Engine) probeDBTools(ctx context.Context, cli *client.Client, id, engine string) bool {
	var probe []string
	switch engine {
	case "postgres":
		probe = []string{"sh", "-c", "command -v pg_dumpall >/dev/null 2>&1"}
	case "mysql":
		probe = []string{"sh", "-c", "command -v mysqldump >/dev/null 2>&1 || command -v mariadb-dump >/dev/null 2>&1"}
	case "mongodb":
		probe = []string{"sh", "-c", "command -v mongodump >/dev/null 2>&1"}
	default:
		return true
	}
	_, err := dockercli.ExecCapture(ctx, cli, id, probe)
	return err == nil
}
