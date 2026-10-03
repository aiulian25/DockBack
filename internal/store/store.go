// Package store is DockBack's embedded persistence layer. It uses a pure-Go
// SQLite driver (modernc.org/sqlite) so the binary builds with CGO disabled
// (PLAN §1.4-§1.5). All app state — nodes, backups, schedules, audit, the
// single admin, sessions — lives here. Backups are *also* self-described by a
// manifest beside each archive so this DB is a cache, not the source of truth
// (PLAN §9.3).
package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// identRe limits a SQL identifier (table/column) to a conservative charset.
var identRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// quoteIdent guards a SQL identifier (table/column name) that CANNOT be a bound
// parameter — the DDL/PRAGMA statements below must interpolate identifiers as
// text. It accepts only [A-Za-z0-9_] and returns the name double-quoted (SEC-6).
//
// Inputs MUST be compile-time constants, never user input: a non-conforming
// identifier is a programming error, so we panic (fail loud) rather than emit a
// statement that could be injectable if a future caller wires in dynamic input.
func quoteIdent(ident string) string {
	if !identRe.MatchString(ident) {
		panic(fmt.Sprintf("store: invalid SQL identifier %q (identifiers must be constant, [A-Za-z0-9_])", ident))
	}
	return `"` + ident + `"`
}

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("not found")

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;

CREATE TABLE IF NOT EXISTS users (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  username      TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  totp_secret   TEXT NOT NULL DEFAULT '',
  totp_last_step INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
  token      TEXT PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  last_seen  INTEGER NOT NULL DEFAULT 0
);

-- Scoped API tokens for non-interactive automation (F45). Only the SHA-256 hash
-- of the token value is stored (the plaintext is shown once at creation); lookup
-- is by exact hash, so comparison is constant-time by construction.
CREATE TABLE IF NOT EXISTS api_tokens (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  hash       TEXT NOT NULL UNIQUE,
  scopes     TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  last_used  INTEGER NOT NULL DEFAULT 0
);

-- Persistent alert inbox (F46): every warning/critical operational alert is
-- recorded so a warning never vanishes when no channel is configured or the app
-- restarts. dedup scopes a recurring condition (e.g. per-destination).
CREATE TABLE IF NOT EXISTS alerts (
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  ts       INTEGER NOT NULL,
  kind     TEXT NOT NULL,
  severity TEXT NOT NULL,
  title    TEXT NOT NULL,
  message  TEXT NOT NULL,
  dedup    TEXT NOT NULL DEFAULT '',
  acked    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_alerts_acked_ts ON alerts(acked, ts DESC);

CREATE TABLE IF NOT EXISTS nodes (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  cluster    TEXT NOT NULL DEFAULT 'default',
  transport  TEXT NOT NULL,            -- local-proxy | tcp-proxy | ssh | mtls
  address    TEXT NOT NULL,            -- DOCKER_HOST value
  secret_enc BLOB,                     -- encrypted ssh key / tls bundle (PLAN §3.8)
  status     TEXT NOT NULL DEFAULT 'unknown',
  last_seen  INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);

-- Registered clusters (F104). Membership still lives on nodes.cluster (which
-- stores the NAME), so this table adds identity — description, colour, rename,
-- delete — without a data migration or a dual-write. NOCASE on the primary key
-- is what makes "prod" and "Prod" the same cluster instead of two fleets.
--
-- A cluster owns no nodes, containers or backups; deleting a row here removes a
-- grouping and its policy scope, never any data.
CREATE TABLE IF NOT EXISTS clusters (
  name        TEXT PRIMARY KEY COLLATE NOCASE,
  description TEXT NOT NULL DEFAULT '',
  color       TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL
);

-- Event-driven cached inventory per node (PLAN §4.13): the container/stack/
-- summary snapshot is refreshed from the Docker event stream + a slow reconcile
-- and persisted here, so page loads read a cached snapshot (and it survives a
-- restart) instead of re-listing every node's Docker API on every request.
CREATE TABLE IF NOT EXISTS node_inventory (
  node_id    TEXT PRIMARY KEY,
  payload    BLOB NOT NULL,                  -- JSON {summary, containers, stacks}
  reachable  INTEGER NOT NULL DEFAULT 0,
  error      TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0
);

-- Pinned SSH host keys (F1 / PLAN §3): the remote host's public key is recorded
-- on first successful connect (trust-on-first-use); a later connect whose key
-- differs is refused, closing the man-in-the-middle hole InsecureIgnoreHostKey
-- left open. The key stored here is PUBLIC (not a secret).
CREATE TABLE IF NOT EXISTS ssh_hostkeys (
  node_id     TEXT PRIMARY KEY,
  hostport    TEXT NOT NULL DEFAULT '',
  key_type    TEXT NOT NULL,        -- e.g. ssh-ed25519, ecdsa-sha2-nistp256
  key_b64     TEXT NOT NULL,        -- base64 of the wire-format public key
  fingerprint TEXT NOT NULL,        -- SHA256:… (for display)
  pinned_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS backups (
  id               TEXT PRIMARY KEY,
  node_id          TEXT NOT NULL,
  stack            TEXT NOT NULL DEFAULT '',
  target_name      TEXT NOT NULL,      -- container or stack name
  status           TEXT NOT NULL,      -- pending|running|success|failed
  verified         TEXT NOT NULL DEFAULT 'unverified', -- verified|unverified|failed
  size_bytes       INTEGER NOT NULL DEFAULT 0,
  cipher_sha256    TEXT NOT NULL DEFAULT '',
  storage_key      TEXT NOT NULL DEFAULT '',
  manifest_json    TEXT NOT NULL DEFAULT '{}',
  verification_json TEXT NOT NULL DEFAULT '{}',
  locations        TEXT NOT NULL DEFAULT '[]',
  error            TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL,
  completed_at     INTEGER NOT NULL DEFAULT 0,
  last_verified_at INTEGER NOT NULL DEFAULT 0   -- last successful re-verify/scrub (PLAN §9.4)
);
CREATE INDEX IF NOT EXISTS idx_backups_node ON backups(node_id, created_at DESC);
-- Perf Fix 5 (F7): the GLOBAL recency sort (list/aggregates with no node filter)
-- was a full scan + temp B-tree without a bare created_at index, and the
-- scheduler's latest-success-per-target lookup scanned a node's whole history.
CREATE INDEX IF NOT EXISTS idx_backups_created ON backups(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_backups_target  ON backups(node_id, target_name, status, created_at DESC);
-- The same narrowing for a compose project. Every stack view (the services a
-- stack has backups for, its recorded bind layout, its consistency groups, its
-- restore plan) used to read the whole node's catalog and filter in Go.
CREATE INDEX IF NOT EXISTS idx_backups_stack   ON backups(node_id, stack, status, created_at DESC);

-- Real per-node event counters (PLAN §5.4 dashboard). The Docker daemon only
-- retains its last ~256 events in memory, so a "--since 30d" query is capped and
-- meaningless; instead the per-node event-stream watcher increments a true,
-- persisted running total here. The daily count resets when the date rolls over.
CREATE TABLE IF NOT EXISTS node_events (
  node_id TEXT PRIMARY KEY,
  total   INTEGER NOT NULL DEFAULT 0,
  today   INTEGER NOT NULL DEFAULT 0,
  day     TEXT NOT NULL DEFAULT ''     -- YYYY-MM-DD the daily count belongs to
);

-- Per-node connection-health TRANSITIONS (F40): one row each time a node flips
-- reachable<->unreachable, so a flapping node is visible as a pattern and a missed
-- backup can be correlated with the node being down. NOT one row per refresh —
-- RecordNodeHealth inserts only on a change from the newest row. Pruned to 90 days.
CREATE TABLE IF NOT EXISTS node_health (
  node_id   TEXT NOT NULL,
  ts        INTEGER NOT NULL,
  reachable INTEGER NOT NULL,
  error     TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (node_id, ts)
);
CREATE INDEX IF NOT EXISTS idx_node_health_node ON node_health(node_id, ts DESC);

-- Durable backup queue (PLAN §4.13/§9.10): jobs waiting for a concurrency slot
-- are persisted here so a restart resumes them (rather than silently dropping a
-- nightly window). A row is deleted when its backup completes or is canceled.
CREATE TABLE IF NOT EXISTS queued_jobs (
  id         TEXT PRIMARY KEY,        -- backup id
  node_id    TEXT NOT NULL,
  node_name  TEXT NOT NULL,
  opts_json  TEXT NOT NULL,           -- marshaled backup.Options
  priority   INTEGER NOT NULL DEFAULT 0,
  seq        INTEGER NOT NULL DEFAULT 0,
  not_before INTEGER NOT NULL DEFAULT 0, -- unix seconds; jittered start floor
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS schedules (
  id           TEXT PRIMARY KEY,
  node_id      TEXT NOT NULL,
  target_name  TEXT NOT NULL,
  cron         TEXT NOT NULL,
  options_json TEXT NOT NULL DEFAULT '{}',
  enabled      INTEGER NOT NULL DEFAULT 1,
  last_run     INTEGER NOT NULL DEFAULT 0,
  next_run     INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

-- Per-scope policy overrides (PLAN §4.13). The global policy (settings table) is
-- the default; a row here overrides retention/destinations for one scope. Scope
-- is "node:<id>" today; "stack:<node>/<stack>" is reserved for per-stack later.
CREATE TABLE IF NOT EXISTS policy_overrides (
  scope TEXT PRIMARY KEY,
  json  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS destinations (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  type       TEXT NOT NULL,        -- smb | webdav | local
  config_enc BLOB,                 -- encrypted JSON of connection config (PLAN §3.8)
  enabled    INTEGER NOT NULL DEFAULT 1,
  status     TEXT NOT NULL DEFAULT 'unknown',
  created_at INTEGER NOT NULL
);

-- Destinations for the APP's OWN backups (PLAN §6.5/§9.3) — deliberately separate
-- from the container-backup 'destinations' table above.
CREATE TABLE IF NOT EXISTS app_destinations (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  type       TEXT NOT NULL,
  config_enc BLOB,
  enabled    INTEGER NOT NULL DEFAULT 1,
  status     TEXT NOT NULL DEFAULT 'unknown',
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS audit (
  id     INTEGER PRIMARY KEY AUTOINCREMENT,
  ts     INTEGER NOT NULL,
  actor  TEXT NOT NULL,
  action TEXT NOT NULL,
  target TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS restore_drills (
  backup_id TEXT PRIMARY KEY,   -- a drill is per backup (per node+container), like verify
  ok        INTEGER NOT NULL,
  detail    TEXT NOT NULL DEFAULT '',
  ran_at    INTEGER NOT NULL
);

-- Pilot-light standby rehearsals (F62): per container, a scheduled proof that its
-- newest verified backup restores + boots on a DESIGNATED FALLBACK NODE (not just
-- a same-node sandbox). Keyed by the SOURCE (node_id, target); standby_node is the
-- fallback the rehearsal restores an isolated clone onto.
CREATE TABLE IF NOT EXISTS standby (
  node_id       TEXT NOT NULL,
  target        TEXT NOT NULL,
  standby_node  TEXT NOT NULL,
  interval_days INTEGER NOT NULL DEFAULT 7,
  last_run      INTEGER NOT NULL DEFAULT 0,
  last_ok       INTEGER NOT NULL DEFAULT 0,
  last_detail   TEXT NOT NULL DEFAULT '',
  boot_ms       INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(node_id, target)
);

CREATE TABLE IF NOT EXISTS destination_samples (
  dest_id     TEXT NOT NULL,
  day         INTEGER NOT NULL,   -- unix day (ts/86400): one sample kept per day
  ts          INTEGER NOT NULL,
  total_bytes INTEGER NOT NULL,   -- 0 when the destination doesn't report a quota
  used_bytes  INTEGER NOT NULL,
  PRIMARY KEY (dest_id, day)
);

-- Persisted per-run logs (F8): the live SSE stream is in-memory only, so a
-- completed run's log is lost on reload. Every backup/restore/mirror line is
-- also appended here keyed by backup id, trimmed to the newest ~2000, and
-- pruned when the backup is deleted.
CREATE TABLE IF NOT EXISTS run_logs (
  backup_id TEXT NOT NULL,
  seq       INTEGER NOT NULL,
  ts        INTEGER,
  level     TEXT,
  msg       TEXT,
  PRIMARY KEY (backup_id, seq)
);
`

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB

	// F68 tamper-evident audit trail: an optional chainer installed once at boot
	// computes each new audit row's HMAC chain value from the previous row's
	// chain + this row's fields. Nil = legacy plain inserts (tests unaffected).
	// auditMu serializes chained writes so concurrent audits can't fork the chain.
	auditMu    sync.Mutex
	auditChain func(prev string, fields ...string) string
}

// Open opens (creating if needed) the SQLite database at path and applies the
// schema.
func Open(path string) (*Store, error) {
	// Durable, concurrency-safe pragmas applied to EVERY connection via the DSN
	// (not just the first), so a recycled connection can't lose the busy_timeout
	// / WAL / FK settings. WAL + synchronous(NORMAL) is durable on commit.
	dsn := path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite: single writer; serialize to avoid lock churn
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("applying schema: %w", err)
	}
	st := &Store{db: db}
	if err := st.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating schema: %w", err)
	}
	return st, nil
}

// migrate applies idempotent column additions for databases created by older
// versions (CREATE TABLE IF NOT EXISTS won't add new columns).
//
// A failure here stops startup. It used to be discarded, so a database that
// could not take a column carried on running without it — and the first symptom
// was an unrelated "no such column" from whatever query needed it next, hours
// later and nowhere near the cause. The one expected failure is a column that is
// already there, which is not a failure at all.
func (s *Store) migrate() error {
	var failed error
	// table/col are compile-time constants below; quoteIdent enforces that (SEC-6).
	// def is a type/constraint clause (e.g. "TEXT NOT NULL DEFAULT ''"), not an
	// identifier, so it is a constant used verbatim.
	addColumn := func(table, col, def string) {
		if failed != nil {
			return // the first failure is the honest one; stop there
		}
		rows, err := s.db.Query("PRAGMA table_info(" + quoteIdent(table) + ")")
		if err != nil {
			failed = fmt.Errorf("reading %s columns: %w", table, err)
			return
		}
		has := false
		for rows.Next() {
			var cid int
			var name, ctype string
			var notnull, pk int
			var dflt any
			if rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk) == nil && name == col {
				has = true
			}
		}
		rows.Close()
		if !has {
			if _, err := s.db.Exec("ALTER TABLE " + quoteIdent(table) + " ADD COLUMN " + quoteIdent(col) + " " + def); err != nil {
				// A column that already exists is the one benign outcome — two
				// instances opening the same file can race the PRAGMA above.
				if !strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
					failed = fmt.Errorf("adding %s.%s: %w", table, col, err)
				}
			}
		}
	}
	addColumn("backups", "locations", "TEXT NOT NULL DEFAULT '[]'")
	addColumn("backups", "last_verified_at", "INTEGER NOT NULL DEFAULT 0") // PLAN §9.4 scrub
	// F2: pin a backup ("keep forever" — exempt from all pruning) + a free-text label.
	addColumn("backups", "pinned", "INTEGER NOT NULL DEFAULT 0")
	addColumn("backups", "label", "TEXT NOT NULL DEFAULT ''")
	addColumn("backups", "duration_ms", "INTEGER NOT NULL DEFAULT 0") // F28: wall-clock run time for drift detection
	addColumn("users", "totp_pending", "TEXT NOT NULL DEFAULT ''")
	addColumn("users", "totp_recovery", "TEXT NOT NULL DEFAULT ''")
	addColumn("users", "totp_last_step", "INTEGER NOT NULL DEFAULT 0") // SEC-4 TOTP replay guard
	addColumn("sessions", "extended", "INTEGER NOT NULL DEFAULT 0")
	// Soft-delete tombstone for nodes: a "forget node" sets deleted_at=now so the
	// removal is DURABLE at click time (survives a page refresh) while a short undo
	// window can still restore it; a background sweep purges expired tombstones.
	addColumn("nodes", "deleted_at", "INTEGER NOT NULL DEFAULT 0")
	// Sliding idle-timeout tracking (PLAN §10.2). Backfill pre-existing rows so a
	// legacy session is measured from its creation, not treated as instantly idle.
	addColumn("sessions", "last_seen", "INTEGER NOT NULL DEFAULT 0")
	// F202: where a session signed in from, so "sign out everywhere else" can
	// become "sign out THAT one". Empty on rows that predate this.
	addColumn("sessions", "ip", "TEXT NOT NULL DEFAULT ''")
	addColumn("sessions", "user_agent", "TEXT NOT NULL DEFAULT ''")
	s.db.Exec(`UPDATE sessions SET last_seen=created_at WHERE last_seen=0`)
	// F65: optional API-token expiry. 0 = never expires (every pre-existing
	// token keeps working unchanged).
	addColumn("api_tokens", "expires_at", "INTEGER NOT NULL DEFAULT 0")
	// F201: optional source-address pin. Empty on every existing row, which means
	// "usable from anywhere" — the behaviour those tokens were minted under.
	addColumn("api_tokens", "allowed_cidrs", "TEXT NOT NULL DEFAULT ''")
	// F69: ransomware tripwire — non-empty holds the human reason a backup's
	// incremental delta looked like a mass-change event.
	addColumn("backups", "suspect", "TEXT NOT NULL DEFAULT ''")
	// F68: tamper-evident audit trail — each row's HMAC chain value. Empty on
	// pre-feature rows (grandfathered behind the recorded chain-start anchor).
	addColumn("audit", "chain", "TEXT NOT NULL DEFAULT ''")

	// restore_drills was re-keyed from (node_id, stack) to per-backup (backup_id).
	// Drill results are ephemeral point-in-time proofs (regenerated on the next
	// run), so an old-schema table is simply dropped and recreated.
	if s.hasColumn("restore_drills", "stack") {
		s.db.Exec(`DROP TABLE IF EXISTS restore_drills`)
		s.db.Exec(`CREATE TABLE restore_drills (backup_id TEXT PRIMARY KEY, ok INTEGER NOT NULL, detail TEXT NOT NULL DEFAULT '', ran_at INTEGER NOT NULL)`)
	}

	// F104: adopt the cluster names already typed into node rows, so an upgrade
	// lands with the clusters the fleet was using rather than an empty registry.
	s.seedClusters()
	return failed
}

// hasColumn reports whether a table has a given column (used by migrations).
func (s *Store) hasColumn(table, col string) bool {
	rows, err := s.db.Query("PRAGMA table_info(" + quoteIdent(table) + ")") // table is a constant (SEC-6)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk) == nil && name == col {
			return true
		}
	}
	return false
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// ---------------- App (config) backup & restore (PLAN §6.5/§9.3) ----------------

// SnapshotTo writes a consistent, standalone copy of the database to path
// (SQLite VACUUM INTO — safe while the DB is live), then scrubs transient auth
// state (active sessions, their csrf tokens, and their step-up grants) so an
// application backup never carries live session credentials. The copy is
// otherwise a faithful image:
// nodes, the backup catalog, settings, destinations, and the admin account
// including its sealed TOTP secret. Node connection secrets (SSH keys / mTLS
// bundles) are sealed at rest with the master key (F2), so an app backup no
// longer carries plaintext keys either.
func (s *Store) SnapshotTo(path string) error {
	// VACUUM INTO takes a string LITERAL path (not a bound parameter, and not an
	// identifier — so quoteIdent doesn't apply); single-quote-escape it (SEC-6).
	lit := "'" + strings.ReplaceAll(path, "'", "''") + "'"
	if _, err := s.db.Exec("VACUUM INTO " + lit); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	cp, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("snapshot scrub open: %w", err)
	}
	defer cp.Close()
	if _, err := cp.Exec(`DELETE FROM sessions;
		DELETE FROM settings WHERE key LIKE 'csrf:%';
		DELETE FROM settings WHERE key LIKE 'stepup:%';`); err != nil {
		return fmt.Errorf("snapshot scrub: %w", err)
	}
	return nil
}

// ValidateDBFile sanity-checks that path is a readable DockBack database (a
// valid SQLite file with the core tables) — used before staging/applying a
// restore so a corrupt or wrong-format upload can never brick startup.
func ValidateDBFile(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	var n int
	for _, t := range []string{"users", "nodes", "backups", "settings"} { // constant table list (SEC-6)
		if err := db.QueryRow("SELECT count(*) FROM " + quoteIdent(t)).Scan(&n); err != nil {
			return fmt.Errorf("not a valid DockBack database (table %q): %w", t, err)
		}
	}
	return nil
}

// CheckSnapshot proves a stored app-backup's SQLite snapshot is intact and usable
// (F58): it opens the file READ-ONLY, runs `PRAGMA integrity_check` (which must
// report "ok"), and confirms the core catalog tables (nodes, backups) are readable.
// Package-level (no receiver) so appbackup can prove an archive without a live
// store. Read-only: it never writes to the file, and can't be swapped in anywhere.
func CheckSnapshot(path string) error {
	db, err := sql.Open("sqlite", path+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		return err
	}
	defer db.Close()

	var res string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&res); err != nil {
		return fmt.Errorf("integrity check could not run (corrupt file?): %w", err)
	}
	if res != "ok" {
		return fmt.Errorf("integrity_check reported: %s", res)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM nodes").Scan(&n); err != nil {
		return fmt.Errorf("nodes table unreadable: %w", err)
	}
	if err := db.QueryRow("SELECT count(*) FROM backups").Scan(&n); err != nil {
		return fmt.Errorf("backups table unreadable: %w", err)
	}
	return nil
}

// ApplyPendingRestore swaps in a staged restore database (written as
// <dataDir>/restore.db by the restore handler) on startup, BEFORE the store is
// opened — the only safe moment to replace the live DB file. A staged file that
// fails validation is set aside (.bad) rather than applied, so a bad restore
// never prevents the app from starting.
func ApplyPendingRestore(dataDir string) error {
	pending := filepath.Join(dataDir, "restore.db")
	if _, err := os.Stat(pending); err != nil {
		return nil // nothing staged
	}
	if err := ValidateDBFile(pending); err != nil {
		_ = os.Rename(pending, pending+".bad")
		return fmt.Errorf("staged restore invalid, ignored: %w", err)
	}
	live := filepath.Join(dataDir, "dockback.db")
	// Drop the old WAL/SHM so SQLite doesn't reconcile them against the new file.
	for _, suf := range []string{"-wal", "-shm"} {
		_ = os.Remove(live + suf)
	}
	if err := os.Rename(pending, live); err != nil {
		return fmt.Errorf("apply restore: %w", err)
	}
	return nil
}

func now() int64 { return time.Now().Unix() }

// ---------------- Users ----------------

// User is the single admin account.
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	TOTPSecret   string // sealed active 2FA secret (empty = 2FA disabled)
	TOTPPending  string // sealed secret mid-enrolment (not yet confirmed)
	TOTPRecovery string // JSON array of SHA-256-hashed one-time recovery codes
	TOTPLastStep int64  // last consumed TOTP 30s step — single-use/replay guard (SEC-4)
}

// UserCount returns how many users exist (used to decide first-run bootstrap).
func (s *Store) UserCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CreateUser inserts a new user.
func (s *Store) CreateUser(username, passwordHash string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO users(username, password_hash, created_at) VALUES(?,?,?)`,
		username, passwordHash, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetSoleUser returns the single account when exactly one exists (the
// single-admin invariant, PLAN §3.1). Returns ErrNotFound if there are zero or
// more than one, so callers can safely no-op instead of guessing which to touch.
func (s *Store) GetSoleUser() (*User, error) {
	n, err := s.UserCount()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, ErrNotFound
	}
	u := &User{}
	err = s.db.QueryRow(`SELECT id, username, password_hash, totp_secret, totp_pending, totp_recovery, totp_last_step FROM users`).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.TOTPSecret, &u.TOTPPending, &u.TOTPRecovery, &u.TOTPLastStep)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

// RenameUser changes only a user's login/display name, keyed by the stable id so
// the password hash, 2FA secret/recovery, sessions (by user_id), and all
// (non-user-scoped) app data are preserved — a rename never loses data.
func (s *Store) RenameUser(id int64, newName string) error {
	_, err := s.db.Exec(`UPDATE users SET username=? WHERE id=?`, newName, id)
	return err
}

// GetUserByName looks up a user by username.
func (s *Store) GetUserByName(username string) (*User, error) {
	u := &User{}
	err := s.db.QueryRow(`SELECT id, username, password_hash, totp_secret, totp_pending, totp_recovery, totp_last_step FROM users WHERE username=?`, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.TOTPSecret, &u.TOTPPending, &u.TOTPRecovery, &u.TOTPLastStep)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

// SetTOTPPending stores a sealed secret during enrolment (not yet active).
func (s *Store) SetTOTPPending(userID int64, sealed string) error {
	_, err := s.db.Exec(`UPDATE users SET totp_pending=? WHERE id=?`, sealed, userID)
	return err
}

// EnableTOTP activates 2FA: promotes the (sealed) secret, stores hashed recovery
// codes, clears the pending slot, and seeds the last-consumed step (SEC-4) so the
// enrolment code can't be immediately replayed at the login prompt.
func (s *Store) EnableTOTP(userID int64, sealed, recoveryJSON string, lastStep int64) error {
	_, err := s.db.Exec(`UPDATE users SET totp_secret=?, totp_recovery=?, totp_pending='', totp_last_step=? WHERE id=?`,
		sealed, recoveryJSON, lastStep, userID)
	return err
}

// DisableTOTP turns 2FA off and clears all related state (including the replay
// guard's last-step, so a fresh re-enrolment starts clean).
func (s *Store) DisableTOTP(userID int64) error {
	_, err := s.db.Exec(`UPDATE users SET totp_secret='', totp_pending='', totp_recovery='', totp_last_step=0 WHERE id=?`, userID)
	return err
}

// SetTOTPSecret replaces only the sealed active 2FA secret — used by master-key
// rotation (F16) to re-seal it under the new key, leaving recovery codes (stored
// as hashes) and the replay guard untouched.
func (s *Store) SetTOTPSecret(userID int64, sealed string) error {
	_, err := s.db.Exec(`UPDATE users SET totp_secret=? WHERE id=?`, sealed, userID)
	return err
}

// SetTOTPLastStep records the newest consumed TOTP step (SEC-4 single-use guard).
// The WHERE guard keeps it monotonic so a racing/older login can't roll it back.
func (s *Store) SetTOTPLastStep(userID, step int64) error {
	_, err := s.db.Exec(`UPDATE users SET totp_last_step=? WHERE id=? AND totp_last_step<?`, step, userID, step)
	return err
}

// UpdateRecovery persists the remaining recovery-code hashes (after one is used).
func (s *Store) UpdateRecovery(userID int64, recoveryJSON string) error {
	_, err := s.db.Exec(`UPDATE users SET totp_recovery=? WHERE id=?`, recoveryJSON, userID)
	return err
}

// UpdatePassword sets a new password hash for a user.
func (s *Store) UpdatePassword(userID int64, hash string) error {
	_, err := s.db.Exec(`UPDATE users SET password_hash=? WHERE id=?`, hash, userID)
	return err
}

// ---------------- Sessions ----------------

// SessionIdleTTL is the sliding inactivity window (PLAN §10.2). A session is
// invalidated this long after the last *user activity* ping (TouchSession),
// independent of — and shorter than — the 12h absolute lifetime. Background
// polling does NOT slide it; only genuine interaction does, so an abandoned tab
// (or a stolen cookie left idle) times out even while the UI keeps polling.
var SessionIdleTTL = 30 * time.Minute

// SetSessionIdleTTL sets the sliding inactivity window (F203).
//
// Called once at boot from config.Load, before the server accepts a request, so
// there is no read/write race with live traffic — the same shape as the process-
// wide egress policy configured beside it. A non-positive duration is ignored
// rather than applied: an idle window of zero would sign everyone out on their
// next click, and a misread env value must never do that.
func SetSessionIdleTTL(d time.Duration) {
	if d <= 0 {
		return
	}
	SessionIdleTTL = d
}

// idleDeadline returns the unix time a session goes idle. lastSeen==0 (a legacy
// row) falls back to createdAt so it isn't treated as instantly stale.
func idleDeadline(createdAt, lastSeen int64) int64 {
	base := lastSeen
	if base == 0 {
		base = createdAt
	}
	return base + int64(SessionIdleTTL.Seconds())
}

// CreateSession stores a session token for a user. last_seen starts at now —
// login counts as activity, so the idle window opens fully.
func (s *Store) CreateSession(token string, userID int64, ttl time.Duration) error {
	return s.CreateSessionFrom(token, userID, ttl, "", "")
}

// SessionKey is the form a session token is stored in: the SHA-256 of the
// cookie value, hex-encoded.
//
// API tokens have always been hashed at rest; sessions were not, so a copy of
// dockback.db handed whoever held it a set of live admin sessions — and that
// file travels: into an app-backup of the data volume, onto a NAS share, into
// whatever a support bundle sweeps up. The raw token now exists only in the
// cookie and in the request carrying it.
//
// Exported because the settings rows paired with a session (csrf:, stepup:) are
// keyed the same way, and those live in the API layer.
func SessionKey(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(sum[:])
}

// CreateSessionFrom is CreateSession plus where the sign-in came from (F202).
//
// Separate rather than a changed signature because most callers legitimately
// have no request to describe — a test, a restore of the control plane — and
// "unknown device" is an honest value for those, not a missing argument.
func (s *Store) CreateSessionFrom(token string, userID int64, ttl time.Duration, ip, userAgent string) error {
	n := now()
	_, err := s.db.Exec(`INSERT INTO sessions(token, user_id, created_at, expires_at, last_seen, ip, user_agent) VALUES(?,?,?,?,?,?,?)`,
		SessionKey(token), userID, n, time.Now().Add(ttl).Unix(), n, ip, userAgent)
	return err
}

// SessionDevice is one signed-in session as the owner sees it (F202).
//
// Key is the STORED session key (SessionKey of the cookie value), not the
// cookie value itself — that is never read back out of the database. It still
// carries `json:"-"`: the API layer derives a short public id from it and never
// emits it.
type SessionDevice struct {
	Key       string `json:"-"`
	CreatedAt int64  `json:"created_at"`
	LastSeen  int64  `json:"last_seen"`
	ExpiresAt int64  `json:"expires_at"`
	IP        string `json:"ip"`
	UserAgent string `json:"user_agent"`
}

// ListUserSessions returns a user's live sessions, newest first. Expired and
// idled-out rows are filtered out rather than shown as devices that are still
// signed in — the sweep deletes them shortly anyway, and a list that overstates
// who is logged in is worse than one that is briefly short.
func (s *Store) ListUserSessions(userID int64) ([]*SessionDevice, error) {
	rows, err := s.db.Query(
		`SELECT token, created_at, last_seen, expires_at, ip, user_agent FROM sessions WHERE user_id=? ORDER BY last_seen DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*SessionDevice{}
	nowUnix := time.Now().Unix()
	for rows.Next() {
		d := &SessionDevice{}
		if err := rows.Scan(&d.Key, &d.CreatedAt, &d.LastSeen, &d.ExpiresAt, &d.IP, &d.UserAgent); err != nil {
			return nil, err
		}
		if nowUnix > d.ExpiresAt || nowUnix > idleDeadline(d.CreatedAt, d.LastSeen) {
			continue
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SessionUser returns the user id for a valid session token. A session is
// invalid (and deleted) once it passes EITHER the absolute expiry OR the sliding
// idle window. This is enforced on every request — including background polls —
// but polls do not extend the window (only TouchSession does), so an idle
// session reliably dies even with a dashboard tab open (PLAN §10.2).
func (s *Store) SessionUser(token string) (int64, string, error) {
	var uid, exp, created, seen int64
	var username string
	err := s.db.QueryRow(`SELECT s.user_id, s.expires_at, s.created_at, s.last_seen, u.username
		FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token=?`, SessionKey(token)).
		Scan(&uid, &exp, &created, &seen, &username)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrNotFound
	}
	if err != nil {
		return 0, "", err
	}
	nowUnix := time.Now().Unix()
	if nowUnix > exp || nowUnix > idleDeadline(created, seen) {
		s.DeleteSession(token)
		return 0, "", ErrNotFound
	}
	return uid, username, nil
}

// TouchSession slides the idle window by recording user activity (now) as
// last_seen, but never past the absolute expiry and only while the session is
// still valid. Returns ErrNotFound for an expired/idle/unknown token so a dead
// session can't be revived by a ping.
func (s *Store) TouchSession(token string) error {
	var exp, created, seen int64
	err := s.db.QueryRow(`SELECT expires_at, created_at, last_seen FROM sessions WHERE token=?`, SessionKey(token)).
		Scan(&exp, &created, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	nowUnix := time.Now().Unix()
	if nowUnix > exp || nowUnix > idleDeadline(created, seen) {
		s.DeleteSession(token)
		return ErrNotFound
	}
	_, err = s.db.Exec(`UPDATE sessions SET last_seen=? WHERE token=?`, nowUnix, SessionKey(token))
	return err
}

// TouchSessionFrom is TouchSession that also records the address the session is
// currently being used from (F202), so a laptop that moved between networks
// shows where it is now rather than where it first signed in.
//
// A blank ip leaves the recorded one alone: an unknown address must never
// overwrite a known one.
func (s *Store) TouchSessionFrom(token, ip string) error {
	if err := s.TouchSession(token); err != nil {
		return err
	}
	if strings.TrimSpace(ip) == "" {
		return nil
	}
	_, err := s.db.Exec(`UPDATE sessions SET ip=? WHERE token=?`, ip, SessionKey(token))
	return err
}

// SweepExpiredSessions deletes every session past its absolute expiry or idle
// window and drops the paired csrf: settings entry, returning how many were
// removed. Called periodically so dead rows don't accumulate (they are also
// pruned lazily on access). (PLAN §10.2)
func (s *Store) SweepExpiredSessions() (int, error) {
	rows, err := s.db.Query(`SELECT token, expires_at, created_at, last_seen FROM sessions`)
	if err != nil {
		return 0, err
	}
	nowUnix := time.Now().Unix()
	var doomed []string
	for rows.Next() {
		var tok string
		var exp, created, seen int64
		if err := rows.Scan(&tok, &exp, &created, &seen); err != nil {
			rows.Close()
			return 0, err
		}
		if nowUnix > exp || nowUnix > idleDeadline(created, seen) {
			doomed = append(doomed, tok)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, key := range doomed {
		// These rows are read straight out of the table, so they are already the
		// stored key — the csrf: and stepup: entries are keyed the same way.
		s.DeleteSetting("csrf:" + key)
		s.DeleteSetting("stepup:" + key)
		s.DeleteSessionByKey(key)
	}
	return len(doomed), nil
}

// ClearSessions removes every session plus its paired csrf and step-up entries.
// Called once at startup so a container restart / rebuild / update forces all
// users to sign in again (sessions never survive a restart).
func (s *Store) ClearSessions() error {
	if _, err := s.db.Exec(`DELETE FROM sessions`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM settings WHERE key LIKE 'csrf:%'`); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM settings WHERE key LIKE 'stepup:%'`)
	return err
}

// DeleteSession removes a session (logout). Takes the RAW cookie value.
func (s *Store) DeleteSession(token string) error {
	return s.DeleteSessionByKey(SessionKey(token))
}

// DeleteSessionByKey removes a session identified by its STORED key — for the
// paths that read keys out of the database (the expiry sweep, revoking another
// device) and never see the cookie value at all.
func (s *Store) DeleteSessionByKey(key string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token=?`, key)
	return err
}

// APIToken is a scoped automation token (F45). The plaintext value is NEVER
// stored or returned after creation — only its SHA-256 hash (in the hash column,
// not exposed on this struct).
type APIToken struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Scopes    string `json:"scopes"` // comma-separated: read|backup|metrics
	CreatedAt int64  `json:"created_at"`
	LastUsed  int64  `json:"last_used"`
	ExpiresAt int64  `json:"expires_at"` // F65: unix seconds; 0 = never expires
	// AllowedCIDRs pins the token to source networks, comma-separated (F201).
	// Empty = usable from any address, which is what every token minted before
	// this existed was promised.
	AllowedCIDRs string `json:"allowed_cidrs"`
}

// CreateAPIToken stores a new token by its hash (never the plaintext).
// expiresAt is a unix timestamp after which auth rejects the token (F65);
// 0 means the token never expires.
func (s *Store) CreateAPIToken(id, name, hash, scopes string, expiresAt int64, allowedCIDRs string) error {
	_, err := s.db.Exec(`INSERT INTO api_tokens(id, name, hash, scopes, created_at, last_used, expires_at, allowed_cidrs) VALUES(?,?,?,?,?,0,?,?)`,
		id, name, hash, scopes, now(), expiresAt, allowedCIDRs)
	return err
}

// ListAPITokens returns every token's metadata (name/scopes/timestamps) — never
// the hash — newest first.
func (s *Store) ListAPITokens() ([]*APIToken, error) {
	rows, err := s.db.Query(`SELECT id, name, scopes, created_at, last_used, expires_at, allowed_cidrs FROM api_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIToken
	for rows.Next() {
		t := &APIToken{}
		if err := rows.Scan(&t.ID, &t.Name, &t.Scopes, &t.CreatedAt, &t.LastUsed, &t.ExpiresAt, &t.AllowedCIDRs); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteAPIToken revokes a token by id.
func (s *Store) DeleteAPIToken(id string) error {
	_, err := s.db.Exec(`DELETE FROM api_tokens WHERE id=?`, id)
	return err
}

// TouchAPIToken records that a token was just used (last-used tracking).
func (s *Store) TouchAPIToken(id string) error {
	_, err := s.db.Exec(`UPDATE api_tokens SET last_used=? WHERE id=?`, now(), id)
	return err
}

// GetAPITokenByHash looks a token up by the exact SHA-256 hash of its plaintext
// value — an indexed equality on a 256-bit hash of a 256-bit secret, so there is
// no useful timing side-channel to exploit. Returns ErrNotFound when unknown.
func (s *Store) GetAPITokenByHash(hash string) (*APIToken, error) {
	t := &APIToken{}
	err := s.db.QueryRow(`SELECT id, name, scopes, created_at, last_used, expires_at, allowed_cidrs FROM api_tokens WHERE hash=?`, hash).
		Scan(&t.ID, &t.Name, &t.Scopes, &t.CreatedAt, &t.LastUsed, &t.ExpiresAt, &t.AllowedCIDRs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// Alert is one persisted operational alert (F46).
type Alert struct {
	ID       int64  `json:"id"`
	TS       int64  `json:"ts"`
	Kind     string `json:"kind"`
	Severity string `json:"severity"` // warning | critical
	Title    string `json:"title"`
	Message  string `json:"message"`
	Dedup    string `json:"dedup"`
	Acked    bool   `json:"acked"`
}

// InsertAlert records an alert row (F46). The id/ts on the passed struct are set.
func (s *Store) InsertAlert(a *Alert) error {
	res, err := s.db.Exec(`INSERT INTO alerts(ts, kind, severity, title, message, dedup, acked) VALUES(?,?,?,?,?,?,0)`,
		a.TS, a.Kind, a.Severity, a.Title, a.Message, a.Dedup)
	if err != nil {
		return err
	}
	a.ID, _ = res.LastInsertId()
	return nil
}

// ListAlerts returns alerts newest-first, optionally only unacknowledged, paged.
func (s *Store) ListAlerts(onlyUnacked bool, limit, offset int) ([]*Alert, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT id, ts, kind, severity, title, message, dedup, acked FROM alerts`
	if onlyUnacked {
		q += ` WHERE acked=0`
	}
	q += ` ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?`
	rows, err := s.db.Query(q, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Alert
	for rows.Next() {
		a := &Alert{}
		var acked int
		if err := rows.Scan(&a.ID, &a.TS, &a.Kind, &a.Severity, &a.Title, &a.Message, &a.Dedup, &acked); err != nil {
			return nil, err
		}
		a.Acked = acked != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountUnacked returns the number of unacknowledged alerts (all are warning/critical).
func (s *Store) CountUnacked() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM alerts WHERE acked=0`).Scan(&n)
	return n, err
}

// AckAlert marks one alert acknowledged.
func (s *Store) AckAlert(id int64) error {
	_, err := s.db.Exec(`UPDATE alerts SET acked=1 WHERE id=?`, id)
	return err
}

// AckAllAlerts marks every alert acknowledged.
func (s *Store) AckAllAlerts() error {
	_, err := s.db.Exec(`UPDATE alerts SET acked=1 WHERE acked=0`)
	return err
}

// LastAlertForDedup returns the most recent alert with the given dedup key, or nil
// (ErrNotFound is treated as "none" by the caller). Used to decide whether a
// throttled, still-firing condition should resurface once acknowledged (F46).
func (s *Store) LastAlertForDedup(dedup string) (*Alert, error) {
	a := &Alert{}
	var acked int
	err := s.db.QueryRow(`SELECT id, ts, kind, severity, title, message, dedup, acked FROM alerts WHERE dedup=? ORDER BY ts DESC, id DESC LIMIT 1`, dedup).
		Scan(&a.ID, &a.TS, &a.Kind, &a.Severity, &a.Title, &a.Message, &a.Dedup, &acked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.Acked = acked != 0
	return a, nil
}

// PruneAlerts keeps only the newest `keep` alerts (F46), so the inbox stays bounded.
func (s *Store) PruneAlerts(keep int) error {
	if keep <= 0 {
		keep = 2000
	}
	_, err := s.db.Exec(`DELETE FROM alerts WHERE id NOT IN (SELECT id FROM alerts ORDER BY ts DESC, id DESC LIMIT ?)`, keep)
	return err
}

// CountAPITokensWithScope counts tokens whose scope set contains scope (F45) — used
// to decide whether /metrics has been locked to a metrics token.
func (s *Store) CountAPITokensWithScope(scope string) (int, error) {
	rows, err := s.db.Query(`SELECT scopes FROM api_tokens`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var scopes string
		if err := rows.Scan(&scopes); err != nil {
			return 0, err
		}
		for _, sc := range strings.Split(scopes, ",") {
			if strings.TrimSpace(sc) == scope {
				n++
				break
			}
		}
	}
	return n, rows.Err()
}

// SessionInfo returns a valid session's absolute expiry, its sliding idle
// deadline (both unix), and whether it has already used its one allowed
// extension. Expired or idle sessions are deleted → ErrNotFound.
func (s *Store) SessionInfo(token string) (expiresAt, idleExpiresAt int64, extended bool, err error) {
	var ext int
	var created, seen int64
	err = s.db.QueryRow(`SELECT expires_at, created_at, last_seen, extended FROM sessions WHERE token=?`, SessionKey(token)).
		Scan(&expiresAt, &created, &seen, &ext)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, ErrNotFound
	}
	if err != nil {
		return 0, 0, false, err
	}
	idle := idleDeadline(created, seen)
	if nowUnix := time.Now().Unix(); nowUnix > expiresAt || nowUnix > idle {
		s.DeleteSession(token)
		return 0, 0, false, ErrNotFound
	}
	return expiresAt, idle, ext != 0, nil
}

// ExtendSession sets a new expiry (unix) and marks the session as extended (only
// one extension is permitted per session).
func (s *Store) ExtendSession(token string, expiresAt int64) error {
	_, err := s.db.Exec(`UPDATE sessions SET expires_at=?, extended=1 WHERE token=?`, expiresAt, SessionKey(token))
	return err
}

// ListUserSessionKeys returns every STORED session key for a user, so a password
// change can revoke them all and drop their paired CSRF entries (§10.1).
func (s *Store) ListUserSessionKeys(userID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT token FROM sessions WHERE user_id=?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---------------- Nodes ----------------

// Node is a managed Docker host (PLAN §2.14, §4.13).
type Node struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Cluster   string `json:"cluster"`
	Transport string `json:"transport"`
	Address   string `json:"address"`
	Status    string `json:"status"`
	LastSeen  int64  `json:"last_seen"`
	CreatedAt int64  `json:"created_at"`
	SecretEnc []byte `json:"-"`
}

// UpsertNode inserts or updates a node.
func (s *Store) UpsertNode(n *Node) error {
	if n.CreatedAt == 0 {
		n.CreatedAt = now()
	}
	if n.Cluster == "" {
		n.Cluster = "default"
	}
	_, err := s.db.Exec(`
		INSERT INTO nodes(id, name, cluster, transport, address, secret_enc, status, last_seen, created_at)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name, cluster=excluded.cluster, transport=excluded.transport,
			address=excluded.address, secret_enc=excluded.secret_enc`,
		n.ID, n.Name, n.Cluster, n.Transport, n.Address, n.SecretEnc, n.Status, n.LastSeen, n.CreatedAt)
	return err
}

// SetNodeStatus updates a node's reachability status and last-seen timestamp.
func (s *Store) SetNodeStatus(id, status string) error {
	_, err := s.db.Exec(`UPDATE nodes SET status=?, last_seen=? WHERE id=?`, status, now(), id)
	return err
}

// ListNodes returns all managed nodes. Soft-deleted (tombstoned) nodes are
// excluded, so a "forgotten" node disappears everywhere — lists, scheduler,
// retention, coverage — the instant it is deleted.
func (s *Store) ListNodes() ([]*Node, error) {
	rows, err := s.db.Query(`SELECT id, name, cluster, transport, address, secret_enc, status, last_seen, created_at FROM nodes WHERE deleted_at=0 ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Node{}
	for rows.Next() {
		n := &Node{}
		if err := rows.Scan(&n.ID, &n.Name, &n.Cluster, &n.Transport, &n.Address, &n.SecretEnc, &n.Status, &n.LastSeen, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// GetNode fetches a node by id.
func (s *Store) GetNode(id string) (*Node, error) {
	n := &Node{}
	err := s.db.QueryRow(`SELECT id, name, cluster, transport, address, secret_enc, status, last_seen, created_at FROM nodes WHERE id=?`, id).
		Scan(&n.ID, &n.Name, &n.Cluster, &n.Transport, &n.Address, &n.SecretEnc, &n.Status, &n.LastSeen, &n.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

// DeleteNode permanently removes a node row (hard delete). Used by the tombstone
// purge sweep once the undo window has elapsed; the interactive "forget node"
// path uses SoftDeleteNode so the removal is reversible within a short window.
func (s *Store) DeleteNode(id string) error {
	_, err := s.db.Exec(`DELETE FROM nodes WHERE id=?`, id)
	return err
}

// SoftDeleteNode tombstones a node: it is hidden from ListNodes immediately and
// durably (survives a refresh/restart), but its row + sealed secret are retained
// so RestoreNode can bring it back within the undo window (never touches the
// host, PLAN §5.4).
func (s *Store) SoftDeleteNode(id string) error {
	_, err := s.db.Exec(`UPDATE nodes SET deleted_at=? WHERE id=?`, now(), id)
	return err
}

// RestoreNode clears a node's tombstone (Undo of a "forget node").
func (s *Store) RestoreNode(id string) error {
	_, err := s.db.Exec(`UPDATE nodes SET deleted_at=0 WHERE id=?`, id)
	return err
}

// PurgeNodeData removes every per-node and per-container setting and policy
// override tied to a node — used when a forgotten node is finally purged, so no
// stored preference, credential-derived, or override data survives it. Backups
// and their artifacts (which also touch external storage) and schedule targets
// are handled at the API layer.
func (s *Store) PurgeNodeData(nodeID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Policy overrides: the node scope itself + every container scope under it.
	if _, err := tx.Exec(`DELETE FROM policy_overrides WHERE scope=? OR scope LIKE ?`,
		NodeScope(nodeID), "container:"+nodeID+"/%"); err != nil {
		return err
	}
	// Per-node / per-container settings across every subsystem that keys by node
	// (pause mode, remembered backup options, hooks, export profiles, mount
	// selections, and missing-target bookkeeping).
	for _, like := range []string{
		"pause:" + nodeID + ":%",
		"backupopts:" + nodeID + ":%",
		"hooks:" + nodeID + ":%",
		"export:" + nodeID + ":%",
		"mounts." + nodeID + ".%",
		"schedule.missing_since:" + nodeID + "/%",
	} {
		if _, err := tx.Exec(`DELETE FROM settings WHERE key LIKE ?`, like); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListDeletedNodesBefore returns the ids of nodes tombstoned at or before cutoff
// — the purge sweep hard-deletes these once their undo window has elapsed.
func (s *Store) ListDeletedNodesBefore(cutoff int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM nodes WHERE deleted_at>0 AND deleted_at<=?`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ---------------- SSH host-key pinning (F1 / PLAN §3) ----------------

// HostKey is a pinned SSH host key for a node (trust-on-first-use). The key is
// PUBLIC; it identifies the host so a later connect with a different key (a
// possible man-in-the-middle) is refused.
type HostKey struct {
	NodeID      string `json:"node_id"`
	HostPort    string `json:"hostport"`
	KeyType     string `json:"key_type"`
	KeyB64      string `json:"key_b64"`
	Fingerprint string `json:"fingerprint"`
	PinnedAt    int64  `json:"pinned_at"`
}

// GetHostKey returns a node's pinned host key and whether one exists.
func (s *Store) GetHostKey(nodeID string) (HostKey, bool) {
	hk := HostKey{NodeID: nodeID}
	err := s.db.QueryRow(`SELECT hostport, key_type, key_b64, fingerprint, pinned_at FROM ssh_hostkeys WHERE node_id=?`, nodeID).
		Scan(&hk.HostPort, &hk.KeyType, &hk.KeyB64, &hk.Fingerprint, &hk.PinnedAt)
	if err != nil {
		return HostKey{}, false
	}
	return hk, true
}

// SetHostKey pins (or re-pins) a node's host key.
func (s *Store) SetHostKey(hk HostKey) error {
	if hk.PinnedAt == 0 {
		hk.PinnedAt = now()
	}
	_, err := s.db.Exec(`
		INSERT INTO ssh_hostkeys(node_id, hostport, key_type, key_b64, fingerprint, pinned_at)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(node_id) DO UPDATE SET
		  hostport=excluded.hostport, key_type=excluded.key_type, key_b64=excluded.key_b64,
		  fingerprint=excluded.fingerprint, pinned_at=excluded.pinned_at`,
		hk.NodeID, hk.HostPort, hk.KeyType, hk.KeyB64, hk.Fingerprint, hk.PinnedAt)
	return err
}

// DeleteHostKey drops a node's pinned host key (on node delete, or an operator
// reset after a legitimate host re-key).
func (s *Store) DeleteHostKey(nodeID string) error {
	_, err := s.db.Exec(`DELETE FROM ssh_hostkeys WHERE node_id=?`, nodeID)
	return err
}

// ---------------- Node inventory cache (PLAN §4.13) ----------------

// NodeInventoryRow is one node's persisted inventory snapshot.
type NodeInventoryRow struct {
	NodeID    string
	Payload   []byte // JSON {summary, containers, stacks}
	Reachable bool
	Error     string
	UpdatedAt int64
}

// SaveNodeInventory upserts a node's cached inventory snapshot.
func (s *Store) SaveNodeInventory(nodeID string, payload []byte, reachable bool, errMsg string, updatedAt int64) error {
	reach := 0
	if reachable {
		reach = 1
	}
	_, err := s.db.Exec(`
		INSERT INTO node_inventory(node_id, payload, reachable, error, updated_at)
		VALUES(?,?,?,?,?)
		ON CONFLICT(node_id) DO UPDATE SET
		  payload=excluded.payload, reachable=excluded.reachable,
		  error=excluded.error, updated_at=excluded.updated_at`,
		nodeID, payload, reach, errMsg, updatedAt)
	return err
}

// ListNodeInventories returns every persisted node inventory snapshot (loaded
// into the in-memory cache at startup so the dashboard is instant and survives
// restarts).
func (s *Store) ListNodeInventories() ([]*NodeInventoryRow, error) {
	rows, err := s.db.Query(`SELECT node_id, payload, reachable, error, updated_at FROM node_inventory`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NodeInventoryRow{}
	for rows.Next() {
		r := &NodeInventoryRow{}
		var reachable int
		if err := rows.Scan(&r.NodeID, &r.Payload, &reachable, &r.Error, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Reachable = reachable != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteNodeInventory drops a node's cached inventory (on node removal).
func (s *Store) DeleteNodeInventory(nodeID string) error {
	_, err := s.db.Exec(`DELETE FROM node_inventory WHERE node_id=?`, nodeID)
	return err
}

// ---------------- Policy overrides (PLAN §4.13 per-node inheritance) --------

// NodeScope builds the policy_overrides key for a node.
func NodeScope(nodeID string) string { return "node:" + nodeID }

// ContainerScope builds the policy_overrides key for a single container on a
// node, keyed by container NAME (the stable target identifier used across
// backups) so an override survives container recreation (PLAN §4.13 finer
// scopes). Container-scope overrides take precedence over node then global.
func ContainerScope(nodeID, name string) string { return "container:" + nodeID + "/" + name }

// PolicyOverride is a per-scope override of the global backup policy. Each group
// of fields is gated by an Override* flag; when false those fields inherit the
// global policy. This mirrors the Settings UX (override destinations and/or
// retention independently).
type PolicyOverride struct {
	OverrideDestinations bool     `json:"override_destinations"`
	Destinations         []string `json:"destinations"`
	OverrideRetention    bool     `json:"override_retention"`
	Generations          int      `json:"generations"`
	KeepDaily            int      `json:"keep_daily"`
	KeepWeekly           int      `json:"keep_weekly"`
	KeepMonthly          int      `json:"keep_monthly"`
	KeepYearly           int      `json:"keep_yearly"`
	Autoprune            bool     `json:"autoprune"`
	// OverrideFrequency throttles how often a scheduled run backs this scope up:
	// when set, the scheduler skips it if its last successful backup is younger
	// than MinIntervalHours (0 = every run). Container-scope only in the UI, but
	// stored generically (PLAN §4.2 granular control).
	OverrideFrequency bool `json:"override_frequency"`
	MinIntervalHours  int  `json:"min_interval_hours"`
}

// GetPolicyOverride returns the override for a scope and whether one exists.
// Destinations is always a non-nil slice so callers/JSON never see null (a nil
// slice would crash .length/.filter on the client).
func (s *Store) GetPolicyOverride(scope string) (PolicyOverride, bool) {
	empty := PolicyOverride{Destinations: []string{}}
	var raw string
	err := s.db.QueryRow(`SELECT json FROM policy_overrides WHERE scope=?`, scope).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return empty, false
	}
	if err != nil {
		return empty, false
	}
	var ov PolicyOverride
	if json.Unmarshal([]byte(raw), &ov) != nil {
		return empty, false
	}
	if ov.Destinations == nil {
		ov.Destinations = []string{}
	}
	return ov, true
}

// SetPolicyOverride upserts a scope's override. If the override carries no active
// flags it is deleted (so "inherit everything" leaves no row).
func (s *Store) SetPolicyOverride(scope string, ov PolicyOverride) error {
	if !ov.OverrideDestinations && !ov.OverrideRetention && !ov.OverrideFrequency {
		return s.DeletePolicyOverride(scope)
	}
	b, err := json.Marshal(ov)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO policy_overrides(scope, json) VALUES(?,?)
		ON CONFLICT(scope) DO UPDATE SET json=excluded.json`, scope, string(b))
	return err
}

// DeletePolicyOverride removes a scope's override (revert to inheriting global).
func (s *Store) DeletePolicyOverride(scope string) error {
	_, err := s.db.Exec(`DELETE FROM policy_overrides WHERE scope=?`, scope)
	return err
}

// ---------------- Node event counters (PLAN §5.4 dashboard) ----------------

// IncrNodeEvents records one event for a node: bumps the lifetime total and the
// per-day count, resetting `today` when the day rolls over. Atomic via UPSERT.
func (s *Store) IncrNodeEvents(nodeID, day string) error {
	_, err := s.db.Exec(`
		INSERT INTO node_events(node_id, total, today, day) VALUES(?, 1, 1, ?)
		ON CONFLICT(node_id) DO UPDATE SET
		  total = total + 1,
		  today = CASE WHEN day = excluded.day THEN today + 1 ELSE 1 END,
		  day = excluded.day`,
		nodeID, day)
	return err
}

// GetNodeEvents returns a node's lifetime total and the count for `day` (0 if the
// stored day differs, i.e. nothing yet today). Missing node => 0,0.
func (s *Store) GetNodeEvents(nodeID, day string) (total, today int, err error) {
	err = s.db.QueryRow(
		`SELECT total, CASE WHEN day=? THEN today ELSE 0 END FROM node_events WHERE node_id=?`,
		day, nodeID,
	).Scan(&total, &today)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	return total, today, err
}

// DeleteNodeEvents drops a node's event counters (on node removal).
func (s *Store) DeleteNodeEvents(nodeID string) error {
	_, err := s.db.Exec(`DELETE FROM node_events WHERE node_id=?`, nodeID)
	return err
}

// ---------------- Durable backup queue (PLAN §4.13/§9.10) ----------------

// QueuedJobRow is one persisted queued backup, restored on restart.
type QueuedJobRow struct {
	ID        string
	NodeID    string
	NodeName  string
	OptsJSON  string
	Priority  int
	Seq       int64
	NotBefore int64 // unix seconds
}

// SaveQueuedJob persists a queued backup so it survives a restart.
func (s *Store) SaveQueuedJob(r QueuedJobRow) error {
	_, err := s.db.Exec(`
		INSERT INTO queued_jobs(id, node_id, node_name, opts_json, priority, seq, not_before, created_at)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		  node_id=excluded.node_id, node_name=excluded.node_name, opts_json=excluded.opts_json,
		  priority=excluded.priority, seq=excluded.seq, not_before=excluded.not_before`,
		r.ID, r.NodeID, r.NodeName, r.OptsJSON, r.Priority, r.Seq, r.NotBefore, now())
	return err
}

// DeleteQueuedJob removes a queued job once it completes or is canceled.
func (s *Store) DeleteQueuedJob(id string) error {
	_, err := s.db.Exec(`DELETE FROM queued_jobs WHERE id=?`, id)
	return err
}

// ListQueuedJobs returns all persisted queued jobs (FIFO by seq) for resume.
func (s *Store) ListQueuedJobs() ([]*QueuedJobRow, error) {
	rows, err := s.db.Query(`SELECT id, node_id, node_name, opts_json, priority, seq, not_before
		FROM queued_jobs ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*QueuedJobRow{}
	for rows.Next() {
		r := &QueuedJobRow{}
		if err := rows.Scan(&r.ID, &r.NodeID, &r.NodeName, &r.OptsJSON, &r.Priority, &r.Seq, &r.NotBefore); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------- Backups ----------------

// Backup is one backup record (PLAN §4.12 manifest is the contract).
type Backup struct {
	ID         string `json:"id"`
	NodeID     string `json:"node_id"`
	Stack      string `json:"stack"`
	TargetName string `json:"target_name"`
	Status     string `json:"status"`
	Verified   string `json:"verified"`
	SizeBytes  int64  `json:"size_bytes"`
	// omitempty on the heavy string fields (perf Fix 8): slim list rows blank
	// them, and emitting `"manifest_json":""` × thousands of rows is pure
	// waste. Full rows carry values, so their JSON is unchanged.
	CipherSHA256     string `json:"cipher_sha256,omitempty"`
	StorageKey       string `json:"storage_key,omitempty"`
	ManifestJSON     string `json:"manifest_json,omitempty"`
	VerificationJSON string `json:"verification_json,omitempty"`
	LocationsJSON    string `json:"locations,omitempty"`
	Error            string `json:"error,omitempty"`
	CreatedAt        int64  `json:"created_at"`
	CompletedAt      int64  `json:"completed_at"`
	LastVerifiedAt   int64  `json:"last_verified_at"`  // last successful scrub/re-verify (PLAN §9.4)
	Pinned           bool   `json:"pinned"`            // F2: "keep forever" — exempt from all pruning
	Label            string `json:"label"`             // F2: user note (e.g. "pre-Immich-upgrade"), searchable
	DurationMs       int64  `json:"duration_ms"`       // F28: wall-clock run time (ms), for run-to-run drift detection
	Suspect          string `json:"suspect,omitempty"` // F69: reason this delta looked like a mass-change event ("" = clean)

	// KeyMismatch is computed (not stored): true when the backup's master-key
	// fingerprint differs from the current key, so the UI can flag "encrypted
	// with a key you no longer have" (PLAN §3.3). Populated by the API layer.
	KeyMismatch bool `json:"key_mismatch,omitempty"`

	// Confidence is the computed restore-confidence grade (F50): a single A–F
	// answer to "will this restore?" plus the signals that cost points. Computed by
	// the API layer from already-loaded row data (no Docker/network), never stored.
	Confidence *ConfidenceView `json:"confidence,omitempty"`

	// Summary is the compact per-row digest LIST responses carry instead of the
	// three raw JSON blobs (perf Fix 8) — everything the list/timeline UI
	// derives, pre-computed once server-side. Computed by the API layer for
	// ?slim=1 list requests, never stored; the full-row endpoint keeps the blobs.
	Summary *BackupSummary `json:"summary,omitempty"`

	// ChainDependents (F63) is how many live incremental backups depend on this
	// one as a chain ancestor. Computed by the full-row endpoint, never stored —
	// the drawer's "baseline of N deltas" chip and the delete guard's warning.
	ChainDependents int `json:"chain_dependents,omitempty"`
}

// BackupSummary is the list-row digest (perf Fix 8). Fields mirror exactly what
// the Backups page chips and the restore timeline read from the manifest and
// locations blobs.
type BackupSummary struct {
	Partial      bool         `json:"partial"`                 // any UNCOVERED skipped mount (F83 semantics)
	CoveredSkips int          `json:"covered_skips,omitempty"` // skips captured via another container (F83)
	CoveredBy    string       `json:"covered_by,omitempty"`    // first covering owner, for the chip label
	Incremental  bool         `json:"incremental,omitempty"`   // F61 delta
	ChainDepth   int          `json:"chain_depth,omitempty"`
	ImageBundled bool         `json:"image_bundled,omitempty"` // image.tar inside (air-gapped restore)
	DBCount      int          `json:"db_count,omitempty"`
	Image        string       `json:"image,omitempty"`        // for the timeline's version labels
	ImageDigest  string       `json:"image_digest,omitempty"` // for the timeline's version-bump flags
	Copies       []BackupCopy `json:"copies,omitempty"`
	// WriteOnly marks a backup sealed to an OFFLINE public key (F86): DockBack
	// cannot open it, so the list can flag that restoring it needs the private key
	// without the row carrying a manifest blob.
	WriteOnly bool `json:"write_only,omitempty"`
	// DBFallback marks a backup whose container was detected as a database but
	// whose dump tools were missing, so its data was captured as raw files (F103).
	// A boolean here, not the note: the list only needs to render a chip, and the
	// full wording lives in the manifest the drawer already fetches.
	DBFallback bool `json:"db_fallback,omitempty"`
}

// BackupCopy is one location's digest inside a BackupSummary.
type BackupCopy struct {
	Name      string `json:"name"`
	Type      string `json:"type,omitempty"`
	Status    string `json:"status,omitempty"` // "" ok | failed | deferred
	Detail    string `json:"detail,omitempty"`
	Immutable bool   `json:"immutable,omitempty"`
}

// ConfidenceView is the restore-confidence grade attached to a backup (F50).
type ConfidenceView struct {
	Grade   string   `json:"grade"`             // A | B | C | D | F
	Reasons []string `json:"reasons,omitempty"` // signals that lowered the grade (empty for A)
}

// CreateBackup inserts a new backup row (status pending/running).
func (s *Store) CreateBackup(b *Backup) error {
	if b.CreatedAt == 0 {
		b.CreatedAt = now()
	}
	if b.Verified == "" {
		b.Verified = "unverified"
	}
	_, err := s.db.Exec(`INSERT INTO backups
		(id, node_id, stack, target_name, status, verified, created_at) VALUES(?,?,?,?,?,?,?)`,
		b.ID, b.NodeID, b.Stack, b.TargetName, b.Status, b.Verified, b.CreatedAt)
	return err
}

// UpdateBackup persists mutable fields after work completes.
func (s *Store) UpdateBackup(b *Backup) error {
	if b.LocationsJSON == "" {
		b.LocationsJSON = "[]"
	}
	_, err := s.db.Exec(`UPDATE backups SET status=?, verified=?, size_bytes=?, cipher_sha256=?,
		storage_key=?, manifest_json=?, verification_json=?, locations=?, error=?, completed_at=?, last_verified_at=?, duration_ms=? WHERE id=?`,
		b.Status, b.Verified, b.SizeBytes, b.CipherSHA256, b.StorageKey, b.ManifestJSON,
		b.VerificationJSON, b.LocationsJSON, b.Error, b.CompletedAt, b.LastVerifiedAt, b.DurationMs, b.ID)
	return err
}

// UpdateBackupManifest replaces only a backup's manifest-of-record JSON — used by
// master-key rotation (F16) to persist a re-wrapped DEK + new key fingerprint
// without touching any other field of the row.
func (s *Store) UpdateBackupManifest(id, manifestJSON string) error {
	_, err := s.db.Exec(`UPDATE backups SET manifest_json=? WHERE id=?`, manifestJSON, id)
	return err
}

// SetVerification records a re-verification/scrub result (PLAN §9.4): updates the
// verified state, the report, and the last-verified timestamp without touching
// the rest of the backup record.
func (s *Store) SetVerification(id, verified, reportJSON string, at int64) error {
	_, err := s.db.Exec(`UPDATE backups SET verified=?, verification_json=?, last_verified_at=? WHERE id=?`,
		verified, reportJSON, at, id)
	return err
}

// OldestVerified returns the oldest successful re-verification time among
// successful backups (for the staleness metric, PLAN §9.4/§9.12). ok=false when
// nothing has been verified yet.
func (s *Store) OldestVerified() (at int64, ok bool) {
	var v sql.NullInt64
	err := s.db.QueryRow(`SELECT MIN(last_verified_at) FROM backups WHERE status='success' AND last_verified_at>0`).Scan(&v)
	if err != nil || !v.Valid {
		return 0, false
	}
	return v.Int64, true
}

// SetBackupPin marks/unmarks a backup as "keep forever" — exempt from all
// retention pruning (F2).
func (s *Store) SetBackupPin(id string, pinned bool) error {
	v := 0
	if pinned {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE backups SET pinned=? WHERE id=?`, v, id)
	return err
}

// SetBackupLabel sets a backup's free-text label (F2). An empty string clears it.
func (s *Store) SetBackupLabel(id, label string) error {
	_, err := s.db.Exec(`UPDATE backups SET label=? WHERE id=?`, label, id)
	return err
}

// SetBackupStatus updates just the status + error + completion of a backup.
func (s *Store) SetBackupStatus(id, status, errMsg string) error {
	_, err := s.db.Exec(`UPDATE backups SET status=?, error=?, completed_at=? WHERE id=?`,
		status, errMsg, time.Now().Unix(), id)
	return err
}

// SetBackupStorageKey repoints a backup row at a new storage key — the layout
// migration's commit step (F77), called only after every location holds a
// verified copy at the new key.
func (s *Store) SetBackupStorageKey(id, key string) error {
	_, err := s.db.Exec(`UPDATE backups SET storage_key=? WHERE id=?`, key, id)
	return err
}

// SetBackupSuspect stamps the tripwire reason on a backup row (F69) — the delta
// looked like a mass-change event. The run itself still succeeds.
func (s *Store) SetBackupSuspect(id, reason string) error {
	_, err := s.db.Exec(`UPDATE backups SET suspect=? WHERE id=?`, reason, id)
	return err
}

// ClearBackupSuspects resets the suspect flag on every row of a (node, target)
// after the operator reviewed the event (F69). Returns how many were cleared.
func (s *Store) ClearBackupSuspects(nodeID, target string) (int64, error) {
	res, err := s.db.Exec(`UPDATE backups SET suspect='' WHERE node_id=? AND target_name=? AND suspect<>''`, nodeID, target)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PurgeInterruptedBackups removes backups left "running" by an app restart/crash
// (and any prior rows already flagged 'interrupted by restart'). Such runs never
// produced a usable artifact, so they're discarded rather than kept as confusing
// FAILED entries that look like the target/node is broken. Returns how many were
// removed.
func (s *Store) PurgeInterruptedBackups() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM backups
		WHERE status='running' OR (status='failed' AND error='interrupted by restart')`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// GetBackup fetches one backup.
func (s *Store) GetBackup(id string) (*Backup, error) {
	b := &Backup{}
	var pin int
	err := s.db.QueryRow(`SELECT id, node_id, stack, target_name, status, verified, size_bytes,
		cipher_sha256, storage_key, manifest_json, verification_json, locations, error, created_at, completed_at, last_verified_at, pinned, label, duration_ms, suspect
		FROM backups WHERE id=?`, id).
		Scan(&b.ID, &b.NodeID, &b.Stack, &b.TargetName, &b.Status, &b.Verified, &b.SizeBytes,
			&b.CipherSHA256, &b.StorageKey, &b.ManifestJSON, &b.VerificationJSON, &b.LocationsJSON, &b.Error, &b.CreatedAt, &b.CompletedAt, &b.LastVerifiedAt, &pin, &b.Label, &b.DurationMs, &b.Suspect)
	b.Pinned = pin == 1
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

// ListBackups returns backups, optionally filtered by node, newest first.
// CountBackups returns the number of catalog entries (for the app-backup summary).
func (s *Store) CountBackups() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM backups`).Scan(&n)
	return n, err
}

// ProtectedTargets returns, for a node, each container target_name that has at
// least one SUCCESSFUL backup on record, mapped to the newest such backup's
// created_at. Used by the coverage / "unprotected containers" view (B2). Cheap:
// a single grouped query, no per-row scan of the whole catalog.
func (s *Store) ProtectedTargets(nodeID string) (map[string]int64, error) {
	rows, err := s.db.Query(
		`SELECT target_name, MAX(created_at) FROM backups WHERE node_id=? AND status='success' GROUP BY target_name`,
		nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]int64{}
	for rows.Next() {
		var name string
		var ts int64
		if err := rows.Scan(&name, &ts); err != nil {
			return nil, err
		}
		m[name] = ts
	}
	return m, rows.Err()
}

func (s *Store) ListBackups(nodeID string, limit int) ([]*Backup, error) {
	if limit <= 0 {
		limit = 100
	}
	var rows *sql.Rows
	var err error
	q := `SELECT id, node_id, stack, target_name, status, verified, size_bytes,
		cipher_sha256, storage_key, manifest_json, verification_json, locations, error, created_at, completed_at, last_verified_at, pinned, label, duration_ms, suspect
		FROM backups`
	if nodeID != "" {
		rows, err = s.db.Query(q+` WHERE node_id=? ORDER BY created_at DESC LIMIT ?`, nodeID, limit)
	} else {
		rows, err = s.db.Query(q+` ORDER BY created_at DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Backup{}
	for rows.Next() {
		b := &Backup{}
		var pin int
		if err := rows.Scan(&b.ID, &b.NodeID, &b.Stack, &b.TargetName, &b.Status, &b.Verified, &b.SizeBytes,
			&b.CipherSHA256, &b.StorageKey, &b.ManifestJSON, &b.VerificationJSON, &b.LocationsJSON, &b.Error, &b.CreatedAt, &b.CompletedAt, &b.LastVerifiedAt, &pin, &b.Label, &b.DurationMs, &b.Suspect); err != nil {
			return nil, err
		}
		b.Pinned = pin == 1
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListBackupsForTarget returns the newest `limit` backups of ONE container
// (node_id + target_name), newest first.
//
// This exists because "the newest N backups of this node, filtered by name in
// Go" is not the same query: on a node with several containers — especially one
// running a low-RPO critical database — a container's own backups fall outside
// the node-wide window, and the caller concludes it has none. The composite
// index idx_backups_target(node_id, target_name, status, created_at DESC) makes
// the correct query cheaper than the wrong one.
func (s *Store) ListBackupsForTarget(nodeID, target string, limit int) ([]*Backup, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id, node_id, stack, target_name, status, verified, size_bytes,
		cipher_sha256, storage_key, manifest_json, verification_json, locations, error, created_at, completed_at, last_verified_at, pinned, label, duration_ms, suspect
		FROM backups WHERE node_id=? AND target_name=? ORDER BY created_at DESC LIMIT ?`,
		nodeID, target, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Backup{}
	for rows.Next() {
		b := &Backup{}
		var pin int
		if err := rows.Scan(&b.ID, &b.NodeID, &b.Stack, &b.TargetName, &b.Status, &b.Verified, &b.SizeBytes,
			&b.CipherSHA256, &b.StorageKey, &b.ManifestJSON, &b.VerificationJSON, &b.LocationsJSON, &b.Error,
			&b.CreatedAt, &b.CompletedAt, &b.LastVerifiedAt, &pin, &b.Label, &b.DurationMs, &b.Suspect); err != nil {
			return nil, err
		}
		b.Pinned = pin == 1
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListBackupsForStack returns one compose project's backups on a node, newest
// first — the stack counterpart of ListBackupsForTarget, served by
// idx_backups_stack(node_id, stack, status, created_at DESC).
//
// The limit now bounds THAT STACK's history rather than the node's, which is
// also a correctness gain: a node with more backups than the old window could
// push a stack's rows out of it entirely, and the stack would read as having no
// backups at all.
func (s *Store) ListBackupsForStack(nodeID, stack string, limit int) ([]*Backup, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id, node_id, stack, target_name, status, verified, size_bytes,
		cipher_sha256, storage_key, manifest_json, verification_json, locations, error, created_at, completed_at, last_verified_at, pinned, label, duration_ms, suspect
		FROM backups WHERE node_id=? AND stack=? ORDER BY created_at DESC LIMIT ?`,
		nodeID, stack, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Backup{}
	for rows.Next() {
		b := &Backup{}
		var pin int
		if err := rows.Scan(&b.ID, &b.NodeID, &b.Stack, &b.TargetName, &b.Status, &b.Verified, &b.SizeBytes,
			&b.CipherSHA256, &b.StorageKey, &b.ManifestJSON, &b.VerificationJSON, &b.LocationsJSON, &b.Error,
			&b.CreatedAt, &b.CompletedAt, &b.LastVerifiedAt, &pin, &b.Label, &b.DurationMs, &b.Suspect); err != nil {
			return nil, err
		}
		b.Pinned = pin == 1
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListBackupSummaries is ListBackups minus the three JSON blob columns
// (manifest_json / verification_json / locations — those fields come back
// EMPTY) plus the other heavy text columns, for aggregate consumers that only
// read scalars (perf Fix 6: stats/insights/growth read 5,000 rows to compute
// counters — the blob projection made that ~8× more expensive than needed).
// Same ordering and filters as ListBackups; reuses the Backup struct.
func (s *Store) ListBackupSummaries(nodeID string, limit int) ([]*Backup, error) {
	if limit <= 0 {
		limit = 100
	}
	var rows *sql.Rows
	var err error
	q := `SELECT id, node_id, stack, target_name, status, verified, size_bytes,
		created_at, completed_at, pinned, label, duration_ms
		FROM backups`
	if nodeID != "" {
		rows, err = s.db.Query(q+` WHERE node_id=? ORDER BY created_at DESC LIMIT ?`, nodeID, limit)
	} else {
		rows, err = s.db.Query(q+` ORDER BY created_at DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Backup{}
	for rows.Next() {
		b := &Backup{}
		var pin int
		if err := rows.Scan(&b.ID, &b.NodeID, &b.Stack, &b.TargetName, &b.Status, &b.Verified, &b.SizeBytes,
			&b.CreatedAt, &b.CompletedAt, &pin, &b.Label, &b.DurationMs); err != nil {
			return nil, err
		}
		b.Pinned = pin == 1
		out = append(out, b)
	}
	return out, rows.Err()
}

// BackupParentMap returns id → chain-parent-id for a target's successful
// backups whose manifests record a parent (F61 incremental chains) — a
// json_extract micro-projection so retention can protect ancestor chains
// without materializing whole manifest blobs (perf Fix 7). Rows without a
// parent contribute nothing.
func (s *Store) BackupParentMap(nodeID, target string) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT id, json_extract(manifest_json,'$.parent')
		FROM backups WHERE node_id=? AND target_name=? AND status='success'`, nodeID, target)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pm := map[string]string{}
	for rows.Next() {
		var id string
		var parent sql.NullString
		if err := rows.Scan(&id, &parent); err != nil {
			return nil, err
		}
		if parent.Valid && parent.String != "" {
			pm[id] = parent.String
		}
	}
	return pm, rows.Err()
}

// ListBackupScrubInfo projects exactly what scrub-batch selection reads —
// id, node_id, status, locations, last_verified_at, created_at (perf Fix 7).
// Every other field is zero; the few selected backups are re-fetched in full
// before verification runs.
func (s *Store) ListBackupScrubInfo(limit int) ([]*Backup, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id, node_id, status, locations, last_verified_at, created_at
		FROM backups ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Backup{}
	for rows.Next() {
		b := &Backup{}
		if err := rows.Scan(&b.ID, &b.NodeID, &b.Status, &b.LocationsJSON, &b.LastVerifiedAt, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// LastSuccessfulBackupAt returns the created_at (unix) of the newest successful
// backup for a container (node + target name) and whether one exists. Used by
// the scheduler's per-container min-interval throttle (PLAN §4.2 granular control).
func (s *Store) LastSuccessfulBackupAt(nodeID, target string) (int64, bool) {
	var ts int64
	err := s.db.QueryRow(
		`SELECT created_at FROM backups WHERE node_id=? AND target_name=? AND status='success'
		 ORDER BY created_at DESC LIMIT 1`, nodeID, target).Scan(&ts)
	if err != nil {
		return 0, false
	}
	return ts, true
}

// BackupFilter parameterizes a paged/searched/filtered backup-history query
// (PLAN §4.13 "UI for volume"). Empty string fields are ignored; Limit<=0 means
// "no pagination" (all matching rows).
type BackupFilter struct {
	NodeID   string
	Query    string // substring match on target_name or stack
	Status   string // pending|running|success|failed
	Verified string // verified|unverified|failed
	Limit    int
	Offset   int
}

// ListBackupsPage returns a filtered, newest-first slice of backups plus the
// total number of matching rows (before pagination), so the UI can page through
// thousands of history rows without shipping them all (PLAN §4.13).
func (s *Store) ListBackupsPage(f BackupFilter) ([]*Backup, int, error) {
	var where []string
	var args []any
	if f.NodeID != "" {
		where = append(where, "node_id=?")
		args = append(args, f.NodeID)
	}
	if f.Status != "" {
		where = append(where, "status=?")
		args = append(args, f.Status)
	}
	if f.Verified != "" {
		where = append(where, "verified=?")
		args = append(args, f.Verified)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		where = append(where, `(target_name LIKE ? ESCAPE '\' OR stack LIKE ? ESCAPE '\' OR label LIKE ? ESCAPE '\')`)
		like := "%" + escapeLike(q) + "%"
		args = append(args, like, like, like)
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := s.db.QueryRow(`SELECT count(*) FROM backups`+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `SELECT id, node_id, stack, target_name, status, verified, size_bytes,
		cipher_sha256, storage_key, manifest_json, verification_json, locations, error, created_at, completed_at, last_verified_at, pinned, label, duration_ms, suspect
		FROM backups` + clause + ` ORDER BY created_at DESC`
	pargs := append([]any{}, args...)
	if f.Limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		pargs = append(pargs, f.Limit, f.Offset)
	}
	rows, err := s.db.Query(query, pargs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*Backup{}
	for rows.Next() {
		b := &Backup{}
		var pin int
		if err := rows.Scan(&b.ID, &b.NodeID, &b.Stack, &b.TargetName, &b.Status, &b.Verified, &b.SizeBytes,
			&b.CipherSHA256, &b.StorageKey, &b.ManifestJSON, &b.VerificationJSON, &b.LocationsJSON, &b.Error, &b.CreatedAt, &b.CompletedAt, &b.LastVerifiedAt, &pin, &b.Label, &b.DurationMs, &b.Suspect); err != nil {
			return nil, 0, err
		}
		b.Pinned = pin == 1
		out = append(out, b)
	}
	return out, total, rows.Err()
}

// escapeLike escapes LIKE wildcards so a user's search text is matched literally
// (the query uses ESCAPE '\').
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// LastSuccessfulByNode returns the newest successful backup time per node_id
// (unix seconds), for per-node metrics (PLAN §4.13 node-keyed metrics).
func (s *Store) LastSuccessfulByNode() (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT node_id, MAX(created_at) FROM backups WHERE status='success' GROUP BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var t int64
		if err := rows.Scan(&id, &t); err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, rows.Err()
}

// DeleteBackup removes a backup record (caller deletes the artifact separately).
func (s *Store) DeleteBackup(id string) error {
	// One transaction: a crash between these statements used to leave the catalog
	// row gone and its drill result and run log behind — orphans keyed to a
	// backup nothing can look up — or, in the other order, a row still listed for
	// an archive whose dependants were already removed. All three, or none.
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op once committed
	if _, err := tx.Exec(`DELETE FROM backups WHERE id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM restore_drills WHERE backup_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM run_logs WHERE backup_id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------------- Persisted run logs (F8) ----------------

// RunLogLine is one persisted log line of a backup/restore/mirror run.
type RunLogLine struct {
	Seq   int64  `json:"seq"`
	TS    int64  `json:"ts"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

// AppendRunLog appends one line to a run's persisted log. The sequence number is
// assigned atomically as MAX(seq)+1 for this backup id in a single statement, so
// concurrent appenders (e.g. an interleaved mirror) can't collide on the PK.
func (s *Store) AppendRunLog(backupID, level, msg string) error {
	_, err := s.db.Exec(
		`INSERT INTO run_logs(backup_id, seq, ts, level, msg)
		 VALUES(?, (SELECT COALESCE(MAX(seq),0)+1 FROM run_logs WHERE backup_id=?), ?, ?, ?)`,
		backupID, backupID, now(), level, msg)
	return err
}

// GetRunLog returns a run's persisted log lines in order (oldest first).
func (s *Store) GetRunLog(backupID string) ([]RunLogLine, error) {
	rows, err := s.db.Query(`SELECT seq, ts, level, msg FROM run_logs WHERE backup_id=? ORDER BY seq`, backupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunLogLine{}
	for rows.Next() {
		var l RunLogLine
		if err := rows.Scan(&l.Seq, &l.TS, &l.Level, &l.Msg); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// TrimRunLog keeps only the newest maxLines lines of a run's log (by seq),
// bounding unbounded growth for very chatty runs.
func (s *Store) TrimRunLog(backupID string, maxLines int) error {
	if maxLines <= 0 {
		return nil
	}
	_, err := s.db.Exec(
		`DELETE FROM run_logs WHERE backup_id=? AND seq <= (
			SELECT COALESCE(MAX(seq),0)-? FROM run_logs WHERE backup_id=?)`,
		backupID, maxLines, backupID)
	return err
}

// ---------------- Audit (PLAN §3.6) ----------------

// SetAuditChainer installs the audit hash-chainer (F68), called once at boot.
// fn receives the previous row's chain value plus this row's fields in the fixed
// order (ts, actor, action, target, detail) and returns the new chain value.
func (s *Store) SetAuditChainer(fn func(prev string, fields ...string) string) {
	s.auditMu.Lock()
	s.auditChain = fn
	s.auditMu.Unlock()
}

// Audit appends an immutable audit entry. With a chainer installed (F68) the row
// also stores chain = fn(prev_chain, ts, actor, action, target, detail), linking
// it cryptographically to its predecessor — read-prev + insert run inside one
// transaction under auditMu so concurrent audits can't fork the chain.
func (s *Store) Audit(actor, action, target, detail string) error {
	s.auditMu.Lock()
	fn := s.auditChain
	if fn == nil {
		s.auditMu.Unlock()
		_, err := s.db.Exec(`INSERT INTO audit(ts, actor, action, target, detail) VALUES(?,?,?,?,?)`,
			now(), actor, action, target, detail)
		return err
	}
	defer s.auditMu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var prev string
	if err := tx.QueryRow(`SELECT COALESCE((SELECT chain FROM audit ORDER BY id DESC LIMIT 1), '')`).Scan(&prev); err != nil {
		return err
	}
	ts := now()
	chain := fn(prev, strconv.FormatInt(ts, 10), actor, action, target, detail)
	if _, err := tx.Exec(`INSERT INTO audit(ts, actor, action, target, detail, chain) VALUES(?,?,?,?,?,?)`,
		ts, actor, action, target, detail, chain); err != nil {
		return err
	}
	return tx.Commit()
}

// MaxAuditID returns the newest audit row id (0 for an empty table) — the
// chain-start anchor recorded when the chainer is first installed (F68).
func (s *Store) MaxAuditID() (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(id),0) FROM audit`).Scan(&id)
	return id, err
}

// AuditChainRow is one audit row projected for chain verification (F68) — the
// stored fields in insertion order plus the row's recorded chain value.
type AuditChainRow struct {
	ID     int64
	TS     int64
	Actor  string
	Action string
	Target string
	Detail string
	Chain  string
}

// auditPageSize is how many audit rows are held at once by the walks below.
// The audit table is append-only and unbounded, so anything that reads "all of
// it" grows with the deployment's age until it meets the container's memory
// limit — during an export or an integrity check, which are exactly the moments
// an operator needs to succeed.
const auditPageSize = 1000

// WalkAuditSince visits every row after afterID in id order, a page at a time.
// fn returning false stops the walk early (a broken chain link needs no more).
func (s *Store) WalkAuditSince(afterID int64, fn func(*AuditChainRow) bool) error {
	for {
		rows, err := s.db.Query(`SELECT id, ts, actor, action, target, detail, chain
			FROM audit WHERE id > ? ORDER BY id ASC LIMIT ?`, afterID, auditPageSize)
		if err != nil {
			return err
		}
		n := 0
		stopped := false
		for rows.Next() {
			r := &AuditChainRow{}
			if err := rows.Scan(&r.ID, &r.TS, &r.Actor, &r.Action, &r.Target, &r.Detail, &r.Chain); err != nil {
				rows.Close()
				return err
			}
			n++
			afterID = r.ID
			if !fn(r) {
				stopped = true
				break
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if stopped || n < auditPageSize {
			return nil
		}
	}
}

// WalkAuditFiltered visits matching rows newest-first, a page at a time, so an
// export of a years-old trail streams instead of being assembled in memory.
func (s *Store) WalkAuditFiltered(f AuditFilter, fn func(*AuditEntry) bool) error {
	where, args := auditWhere(f)
	before := int64(0) // 0 = start at the newest
	for {
		clause := where
		pargs := append([]any{}, args...)
		if before > 0 {
			if clause == "" {
				clause = " WHERE id < ?"
			} else {
				clause += " AND id < ?"
			}
			pargs = append(pargs, before)
		}
		rows, err := s.db.Query(`SELECT id, ts, actor, action, target, detail FROM audit`+
			clause+` ORDER BY id DESC LIMIT ?`, append(pargs, auditPageSize)...)
		if err != nil {
			return err
		}
		n := 0
		stopped := false
		for rows.Next() {
			var id int64
			e := &AuditEntry{}
			if err := rows.Scan(&id, &e.TS, &e.Actor, &e.Action, &e.Target, &e.Detail); err != nil {
				rows.Close()
				return err
			}
			n++
			before = id
			if !fn(e) {
				stopped = true
				break
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if stopped || n < auditPageSize {
			return nil
		}
	}
}

// AuditChainAt returns the chain value stored on row id ("" when the row does
// not exist or predates the feature) — the verification walk's starting prev.
func (s *Store) AuditChainAt(id int64) (string, error) {
	if id <= 0 {
		return "", nil
	}
	var chain string
	err := s.db.QueryRow(`SELECT chain FROM audit WHERE id=?`, id).Scan(&chain)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return chain, err
}

// AuditEntry is one audit row.
type AuditEntry struct {
	TS     int64  `json:"ts"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target"`
	Detail string `json:"detail"`
}

// ListAudit returns recent audit entries.
func (s *Store) ListAudit(limit int) ([]*AuditEntry, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT ts, actor, action, target, detail FROM audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AuditEntry{}
	for rows.Next() {
		e := &AuditEntry{}
		if err := rows.Scan(&e.TS, &e.Actor, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountAudit returns the total number of audit entries.
func (s *Store) CountAudit() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM audit`).Scan(&n)
	return n, err
}

// AuditFilter parameterizes a paged/searched audit query (F7). Empty fields are
// ignored; Limit<=0 means "no pagination" (all matching rows — used by export).
type AuditFilter struct {
	Query  string // substring match across actor/action/target/detail
	From   int64  // unix seconds, inclusive lower bound (0 = none)
	To     int64  // unix seconds, inclusive upper bound (0 = none)
	Limit  int
	Offset int
}

// ListAuditPage returns a filtered, newest-first slice of audit entries plus the
// total number of matching rows (before pagination), so the audit page can browse
// the whole (unbounded) log and an export can stream every matching row (F7).
// Mirrors ListBackupsPage: bound parameters + escapeLike, count then page.
// auditWhere renders one filter into a WHERE clause and its arguments, so the
// paged listing and the streaming walk select exactly the same rows.
func auditWhere(f AuditFilter) (string, []any) {
	var where []string
	var args []any
	if f.From > 0 {
		where = append(where, "ts>=?")
		args = append(args, f.From)
	}
	if f.To > 0 {
		where = append(where, "ts<=?")
		args = append(args, f.To)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		where = append(where, `(actor LIKE ? ESCAPE '\' OR action LIKE ? ESCAPE '\' OR target LIKE ? ESCAPE '\' OR detail LIKE ? ESCAPE '\')`)
		like := "%" + escapeLike(q) + "%"
		args = append(args, like, like, like, like)
	}
	if len(where) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(where, " AND "), args
}

func (s *Store) ListAuditPage(f AuditFilter) ([]*AuditEntry, int, error) {
	clause, args := auditWhere(f)

	var total int
	if err := s.db.QueryRow(`SELECT count(*) FROM audit`+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `SELECT ts, actor, action, target, detail FROM audit` + clause + ` ORDER BY id DESC`
	pargs := append([]any{}, args...)
	if f.Limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		pargs = append(pargs, f.Limit, f.Offset)
	}
	rows, err := s.db.Query(query, pargs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*AuditEntry{}
	for rows.Next() {
		e := &AuditEntry{}
		if err := rows.Scan(&e.TS, &e.Actor, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// ---------------- Named backup schedules (F6) ----------------
// Several independent schedules, each with its own frequency/targets, replace the
// former single global schedule. The friendly fields (name, kind, time, targets,
// …) live as JSON in OptionsJSON; Cron is the computed 5-field spec; Enabled,
// LastRun and NextRun get their own columns for cheap scheduler queries. The
// legacy node_id/target_name columns are unused here (written '').

// Schedule is one persisted named backup schedule row.
type Schedule struct {
	ID          string
	OptionsJSON string // full friendly schedule JSON ({name,kind,time,targets,…})
	Cron        string // computed 5-field cron spec
	Enabled     bool
	LastRun     int64 // unix seconds; 0 = never (scheduler baselines on first tick)
	NextRun     int64 // unix seconds; informational
}

// UpsertSchedule inserts or replaces a schedule row by id.
func (s *Store) UpsertSchedule(sc *Schedule) error {
	en := 0
	if sc.Enabled {
		en = 1
	}
	_, err := s.db.Exec(`INSERT INTO schedules(id, node_id, target_name, cron, options_json, enabled, last_run, next_run)
		VALUES(?,'','',?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET cron=excluded.cron, options_json=excluded.options_json,
			enabled=excluded.enabled, last_run=excluded.last_run, next_run=excluded.next_run`,
		sc.ID, sc.Cron, sc.OptionsJSON, en, sc.LastRun, sc.NextRun)
	return err
}

// ListSchedules returns all schedules, oldest first (stable UI order by id).
func (s *Store) ListSchedules() ([]*Schedule, error) {
	rows, err := s.db.Query(`SELECT id, cron, options_json, enabled, last_run, next_run FROM schedules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Schedule{}
	for rows.Next() {
		sc := &Schedule{}
		var en int
		if err := rows.Scan(&sc.ID, &sc.Cron, &sc.OptionsJSON, &en, &sc.LastRun, &sc.NextRun); err != nil {
			return nil, err
		}
		sc.Enabled = en == 1
		out = append(out, sc)
	}
	return out, rows.Err()
}

// GetSchedule fetches one schedule by id.
func (s *Store) GetSchedule(id string) (*Schedule, error) {
	sc := &Schedule{}
	var en int
	err := s.db.QueryRow(`SELECT id, cron, options_json, enabled, last_run, next_run FROM schedules WHERE id=?`, id).
		Scan(&sc.ID, &sc.Cron, &sc.OptionsJSON, &en, &sc.LastRun, &sc.NextRun)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	sc.Enabled = en == 1
	return sc, err
}

// DeleteSchedule removes a schedule by id.
func (s *Store) DeleteSchedule(id string) error {
	_, err := s.db.Exec(`DELETE FROM schedules WHERE id=?`, id)
	return err
}

// ---------------- Destinations (external backup storage, PLAN §4.9) ----------------

// Destination is an external backup storage endpoint.
type Destination struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Enabled   bool   `json:"enabled"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"created_at"`
	ConfigEnc []byte `json:"-"`
}

// DestSample is one point in a destination's capacity history (PLAN §9.13).
type DestSample struct {
	Ts    int64  `json:"ts"`
	Total uint64 `json:"total_bytes"`
	Used  uint64 `json:"used_bytes"`
}

// RecordDestSample stores today's capacity reading for a destination (one row
// per day, latest wins) and prunes readings older than ~180 days so the table
// stays small (PLAN §9.13 storage-capacity forecasting).
func (s *Store) RecordDestSample(destID string, total, used uint64, ts int64) error {
	day := ts / 86400
	_, err := s.db.Exec(`INSERT INTO destination_samples(dest_id, day, ts, total_bytes, used_bytes) VALUES(?,?,?,?,?)
		ON CONFLICT(dest_id, day) DO UPDATE SET ts=excluded.ts, total_bytes=excluded.total_bytes, used_bytes=excluded.used_bytes`,
		destID, day, ts, int64(total), int64(used))
	if err == nil {
		s.db.Exec(`DELETE FROM destination_samples WHERE dest_id=? AND ts < ?`, destID, ts-180*24*3600)
	}
	return err
}

// DestSamples returns a destination's capacity history since sinceTs, oldest
// first (for trend/forecast computation).
func (s *Store) DestSamples(destID string, sinceTs int64) ([]DestSample, error) {
	rows, err := s.db.Query(`SELECT ts, total_bytes, used_bytes FROM destination_samples WHERE dest_id=? AND ts>=? ORDER BY ts`, destID, sinceTs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DestSample
	for rows.Next() {
		var d DestSample
		var total, used int64
		if err := rows.Scan(&d.Ts, &total, &used); err != nil {
			return nil, err
		}
		d.Total, d.Used = uint64(total), uint64(used)
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteDestSamples removes a destination's capacity history (called when the
// destination itself is deleted).
func (s *Store) DeleteDestSamples(destID string) error {
	_, err := s.db.Exec(`DELETE FROM destination_samples WHERE dest_id=?`, destID)
	return err
}

// ---------------- Node connection-health history (F40) ----------------

// NodeHealthRow is one reachability transition for a node.
type NodeHealthRow struct {
	Ts        int64  `json:"ts"`
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
}

// RecordNodeHealth appends a reachability TRANSITION for a node — a row is written
// only when the reachable state differs from the node's newest recorded row (F40),
// so the periodic ~30s inventory refresh never churns one row per tick. The first
// observation of a node always records a baseline row. The per-node timestamp is
// kept strictly increasing so two transitions in the same second don't collide on
// the (node_id, ts) primary key.
func (s *Store) RecordNodeHealth(nodeID string, reachable bool, errStr string) error {
	var lastTs, lastReach int64
	haveLast := false
	err := s.db.QueryRow(`SELECT ts, reachable FROM node_health WHERE node_id=? ORDER BY ts DESC LIMIT 1`, nodeID).
		Scan(&lastTs, &lastReach)
	switch {
	case err == nil:
		haveLast = true
	case errors.Is(err, sql.ErrNoRows):
		haveLast = false
	default:
		return err
	}
	r := int64(0)
	if reachable {
		r = 1
	}
	if haveLast && lastReach == r {
		return nil // no transition — don't record a row per refresh tick
	}
	ts := now()
	if haveLast && ts <= lastTs {
		ts = lastTs + 1 // keep ts strictly increasing so same-second flips don't clash
	}
	_, err = s.db.Exec(`INSERT INTO node_health(node_id, ts, reachable, error) VALUES(?,?,?,?)`,
		nodeID, ts, r, errStr)
	return err
}

// NodeHealth returns a node's reachability transitions with ts >= since, NEWEST
// first (since=0 returns them all). Bounded by the 90-day prune.
func (s *Store) NodeHealth(nodeID string, since int64) ([]NodeHealthRow, error) {
	rows, err := s.db.Query(`SELECT ts, reachable, error FROM node_health WHERE node_id=? AND ts>=? ORDER BY ts DESC`, nodeID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NodeHealthRow{}
	for rows.Next() {
		var h NodeHealthRow
		var r int64
		if err := rows.Scan(&h.Ts, &r, &h.Error); err != nil {
			return nil, err
		}
		h.Reachable = r == 1
		out = append(out, h)
	}
	return out, rows.Err()
}

// PruneNodeHealth removes transition rows older than `before` (unix seconds).
func (s *Store) PruneNodeHealth(before int64) error {
	_, err := s.db.Exec(`DELETE FROM node_health WHERE ts < ?`, before)
	return err
}

// CreateDestination inserts a destination.
func (s *Store) CreateDestination(d *Destination) error {
	if d.CreatedAt == 0 {
		d.CreatedAt = now()
	}
	en := 0
	if d.Enabled {
		en = 1
	}
	_, err := s.db.Exec(`INSERT INTO destinations(id, name, type, config_enc, enabled, status, created_at)
		VALUES(?,?,?,?,?,?,?)`, d.ID, d.Name, d.Type, d.ConfigEnc, en, d.Status, d.CreatedAt)
	return err
}

// ListDestinations returns all destinations.
func (s *Store) ListDestinations() ([]*Destination, error) {
	rows, err := s.db.Query(`SELECT id, name, type, config_enc, enabled, status, created_at FROM destinations ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Destination{}
	for rows.Next() {
		d := &Destination{}
		var en int
		if err := rows.Scan(&d.ID, &d.Name, &d.Type, &d.ConfigEnc, &en, &d.Status, &d.CreatedAt); err != nil {
			return nil, err
		}
		d.Enabled = en == 1
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDestination fetches one destination by id.
func (s *Store) GetDestination(id string) (*Destination, error) {
	d := &Destination{}
	var en int
	err := s.db.QueryRow(`SELECT id, name, type, config_enc, enabled, status, created_at FROM destinations WHERE id=?`, id).
		Scan(&d.ID, &d.Name, &d.Type, &d.ConfigEnc, &en, &d.Status, &d.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	d.Enabled = en == 1
	return d, err
}

// UpdateDestination edits an existing destination's name, type, encrypted config
// and status in place (PLAN §3.8). Credentials stay sealed; the caller re-encrypts.
func (s *Store) UpdateDestination(d *Destination) error {
	en := 0
	if d.Enabled {
		en = 1
	}
	_, err := s.db.Exec(`UPDATE destinations SET name=?, type=?, config_enc=?, enabled=?, status=? WHERE id=?`,
		d.Name, d.Type, d.ConfigEnc, en, d.Status, d.ID)
	return err
}

// SetDestinationStatus updates a destination's status.
func (s *Store) SetDestinationStatus(id, status string) error {
	_, err := s.db.Exec(`UPDATE destinations SET status=? WHERE id=?`, status, id)
	return err
}

// DeleteDestination removes a destination.
func (s *Store) DeleteDestination(id string) error {
	_, err := s.db.Exec(`DELETE FROM destinations WHERE id=?`, id)
	return err
}

// ---------------- App-backup destinations (PLAN §6.5/§9.3) ----------------
// Separate from container destinations; same Destination shape.

func (s *Store) CreateAppDestination(d *Destination) error {
	if d.CreatedAt == 0 {
		d.CreatedAt = now()
	}
	en := 0
	if d.Enabled {
		en = 1
	}
	_, err := s.db.Exec(`INSERT INTO app_destinations(id, name, type, config_enc, enabled, status, created_at)
		VALUES(?,?,?,?,?,?,?)`, d.ID, d.Name, d.Type, d.ConfigEnc, en, d.Status, d.CreatedAt)
	return err
}

func (s *Store) ListAppDestinations() ([]*Destination, error) {
	rows, err := s.db.Query(`SELECT id, name, type, config_enc, enabled, status, created_at FROM app_destinations ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Destination{}
	for rows.Next() {
		d := &Destination{}
		var en int
		if err := rows.Scan(&d.ID, &d.Name, &d.Type, &d.ConfigEnc, &en, &d.Status, &d.CreatedAt); err != nil {
			return nil, err
		}
		d.Enabled = en == 1
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) GetAppDestination(id string) (*Destination, error) {
	d := &Destination{}
	var en int
	err := s.db.QueryRow(`SELECT id, name, type, config_enc, enabled, status, created_at FROM app_destinations WHERE id=?`, id).
		Scan(&d.ID, &d.Name, &d.Type, &d.ConfigEnc, &en, &d.Status, &d.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	d.Enabled = en == 1
	return d, err
}

// UpdateAppDestinationConfig replaces an app-backup destination's sealed
// credentials — used by key rotation to re-seal them onto the new master key (F72).
func (s *Store) UpdateAppDestinationConfig(id string, configEnc []byte) error {
	_, err := s.db.Exec(`UPDATE app_destinations SET config_enc=? WHERE id=?`, configEnc, id)
	return err
}

func (s *Store) SetAppDestinationEnabled(id string, enabled bool) error {
	en := 0
	if enabled {
		en = 1
	}
	_, err := s.db.Exec(`UPDATE app_destinations SET enabled=? WHERE id=?`, en, id)
	return err
}

func (s *Store) DeleteAppDestination(id string) error {
	_, err := s.db.Exec(`DELETE FROM app_destinations WHERE id=?`, id)
	return err
}

// ---------------- Settings ----------------

// SetSetting stores a key/value setting.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// SetSettings writes several settings as one unit. A group of values that only
// makes sense together — a retention policy, a schedule and the baseline it is
// measured from — must not be left half-applied by a crash between two writes.
func (s *Store) SetSettings(values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO settings(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for key, value := range values {
		if _, err := stmt.Exec(key, value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---------------- Restore drills (PLAN §9.4) ----------------

// Drill is the outcome of the most recent restore drill for one backup (per
// node+container) — the same granularity as verification.
type Drill struct {
	BackupID string `json:"backup_id"`
	OK       bool   `json:"ok"`
	Detail   string `json:"detail"`
	RanAt    int64  `json:"ran_at"`
}

// UpsertDrill records the latest drill result for a backup (one row per backup —
// a drill is a point-in-time proof, only the newest matters).
func (s *Store) UpsertDrill(backupID string, ok bool, detail string, ranAt int64) error {
	okv := 0
	if ok {
		okv = 1
	}
	_, err := s.db.Exec(`INSERT INTO restore_drills(backup_id,ok,detail,ran_at) VALUES(?,?,?,?)
		ON CONFLICT(backup_id) DO UPDATE SET ok=excluded.ok, detail=excluded.detail, ran_at=excluded.ran_at`,
		backupID, okv, detail, ranAt)
	return err
}

// GetDrill returns the last drill for a backup, or ErrNotFound if never drilled.
func (s *Store) GetDrill(backupID string) (*Drill, error) {
	d := &Drill{BackupID: backupID}
	var okv int
	err := s.db.QueryRow(`SELECT ok, detail, ran_at FROM restore_drills WHERE backup_id=?`, backupID).
		Scan(&okv, &d.Detail, &d.RanAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	d.OK = okv == 1
	return d, err
}

// ListDrills returns every recorded drill result (for the UI map + metrics).
func (s *Store) ListDrills() ([]*Drill, error) {
	rows, err := s.db.Query(`SELECT backup_id, ok, detail, ran_at FROM restore_drills`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Drill
	for rows.Next() {
		d := &Drill{}
		var okv int
		if err := rows.Scan(&d.BackupID, &okv, &d.Detail, &d.RanAt); err != nil {
			return nil, err
		}
		d.OK = okv == 1
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------------- Pilot-light standby rehearsals (F62) ----------------

// Standby is a container's scheduled cross-node restore rehearsal: proof that its
// newest verified backup restores + boots on a designated fallback node. Keyed by
// the SOURCE (NodeID, Target); StandbyNode is the fallback it rehearses onto.
type Standby struct {
	NodeID       string `json:"node_id"`
	Target       string `json:"target"`
	StandbyNode  string `json:"standby_node"`
	IntervalDays int    `json:"interval_days"`
	LastRun      int64  `json:"last_run"`
	LastOK       bool   `json:"last_ok"`
	LastDetail   string `json:"last_detail"`
	BootMs       int64  `json:"boot_ms"`
}

// SetStandby creates or updates a container's standby config, preserving any
// previously recorded result (last_run/ok/detail/boot_ms) so re-saving the config
// doesn't erase the last proof.
func (s *Store) SetStandby(nodeID, target, standbyNode string, intervalDays int) error {
	_, err := s.db.Exec(`INSERT INTO standby(node_id,target,standby_node,interval_days) VALUES(?,?,?,?)
		ON CONFLICT(node_id,target) DO UPDATE SET standby_node=excluded.standby_node, interval_days=excluded.interval_days`,
		nodeID, target, standbyNode, intervalDays)
	return err
}

// DeleteStandby removes a container's standby config (and its recorded result).
func (s *Store) DeleteStandby(nodeID, target string) error {
	_, err := s.db.Exec(`DELETE FROM standby WHERE node_id=? AND target=?`, nodeID, target)
	return err
}

// GetStandby returns a container's standby config, or ErrNotFound.
func (s *Store) GetStandby(nodeID, target string) (*Standby, error) {
	sb := &Standby{NodeID: nodeID, Target: target}
	var okv int
	err := s.db.QueryRow(`SELECT standby_node, interval_days, last_run, last_ok, last_detail, boot_ms
		FROM standby WHERE node_id=? AND target=?`, nodeID, target).
		Scan(&sb.StandbyNode, &sb.IntervalDays, &sb.LastRun, &okv, &sb.LastDetail, &sb.BootMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	sb.LastOK = okv == 1
	return sb, err
}

// ListStandby returns every configured standby rehearsal (for the loop + runbook).
func (s *Store) ListStandby() ([]*Standby, error) {
	rows, err := s.db.Query(`SELECT node_id, target, standby_node, interval_days, last_run, last_ok, last_detail, boot_ms FROM standby`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Standby
	for rows.Next() {
		sb := &Standby{}
		var okv int
		if err := rows.Scan(&sb.NodeID, &sb.Target, &sb.StandbyNode, &sb.IntervalDays, &sb.LastRun, &okv, &sb.LastDetail, &sb.BootMs); err != nil {
			return nil, err
		}
		sb.LastOK = okv == 1
		out = append(out, sb)
	}
	return out, rows.Err()
}

// RecordStandbyResult stores the outcome of a rehearsal (a point-in-time proof —
// only the newest matters). A no-op INSERT-side keeps it robust if the config row
// was deleted mid-run.
func (s *Store) RecordStandbyResult(nodeID, target string, ok bool, bootMs int64, detail string, ranAt int64) error {
	okv := 0
	if ok {
		okv = 1
	}
	_, err := s.db.Exec(`UPDATE standby SET last_run=?, last_ok=?, boot_ms=?, last_detail=? WHERE node_id=? AND target=?`,
		ranAt, okv, bootMs, detail, nodeID, target)
	return err
}

// GetSetting reads a setting, returning def if absent.
func (s *Store) GetSetting(key, def string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	return v, err
}

// DeleteSetting removes a setting (idempotent). Used to clear transient state
// such as login lockout records on a successful sign-in.
func (s *Store) DeleteSetting(key string) error {
	_, err := s.db.Exec(`DELETE FROM settings WHERE key=?`, key)
	return err
}

// SettingKeysWithPrefix lists setting keys under a namespace, for sweeping
// short-lived records (F199 export tickets). Keys only — values may be sealed
// secrets, and a caller that wants one can ask for it by name.
func (s *Store) SettingKeysWithPrefix(prefix string) ([]string, error) {
	rows, err := s.db.Query(`SELECT key FROM settings WHERE key LIKE ? || '%'`, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ConsumeSetting reads a setting and deletes it in ONE transaction, reporting
// whether it existed (F199).
//
// Written for single-use export tickets, where "read it, then delete it" is not
// good enough: two requests arriving together can both complete the read before
// either deletes, and a ticket that is supposed to authorise one download
// authorises two. Wrapping both statements in a transaction makes the redemption
// indivisible — the second caller finds nothing.
//
// The delete happens whether or not the caller goes on to accept the value, and
// that is deliberate: a ticket presented with the wrong target or an expired
// deadline is spent, not returned to the pool for another guess.
func (s *Store) ConsumeSetting(key string) (string, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()

	var v string
	err = tx.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if _, err := tx.Exec(`DELETE FROM settings WHERE key=?`, key); err != nil {
		return "", false, err
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	return v, true, nil
}

// ---------------- Volume-sidecar image pin (F88) ----------------

// sidecarPinKey namespaces a node's pinned sidecar image in the settings table.
// No schema change: the pin is a short opaque string ("<ref>|<digest>"), and it
// is PUBLIC information — a content address, not a secret.
func sidecarPinKey(nodeID string) string { return "sidecar.pin." + nodeID }

// SetSidecarPin records the sidecar image a node is pinned to. An empty value
// clears the pin, so the next use re-pins whatever is present (the "Re-pin"
// action).
func (s *Store) SetSidecarPin(nodeID, pin string) error {
	return s.SetSetting(sidecarPinKey(nodeID), pin)
}

// GetSidecarPin returns a node's pinned sidecar image, and whether one exists.
func (s *Store) GetSidecarPin(nodeID string) (string, bool) {
	v, err := s.GetSetting(sidecarPinKey(nodeID), "")
	if err != nil || v == "" {
		return "", false
	}
	return v, true
}
