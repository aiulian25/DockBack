package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
)

// Post-import completeness verification.
//
// Classifying the client's error output (see dbimport.go) catches a NOISY
// failure. It cannot catch a SILENT one: if the dump stream simply ends early,
// psql reaches EOF, prints nothing unusual and exits 0 — and you are left with
// a database holding all its rows but only part of its schema. That is a real
// production failure mode (a Paperless database restored with 27 of its 72
// primary keys and none of its 115 foreign keys, reported as a successful
// restore; the missing constraints only surfaced later as
// "there is no unique constraint matching given keys for referenced table …").
//
// So the import is verified two ways, both cheap:
//
//  1. SOURCE — the dump is tallied AS IT STREAMS (no extra pass, no buffering):
//     how many primary/foreign keys it declares, and whether it ended with
//     pg_dump's completion trailer. A missing trailer means the stream was cut.
//  2. TARGET — after the import, the restored cluster is asked how many
//     primary/foreign keys it actually has. Fewer than the dump declared means
//     the import did not fully apply.
//
// Only a SHORTFALL fails: the target legitimately holds more (other databases
// in the same cluster), so `actual > expected` is fine.

// pgDumpConstraintRe matches pg_dump's own emission of a key constraint, e.g.
//
//	ADD CONSTRAINT documents_workflowtrigger_pkey PRIMARY KEY (id);
//
// Anchored and strict so a COPY data line can't be mistaken for one.
var pgDumpConstraintRe = regexp.MustCompile(`^\s*ADD CONSTRAINT \S+ (PRIMARY KEY|FOREIGN KEY)\b`)

// mysqlDumpTableRe matches mysqldump's table definitions. Anchored at line start
// and requiring the backquoted name mysqldump always emits, so a row of INSERT
// data that happens to contain the words cannot be miscounted as a table.
var mysqlDumpTableRe = regexp.MustCompile("^\\s*CREATE TABLE (IF NOT EXISTS )?`")

// mysqlDumpInsertRe matches an INSERT statement. Note what this is NOT: a row
// count. mysqldump writes EXTENDED inserts by default, so one statement carries
// many rows. See DumpExpect.Rows.
var mysqlDumpInsertRe = regexp.MustCompile("^\\s*INSERT INTO `")

// DumpExpect is what a dump DECLARES, tallied while it streams past.
type DumpExpect struct {
	PrimaryKeys int
	ForeignKeys int
	Complete    bool  // pg_dump's "…dump complete" trailer was seen
	Bytes       int64 // how much of the dump we actually streamed
	// HeaderSeen means pg_dump's opening banner went past. Paired with Complete
	// it is what distinguishes "this is a pg_dump that was CUT SHORT" from "this
	// isn't a pg_dump at all" — without it, a dump truncated BEFORE its first
	// constraint looks identical to a MySQL dump or a binary archive (F87).
	HeaderSeen bool
	// SHA256 of the dump bytes exactly as written. Recorded at capture so the
	// stored dump can be re-checked later without decrypting it twice, and so
	// bit-rot in storage is caught by a scrub rather than at restore.
	SHA256 string

	// --- MySQL (F99) ---

	// Tables is how many CREATE TABLE statements the dump declares. This is the
	// MySQL equivalent of the Postgres constraint count: exact, cheap on both
	// sides, and a truncated dump declares fewer of them.
	Tables int
	// Rows counts INSERT STATEMENTS, which is deliberately NOT a row count —
	// mysqldump writes extended inserts, so one statement carries many rows. It
	// is recorded for the log line only and is NEVER a pass/fail gate: comparing
	// it against the server would fail restores that worked perfectly.
	Rows int64

	// --- MongoDB / Redis (F99) ---
	//
	// These engines dump a BINARY stream, so nothing structural can be tallied
	// from the bytes as they pass. Their expectations are instead queried from the
	// live source at capture time and carried in the manifest.
	Collections int
	Keys        int64
	// VolatileKeys is how many of Keys carried a TTL at capture (F150).
	//
	// Redis deletes already-expired keys when it LOADS an RDB, so a snapshot
	// restored later legitimately comes back short by however many of its keys
	// have since expired. Recording how many COULD expire turns "fewer than
	// expected" from a guess into an arithmetic fact: anything below
	// Keys-VolatileKeys is real loss, anything between that and Keys is a TTL
	// doing its job.
	//
	// Zero on a backup taken before this was recorded, which the verdict reads as
	// "unknown" rather than "none volatile" — see redisImportVerdict.
	VolatileKeys int64
}

// Truncated reports a dump that began as a pg_dump and did not finish.
//
// This is the capture-time gate: storing such a dump is storing a backup that
// cannot restore, and the only honest thing to do is fail the backup now, while
// the source is still there, rather than discover it during a recovery.
func (e DumpExpect) Truncated() bool {
	return (e.HeaderSeen || e.PrimaryKeys > 0) && !e.Complete
}

// maxScanLine bounds how much of a single line is kept for matching. COPY data
// lines can be enormous; our patterns are short and anchored at line start, so
// keeping the head is sufficient and the memory stays bounded.
const maxScanLine = 4096

// DumpTally accumulates a dump's structural fingerprint as its bytes go past.
//
// It is an io.Writer because the two callers move data in opposite directions:
// CAPTURE writes the dump to a file (F87) and RESTORE reads it into the import.
// One implementation serves both — a shared tally with a thin reader wrapper —
// so the numbers recorded at capture and the numbers checked at restore can
// never be computed by two subtly different pieces of code.
//
// It alters nothing and withholds nothing: every byte reaches its destination.
type DumpTally struct {
	partial []byte
	exp     DumpExpect
	sum     hash.Hash
	flushed bool
	// engine selects which dialect's structure to look for. Empty behaves as
	// Postgres, so every pre-F99 caller is unchanged.
	engine string
}

// NewDumpTally returns a tally ready to receive a Postgres dump stream.
func NewDumpTally() *DumpTally { return NewDumpTallyFor("postgres") }

// NewDumpTallyFor returns a tally that looks for the given engine's structure.
// An engine with no text structure to scan (mongodb, redis) still gets the size
// and checksum, which is all its binary stream can honestly offer.
func NewDumpTallyFor(engine string) *DumpTally {
	return &DumpTally{sum: sha256.New(), engine: engine}
}

// Write consumes a chunk of the dump. Never returns a short write or an error —
// the tally must never be the reason a dump fails to reach its destination.
func (d *DumpTally) Write(p []byte) (int, error) {
	if len(p) > 0 {
		d.exp.Bytes += int64(len(p))
		d.sum.Write(p)
		d.scan(p)
	}
	return len(p), nil
}

// scan splits the chunk into lines, carrying an incomplete tail to the next write.
func (d *DumpTally) scan(b []byte) {
	for {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			if len(d.partial) < maxScanLine {
				room := maxScanLine - len(d.partial)
				if room > len(b) {
					room = len(b)
				}
				d.partial = append(d.partial, b[:room]...)
			}
			return
		}
		line := b[:i]
		if len(d.partial) > 0 {
			line = append(d.partial, line...)
			d.partial = d.partial[:0]
		}
		d.line(line)
		d.partial = nil
		b = b[i+1:]
	}
}

func (d *DumpTally) line(b []byte) {
	if len(b) > maxScanLine {
		b = b[:maxScanLine]
	}
	s := string(b)
	switch d.engine {
	case "mysql":
		mysqlDumpScan(s, &d.exp)
	case "mongodb", "redis":
		// Binary streams: size and checksum only. Scanning them for text would
		// find coincidences, not structure.
	default:
		pgDumpScan(s, &d.exp)
	}
}

// mysqlDumpScan tallies one line of a mysqldump stream (F99).
//
// mysqldump's completion trailer is "-- Dump completed on <date>", which is the
// same role pg_dump's marker plays: its absence means the stream was cut. The
// header is mysqldump's own version banner.
func mysqlDumpScan(s string, e *DumpExpect) {
	if mysqlDumpTableRe.MatchString(s) {
		e.Tables++
		return
	}
	if mysqlDumpInsertRe.MatchString(s) {
		e.Rows++
		return
	}
	if strings.HasPrefix(strings.TrimSpace(s), "-- Dump completed") {
		e.Complete = true
		return
	}
	// "-- MySQL dump 10.13  Distrib …" / "-- MariaDB dump 10.19 …"
	if t := strings.TrimSpace(s); strings.HasPrefix(t, "--") &&
		(strings.Contains(t, "MySQL dump") || strings.Contains(t, "MariaDB dump")) {
		e.HeaderSeen = true
	}
}

// pgDumpScan tallies one line of a pg_dump/pg_dumpall stream.
func pgDumpScan(s string, e *DumpExpect) {
	if m := pgDumpConstraintRe.FindStringSubmatch(s); m != nil {
		if m[1] == "PRIMARY KEY" {
			e.PrimaryKeys++
		} else {
			e.ForeignKeys++
		}
		return
	}
	// pg_dump:    "-- PostgreSQL database dump complete"
	// pg_dumpall: "-- PostgreSQL database cluster dump complete"
	// The opening banner is the same line without "complete".
	if strings.Contains(s, "PostgreSQL database") && strings.Contains(s, "dump") {
		if strings.Contains(s, "dump complete") {
			e.Complete = true
		} else {
			e.HeaderSeen = true
		}
	}
}

// Expect returns the tally, flushing any final line that had no trailing newline
// and finalizing the checksum. Safe to call more than once.
func (d *DumpTally) Expect() DumpExpect {
	if !d.flushed {
		if len(d.partial) > 0 {
			d.line(d.partial)
			d.partial = nil
		}
		d.flushed = true
	}
	d.exp.SHA256 = hex.EncodeToString(d.sum.Sum(nil))
	return d.exp
}

// dumpScanner is the READ-side wrapper: it passes a dump through unchanged while
// the shared tally counts what goes by. Used by the restore path, where the dump
// is being read into the database client.
type dumpScanner struct {
	r io.Reader
	t *DumpTally
}

func newDumpScanner(r io.Reader) *dumpScanner { return newDumpScannerFor("postgres", r) }

// newDumpScannerFor reads a dump of a specific engine's dialect (F99).
func newDumpScannerFor(engine string, r io.Reader) *dumpScanner {
	return &dumpScanner{r: r, t: NewDumpTallyFor(engine)}
}

func (d *dumpScanner) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	if n > 0 {
		_, _ = d.t.Write(p[:n])
	}
	return n, err
}

// Expect returns the tally observed so far.
func (d *dumpScanner) Expect() DumpExpect { return d.t.Expect() }

// pgConstraintCountCmd counts primary/foreign keys across every connectable
// database in the target cluster, printing `contype|count` lines. Read-only.
func pgConstraintCountCmd() []string {
	q := `SELECT contype, count(*) FROM pg_constraint c ` +
		`JOIN pg_class t ON t.oid = c.conrelid ` +
		`JOIN pg_namespace n ON n.oid = t.relnamespace ` +
		`WHERE n.nspname NOT IN ('pg_catalog','information_schema') ` +
		`AND c.contype IN ('p','f') GROUP BY contype`
	sh := `set -e; U="${POSTGRES_USER:-postgres}"; ` +
		`for db in $(psql -tAqX -U "$U" -d postgres -c "SELECT datname FROM pg_database WHERE datallowconn AND datname NOT IN ('template0','template1')"); do ` +
		`psql -tAqX -U "$U" -d "$db" -c "` + q + `"; done`
	return []string{"/bin/sh", "-c", sh}
}

// parsePGConstraintCounts sums the `contype|count` lines the command prints.
func parsePGConstraintCounts(out string) (pk, fk int) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		typ, cnt, ok := strings.Cut(line, "|")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(cnt))
		if err != nil {
			continue
		}
		switch strings.TrimSpace(typ) {
		case "p":
			pk += n
		case "f":
			fk += n
		}
	}
	return pk, fk
}

// mysqlCountCmd counts USER tables across the server, printing `tables|N`.
//
// Read-only, and it uses the same credential ladder as importCommand: an engine
// DockBack could import into is an engine it can also count in, so the check
// never fails for a reason the import itself would not have.
func mysqlCountCmd() []string {
	q := `SELECT 'tables', count(*) FROM information_schema.tables ` +
		`WHERE table_type='BASE TABLE' AND table_schema NOT IN ` +
		`('information_schema','performance_schema','mysql','sys')`
	return []string{"/bin/sh", "-c", "" +
		`CLI=mysql; command -v mariadb >/dev/null 2>&1 && CLI=mariadb; ` +
		`RP="${MYSQL_ROOT_PASSWORD:-$MARIADB_ROOT_PASSWORD}"; ` +
		`AU="${MYSQL_USER:-$MARIADB_USER}"; AP="${MYSQL_PASSWORD:-$MARIADB_PASSWORD}"; ` +
		`Q="` + q + `"; ` +
		`if [ -n "$RP" ] && MYSQL_PWD="$RP" "$CLI" -h127.0.0.1 -P3306 -uroot -e 'SELECT 1' >/dev/null 2>&1; then ` +
		`exec env MYSQL_PWD="$RP" "$CLI" -N -B -h127.0.0.1 -P3306 -uroot -e "$Q"; ` +
		`elif "$CLI" -h127.0.0.1 -P3306 -uroot -e 'SELECT 1' >/dev/null 2>&1; then ` +
		`exec "$CLI" -N -B -h127.0.0.1 -P3306 -uroot -e "$Q"; ` +
		`elif [ -n "$AU" ] && MYSQL_PWD="$AP" "$CLI" -h127.0.0.1 -P3306 -u"$AU" -e 'SELECT 1' >/dev/null 2>&1; then ` +
		`exec env MYSQL_PWD="$AP" "$CLI" -N -B -h127.0.0.1 -P3306 -u"$AU" -e "$Q"; ` +
		`elif [ -n "$RP" ] && MYSQL_PWD="$RP" "$CLI" -uroot -e 'SELECT 1' >/dev/null 2>&1; then ` +
		`exec env MYSQL_PWD="$RP" "$CLI" -N -B -uroot -e "$Q"; ` +
		`else exec "$CLI" -N -B -uroot -e "$Q"; fi`}
}

// mongoCountCmd counts collections across every non-system database, printing
// `collections|N`. Read-only, with importCommand's auth ladder.
func mongoCountCmd() []string {
	js := `var n=0;db.adminCommand({listDatabases:1}).databases.forEach(function(d){` +
		`if(d.name=="admin"||d.name=="config"||d.name=="local")return;` +
		`n+=db.getSiblingDB(d.name).getCollectionNames().length;});print("collections|"+n);`
	return []string{"/bin/sh", "-c", "" +
		`SH=mongosh; command -v mongosh >/dev/null 2>&1 || SH=mongo; ` +
		`RU="${MONGO_INITDB_ROOT_USERNAME:-$MONGODB_ROOT_USER}"; ` +
		`RP="${MONGO_INITDB_ROOT_PASSWORD:-$MONGODB_ROOT_PASSWORD}"; ` +
		`if [ -z "$RU" ] && [ -n "$RP" ]; then RU=root; fi; ` +
		`J='` + js + `'; ` +
		`if [ -n "$RU" ]; then exec "$SH" --quiet --host 127.0.0.1 -u "$RU" -p "$RP" --authenticationDatabase admin --eval "$J"; fi; ` +
		`exec "$SH" --quiet --host 127.0.0.1 --eval "$J"`}
}

// redisCountCmd sums the key count across all databases, printing `keys|N` and
// `volatile|M` (F150).
//
// DBSIZE only covers the selected database, so this reads INFO keyspace, which
// lists every db and — on the same line — how many of its keys carry a TTL. The
// password comes from the container's own environment via REDISCLI_AUTH — never
// on argv, so it cannot appear in the process list.
//
// The volatile count is what makes a restore verdict possible at all. Redis
// deletes already-expired keys WHEN IT LOADS AN RDB, so a snapshot restored an
// hour later legitimately comes back with fewer keys than were captured. Without
// knowing how many of them could expire, "fewer keys than expected" is
// indistinguishable from "the snapshot did not load" — and one of those is a
// disaster while the other is Tuesday.
func redisCountCmd() []string {
	return []string{"/bin/sh", "-c", "" +
		`command -v redis-cli >/dev/null 2>&1 || { echo "redis-cli: not found" >&2; exit 127; }; ` +
		`[ -n "$REDIS_PASSWORD" ] && export REDISCLI_AUTH="$REDIS_PASSWORD"; ` +
		`I=$(redis-cli --no-auth-warning INFO keyspace); ` +
		`N=$(printf '%s\n' "$I" | sed -n 's/^db[0-9]*:keys=\([0-9]*\),.*/\1/p' | awk '{t+=$1} END {print t+0}'); ` +
		`V=$(printf '%s\n' "$I" | sed -n 's/^db[0-9]*:keys=[0-9]*,expires=\([0-9]*\),.*/\1/p' | awk '{t+=$1} END {print t+0}'); ` +
		`echo "keys|$N"; echo "volatile|$V"`}
}

// dbCountCmd returns the post-import count command for an engine, or nil when
// the engine has no cheap invariant to check.
func dbCountCmd(engine string) []string {
	switch engine {
	case "mysql":
		return mysqlCountCmd()
	case "mongodb":
		return mongoCountCmd()
	case "redis":
		return redisCountCmd()
	}
	return nil
}

// parseNameCount reads the first `name|count` line matching want. Tolerant of
// the banner noise mongosh and redis-cli print around their output.
func parseNameCount(out, want string) (int64, bool) {
	for _, line := range strings.Split(out, "\n") {
		name, cnt, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok || strings.TrimSpace(name) != want {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(cnt), 10, 64)
		if err != nil {
			continue
		}
		return n, true
	}
	return 0, false
}

// importExpectation is what an engine's import is held to: a human-readable
// noun, the number the dump declared, and the label the count command prints.
type importExpectation struct {
	noun   string // "tables", "collections", "keys"
	want   int64
	metric string
}

// expectationFor derives an engine's post-import contract from the dump's tally.
// A zero expectation means there is nothing to check — an empty database is a
// legitimate thing to back up, and must not be reported as a failed restore.
func expectationFor(engine string, exp DumpExpect) (importExpectation, bool) {
	switch engine {
	case "mysql":
		return importExpectation{"tables", int64(exp.Tables), "tables"}, exp.Tables > 0
	case "mongodb":
		return importExpectation{"collections", int64(exp.Collections), "collections"}, exp.Collections > 0
	case "redis":
		return importExpectation{"keys", exp.Keys, "keys"}, exp.Keys > 0
	}
	return importExpectation{}, false
}

// VerifyImportCounts is the MySQL/MongoDB/Redis half of post-import verification
// (F99), holding those engines to the same contract Postgres already had.
//
// Identical shortfall semantics, and for the same reasons: only FEWER than the
// dump declared is a failure (a target legitimately holds more — other databases
// in the same server, keys written since), and a dump that declared nothing is
// never failed. The one addition is the completion marker, which only MySQL's
// text dump carries; a binary archive has none to check.
//
// Redis is judged by redisImportVerdict instead (F150) — a plain shortfall there
// is normal rather than a fault, for a reason no other engine has.
func VerifyImportCounts(engine string, exp DumpExpect, actual int64) error {
	want, checkable := expectationFor(engine, exp)
	if !checkable {
		return nil
	}
	var problems []string
	if engine == "mysql" && !exp.Complete {
		problems = append(problems, "the dump stream ended before its completion marker (it was cut short)")
	}
	if actual < want.want {
		problems = append(problems, fmt.Sprintf("%d of %d %s restored", actual, want.want, want.noun))
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("the restored database is INCOMPLETE — %s; do NOT rely on this restore", strings.Join(problems, "; "))
}

// redisImportVerdict judges a restored Redis against what capture recorded
// (F150), and it is deliberately not the same rule as every other engine.
//
// Redis DELETES ALREADY-EXPIRED KEYS WHEN IT LOADS AN RDB. So a snapshot holding
// keys with a one-hour TTL, restored the next day, comes back with fewer keys
// than were captured — every time, by design, from a perfectly good backup.
// Applying the ordinary "fewer than expected is a failure" rule to that means a
// day-old backup of a cache or a task broker cannot be restored at all, which is
// exactly backwards: the shortfall is the mechanism working.
//
// It would be just as wrong to drop the check, because the failure it was
// written for is real and silent: an RDB that does not load leaves an EMPTY,
// perfectly healthy Redis and a restore reported as successful.
//
// So the count is split by what CAN expire, which capture now records:
//
//		persistent = keys - volatile   (keys with no TTL; these cannot expire)
//
//	  - restored < persistent  → FAILURE. Keys that could not have expired are
//	    missing, so something really was lost.
//	  - persistent <= restored < keys → normal. TTLs did their job; reported so
//	    the numbers are not a surprise, never failed.
//	  - restored >= keys → nothing to say.
//
// Without a recorded volatile count (a backup taken before this existed) the
// split cannot be made, so the only claim left is the unambiguous one: a
// completely empty Redis where keys were captured is a failure, and any other
// shortfall is reported and allowed. An unknown must not be resolved by guessing
// in the direction that refuses a recovery.
func redisImportVerdict(exp DumpExpect, actual int64) (fail error, note string) {
	if exp.Keys <= 0 {
		return nil, "" // nothing recorded, or a genuinely empty Redis
	}
	if actual >= exp.Keys {
		return nil, ""
	}
	if exp.VolatileKeys <= 0 {
		if actual == 0 {
			return fmt.Errorf("the restored Redis is EMPTY but %d key(s) were captured — the snapshot did not load; do NOT rely on this restore", exp.Keys), ""
		}
		return nil, fmt.Sprintf("Redis came back with %d of the %d key(s) captured. That is normal for a cache or task broker — Redis drops keys whose time-to-live has passed as it loads a snapshot — and this backup predates the recording that would let DockBack tell that apart from real loss. Nothing else in the restore depends on it",
			actual, exp.Keys)
	}
	persistent := exp.Keys - exp.VolatileKeys
	if actual < persistent {
		return fmt.Errorf("the restored Redis has %d key(s) but %d were captured WITHOUT an expiry and cannot have expired on their own — %d key(s) are genuinely missing; do NOT rely on this restore",
			actual, persistent, persistent-actual), ""
	}
	return nil, fmt.Sprintf("Redis came back with %d of the %d key(s) captured — %d of those carried an expiry, and Redis drops the ones whose time has passed as it loads a snapshot. All %d key(s) that could not expire are present, so nothing was lost",
		actual, exp.Keys, exp.VolatileKeys, persistent)
}

// VerifyImportCompleteness compares what the dump declared against what the
// restored cluster actually has. It returns a non-nil error describing the
// shortfall when the import did not fully apply.
//
// It is deliberately conservative — it reports a problem ONLY on a shortfall,
// and only when the dump declared something to check — so it can never fail a
// restore that genuinely worked.
func VerifyImportCompleteness(exp DumpExpect, actualPK, actualFK int) error {
	if exp.PrimaryKeys == 0 && exp.ForeignKeys == 0 {
		return nil // not a schema-bearing dump (data-only, or a non-pg_dump format)
	}
	var problems []string
	if !exp.Complete {
		problems = append(problems, "the dump stream ended before its completion marker (it was cut short)")
	}
	if actualPK < exp.PrimaryKeys {
		problems = append(problems, fmt.Sprintf("%d of %d primary keys applied", actualPK, exp.PrimaryKeys))
	}
	if actualFK < exp.ForeignKeys {
		problems = append(problems, fmt.Sprintf("%d of %d foreign keys applied", actualFK, exp.ForeignKeys))
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("the restored database is INCOMPLETE — %s; the data may be present but the schema is not, so do NOT rely on this restore", strings.Join(problems, "; "))
}

// Checking a restore's content baseline back, and saying what it means (#18/#26).
//
// The comparison happens after the import and BEFORE the application (re)starts,
// which R3 §Issue 26 identifies as the only clean point: every second afterwards,
// a difference could be the application's own writing rather than the restore's.

// Hash verdict phases. The label is part of the verdict because the same
// comparison means different things at different moments — before the
// application runs, every difference is the restore's; afterwards, some of them
// are the application doing its job.
const (
	HashPhasePreStart  = "pre-start"
	HashPhasePostStart = "post-start, volatile drift expected"
)

// HashVerdict is the outcome of checking a captured baseline against a restored
// database. Table NAMES only — a hash is not reversible into a row.
type HashVerdict struct {
	Phase string `json:"phase"`
	At    string `json:"at"`
	// Identical, Different: tables the baseline recorded and the restored
	// database has.
	Identical []string `json:"identical,omitempty"`
	Different []string `json:"different,omitempty"`
	// OnlyInBackup are tables the baseline recorded that the restored database
	// does not have — absence, which is a different failure from difference.
	OnlyInBackup []string `json:"only_in_backup,omitempty"`
	// OnlyInRestored are tables the restored database has that the baseline did
	// not record. A dump legitimately creates some — an extension's own tables —
	// so this is worth naming and never worth blocking on.
	OnlyInRestored []string `json:"only_in_restored,omitempty"`
}

// CompareTableHashes checks a captured baseline against a restored one.
//
// The baseline is the MANIFEST, never the source's live database: the source may
// have moved on since the backup, and that is not a defect in the restore.
func CompareTableHashes(captured, restored map[string]string) HashVerdict {
	verdict := HashVerdict{At: nowRFC3339()}
	for _, name := range sortedTableNames(captured) {
		got, present := restored[name]
		switch {
		case !present:
			verdict.OnlyInBackup = append(verdict.OnlyInBackup, name)
		case got == captured[name]:
			verdict.Identical = append(verdict.Identical, name)
		default:
			verdict.Different = append(verdict.Different, name)
		}
	}
	for _, name := range sortedTableNames(restored) {
		if _, recorded := captured[name]; !recorded {
			verdict.OnlyInRestored = append(verdict.OnlyInRestored, name)
		}
	}
	return verdict
}

// Compared is how many of the baseline's tables the verdict covers.
func (v HashVerdict) Compared() int {
	return len(v.Identical) + len(v.Different) + len(v.OnlyInBackup)
}

// Clean reports whether every table the baseline recorded came back identical.
func (v HashVerdict) Clean() bool {
	return len(v.Different) == 0 && len(v.OnlyInBackup) == 0
}

// HashSplit is a verdict read through an application's own classification of
// which tables it writes to by itself (#19/#26).
//
// The split is the difference between a verification an operator trusts and one
// they learn to ignore. R3 §7.2 is the measurement: 229 of 234 Nextcloud tables
// identical, the 5 that differed all runtime state, and oc_filecache — the index
// of every file, 67,234 rows — among the identical ones. Reported flat that is
// "229/234 tables identical", which invites a support ticket. Reported split it
// is "229/229 durable identical; 5 volatile differ (expected)", which is a claim
// the tool can stand behind.
type HashSplit struct {
	// DurableTotal is how many tables the comparison covered, excluding the ones
	// this application declares it rewrites itself.
	DurableTotal int
	// DurableIdentical is how many of those came back byte-for-byte.
	DurableIdentical int
	// DurableDifferent and DurableMissing are the ones that did not. These are
	// the failures.
	DurableDifferent []string
	DurableMissing   []string
	// VolatileDifferent are declared-volatile tables that differ — expected, and
	// reported separately so the number is visible rather than hidden.
	VolatileDifferent []string
}

// Clean reports whether every DURABLE table came back.
func (s HashSplit) Clean() bool {
	return len(s.DurableDifferent) == 0 && len(s.DurableMissing) == 0
}

// Split reads a verdict through the volatile set.
//
// A table is matched by its declared bare name, so a profile can say
// `oc_appconfig` without knowing which database and schema it landed in.
func (v HashVerdict) Split(volatile map[string]bool) HashSplit {
	isVolatile := func(name string) bool {
		return volatile[name] || volatile[bareTableName(name)]
	}
	var s HashSplit
	for _, name := range v.Identical {
		if isVolatile(name) {
			continue // counted as neither: it was never a durable claim
		}
		s.DurableTotal++
		s.DurableIdentical++
	}
	for _, name := range v.Different {
		if isVolatile(name) {
			s.VolatileDifferent = append(s.VolatileDifferent, name)
			continue
		}
		s.DurableTotal++
		s.DurableDifferent = append(s.DurableDifferent, name)
	}
	for _, name := range v.OnlyInBackup {
		if isVolatile(name) {
			continue
		}
		s.DurableTotal++
		s.DurableMissing = append(s.DurableMissing, name)
	}
	return s
}

// Headline is the register's wording bar, in one clause or two.
func (s HashSplit) Headline() string {
	line := fmt.Sprintf("%d/%d durable identical", s.DurableIdentical, s.DurableTotal)
	if len(s.VolatileDifferent) > 0 {
		line += fmt.Sprintf("; %d volatile differ (expected)", len(s.VolatileDifferent))
	}
	return line
}

// bareTableName drops the database and schema qualifiers, so a profile can
// declare `django_session` without knowing which database it landed in.
func bareTableName(qualified string) string {
	if i := strings.LastIndex(qualified, "."); i >= 0 {
		return qualified[i+1:]
	}
	return qualified
}

// namedTables renders a table list for an operator, bounded the way a short-table
// report is: enough to act on, not enough to fill the screen.
func namedTables(names []string) string {
	if len(names) <= maxReportedShortTables {
		return strings.Join(names, ", ")
	}
	shown := names[:maxReportedShortTables]
	return fmt.Sprintf("%s and %d more", strings.Join(shown, ", "), len(names)-maxReportedShortTables)
}

// volatileTablesFor names the tables an application writes to by itself, so a
// difference in one of them is not mistaken for a restore defect.
//
// The set is per application because only the application knows: `django_session`
// is runtime state in one schema and could be durable data in another. Step 13
// populates it from the profiles; until an application declares one, nothing is
// classified — and the second return value says so, which is what keeps the
// failure policy from firing on an unknown schema.
func volatileTablesFor(image string) (map[string]bool, bool) {
	return volatileTableSet(ProfileFor(image))
}

// volatileTableSet turns one profile's declaration into a lookup.
func volatileTableSet(profiles ...*AppProfile) (map[string]bool, bool) {
	volatile := map[string]bool{}
	for _, profile := range profiles {
		if profile == nil {
			continue
		}
		for _, name := range profile.VolatileTables {
			volatile[name] = true
		}
	}
	if len(volatile) == 0 {
		return nil, false
	}
	return volatile, true
}

// stackVolatileTables finds the classification for a DATABASE container from the
// application it belongs to.
//
// Without this the feature does not reach the case it was built for. R3's
// Nextcloud runs its database in a separate `mariadb` container, so the tables
// whose names the Nextcloud profile declares — oc_appconfig and the rest — live
// somewhere whose own image is mariadb and declares nothing. Keyed on the
// container's own image alone, the 229-of-234 verdict would never be reachable
// for the standard deployment.
//
// The compose project is what connects them, and it is already how atomic sets
// are resolved. Every declaring member's list is merged, so a stack running two
// applications classifies both; matching stays on exact table names, so a merged
// list cannot label a table it was not written for.
//
// A listing that fails yields nothing rather than a guess — this decides whether
// a restore can be failed, and that must not rest on an unanswered question.
func (e *Engine) stackVolatileTables(ctx context.Context, cli *client.Client, image, project string) (map[string]bool, bool) {
	if volatile, ok := volatileTablesFor(image); ok {
		return volatile, ok
	}
	if project == "" || cli == nil {
		return nil, false
	}
	containers, err := dockercli.ListContainers(ctx, cli)
	if err != nil {
		return nil, false
	}
	var profiles []*AppProfile
	for _, c := range containers {
		if c == nil || c.Stack != project {
			continue
		}
		if profile := ProfileFor(c.Image); profile != nil && len(profile.VolatileTables) > 0 {
			profiles = append(profiles, profile)
		}
	}
	return volatileTableSet(profiles...)
}
