package backup

import (
	"strconv"
	"strings"

	"github.com/docker/docker/api/types"
)

// detectDBEngine guesses the database engine from the IMAGE NAME only (PLAN
// §4.1). Returns "postgres", "mysql", "mongodb", or "" for non-database
// containers.
//
// We deliberately do NOT infer the engine from environment variables: app
// containers (Mealie, Nextcloud, …) set POSTGRES_*/MYSQL_* to *connect* to a
// database, which is indistinguishable from a real DB server's env. Keying off
// those misclassifies the app and tries to run pg_dumpall inside it (which
// fails — the app has no DB tools). The database runs as its own container (a
// real postgres/mariadb/mongo image) and is detected there; the app container is
// backed up as files/volumes. The env arg is kept for the dump command, which
// reads credentials from it.
// DBEngine reports the database engine for an image ("postgres"/"mysql"/
// "mongodb"), or "" for a non-database image. Exported wrapper around
// detectDBEngine for callers outside the package (PLAN §9.7 critical-DB tier).
func DBEngine(image string) string { return detectDBEngine(image, nil) }

// Credentials are referenced BY NAME, never interpolated by value.
//
// Every script below is handed to `sh -c`, so the script text IS the exec's
// argv: it appears in `docker inspect` of the exec and in the container's own
// process list, where anything running inside that container can read it. A
// database password sitting there is readable by the very application the
// backup exists to protect.
//
// Nothing is lost by referencing instead: these values were read out of the
// CONTAINER's environment in the first place, so the script reads the same
// variables itself and the password never leaves the container it already
// lives in. The Go side still reads them to decide WHICH commands to build.
//
// The `${VAR:-}` form is required, not stylistic: several of these scripts run
// under `set -u`, where a bare reference to an unset variable aborts.
const (
	pgPasswordRef    = `"${POSTGRES_PASSWORD:-}"`
	mysqlRootPWRef   = `"${MYSQL_ROOT_PASSWORD:-${MARIADB_ROOT_PASSWORD:-}}"`
	mysqlAppUserRef  = `"${MYSQL_USER:-${MARIADB_USER:-}}"`
	mysqlAppPWRef    = `"${MYSQL_PASSWORD:-${MARIADB_PASSWORD:-}}"`
	mysqlAppDBRef    = `"${MYSQL_DATABASE:-${MARIADB_DATABASE:-}}"`
	mongoRootUserRef = `"${MONGO_INITDB_ROOT_USERNAME:-${MONGODB_ROOT_USER:-}}"`
	mongoRootPWRef   = `"${MONGO_INITDB_ROOT_PASSWORD:-${MONGODB_ROOT_PASSWORD:-}}"`
	mongoAppUserRef  = `"${MONGODB_USERNAME:-}"`
	mongoAppPWRef    = `"${MONGODB_PASSWORD:-}"`
	mongoAppDBRef    = `"${MONGODB_DATABASE:-${MONGO_INITDB_DATABASE:-}}"`
)

// pgReadinessQuery is a single read-only probe of a Postgres server's
// PITR-relevant settings (PLAN §9.7). It uses current_setting/SHOW only — it
// changes nothing on the server, so it is safe to run against a live DB.
const pgReadinessQuery = "select current_setting('wal_level')||'|'||current_setting('archive_mode')||'|'||current_setting('max_wal_senders')"

// PGReadinessCmd builds the in-container command that prints
// "<wal_level>|<archive_mode>|<max_wal_senders>" for a Postgres container,
// reading credentials from the container's own environment so nothing is
// hardcoded (PLAN §9.7). Detection only — see §9.7 on why DockBack does not
// itself enable archiving (that needs a DB restart it deliberately won't force).
func PGReadinessCmd(env []string) []string {
	e := envMap(env)
	user := firstNonEmpty(e["POSTGRES_USER"], "postgres")
	script := "PGPASSWORD=" + pgPasswordRef + " psql -U '" + shellEscape(user) + "' -tAX -c \"" + pgReadinessQuery + "\""
	return []string{"/bin/sh", "-c", script}
}

// pitrGuidance is the one-line remediation shown when a Postgres isn't
// configured for point-in-time recovery. Kept next to the probe so the message
// and the check can never drift.
const pitrGuidance = "Full-dump recovery only — set wal_level=replica and archive_mode=on for PITR."

// pitrServerReady is the verdict when the server's WAL settings DO support
// point-in-time recovery.
//
// It says "the server", deliberately. DockBack's own Postgres backup is a
// logical pg_dumpall, and a logical dump restores into a freshly initialised
// cluster with a new system identifier — archived WAL from the original cluster
// cannot be replayed onto it, so DockBack itself recovers to the last dump and
// no further. This probe reports what the SERVER is capable of, for WAL
// archiving run alongside it; the critical-data card has always worded it that
// way ("ready for external WAL-PITR tooling") and this now matches.
//
// The distinction is not pedantry. "Point-in-time recovery ready" on a backup
// tool's screen reads as a promise about the backups on that screen, and the one
// thing a backup tool must never do is overstate what it can bring back.
const pitrServerReady = "The server is configured for point-in-time recovery. DockBack's own backups restore to the last dump — PITR needs WAL archiving run alongside them."

// PGReadiness is the parsed verdict of the read-only PITR-readiness probe (F34 /
// PLAN §9.7): whether the server's WAL settings support point-in-time recovery,
// with the raw settings and a one-line guidance string.
type PGReadiness struct {
	Ready         bool   `json:"ready"`
	WALLevel      string `json:"wal_level"`
	ArchiveMode   string `json:"archive_mode"`
	MaxWALSenders string `json:"max_wal_senders,omitempty"`
	Detail        string `json:"detail"`
}

// ParsePGReadiness turns the "<wal_level>|<archive_mode>|<max_wal_senders>"
// output of PGReadinessCmd into a readiness verdict. PITR needs wal_level
// replica or logical AND archive_mode on; anything else can only recover to the
// last full dump (PLAN §9.7). Pure — no I/O — so it is unit-tested directly.
// An empty/garbled probe leaves WALLevel empty; callers treat that as "unknown"
// and surface nothing rather than a misleading verdict.
func ParsePGReadiness(out string) PGReadiness {
	parts := strings.Split(strings.TrimSpace(out), "|")
	var r PGReadiness
	if len(parts) > 0 {
		r.WALLevel = strings.TrimSpace(parts[0])
	}
	if len(parts) > 1 {
		r.ArchiveMode = strings.TrimSpace(parts[1])
	}
	if len(parts) > 2 {
		r.MaxWALSenders = strings.TrimSpace(parts[2])
	}
	r.Ready = (r.WALLevel == "replica" || r.WALLevel == "logical") && r.ArchiveMode == "on"
	if r.Ready {
		r.Detail = pitrServerReady
	} else {
		r.Detail = pitrGuidance
	}
	return r
}

// pgExtensionsQuery lists installed extensions as "name version" pairs,
// comma-separated, for the manifest's restore-compatibility record (PLAN §4.12).
// Read-only — safe against a live DB.
const pgExtensionsQuery = "select coalesce(string_agg(extname||' '||extversion, ',' order by extname),'') from pg_extension"

// PGExtensionsCmd builds the in-container command that prints the installed
// Postgres extension set (e.g. "vectors 0.2.0,vchord 0.4.2"), reading
// credentials from the container's own environment so nothing is hardcoded.
// Used to record which vector/other extensions a dump depends on, so a restore
// target's compatibility can be validated (the Immich pgvecto.rs -> VectorChord
// break is exactly the class of problem this surfaces).
func PGExtensionsCmd(env []string) []string {
	e := envMap(env)
	user := firstNonEmpty(e["POSTGRES_USER"], "postgres")
	db := firstNonEmpty(e["POSTGRES_DB"], "postgres")
	script := "PGPASSWORD=" + pgPasswordRef + " psql -U '" + shellEscape(user) + "' -d '" + shellEscape(db) + "' -tAX -c \"" + pgExtensionsQuery + "\""
	return []string{"/bin/sh", "-c", script}
}

// ParsePGExtensions splits PGExtensionsCmd output ("name version,name version,…")
// into a slice of "name version" entries. Returns a NON-NIL (possibly empty) slice,
// so a caller can distinguish "probed, none found" (empty) from "not probed" (nil)
// — the F41 restore-target extension probe relies on this.
func ParsePGExtensions(out string) []string {
	exts := []string{}
	for _, p := range strings.Split(strings.TrimSpace(out), ",") {
		if p = strings.TrimSpace(p); p != "" {
			exts = append(exts, p)
		}
	}
	return exts
}

func detectDBEngine(image string, _ []string) string {
	img := strings.ToLower(image)
	switch {
	// Postgres and the wider Postgres-derivative family. Beyond the official
	// image we recognise the common vector/extension and distribution builds so
	// they are dumped with pg_dump (a consistent logical dump) instead of falling
	// through to a raw, live-file copy of the data directory:
	//   - pgvector / pgvecto.rs / VectorChord  — vector-search Postgres (e.g. Immich)
	//   - timescale / citus / supabase          — Postgres distributions
	// The vector builds don't contain the substring "postgres", so without these
	// they were misclassified as non-databases (this is what produced the
	// inconsistent hot data-dir "DB backup" for Immich's tensorchord/pgvecto-rs).
	case strings.Contains(img, "postgres") || strings.Contains(img, "postgis") ||
		strings.Contains(img, "pgvector") || strings.Contains(img, "pgvecto") ||
		strings.Contains(img, "vectorchord") || strings.Contains(img, "vchord") ||
		strings.Contains(img, "timescale") || strings.Contains(img, "citus") ||
		strings.Contains(img, "supabase/postgres"):
		return "postgres"
	case strings.Contains(img, "mariadb") || strings.Contains(img, "mysql") || strings.Contains(img, "percona"):
		return "mysql"
	case strings.Contains(img, "mongo"):
		return "mongodb"
	// Redis and its drop-in forks (Valkey, KeyDB) keep an in-memory dataset that
	// only periodically flushes to dump.rdb — a raw live-file copy can catch it
	// mid-write. Dumping via redis-cli yields a consistent point-in-time RDB
	// (PLAN §4.1). A non-DB image whose tag merely contains "redis" is handled
	// by the exit-127 fallback in dbDumpCommand (same as the pg/mysql cases).
	case strings.Contains(img, "redis") || strings.Contains(img, "valkey") || strings.Contains(img, "keydb"):
		return "redis"
	}
	return ""
}

// dumpToolMissing reports whether a dump error means the DB CLI wasn't present
// in the container (exit 127 / "not found") — i.e. it isn't really a database
// server, or the image lacks the tooling. In that case the backup falls back to
// capturing files/volumes instead of failing outright.
func dumpToolMissing(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not found") || strings.Contains(s, "exited 127") || strings.Contains(s, "executable file not found")
}

// redisAuthUnavailable reports whether a Redis dump failed because nothing could
// authenticate to the server (F185).
//
// Matched on DockBack's own message rather than the exit code alone, so it
// cannot be confused with some other exit-3 path. Redis-only by design: see the
// caller for why this one engine may fall back and the SQL engines may not.
func redisAuthUnavailable(engineKind string, err error) bool {
	if engineKind != "redis" || err == nil {
		return false
	}
	return strings.Contains(err.Error(), "this Redis requires a password")
}

// cleanDBNames drops empty entries from a requested database-selection list and
// reports the cleaned slice — the caller treats an empty result as "all
// databases" (the full-cluster default), so a garbage-only selection can never
// silently narrow a dump to nothing.
// maxDBNameLen is the longest database name either engine accepts (MySQL's
// limit is 64; Postgres's is 63). Anything longer is not a name.
const maxDBNameLen = 64

// ValidDBName reports whether a database name may be passed into a shell inside
// the target container.
//
// The names in a backup request travel from an HTTP body into a command run as
// root inside the database container. They ARE correctly single-quoted at every
// builder today — a name carrying a quote, a backtick or a semicolon reaches
// pg_dump as one literal argument and nothing executes. This check is the belt
// to that pair of braces, and it earns its place twice over: it refuses a name
// at the API, where the caller can be told what is wrong, instead of letting a
// hopeless dump fail deep inside the container with an opaque client error.
//
// Deliberately NOT cleanSQLIdent's alphabet. Both engines accept hyphens and
// dots in a database name, and hyphenated names are ordinary — "my-app" is not
// exotic. Rejecting them would break real selections to guard against nothing.
// What is excluded is everything a shell, an option parser or a path could read
// as syntax.
func ValidDBName(s string) bool {
	if s == "" || len(s) > maxDBNameLen {
		return false
	}
	// A leading dash is an option to every client that takes --databases, and a
	// leading dot is a relative path. Neither is a name worth accepting.
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, ".") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.':
		default:
			return false
		}
	}
	return true
}

// InvalidDBNames returns the entries of dbs this application will not pass into
// a container, so a caller can name them all in one refusal rather than one at
// a time.
func InvalidDBNames(dbs []string) []string {
	var bad []string
	for _, d := range dbs {
		if d = strings.TrimSpace(d); d != "" && !ValidDBName(d) {
			bad = append(bad, d)
		}
	}
	return bad
}

func cleanDBNames(dbs []string) []string {
	out := make([]string, 0, len(dbs))
	for _, d := range dbs {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	return out
}

// dbDumpCommand builds the in-container dump command, reading credentials from
// the container's own environment so nothing is hardcoded. The dump runs live
// inside the target's namespaces (PLAN §2.11/§4.1). When dbs is non-empty (F8),
// the dump is narrowed to just those databases so one app's DB can be backed up
// (and restored) independently of the rest of a shared engine; empty preserves
// the full-cluster dump.
func dbDumpCommand(engine string, env []string, dbs []string) []string {
	e := envMap(env)
	dbs = cleanDBNames(dbs)
	switch engine {
	case "postgres":
		if len(dbs) > 0 {
			return pgSubsetDumpCommand(e, dbs)
		}
		user := firstNonEmpty(e["POSTGRES_USER"], "postgres")
		db := shellEscape(firstNonEmpty(e["POSTGRES_DB"], "postgres"))
		u := shellEscape(user)
		// First distinguish "the client tools aren't here" from "tools are here but
		// can't connect". If psql/pg_dumpall are ABSENT the container isn't a
		// Postgres server (e.g. an app image whose tag merely ends in "-postgres"),
		// so exit 127 ("not found") — the engine treats that as dumpToolMissing and
		// gracefully falls back to a file/volume backup. Only when the tools ARE
		// present but the credentials can't connect do we FAIL LOUDLY (exit 3) with
		// an actionable message, instead of silently degrading. pg_dumpall captures
		// every database plus roles/globals.
		script := `set -e; ` +
			`command -v psql >/dev/null 2>&1 || { echo "psql: not found" >&2; exit 127; }; ` +
			`command -v pg_dumpall >/dev/null 2>&1 || { echo "pg_dumpall: not found" >&2; exit 127; }; ` +
			`export PGPASSWORD=` + pgPasswordRef + `; ` +
			`if ! psql -U '` + u + `' -d '` + db + `' -tAc 'SELECT 1' >/dev/null 2>&1; then ` +
			`echo "DockBack: could not connect to Postgres with POSTGRES_USER/POSTGRES_PASSWORD from the container environment" >&2; exit 3; fi; ` +
			`exec pg_dumpall -U '` + u + `'`
		return []string{"/bin/sh", "-c", script}
	case "mysql":
		if len(dbs) > 0 {
			return mysqlSubsetDumpCommand(e, dbs)
		}
		// MariaDB 11+ ships `mariadb`/`mariadb-dump`; older ones ship the `mysql*`
		// names. Dump only USER databases (skip system schemas) so the dump
		// imports cleanly into a freshly-initialized engine on restore — the app
		// DB user is recreated from the container's env (PLAN §4.1).
		//
		// Authentication is tried in order until one works, because a single
		// method does NOT cover real deployments (this is why some backups came
		// back as 0-byte "empty dump"):
		//   1. root + MYSQL_ROOT_PASSWORD — MySQL, and MariaDB whose data dir was
		//      first-initialized with that password.
		//   2. root via the unix socket (no password) — MariaDB's default, where
		//      root@localhost uses the unix_socket plugin and password login is
		//      refused. exec runs as OS root, so socket auth succeeds. Also covers
		//      a stale MYSQL_ROOT_PASSWORD that no longer matches the real one.
		//   3. the application user (MYSQL_USER/MYSQL_PASSWORD) against its own
		//      database — for images where root is locked or its password is
		//      supplied out-of-band (e.g. *_FILE / secrets).
		// If none authenticate, or no user database is found, the script exits
		// non-zero so the backup FAILS LOUDLY instead of writing an empty dump
		// that only verification later catches.
		script := mysqlAuthLadder() +
			`exec "$DUMP" -u"$U" --databases $DBS --single-transaction --routines --triggers --events`
		return []string{"/bin/sh", "-c", script}
	case "mongodb":
		return mongoDumpCommand(e)
	case "redis":
		// Stream a consistent point-in-time RDB to stdout (redis-cli --rdb -).
		// If redis-cli is ABSENT the container isn't a Redis server (e.g. an app
		// image whose tag merely contains "redis") — exit 127 so the engine treats
		// it as dumpToolMissing and falls back to a file/volume backup, matching
		// the pg/mysql cases. The password is taken from the container's own
		// environment and passed via REDISCLI_AUTH (env), NEVER on argv, so it
		// never appears in the process list. REDIS_PASSWORD is the common var
		// (bitnami/others); a pre-set REDISCLI_AUTH is honored as-is. Authentication
		// or transfer failure exits non-zero, so the backup FAILS LOUDLY rather than
		// writing a partial/inconsistent snapshot.
		return redisDumpCommand()
	}
	return nil
}

// redisDumpCommand streams a consistent point-in-time RDB to stdout (F180).
//
// The password is the whole difficulty. It was read from $REDIS_PASSWORD and
// nowhere else, which covers the bitnami-style images and misses the most common
// arrangement of all: `redis-server --requirepass <secret>` in the compose
// command, where the value never becomes an environment variable at all. That
// produced a backup that failed with the server's own words — "NOAUTH
// Authentication required" — and no indication of what DockBack had looked for.
//
// So it now looks in every place the password can actually be, ending with the
// server's own command line, which is authoritative however it was configured:
//
//  1. REDISCLI_AUTH already set on the container — honoured as-is.
//  2. REDIS_PASSWORD / REDIS_PASS / REDIS_AUTH.
//  3. Their _FILE variants, for a mounted secret.
//  4. --requirepass on the redis-server process's own command line, read from
//     /proc. This is the one that covers a compose `command:`.
//  5. REDIS_ARGS, which is how the redis-stack images pass the same flag.
//
// Security: the password is only ever exported as REDISCLI_AUTH, never placed on
// the argv, so it cannot be read from the process list inside the container. It
// is never echoed — the failure message below reports the SERVER's reply, not
// what was tried.
//
// A missing redis-cli exits 127 so the engine falls back to a file backup (the
// image merely has "redis" in its tag). An authentication failure exits 3 and
// FAILS the backup: a Redis we cannot dump must not look like one we did.
func redisDumpCommand() []string {
	script := `command -v redis-cli >/dev/null 2>&1 || { echo "redis-cli: not found" >&2; exit 127; }; ` +
		`if [ -z "$REDISCLI_AUTH" ]; then ` +
		`PW="$REDIS_PASSWORD"; [ -z "$PW" ] && PW="$REDIS_PASS"; [ -z "$PW" ] && PW="$REDIS_AUTH"; ` +
		`if [ -z "$PW" ]; then for f in "$REDIS_PASSWORD_FILE" "$REDIS_PASS_FILE"; do ` +
		`[ -n "$f" ] && [ -r "$f" ] && { PW=$(cat "$f"); break; }; done; fi; ` +
		// The server's own command line. Every redis process is checked, not just
		// PID 1, because an image with an entrypoint wrapper leaves the server
		// somewhere else in the tree.
		`if [ -z "$PW" ]; then for c in /proc/[0-9]*/cmdline; do ` +
		`[ -r "$c" ] || continue; ` +
		`case "$(tr '\0' ' ' < "$c")" in *redis-server*) ;; *) continue;; esac; ` +
		`PW=$(tr '\0' '\n' < "$c" | awk 'p==1{print;exit} /^--requirepass$/{p=1}'); ` +
		`[ -n "$PW" ] && break; done; fi; ` +
		`if [ -z "$PW" ] && [ -n "$REDIS_ARGS" ]; then ` +
		`PW=$(printf '%s\n' "$REDIS_ARGS" | awk '{for(i=1;i<=NF;i++) if ($i=="--requirepass") {print $(i+1); exit}}'); fi; ` +
		// A redis.conf. $REDIS_CONF is where the recorded command line named one
		// (filled in from outside, since redis rewrites its own argv); the rest are
		// the paths the images actually use. `requirepass` first, then an ACL line
		// for the default user, which is the modern spelling of the same thing.
		`if [ -z "$PW" ]; then for cf in "$REDIS_CONF" /usr/local/etc/redis/redis.conf /etc/redis/redis.conf /etc/redis.conf /data/redis.conf /redis.conf; do ` +
		`[ -n "$cf" ] && [ -r "$cf" ] || continue; ` +
		`PW=$(awk '/^[[:space:]]*requirepass[[:space:]]+/{v=$2} END{if(v!="")print v}' "$cf" | tr -d '"'"'"'"'); ` +
		`[ -z "$PW" ] && PW=$(awk '/^[[:space:]]*user[[:space:]]+default[[:space:]]/{for(i=1;i<=NF;i++) if (substr($i,1,1)==">") v=substr($i,2)} END{if(v!="")print v}' "$cf"); ` +
		`[ -n "$PW" ] && break; done; fi; ` +
		`[ -n "$PW" ] && export REDISCLI_AUTH="$PW"; fi; ` +
		// Ask before dumping, so a refusal is reported as a refusal rather than
		// arriving as a torn transfer half way through an RDB stream.
		`P=$(redis-cli --no-auth-warning PING 2>&1); ` +
		`case "$P" in PONG) ;; ` +
		`*NOAUTH*|*WRONGPASS*|*"invalid password"*|*"without any password"*) ` +
		`echo "DockBack: this Redis requires a password and none of the places DockBack looks had a working one - REDIS_PASSWORD, REDIS_PASS, REDIS_AUTH, their _FILE variants, REDIS_ARGS, or --requirepass on the server's own command line. Set REDIS_PASSWORD on this container; it is read inside the container and passed to redis-cli through an environment variable, never on the command line. Server said: $P" >&2; exit 3;; ` +
		`*) echo "DockBack: Redis did not answer PING: $P" >&2; exit 3;; esac; ` +
		`exec redis-cli --no-auth-warning --rdb -`
	return []string{"/bin/sh", "-c", script}
}

// redisAuthFromArgv digs the password out of `redis-server --requirepass X`
// (F180).
//
// This is the arrangement the in-container search cannot reach, and the reason
// is worth recording: Redis REWRITES ITS OWN ARGV to a process title
// ("redis-server *:6379"), so by the time anything reads /proc the flag is gone.
// The value survives only in the container's recorded configuration, which is
// visible from outside — so it is read here and handed to redis-cli through the
// exec's environment, never on a command line.
//
// Accepts both spellings the flag has, and takes the LAST one, matching how
// redis itself resolves a repeated option.
func redisAuthFromArgv(argv ...[]string) string {
	found := ""
	for _, list := range argv {
		for i, a := range list {
			switch {
			case a == "--requirepass" && i+1 < len(list):
				found = list[i+1]
			case strings.HasPrefix(a, "--requirepass="):
				found = strings.TrimPrefix(a, "--requirepass=")
			}
		}
	}
	return strings.TrimSpace(found)
}

// redisDumpExecEnv is the extra environment the redis dump runs with: the
// password recovered from the container's configuration, if it was given there.
//
// Empty when there is nothing to add, which is the common case — the script's
// own in-container search covers every other way the password is configured.
func redisDumpExecEnv(insp types.ContainerJSON) []string {
	if insp.Config == nil {
		return nil
	}
	var out []string
	if pw := redisAuthFromArgv(insp.Config.Cmd, insp.Config.Entrypoint, insp.Args); pw != "" {
		out = append(out, "REDISCLI_AUTH="+pw)
	}
	// The config file the server was STARTED with, for the script to read inside
	// the container. Named from out here for the same reason the password is:
	// redis has overwritten its own argv by the time anything can look.
	if cf := redisConfFromArgv(insp.Config.Cmd, insp.Config.Entrypoint, insp.Args); cf != "" {
		out = append(out, "REDIS_CONF="+cf)
	}
	return out
}

// redisConfFromArgv finds the configuration file a redis-server was started
// with: the first bare argument ending in .conf, which is how redis takes it.
func redisConfFromArgv(argv ...[]string) string {
	for _, list := range argv {
		for _, a := range list {
			a = strings.TrimSpace(a)
			if strings.HasSuffix(a, ".conf") && !strings.HasPrefix(a, "-") {
				return a
			}
		}
	}
	return ""
}

// dbInitPossible reports whether an engine could initialise an EMPTY data
// directory with the environment this container has, and names what is missing
// when it could not (F174).
//
// This is asked immediately before the restore wipes that data directory, and
// the reason it has to be asked is a trap in how these images work. A database
// image only runs its first-time initialisation when the data directory is
// empty — and it REFUSES to initialise without a root password, exiting
// straight away. A container whose directory was initialised long ago runs
// happily forever without one, so the environment can be missing that variable
// for years and nothing ever goes wrong.
//
// Until a restore empties the directory. Then the container that has been fine
// since it was created exits on start, and it does so AFTER its data has been
// deleted — the worst possible ordering, because a failure here returns before
// the health gate and so takes no automatic rollback with it.
//
// So the check happens first, and a "no" refuses the restore while everything is
// still intact. The asymmetry justifies being strict: refusing costs one
// environment variable, and proceeding costs the data directory.
//
// ok=false is only ever returned when the engine is one whose rules are known.
// Anything else — a custom image, an engine not listed — returns true, because
// this must never refuse a restore it does not actually understand.
func dbInitPossible(engine string, env []string) (ok bool, missing string) {
	e := envMap(env)
	// A _FILE variant points at a secret mounted into the container and counts
	// just as well; the entrypoint reads it the same way.
	has := func(names ...string) bool {
		for _, n := range names {
			if strings.TrimSpace(e[n]) != "" || strings.TrimSpace(e[n+"_FILE"]) != "" {
				return true
			}
		}
		return false
	}
	switch engine {
	case "mysql":
		// The accepted set is the image's own, quoted from the error it prints
		// when it refuses: MARIADB_ROOT_PASSWORD, MARIADB_ROOT_PASSWORD_HASH,
		// MARIADB_ALLOW_EMPTY_ROOT_PASSWORD, MARIADB_RANDOM_ROOT_PASSWORD — plus
		// the MYSQL_-prefixed aliases both images honour.
		if has("MYSQL_ROOT_PASSWORD", "MARIADB_ROOT_PASSWORD",
			"MYSQL_ROOT_PASSWORD_HASH", "MARIADB_ROOT_PASSWORD_HASH",
			"MYSQL_ALLOW_EMPTY_PASSWORD", "MYSQL_ALLOW_EMPTY_ROOT_PASSWORD",
			"MARIADB_ALLOW_EMPTY_ROOT_PASSWORD",
			"MYSQL_RANDOM_ROOT_PASSWORD", "MARIADB_RANDOM_ROOT_PASSWORD") {
			return true, ""
		}
		return false, "MYSQL_ROOT_PASSWORD (or MARIADB_ROOT_PASSWORD)"
	case "postgres":
		if has("POSTGRES_PASSWORD", "POSTGRES_HOST_AUTH_METHOD") {
			return true, ""
		}
		return false, "POSTGRES_PASSWORD (or POSTGRES_HOST_AUTH_METHOD)"
	}
	return true, ""
}

// DBInitBlock is the plan-time half of the same check (F174): the warning a
// restore dialog can show BEFORE anything is touched, from the manifest alone.
//
// The pre-wipe check in the restore is the authoritative one — it reads the live
// container's environment, values included. This one has only the KEYS, because
// keys are all the manifest carries (a container environment routinely holds
// passwords, so values are dropped at the boundary). That makes it weaker in one
// specific direction: a variable that is present but empty looks satisfied here
// and is caught there.
//
// Weaker is the right trade. Shown early it costs nothing and saves a stack
// restore from stopping at service three of four; the guarantee still comes from
// the check that runs with the data directory still intact.
//
// Empty string means nothing to say — which includes every backup too old to
// have recorded environment keys at all, because "no evidence" must never render
// as "misconfigured".
func DBInitBlock(man *Manifest) string {
	if man == nil || len(man.ContainerEnvKeys) == 0 || len(man.Databases) == 0 {
		return ""
	}
	// A per-database subset dump re-imports into the RUNNING engine and never
	// empties the data directory, so none of this applies to it.
	for _, d := range man.Databases {
		if len(d.Databases) > 0 {
			return ""
		}
	}
	// The keys are recorded without values; presence is the only question this
	// form can answer, so hand them over as bare keys set to a placeholder.
	env := make([]string, 0, len(man.ContainerEnvKeys))
	for _, k := range man.ContainerEnvKeys {
		env = append(env, k+"=set")
	}
	if ok, missing := dbInitPossible(man.Databases[0].Engine, env); !ok {
		return "This restore re-initializes the database from its dump, and this container has no " + missing +
			" — the engine would refuse to start on an empty data directory. Add it before restoring."
	}
	return ""
}

// embeddedDumpCommand builds the dump for a database server bundled inside an
// application's own container (F126).
//
// Deliberately NOT the standalone-container command. That one dumps the whole
// cluster with pg_dumpall, which needs superuser — and an embedded database's
// application user routinely isn't one, so the dump would fail on the globals
// and take the backup with it. This dumps exactly the one database the app uses,
// self-cleaning so it re-imports over an initialised server.
//
// No password is ever placed on argv. An embedded server listens only on its
// container's own loopback/socket and trusts local connections, which is why the
// dump can run at all without one; PGPASSWORD is still exported when the
// environment happens to supply it, for images that do require it.
func embeddedDumpCommand(d *EmbeddedDump, env []string) []string {
	if d == nil {
		return nil
	}
	e := envMap(env)
	switch d.Engine {
	case "postgres":
		user := shellEscape(firstNonEmpty(d.User, e["POSTGRES_USER"], "postgres"))
		db := shellEscape(firstNonEmpty(d.DBName, e["POSTGRES_DB"], "postgres"))
		script := `command -v pg_dump >/dev/null 2>&1 || { echo "pg_dump: not found" >&2; exit 127; }; ` +
			pgSocketDirExport(d) +
			`[ -n ` + pgPasswordRef + ` ] && export PGPASSWORD=` + pgPasswordRef + `; ` +
			`if ! psql -U '` + user + `' -d '` + db + `' -tAc 'SELECT 1' >/dev/null 2>&1; then ` +
			`echo "DockBack: the embedded PostgreSQL did not accept a local connection as ` + user + `" >&2; exit 3; fi; ` +
			`exec pg_dump -U '` + user + `' --create --clean --if-exists -d '` + db + `'`
		return []string{"/bin/sh", "-c", script}
	case "mysql":
		// Embedded MariaDB (Uptime Kuma's shape): root over the local socket,
		// which the unix_socket plugin trusts without a password.
		//
		// --single-transaction is the whole reason this is safe to run against a
		// live application: InnoDB gives a consistent point-in-time view inside one
		// transaction, so the dump is coherent with NO downtime — the application
		// keeps running and keeps writing throughout.
		user := shellEscape(firstNonEmpty(d.User, "root"))
		var names string
		if d.DBName != "" {
			names = ` --databases '` + shellEscape(d.DBName) + `' --add-drop-database`
		} else {
			names = ` --all-databases`
		}
		script := `CLI=mysql; command -v mariadb >/dev/null 2>&1 && CLI=mariadb; ` +
			`DUMP=mysqldump; command -v mariadb-dump >/dev/null 2>&1 && DUMP=mariadb-dump; ` +
			`command -v "$DUMP" >/dev/null 2>&1 || { echo "$DUMP: not found" >&2; exit 127; }; ` +
			mysqlSocketOpt(d) +
			`if ! "$CLI" $SOCK -u'` + user + `' -N -e 'SELECT 1' >/dev/null 2>&1; then ` +
			`echo "DockBack: the embedded MariaDB did not accept a local connection as ` + user + `" >&2; exit 3; fi; ` +
			`exec "$DUMP" $SOCK -u'` + user + `'` + names + ` --single-transaction --routines --triggers --events`
		return []string{"/bin/sh", "-c", script}
	}
	return nil
}

// pgSocketDirExport exports PGHOST when a profile names where the embedded
// PostgreSQL keeps its socket (F166, the postgres half).
//
// Socket was honoured for mysql only, so an application whose server listens
// somewhere other than the client's compiled-in default (/var/run/postgresql)
// failed the preflight below — and a preflight failure is exit 3, which is NOT
// the graceful "tools missing" signal: it fails the whole backup of a perfectly
// healthy application. A bundled server listening on /tmp is exactly that case.
//
// PGHOST rather than a -h flag on each command: libpq reads it natively, so both
// psql and pg_dump pick it up with no argv juggling. libpq wants the socket's
// DIRECTORY while the mysql client wants the socket FILE, so a profile naming
// either is accepted and reduced to the directory.
func pgSocketDirExport(d *EmbeddedDump) string {
	dir := pgSocketDir(d)
	if dir == "" {
		return ""
	}
	return `export PGHOST='` + shellEscape(dir) + `'; `
}

// pgSocketDir reduces a profile's declared socket to the DIRECTORY libpq wants,
// or "" when none is declared. Shared with the RESTORE side, which has to reach
// the same server the dump was taken from.
func pgSocketDir(d *EmbeddedDump) string {
	if d == nil {
		return ""
	}
	dir := strings.TrimSpace(d.Socket)
	if dir == "" {
		return ""
	}
	if i := strings.LastIndex(dir, "/"); i > 0 && strings.HasPrefix(dir[i+1:], ".s.PGSQL") {
		dir = dir[:i]
	}
	return dir
}

// mysqlSocketOpt sets $SOCK to the client's socket option, or nothing (F166).
//
// An application that puts its server's socket somewhere non-standard —
// /app/data/run/mariadb.sock rather than /var/run/mysqld/ — leaves the client
// unable to find it, so every connection fails for a reason that reads like an
// authentication problem. Set as a shell variable rather than interpolated into
// each command so the empty case is genuinely empty rather than an option with
// no value.
func mysqlSocketOpt(d *EmbeddedDump) string {
	if d == nil || strings.TrimSpace(d.Socket) == "" {
		return `SOCK=""; `
	}
	return `SOCK="--socket=` + shellEscape(strings.TrimSpace(d.Socket)) + `"; `
}

// embeddedMySQLCountCmd asks the embedded server for the row count of each named
// table, printing "<table>|<rows>" per line (F168).
//
// Uses the same client, socket and user the dump does, so a deployment the dump
// can reach is one this can reach. A table that does not exist prints nothing
// rather than failing: the comparison then has nothing to compare, which is the
// honest outcome for a table the application has since renamed away.
func embeddedMySQLCountCmd(d *EmbeddedDump, tables []string) []string {
	if d == nil || d.Engine != "mysql" || len(tables) == 0 || d.DBName == "" {
		return nil
	}
	user := shellEscape(firstNonEmpty(d.User, "root"))
	db := shellEscape(d.DBName)
	var b strings.Builder
	b.WriteString(`CLI=mysql; command -v mariadb >/dev/null 2>&1 && CLI=mariadb; `)
	b.WriteString(`command -v "$CLI" >/dev/null 2>&1 || exit 0; `)
	b.WriteString(mysqlSocketOpt(d))
	for _, t := range tables {
		tn := shellEscape(cleanSQLIdent(t))
		if tn == "" {
			continue
		}
		// Backquoted identifier, and the name has already been reduced to
		// identifier characters — a table name is never operator input here, but
		// the profile registry is not a place to rely on that.
		b.WriteString(`N=$("$CLI" $SOCK -u'` + user + `' -N -B -e 'SELECT COUNT(*) FROM ` + "`" + tn + "`" + `' '` + db + `' 2>/dev/null); `)
		b.WriteString(`[ -n "$N" ] && printf '` + tn + `|%s\n' "$N"; `)
	}
	b.WriteString(`exit 0`)
	return []string{"/bin/sh", "-c", b.String()}
}

// cleanSQLIdent reduces a name to the characters a table identifier may contain,
// so nothing declared in the registry can ever become SQL of its own.
func cleanSQLIdent(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// pgSubsetDumpCommand dumps only the named Postgres databases (F8): the shared
// roles/globals first (so a restored DB's owner exists), then each selected
// database with pg_dump --create --clean --if-exists so a restore recreates ONLY
// those databases and leaves the rest of the cluster untouched. `set -e` makes
// any dump failure abort loudly rather than write a partial dump. DB names are
// single-quote-escaped for the shell (SEC-8); a non-existent name makes pg_dump
// exit non-zero, failing the backup rather than silently skipping.
func pgSubsetDumpCommand(e map[string]string, dbs []string) []string {
	user := shellEscape(firstNonEmpty(e["POSTGRES_USER"], "postgres"))
	probe := shellEscape(firstNonEmpty(e["POSTGRES_DB"], "postgres"))
	var b strings.Builder
	b.WriteString(`set -e; `)
	b.WriteString(`command -v psql >/dev/null 2>&1 || { echo "psql: not found" >&2; exit 127; }; `)
	b.WriteString(`command -v pg_dump >/dev/null 2>&1 || { echo "pg_dump: not found" >&2; exit 127; }; `)
	b.WriteString(`command -v pg_dumpall >/dev/null 2>&1 || { echo "pg_dumpall: not found" >&2; exit 127; }; `)
	b.WriteString(`export PGPASSWORD=` + pgPasswordRef + `; `)
	b.WriteString(`if ! psql -U '` + user + `' -d '` + probe + `' -tAc 'SELECT 1' >/dev/null 2>&1; then `)
	b.WriteString(`echo "DockBack: could not connect to Postgres with POSTGRES_USER/POSTGRES_PASSWORD from the container environment" >&2; exit 3; fi; `)
	// Roles/globals (no data) so an owner/grant referenced by a selected DB exists
	// on restore; benign "already exists" errors are non-fatal at import time.
	b.WriteString(`pg_dumpall -U '` + user + `' --globals-only; `)
	for _, d := range dbs {
		b.WriteString(`pg_dump -U '` + user + `' --create --clean --if-exists -d '` + shellEscape(d) + `'; `)
	}
	return []string{"/bin/sh", "-c", strings.TrimRight(b.String(), " ")}
}

// mysqlSubsetDumpCommand dumps only the named MySQL/MariaDB databases (F8), using
// the same auth ladder as the full dump. --add-drop-database makes each selected
// database cleanly REPLACE its target on restore (drop+recreate) while leaving
// the other databases on a shared server untouched. DB names are single-quote
// escaped for the shell (SEC-8).
func mysqlSubsetDumpCommand(_ map[string]string, dbs []string) []string {
	var names strings.Builder
	for i, d := range dbs {
		if i > 0 {
			names.WriteByte(' ')
		}
		names.WriteString(`'` + shellEscape(d) + `'`)
	}
	script := `set -u; ` +
		`CLI=mysql; command -v mariadb >/dev/null 2>&1 && CLI=mariadb; ` +
		`DUMP=mysqldump; command -v mariadb-dump >/dev/null 2>&1 && DUMP=mariadb-dump; ` +
		`command -v "$CLI" >/dev/null 2>&1 || { echo "$CLI: not found" >&2; exit 127; }; ` +
		`RP=` + mysqlRootPWRef + `; AU=` + mysqlAppUserRef + `; AP=` + mysqlAppPWRef + `; ` +
		`U=root; ` +
		`if [ -n "$RP" ] && MYSQL_PWD="$RP" "$CLI" -uroot -N -e "SELECT 1" >/dev/null 2>&1; then U=root; export MYSQL_PWD="$RP"; ` +
		`elif "$CLI" -uroot -N -e "SELECT 1" >/dev/null 2>&1; then U=root; ` +
		`elif [ -n "$AU" ] && MYSQL_PWD="$AP" "$CLI" -u"$AU" -N -e "SELECT 1" >/dev/null 2>&1; then U="$AU"; export MYSQL_PWD="$AP"; ` +
		`else echo "DockBack: could not authenticate to MariaDB/MySQL with root or application credentials from the container environment" >&2; exit 3; fi; ` +
		`exec "$DUMP" -u"$U" --databases ` + names.String() + ` --add-drop-database --single-transaction --routines --triggers --events`
	return []string{"/bin/sh", "-c", script}
}

// mongoDumpCommand builds the MongoDB dump with an auth ladder mirroring the
// other engines (F9): it PROBES with the shell client (mongosh, or the legacy
// mongo shell) and streams the archive with the FIRST method that authenticates —
// root credentials (admin), then a no-auth connection (auth disabled), then the
// application user against its own database (Bitnami MONGODB_USERNAME/DATABASE, or
// MONGO_INITDB_DATABASE). If none authenticate it FAILS LOUDLY (exit 3) rather
// than writing a thin/partial archive. An absent mongodump means the container
// isn't a Mongo server (exit 127 → the engine falls back to a file backup, like
// the pg/mysql/redis cases). Credentials come from the container's own
// environment; passwords are passed to the client, never echoed. Probes read from
// /dev/null so an empty password can never hang on an interactive prompt.
func mongoDumpCommand(_ map[string]string) []string {
	script := `set -u; ` +
		`command -v mongodump >/dev/null 2>&1 || { echo "mongodump: not found" >&2; exit 127; }; ` +
		`RU=` + mongoRootUserRef + `; RP=` + mongoRootPWRef + `; ` +
		`AU=` + mongoAppUserRef + `; AP=` + mongoAppPWRef + `; ADB=` + mongoAppDBRef + `; ` +
		// Bitnami defaults the root user to "root" when only a root password is set.
		`if [ -z "$RU" ] && [ -n "$RP" ]; then RU=root; fi; ` +
		`SH=''; command -v mongosh >/dev/null 2>&1 && SH=mongosh; ` +
		`{ [ -z "$SH" ] && command -v mongo >/dev/null 2>&1; } && SH=mongo; ` +
		`if [ -n "$SH" ]; then ` +
		// 1) root credentials against admin
		`if [ -n "$RU" ] && "$SH" --username="$RU" --password="$RP" --authenticationDatabase=admin --quiet --eval 'db.runCommand({ping:1})' </dev/null >/dev/null 2>&1; then ` +
		`exec mongodump --archive --username="$RU" --password="$RP" --authenticationDatabase=admin; fi; ` +
		// 2) no auth (auth disabled)
		`if "$SH" --quiet --eval 'db.runCommand({ping:1})' </dev/null >/dev/null 2>&1; then exec mongodump --archive; fi; ` +
		// 3) application user against its own database
		`if [ -n "$AU" ] && "$SH" --username="$AU" --password="$AP" --authenticationDatabase="$ADB" --quiet --eval 'db.runCommand({ping:1})' </dev/null >/dev/null 2>&1; then ` +
		`exec mongodump --archive --username="$AU" --password="$AP" --authenticationDatabase="$ADB" --db="$ADB"; fi; ` +
		`echo "DockBack: could not authenticate to MongoDB with root, no-auth, or application credentials from the container environment" >&2; exit 3; ` +
		`fi; ` +
		// No shell client to probe with (rare): best-effort by whichever creds are configured.
		`if [ -n "$RU" ]; then exec mongodump --archive --username="$RU" --password="$RP" --authenticationDatabase=admin; fi; ` +
		`if [ -n "$AU" ]; then exec mongodump --archive --username="$AU" --password="$AP" --authenticationDatabase="$ADB" --db="$ADB"; fi; ` +
		`exec mongodump --archive`
	return []string{"/bin/sh", "-c", script}
}

// MongoVersionCmd prints a MongoDB server's version string for the manifest
// (restore-compatibility, F9). It uses `mongod --version` — the server binary IS
// the running server in a mongo container — so it needs no credentials and works
// even with auth enabled, unlike a mongosh query (which can be absent on older
// images and would require a successful login).
func MongoVersionCmd() []string {
	return []string{"sh", "-c", "mongod --version 2>/dev/null | head -1"}
}

// DBVersionCmd returns the in-container command that prints an engine's version
// string on its first line, so a backup (recording the dump's version) and a
// restore (probing the target's version) read it the SAME way (F36). Nil for a
// non-database engine. No credentials needed — these are `--version` calls on the
// server/client binaries the image already ships.
func DBVersionCmd(engine string) []string {
	switch engine {
	case "postgres":
		return []string{"sh", "-c", "pg_dumpall --version 2>/dev/null || postgres --version 2>/dev/null"}
	case "mysql":
		return []string{"sh", "-c", "mariadb --version 2>/dev/null || mysql --version 2>/dev/null"}
	case "mongodb":
		return MongoVersionCmd()
	case "redis":
		return []string{"sh", "-c", "redis-server --version 2>/dev/null || redis-cli --version 2>/dev/null"}
	}
	return nil
}

// parseMajor extracts the MAJOR version integer from a DBVersionCmd() output line
// (F36), for the restore version-downgrade guard. Handles the real shapes:
//
//	"pg_dumpall (PostgreSQL) 16.2"                    -> 16
//	"mysql  Ver 8.0.35 for Linux ..."                -> 8
//	"mariadb  Ver 15.1 Distrib 10.11.6-MariaDB, ..." -> 10   (see below)
//	"db version v7.0.5"                              -> 7
//	"Redis server v=7.2.4 sha=..."                   -> 7
//
// MariaDB's leading "Ver 15.1" is the client-protocol version, NOT the server —
// the real server version follows "Distrib", so that segment is preferred. Returns
// ok=false when no version integer is present, so the caller never gates on a
// probe it couldn't read.
func parseMajor(version string) (int, bool) {
	v := strings.ToLower(strings.TrimSpace(version))
	if v == "" {
		return 0, false
	}
	if i := strings.Index(v, "distrib "); i >= 0 {
		v = v[i+len("distrib "):] // MariaDB: real server version follows "Distrib"
	}
	// First run of ASCII digits is the major.
	start := -1
	for i := 0; i < len(v); i++ {
		if v[i] >= '0' && v[i] <= '9' {
			start = i
			break
		}
	}
	if start < 0 {
		return 0, false
	}
	end := start
	for end < len(v) && v[end] >= '0' && v[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(v[start:end])
	if err != nil {
		return 0, false
	}
	return n, true
}

// DBListCommand builds the in-container command that lists the app (non-system)
// databases of a Postgres or MySQL/MariaDB server, one per line, so the UI can
// offer a per-database backup selection (F8). Read-only; reads credentials from
// the container's own environment. Returns nil for engines without per-database
// selection (mongodb/redis) or a non-database container.
func DBListCommand(engine string, env []string) []string {
	e := envMap(env)
	switch engine {
	case "postgres":
		user := shellEscape(firstNonEmpty(e["POSTGRES_USER"], "postgres"))
		q := "SELECT datname FROM pg_database WHERE datistemplate=false ORDER BY datname"
		script := `export PGPASSWORD=` + pgPasswordRef + `; exec psql -U '` + user + `' -tAc "` + q + `"`
		return []string{"/bin/sh", "-c", script}
	case "mysql":
		script := `CLI=mysql; command -v mariadb >/dev/null 2>&1 && CLI=mariadb; ` +
			`RP=` + mysqlRootPWRef + `; AU=` + mysqlAppUserRef + `; AP=` + mysqlAppPWRef + `; U=root; ` +
			`if [ -n "$RP" ] && MYSQL_PWD="$RP" "$CLI" -uroot -N -e "SELECT 1" >/dev/null 2>&1; then U=root; export MYSQL_PWD="$RP"; ` +
			`elif "$CLI" -uroot -N -e "SELECT 1" >/dev/null 2>&1; then U=root; ` +
			`elif [ -n "$AU" ] && MYSQL_PWD="$AP" "$CLI" -u"$AU" -N -e "SELECT 1" >/dev/null 2>&1; then U="$AU"; export MYSQL_PWD="$AP"; ` +
			`else exit 3; fi; ` +
			`exec "$CLI" -u"$U" -N -e "SHOW DATABASES" | grep -Ev '^(information_schema|performance_schema|mysql|sys)$'`
		return []string{"/bin/sh", "-c", script}
	}
	return nil
}

func dbExt(engine string) string {
	switch engine {
	case "postgres", "mysql":
		return ".sql"
	case "mongodb":
		return ".archive"
	case "redis":
		return ".rdb"
	}
	return ".dump"
}

func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// shellEscape neutralises single quotes for safe embedding in a '...' shell
// literal (PLAN §2.10 input validation discipline applied to exec).
func shellEscape(s string) string {
	return strings.ReplaceAll(s, "'", `'\''`)
}

// SQLiteBackupCmd builds the sqlite3 invocation that snapshots dbPath into a
// sibling "<dbPath>.dbk" via the online backup API (F22) — a transactionally
// consistent copy taken while the app keeps writing, so no torn WAL/journal ends
// up in the backup. The destination inside the `.backup` dot-command is quoted for
// sqlite3's own parser; a `.timeout` guards a busy database.
func SQLiteBackupCmd(dbPath string) []string {
	return []string{"sqlite3", dbPath, ".timeout 5000", ".backup '" + shellEscape(dbPath) + ".dbk'"}
}

// SQLiteRestoreCmd builds the sqlite3 invocation that restores dbPath from its
// consistent "<dbPath>.dbk" snapshot (F22), the inverse of SQLiteBackupCmd. The
// restore path can also simply move the .dbk over the .db; this is provided for a
// target/sidecar that has sqlite3.
func SQLiteRestoreCmd(dbPath string) []string {
	return []string{"sqlite3", dbPath, ".timeout 5000", ".restore '" + shellEscape(dbPath) + ".dbk'"}
}

// backupableBind reports whether a host bind-mount source should be backed up.
// Excludes the docker socket and system/host paths that aren't app data.
func backupableBind(src string) bool {
	if src == "" {
		return false
	}
	switch src {
	case "/var/run/docker.sock", "/etc/localtime", "/etc/timezone", "/etc/hosts", "/etc/resolv.conf", "/etc/hostname":
		return false
	// The whole host filesystem. Monitoring agents mount it read-only as a matter
	// of course — node-exporter's /:/rootfs:ro, cAdvisor, netdata — and it is
	// never a container's own data: it is the machine the container runs on, which
	// is not something a container backup has any business archiving.
	//
	// It matters now that read-only binds are candidates. Before, such a mount was
	// invisible; now it would be measured, and `du` over an entire host
	// filesystem spends the size probe's full four-minute budget on every backup
	// of every monitoring container to arrive at "too large, skipped by default".
	case "/":
		return false
	}
	for _, p := range []string{"/proc", "/sys", "/dev"} {
		if src == p || strings.HasPrefix(src, p+"/") {
			return false
		}
	}
	return true
}

// Probing credentials before trusting them, and recording what they reached
// (#13).
//
// R2 §Issue 13: an env declaring `MYSQL_ROOT_PASSWORD` where that password had
// never applied — the entrypoint only honours it when the data directory is
// empty at first start, and this one predated the value. "The declared
// configuration does not describe the running system." The dump was taken as the
// application user instead, which was sufficient, but it covers ONE SCHEMA and
// not `mysql.user` grants — "a real gap on a full-server restore".
//
// The ladder below already existed and already probes with SELECT 1, in the
// order the register asks for. What it never did was SAY which credential won,
// so nothing downstream could record the privilege level captured or report that
// the declared one is inert.

// mysqlAuthLadder is the shared preamble: it picks the client, authenticates in
// order, and leaves $CLI, $DUMP, $U and $DBS set.
//
// One constant, used by the dump command and by the probe that reports which
// candidate authenticated. They cannot disagree about the answer because they
// are the same code — which matters more here than anywhere, since a probe that
// reported a different user than the dump used would put a wrong privilege scope
// in the manifest.
//
// Order, and why each rung exists:
//  1. root + MYSQL_ROOT_PASSWORD — MySQL, and MariaDB whose data dir was
//     first-initialized with that password.
//  2. root via the unix socket, no password — MariaDB's default, where
//     root@localhost uses the unix_socket plugin. Also covers R2's case: a
//     declared root password that never applied.
//  3. the application user — for images where root is locked or its password is
//     supplied out of band.
//
// Nothing outside the container's own environment is ever tried.
func mysqlAuthLadder() string {
	// `set -f` disables pathname expansion for the whole script. $DBS below is
	// deliberately UNQUOTED so it splits into one argument per database, and
	// without this a database named `*` expanded to the working directory's file
	// names — mysqldump then received filenames where it expected databases.
	return `set -u; set -f; ` +
		`CLI=mysql; command -v mariadb >/dev/null 2>&1 && CLI=mariadb; ` +
		`DUMP=mysqldump; command -v mariadb-dump >/dev/null 2>&1 && DUMP=mariadb-dump; ` +
		// If NEITHER client is present the container isn't a MariaDB/MySQL server
		// (e.g. an app image whose tag merely contains "mysql"): exit 127 ("not
		// found") so the engine falls back to a file backup, rather than reaching
		// the exit-3 auth-failure path below and hard-failing.
		`command -v "$CLI" >/dev/null 2>&1 || { echo "$CLI: not found" >&2; exit 127; }; ` +
		`RP=` + mysqlRootPWRef + `; AU=` + mysqlAppUserRef + `; AP=` + mysqlAppPWRef + `; DB=` + mysqlAppDBRef + `; ` +
		`U=root; USEPW=0; ` +
		`if [ -n "$RP" ] && MYSQL_PWD="$RP" "$CLI" -uroot -N -e "SELECT 1" >/dev/null 2>&1; then U=root; USEPW=1; export MYSQL_PWD="$RP"; ` +
		`elif "$CLI" -uroot -N -e "SELECT 1" >/dev/null 2>&1; then U=root; USEPW=0; ` +
		`elif [ -n "$AU" ] && MYSQL_PWD="$AP" "$CLI" -u"$AU" -N -e "SELECT 1" >/dev/null 2>&1; then U="$AU"; USEPW=1; export MYSQL_PWD="$AP"; ` +
		`else echo "DockBack: could not authenticate to MariaDB/MySQL with root or application credentials from the container environment" >&2; exit 3; fi; ` +
		`DBS=$("$CLI" -u"$U" -N -e "SHOW DATABASES" 2>/dev/null | grep -Ev '^(information_schema|performance_schema|mysql|sys)$' | tr '\n' ' '); ` +
		`if [ -z "$(printf %s "$DBS" | tr -d ' ')" ] && [ -n "$DB" ]; then DBS="$DB"; fi; ` +
		`if [ -z "$(printf %s "$DBS" | tr -d ' ')" ]; then echo "DockBack: authenticated but found no user database to dump" >&2; exit 4; fi; ` +
		// A database whose name begins with a dash would be read as an OPTION by
		// the client, not as a name — `--all-databases` is a legal MySQL database
		// name. Refuse loudly rather than dump a different set than was asked for.
		`for d in $DBS; do case "$d" in -*) echo "DockBack: refusing to dump — this server has a database named $d, which the client would read as a command-line option rather than a database" >&2; exit 5;; esac; done; `
}

// authProbePrefix tags the probe's one line of output.
const authProbePrefix = "AUTH|"

// MySQLAuthProbeCommand runs the same ladder and reports the identity it reached
// and the databases that identity can see.
//
// A NAME and a database list. No password is printed, read back, or returned —
// the probe answers "who got in", never "with what".
func MySQLAuthProbeCommand(_ []string) []string {
	return []string{"/bin/sh", "-c", mysqlAuthLadder() +
		`printf '` + authProbePrefix + `%s|%s\n' "$U" "$DBS"`}
}

// parseAuthProbe reads the probe's report.
func parseAuthProbe(out string) (user string, databases []string, ok bool) {
	for _, line := range strings.Split(out, "\n") {
		rest, found := strings.CutPrefix(strings.TrimSpace(line), authProbePrefix)
		if !found {
			continue
		}
		u, dbs, split := strings.Cut(rest, "|")
		if u = strings.TrimSpace(u); u == "" {
			continue
		}
		if split {
			databases = strings.Fields(dbs)
		}
		return u, databases, true
	}
	return "", nil, false
}
