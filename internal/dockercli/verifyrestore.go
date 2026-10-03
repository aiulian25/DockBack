package dockercli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// VerifyRestoreDB test-restores a database dump into a THROWAWAY, fully isolated
// container and runs a sanity query — proving the dump actually re-imports and
// returns data, not just that it decompresses (PLAN §4.3). The container is
// network-isolated (`none`), publishes no ports, mounts no host data, uses
// ephemeral credentials, and is force-removed (with its anonymous volume) on
// exit, so it can never touch the real container or its data. Returns a short
// human summary (e.g. "12 tables").
func VerifyRestoreDB(ctx context.Context, c *client.Client, image, engine string, dump io.Reader) (string, error) {
	// Redis snapshots are a binary RDB loaded at boot, not a stream importable
	// into a throwaway server over a socket. Validate the RDB magic header (every
	// valid RDB begins with the ASCII bytes "REDIS") as the integrity proof — the
	// same "archive intact" guarantee file-volume backups get, without spinning a
	// container. The rest of the stream is drained so the caller's reader is fully
	// consumed.
	if engine == "redis" {
		head := make([]byte, 5)
		n, _ := io.ReadFull(dump, head)
		_, _ = io.Copy(io.Discard, dump)
		if n < 5 || string(head[:n]) != "REDIS" {
			return "", fmt.Errorf("not a valid Redis RDB snapshot (missing REDIS magic header)")
		}
		return "RDB snapshot header OK", nil
	}
	if image == "" {
		return "", fmt.Errorf("no image recorded for the database — cannot test-restore")
	}
	drillImage, err := EnsureDrillImage(ctx, c, image)
	if err != nil {
		return "", fmt.Errorf("pull %s: %w", image, err)
	}
	// #36: registered BEFORE the container's own removal, so LIFO runs it after —
	// an image is never removable while the container built on it still exists.
	var drillContainerID string
	defer func() { ReleaseDrillImage(ctx, c, drillImage, drillContainerID) }()

	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: image, Env: dbInitEnv(engine, randPassword()), Labels: sidecarLabels()},
		&container.HostConfig{NetworkMode: "none", AutoRemove: false},
		nil, nil, "")
	if err != nil {
		return "", fmt.Errorf("create throwaway db: %w", err)
	}
	id := created.ID
	drillContainerID = id
	defer func() {
		rmCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_ = c.ContainerRemove(rmCtx, id, container.RemoveOptions{Force: true, RemoveVolumes: true})
	}()

	if err := c.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("start throwaway db: %w", err)
	}
	if err := WaitForDB(ctx, c, id, engine); err != nil {
		return "", fmt.Errorf("throwaway db not ready: %w", err)
	}
	if err := ExecStdin(ctx, c, id, dbImportCommand(engine), dump); err != nil {
		return "", fmt.Errorf("re-import failed: %w", err)
	}
	out, err := ExecCapture(ctx, c, id, dbSanityQuery(engine))
	if err != nil {
		return "", fmt.Errorf("sanity query failed: %w", err)
	}
	return dbSanitySummary(engine, out), nil
}

// defaultDrillBootWait bounds how long the volume drill waits for the throwaway app
// to come up before judging it. Kept short by default: many apps need external
// dependencies (a database, another service) that aren't present in isolation, so
// "started and stayed up" is the realistic bar — a full healthy state is a bonus,
// not required. Configurable so a slow image isn't failed on a drill (F30).
const defaultDrillBootWait = 45 * time.Second

// drillBootWaitOverride is a runtime override (seconds) set from the in-app
// setting (F30). <=0 = use the default. Mirrors dbReadyTimeoutOverride.
var drillBootWaitOverride atomic.Int64

// SetDrillBootWait sets (or clears, with <=0) the drill boot-wait override (seconds).
func SetDrillBootWait(seconds int) {
	if seconds < 0 {
		seconds = 0
	}
	drillBootWaitOverride.Store(int64(seconds))
}

// drillBootWait returns the effective drill boot-wait: the runtime override when
// set, else the default (45s).
func drillBootWait() time.Duration {
	if n := drillBootWaitOverride.Load(); n > 0 {
		return time.Duration(n) * time.Second
	}
	return defaultDrillBootWait
}

// DrillBootWaitSeconds returns the effective drill boot-wait in seconds, so the API
// can show the value a drill will actually use (F30).
func DrillBootWaitSeconds() int { return int(drillBootWait() / time.Second) }

// VerifyRestoreVolumes test-restores a volume/app backup into a THROWAWAY, fully
// isolated container — proving the app actually reconstitutes from its captured
// volume data, not just that the archive decompresses (F5). It extracts the
// volume payload into FRESH anonymous volumes at the recorded mount destinations
// (never the real named volumes or host binds), boots the recorded image with the
// backup's own Config (env/cmd/entrypoint/healthcheck, so it starts realistically)
// but a LOCKED-DOWN HostConfig — network "none", no host mounts, no inherited
// privileges/capabilities/devices, no restart loop, no published ports — waits
// briefly for it to come up healthy/running, then force-removes the container AND
// its anonymous volumes. Returns a short human summary. dests are the mount
// destinations to (re)create as throwaway volumes (the manifest's volume list).
func VerifyRestoreVolumes(ctx context.Context, c *client.Client, image string, inspectJSON []byte, dests []string, volTar io.Reader) (string, []string, error) {
	if image == "" {
		return "", nil, fmt.Errorf("no image recorded for this backup — cannot boot a test-restore")
	}
	drillImage, err := EnsureDrillImage(ctx, c, image)
	if err != nil {
		return "", nil, fmt.Errorf("pull %s: %w", image, err)
	}
	// #36: before the container's removal defer, so it runs last.
	var drillContainerID string
	defer func() { ReleaseDrillImage(ctx, c, drillImage, drillContainerID) }()

	// Start from just the image + our sidecar marker, then layer on ONLY the safe
	// parts of the recorded config so the app boots the way it really does. The
	// HostConfig is built fresh and locked down — we never reuse the original's
	// binds, privileges, capabilities, devices, or restart policy.
	cfg := &container.Config{Image: image, Labels: sidecarLabels()}
	if len(inspectJSON) > 0 {
		var insp types.ContainerJSON
		if json.Unmarshal(inspectJSON, &insp) == nil && insp.Config != nil {
			cfg.Env = insp.Config.Env
			cfg.Cmd = insp.Config.Cmd
			cfg.Entrypoint = insp.Config.Entrypoint
			cfg.WorkingDir = insp.Config.WorkingDir
			cfg.User = insp.Config.User
			cfg.Healthcheck = insp.Config.Healthcheck
			// Keep the app's labels but always re-stamp the sidecar marker so leftover
			// sweeps still recognize this as an ephemeral DockBack container.
			if len(insp.Config.Labels) > 0 {
				merged := make(map[string]string, len(insp.Config.Labels)+1)
				for k, v := range insp.Config.Labels {
					merged[k] = v
				}
				merged[sidecarLabelKey] = "1"
				cfg.Labels = merged
			}
		}
	}
	// Fresh ANONYMOUS volumes at each recorded destination — Docker materializes
	// them on create; the tar payload is extracted into them below. This is what
	// keeps the drill off the real named volumes and host bind paths.
	if len(dests) > 0 {
		cfg.Volumes = make(map[string]struct{}, len(dests))
		for _, d := range dests {
			if d != "" {
				cfg.Volumes[d] = struct{}{}
			}
		}
	}

	name := "dockback-drill-" + randHex(6)
	hostCfg := &container.HostConfig{
		NetworkMode:   "none", // fully isolated — no access to the real stack or the internet
		AutoRemove:    false,
		RestartPolicy: container.RestartPolicy{Name: "no"}, // no crash-loop; a clean exit signal
		// Binds / Mounts deliberately empty — no host data, ever.
	}
	created, err := c.ContainerCreate(ctx, cfg, hostCfg, nil, nil, name)
	if err != nil {
		return "", nil, fmt.Errorf("create throwaway app: %w", err)
	}
	id := created.ID
	drillContainerID = id
	defer func() {
		rmCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_ = c.ContainerRemove(rmCtx, id, container.RemoveOptions{Force: true, RemoveVolumes: true})
	}()

	// Extract the captured volume payload into the throwaway's fresh volumes via the
	// same --volumes-from tar path a real restore uses. A corrupt/short payload
	// fails here.
	if err := UntarToVolumes(ctx, c, id, volTar); err != nil {
		return "", nil, fmt.Errorf("volume data failed to extract into a fresh volume: %w", err)
	}

	if err := c.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return "", nil, fmt.Errorf("throwaway app did not start: %w", err)
	}
	// #38: the environment the throwaway ACTUALLY booted with, read back from the
	// daemon rather than assumed from what was asked for. cfg.Env above is set
	// only when the recorded inspect parses; if it did not, the drill boots with
	// no environment at all and looks exactly as healthy — which is R5 §4.2's
	// shape, a service running with none of its variables.
	bootEnv := cfg.Env
	if insp, ierr := c.ContainerInspect(ctx, id); ierr == nil && insp.Config != nil {
		bootEnv = insp.Config.Env
	}
	summary, err := waitBootProof(ctx, c, id)
	return summary, bootEnv, err
}

// waitBootProof polls a throwaway app until a terminal verdict (see bootVerdict)
// or the boot deadline.
func waitBootProof(ctx context.Context, c *client.Client, id string) (string, error) {
	deadline := time.Now().Add(drillBootWait())
	for {
		atDeadline := time.Now().After(deadline)
		insp, err := c.ContainerInspect(ctx, id)
		if err == nil && insp.State != nil {
			st := insp.State
			hs := ""
			if st.Health != nil {
				hs = st.Health.Status
			}
			if summary, done, verr := bootVerdict(hs, st.Running, st.Restarting, st.ExitCode, atDeadline); done {
				// F91: a failed drill used to report only "container exited on boot
				// (code 1)", which says nothing about WHY. The app's own last words
				// usually do — and a drill result is recorded and alerted, so the
				// excerpt is redacted before it can leave the machine.
				if verr != nil {
					if tail, terr := ContainerLogTail(ctx, c, id, 20); terr == nil && strings.TrimSpace(tail) != "" {
						verr = fmt.Errorf("%w — last output: %s", verr, RedactLogSecrets(LastLines(tail, 8)))
					}
				}
				return summary, verr
			}
		} else if atDeadline {
			return "", fmt.Errorf("could not inspect throwaway container: %v", err)
		}
		time.Sleep(2 * time.Second)
	}
}

// bootVerdict judges a throwaway app's state during a volume drill. done=true
// means the verdict is final (summary or err); done=false means keep polling.
// Healthy is the strongest proof; still running at the deadline is a pass (its
// dependencies may simply be unavailable in isolation); an early NON-ZERO exit is
// a real failure — the restored data made the app crash. Pure, so it's
// unit-testable without Docker.
func bootVerdict(healthStatus string, running, restarting bool, exitCode int, atDeadline bool) (summary string, done bool, err error) {
	if healthStatus == "healthy" {
		return "app booted healthy in isolation", true, nil
	}
	if !running && !restarting {
		if exitCode == 0 {
			return "app booted and exited cleanly in isolation", true, nil
		}
		return "", true, fmt.Errorf("container exited on boot (code %d) — restored data may be unusable", exitCode)
	}
	if atDeadline {
		if running {
			if healthStatus != "" {
				return "app booted and is running in isolation (healthcheck needs its dependencies)", true, nil
			}
			return "app booted and stayed running in isolation", true, nil
		}
		return "", true, fmt.Errorf("container did not come up in isolation")
	}
	return "", false, nil // keep polling
}

// randHex returns n random bytes hex-encoded, for a unique throwaway name. A
// crypto/rand failure is fatal (impossible on Linux) rather than a guessable name.
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// randPassword returns an ephemeral password for the throwaway verify container.
// A crypto/rand failure is fatal (never seed a guessable DB password), so panic
// rather than emit a weak one — impossible on Linux, belt-and-suspenders (SEC-9).
func randPassword() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// dbInitEnv is the environment a throwaway DB container needs to initialize so a
// dump can be imported over loopback. Isolation (network "none") makes trust
// auth safe for postgres.
func dbInitEnv(engine, pw string) []string {
	switch engine {
	case "mysql":
		return []string{"MYSQL_ROOT_PASSWORD=" + pw, "MARIADB_ROOT_PASSWORD=" + pw}
	case "postgres":
		return []string{"POSTGRES_PASSWORD=" + pw, "POSTGRES_HOST_AUTH_METHOD=trust"}
	default: // mongodb: no auth by default
		return nil
	}
}

// dbImportCommand pipes a dump (stdin) into the engine over loopback.
func dbImportCommand(engine string) []string {
	switch engine {
	case "postgres":
		return []string{"/bin/sh", "-c", `psql -h 127.0.0.1 -U "${POSTGRES_USER:-postgres}" -d postgres`}
	case "mysql":
		// Import as root through whichever method the image actually accepts, tried
		// in order (matches WaitForDB): TCP+password (official), TCP no-password
		// (linuxserver/mariadb, which ignores the root password), socket+password,
		// then socket via unix_socket. Each candidate is probed with SELECT 1 (which
		// doesn't read stdin) before exec'ing the client that streams the dump. The
		// password is passed via MYSQL_PWD, never on the argv.
		return []string{"/bin/sh", "-c", `CLI=mysql; command -v mariadb >/dev/null 2>&1 && CLI=mariadb; ` +
			`RP="${MYSQL_ROOT_PASSWORD:-$MARIADB_ROOT_PASSWORD}"; ` +
			`if MYSQL_PWD="$RP" "$CLI" -h127.0.0.1 -P3306 -uroot -e 'SELECT 1' >/dev/null 2>&1; then exec env MYSQL_PWD="$RP" "$CLI" --force -h127.0.0.1 -P3306 -uroot; fi; ` +
			`if "$CLI" -h127.0.0.1 -P3306 -uroot -e 'SELECT 1' >/dev/null 2>&1; then exec "$CLI" --force -h127.0.0.1 -P3306 -uroot; fi; ` +
			`if MYSQL_PWD="$RP" "$CLI" -uroot -e 'SELECT 1' >/dev/null 2>&1; then exec env MYSQL_PWD="$RP" "$CLI" --force -uroot; fi; ` +
			`exec "$CLI" --force -uroot`}
	case "mongodb":
		return []string{"/bin/sh", "-c", "mongorestore --host 127.0.0.1 --archive --drop"}
	}
	return []string{"/bin/sh", "-c", "cat >/dev/null"}
}

// dbSanityQuery returns a query that proves the imported data is usable.
func dbSanityQuery(engine string) []string {
	switch engine {
	case "postgres":
		// F120: count across EVERY connectable database, not just `postgres`.
		//
		// information_schema in PostgreSQL is per-database — it lists only the
		// tables of the database you are connected to. A pg_dumpall restores each
		// application into its OWN database (commafeed, wikijs, immich, …), so
		// connecting to `postgres` and counting there found nothing and every
		// Postgres backup reported a confident, permanent "0 tables".
		//
		// That is also exactly why MySQL was unaffected and reported real numbers:
		// its information_schema is server-wide. The asymmetry was the tell.
		//
		// pgConstraintCountCmd already walked the database list correctly; this is
		// the same walk, so the two cannot disagree about what exists.
		return []string{"/bin/sh", "-c", `U="${POSTGRES_USER:-postgres}"; ` +
			`T=0; D=0; ` +
			`for db in $(psql -h 127.0.0.1 -tAqX -U "$U" -d postgres -c "SELECT datname FROM pg_database WHERE datallowconn AND datname NOT IN ('template0','template1')" 2>/dev/null); do ` +
			`n=$(psql -h 127.0.0.1 -tAqX -U "$U" -d "$db" -c "SELECT count(*) FROM information_schema.tables WHERE table_schema NOT IN ('pg_catalog','information_schema')" 2>/dev/null); ` +
			// Only databases that actually HOLD tables are counted, so the empty
			// `postgres` maintenance database doesn't inflate the figure into
			// "13 tables in 2 databases" when there is plainly one.
			`[ -n "$n" ] || n=0; T=$((T+n)); [ "$n" -gt 0 ] && D=$((D+1)); done; ` +
			`printf 'tables=%s\ndatabases=%s\n' "$T" "$D"`}
	case "mysql":
		return []string{"/bin/sh", "-c", `CLI=mysql; command -v mariadb >/dev/null 2>&1 && CLI=mariadb; ` +
			`RP="${MYSQL_ROOT_PASSWORD:-$MARIADB_ROOT_PASSWORD}"; ` +
			`Q="SELECT COUNT(*) FROM information_schema.tables WHERE table_schema NOT IN ('information_schema','performance_schema','mysql','sys')"; ` +
			`if MYSQL_PWD="$RP" "$CLI" -h127.0.0.1 -P3306 -uroot -e 'SELECT 1' >/dev/null 2>&1; then exec env MYSQL_PWD="$RP" "$CLI" -N -h127.0.0.1 -P3306 -uroot -e "$Q"; fi; ` +
			`if "$CLI" -h127.0.0.1 -P3306 -uroot -e 'SELECT 1' >/dev/null 2>&1; then exec "$CLI" -N -h127.0.0.1 -P3306 -uroot -e "$Q"; fi; ` +
			`if MYSQL_PWD="$RP" "$CLI" -uroot -e 'SELECT 1' >/dev/null 2>&1; then exec env MYSQL_PWD="$RP" "$CLI" -N -uroot -e "$Q"; fi; ` +
			`exec "$CLI" -N -uroot -e "$Q"`}
	case "mongodb":
		return []string{"/bin/sh", "-c", `mongosh --host 127.0.0.1 --quiet --eval 'db.adminCommand("listDatabases").databases.length' 2>/dev/null || mongo --host 127.0.0.1 --quiet --eval 'db.adminCommand("listDatabases").databases.length'`}
	}
	return []string{"/bin/sh", "-c", "echo 0"}
}

func dbSanitySummary(engine string, out []byte) string {
	n := strings.TrimSpace(string(out))
	switch engine {
	case "mongodb":
		if n == "" {
			n = "0"
		}
		return n + " databases"
	case "postgres":
		// F120: the Postgres query reports both figures, so the summary can say
		// which databases the tables were found in. Naming the database count
		// matters: "0 tables in 0 databases" reads as a broken probe, while a bare
		// "0 tables" read for years as a fact about the backup.
		tables, dbs, ok := parseKeyedCounts(n)
		if !ok {
			if n == "" {
				n = "0"
			}
			return n + " tables"
		}
		return plural(tables, "table") + " in " + plural(dbs, "database")
	default:
		if n == "" {
			n = "0"
		}
		return n + " tables"
	}
}

// parseKeyedCounts reads the "tables=N\ndatabases=M" the Postgres sanity query
// prints. Returns ok=false for anything else, so an unexpected shape falls back
// to the plain rendering rather than inventing numbers.
func parseKeyedCounts(s string) (tables, dbs int, ok bool) {
	seen := 0
	for _, line := range strings.Split(s, "\n") {
		k, v, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 0 {
			continue
		}
		switch strings.TrimSpace(k) {
		case "tables":
			tables, seen = n, seen|1
		case "databases":
			dbs, seen = n, seen|2
		}
	}
	return tables, dbs, seen == 3
}

// plural renders "1 table" / "13 tables" — English pluralisation kept in one
// place so a count and its noun can never disagree.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
