package dockercli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// dbReadyTimeoutOverride is a runtime override (seconds) set from the in-app
// "Performance & tuning" setting; it takes precedence over the env default so an
// operator can tune the wait without editing .env and recreating the container
// (F15). 0 = no override (fall back to env / built-in default).
var dbReadyTimeoutOverride atomic.Int64

// SetDBReadyTimeout sets (or clears, with <=0) the runtime DB-ready wait override.
// Safe for concurrent use.
func SetDBReadyTimeout(seconds int) {
	if seconds < 0 {
		seconds = 0
	}
	dbReadyTimeoutOverride.Store(int64(seconds))
}

// dbReadyTimeout is how long WaitForDB waits for a freshly-initialized database
// to accept connections. A first-init of MySQL/MariaDB/Postgres on constrained
// hardware (e.g. a Synology NAS, especially when several backups verify at once)
// can take well over two minutes — the old 120s cap made verification time out
// with "throwaway db not ready" even though the dump was fine. Precedence: the
// in-app override (F15), then DOCKBACK_DB_READY_TIMEOUT (seconds), then 300s.
func dbReadyTimeout() time.Duration {
	if n := dbReadyTimeoutOverride.Load(); n > 0 {
		return time.Duration(n) * time.Second
	}
	if v := os.Getenv("DOCKBACK_DB_READY_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 300 * time.Second
}

// WipeDir clears the contents of a directory inside the target container's
// volumes via a temporary sidecar (the target may be stopped). Used to reset a
// database data directory so the engine re-initializes cleanly before importing
// a logical dump (PLAN §4.1 — application-consistent DB restore).
func WipeDir(ctx context.Context, c *client.Client, targetID, dir string) error {
	if dir == "" {
		return nil
	}
	cmd := fmt.Sprintf("rm -rf %q/* %q/.[!.]* 2>/dev/null || true", dir, dir)
	if err := runSidecar(ctx, c, targetID, cmd, 5*time.Minute); err != nil {
		return fmt.Errorf("wipe: %w", err)
	}
	return nil
}

// WaitForDB blocks until the database inside the container accepts queries, or
// times out. Probes use the container's own env (inherited by exec).
func WaitForDB(ctx context.Context, c *client.Client, id, engine string) error {
	var probe []string
	switch engine {
	// Probe over TCP (127.0.0.1), not the unix socket: DB images run a temporary
	// socket-only server during first-init, then restart the real (networked)
	// one. Only the real server accepts TCP, so this avoids importing into the
	// throwaway init instance.
	case "mysql":
		// Ready when a query succeeds through ANY auth path the import can also use,
		// so "ready" never lies about a server the import then can't reach. Images
		// differ wildly in how root is exposed, so we try, in order:
		//   1. root over TCP WITH the password  — official mysql/mariadb (root@'%' + pw)
		//   2. root over TCP with NO password   — linuxserver/mariadb (root@'%' passwordless;
		//      it ignores MYSQL_ROOT_PASSWORD) — this was the missing case behind
		//      "throwaway db not ready".
		//   3. the application user over TCP     — real-container restores where root is locked
		//   4/5. root over the LOCAL SOCKET, with the password then via unix_socket
		//      (no password) — MariaDB where root is reachable only through the socket.
		// The socket attempts (4/5) run only once a TCP probe shows the real server is
		// listening (anything other than "can't connect"): the transient first-init
		// server runs with networking disabled, so a live TCP port means the real
		// server, never the init instance. A credential-free reason is printed on the
		// not-ready path so a genuine timeout is actionable instead of empty.
		probe = []string{"sh", "-c", `CLI=mysql; command -v mariadb >/dev/null 2>&1 && CLI=mariadb; ` +
			`RP="${MYSQL_ROOT_PASSWORD:-$MARIADB_ROOT_PASSWORD}"; AU="${MYSQL_USER:-$MARIADB_USER}"; AP="${MYSQL_PASSWORD:-$MARIADB_PASSWORD}"; ` +
			`MYSQL_PWD="$RP" "$CLI" -h127.0.0.1 -P3306 -uroot -e 'SELECT 1' >/dev/null 2>&1 && exit 0; ` +
			`"$CLI" -h127.0.0.1 -P3306 -uroot -e 'SELECT 1' >/dev/null 2>&1 && exit 0; ` +
			`{ [ -n "$AU" ] && MYSQL_PWD="$AP" "$CLI" -h127.0.0.1 -P3306 -u"$AU" -e 'SELECT 1' >/dev/null 2>&1; } && exit 0; ` +
			`tcp=$("$CLI" -h127.0.0.1 -P3306 -uroot -e 'SELECT 1' 2>&1); ` +
			`case "$tcp" in *"Can't connect"*|*"Connection refused"*|*"connect to server"*) ` +
			`echo "waiting: TCP 3306 not accepting connections yet (real server still initializing)"; exit 1;; esac; ` +
			`MYSQL_PWD="$RP" "$CLI" -uroot -e 'SELECT 1' >/dev/null 2>&1 && exit 0; ` +
			`"$CLI" -uroot -e 'SELECT 1' >/dev/null 2>&1 && exit 0; ` +
			`echo "server up but root not usable over TCP or the local socket (with or without password)"; exit 1`}
	case "postgres":
		probe = []string{"sh", "-c", `pg_isready -h 127.0.0.1 -U "${POSTGRES_USER:-postgres}" >/dev/null 2>&1`}
	case "mongodb":
		probe = []string{"sh", "-c", `mongosh --host 127.0.0.1 --quiet --eval 'db.runCommand({ping:1})' >/dev/null 2>&1 || mongo --host 127.0.0.1 --quiet --eval 'db.runCommand({ping:1})' >/dev/null 2>&1`}
	default:
		return nil
	}
	deadline := time.Now().Add(dbReadyTimeout())
	for {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := ExecCapture(cctx, c, id, probe)
		cancel()
		if err == nil {
			return nil
		}
		// The container is not up. Waiting out the rest of the timeout against a
		// process that is gone tells the operator nothing and then blames a
		// timeout for it — "did not become ready within timeout: container is not
		// running" reads as slow when the truth is dead. Say why instead: the
		// exit code and the engine's own last words are one API call away and are
		// the whole diagnosis.
		state, why := containerExitReason(ctx, c, id)
		if state == cStopped {
			return fmt.Errorf("the database container exited instead of starting up%s", why)
		}
		if time.Now().After(deadline) {
			// A container that is crash-looping was never given the early exit
			// above — a restart policy keeps putting it back, and one of those
			// attempts might have succeeded. By the deadline it plainly did not,
			// so the forensics are owed here too.
			if state == cRestarting {
				return fmt.Errorf("the database container kept restarting and never accepted connections%s", why)
			}
			return fmt.Errorf("database did not become ready within timeout: %w", err)
		}
		time.Sleep(2 * time.Second)
	}
}

// dbExitLogLines is how much of the engine's own output to quote. Enough for the
// line that explains it, not so much that the reason scrolls away.
const dbExitLogLines = 20

// containerLiveness is what an inspect could establish about a container that is
// failing to answer its probe.
type containerLiveness int

const (
	// cUnknown covers both "it is running" and "we could not find out", which are
	// treated identically on purpose: neither is grounds for ending the wait.
	cUnknown containerLiveness = iota
	// cRestarting is a crash-loop. Terminal in practice, but not yet — a restart
	// policy is still putting it back, and an attempt may yet come up.
	cRestarting
	// cStopped is a container that is down and staying down.
	cStopped
)

// containerExitReason classifies a container that is not answering, and reports
// what it said on the way out.
//
// Deliberately conservative: only a container Docker positively reports as
// stopped returns cStopped. An inspect that fails — a node that blipped, a
// request that timed out — returns cUnknown, so a momentary API problem can
// never be mistaken for a crashed database and cut short a wait that would have
// succeeded.
func containerExitReason(ctx context.Context, c *client.Client, id string) (containerLiveness, string) {
	ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	insp, err := c.ContainerInspect(ictx, id)
	if err != nil || insp.State == nil || (insp.State.Running && !insp.State.Restarting) {
		return cUnknown, ""
	}
	state := cStopped
	if insp.State.Restarting {
		state = cRestarting
	}
	why := ""
	if insp.State.ExitCode != 0 {
		why = fmt.Sprintf(" (exit code %d)", insp.State.ExitCode)
	}
	if insp.State.OOMKilled {
		why += " — it was killed for running out of memory"
	}
	if tail, terr := ContainerLogTail(ictx, c, id, dbExitLogLines); terr == nil && strings.TrimSpace(tail) != "" {
		why += ". Its last output was:\n" + RedactLogSecrets(tail)
	}
	return state, why
}

// WaitForHealthy waits until a container reports healthy (if it has a
// healthcheck) or is simply running (if it doesn't), and REPORTS the outcome:
// true if that state was reached within the timeout, false if it never did (e.g.
// a crash-loop after a restore). A container with no healthcheck is judged
// healthy purely on staying up — we can't prove more than "it's running" — so a
// good restore is never falsely rolled back for lacking a healthcheck. Used
// before an app-native import (PLAN §9.4) and as the post-restore health gate (F4).
// It honors ctx: a canceled/expired context ends the wait immediately (false),
// so an operator's Cancel isn't stuck behind the remaining health timeout.
func WaitForHealthy(ctx context.Context, c *client.Client, id string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		// Canceled (operator pressed Cancel, or the run's deadline elapsed): stop
		// immediately, before spending another API call.
		if ctx.Err() != nil {
			return false
		}
		insp, err := c.ContainerInspect(ctx, id)
		if err == nil && insp.State != nil {
			if insp.State.Health != nil {
				if insp.State.Health.Status == "healthy" {
					return true
				}
			} else if insp.State.Running {
				return true
			}
		}
		if time.Now().After(deadline) {
			// Final verdict at the deadline: a container with a healthcheck must be
			// "healthy"; one without must at least still be running.
			if err == nil && insp.State != nil {
				if insp.State.Health != nil {
					return insp.State.Health.Status == "healthy"
				}
				return insp.State.Running
			}
			return false
		}
		// Sleep INTERRUPTIBLY: a canceled restore (operator pressed Cancel, or the
		// run's deadline elapsed) must stop waiting immediately instead of sitting
		// here until this health timeout expires.
		if !sleepOrDone(ctx, 3*time.Second) {
			return false
		}
	}
}

// sleepOrDone waits for d, returning false as soon as ctx is done instead of
// sleeping it out. Split out so the interruptibility is unit-testable without a
// Docker daemon.
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// DBDataDir returns the conventional data directory for a database engine,
// which is wiped before a dump-based restore.
func DBDataDir(engine string) string {
	switch engine {
	case "mysql":
		return "/var/lib/mysql"
	case "postgres":
		return "/var/lib/postgresql/data"
	case "mongodb":
		return "/data/db"
	case "redis":
		return "/data"
	}
	return ""
}

// WriteFileToVolume writes a stream to destPath inside the target container's
// volumes via a temporary --volumes-from sidecar (the target may be stopped).
// Used to place a restored Redis dump.rdb back into the data directory before
// the engine starts and loads it (PLAN §4.1 — application-consistent DB
// restore). destPath is a constant, engine-controlled path.
func WriteFileToVolume(ctx context.Context, c *client.Client, targetID, destPath string, r io.Reader) error {
	if err := ensureSidecar(ctx, c); err != nil {
		return err
	}
	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: []string{"sleep", "300"}, Labels: sidecarLabels()},
		&container.HostConfig{VolumesFrom: []string{targetID}},
		nil, nil, "")
	if err != nil {
		return fmt.Errorf("write sidecar create: %w", err)
	}
	defer removeContainer(c, created.ID)
	if err := c.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("write sidecar start: %w", err)
	}
	dir := destPath[:strings.LastIndexByte(destPath, '/')]
	if dir == "" {
		dir = "/"
	}
	if _, err := ExecCapture(ctx, c, created.ID, []string{"sh", "-c", "mkdir -p '" + dir + "'"}); err != nil {
		return fmt.Errorf("preparing data dir: %w", err)
	}
	if err := ExecStdin(ctx, c, created.ID, []string{"sh", "-c", "cat > '" + destPath + "'"}, r); err != nil {
		return fmt.Errorf("writing %s: %w", destPath, err)
	}
	return nil
}
