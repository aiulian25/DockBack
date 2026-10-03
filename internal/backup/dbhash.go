package backup

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// Proving a restore brought back the same CONTENT, not the same count (#18).
//
// The dump contract compares row counts. A count passes on a table whose rows
// all changed, which R2 §Issue 18 asks to close with a schema-agnostic content
// proof. R4 §Issue 31 then dictates how that proof must be computed, because
// getting it wrong is worse than not having it: hashing `t::text` produced
// SEVEN false table mismatches out of eight on a byte-perfect restore, including
// `documents_document` "differing" on 169 of 203 rows. The data was identical
// the whole time; the clone's session TimeZone was not.
//
// So two rules, both load-bearing, both from that measurement:
//
//  1. Never render a timestamp as text. `timestamptz::text` depends on the
//     session TimeZone, and `timestamp`/`date` rendering depends on DateStyle —
//     which differ across exactly the hosts a cross-restore compares. Every
//     temporal column is projected as `extract(epoch from …)`, an absolute
//     number. `money` gets the same treatment through `::numeric`, because its
//     text form follows lc_monetary.
//  2. Never order by rendered text. The aggregate's ORDER BY decides the hash,
//     and collation decides the order — so a cluster initialised with a
//     different collation would hash identical data differently.

// tableHashPrefix tags a hash line. The table name is second and the hash last,
// so a name containing the separator still parses from the left.
const tableHashPrefix = "TBLHASH|"

// maxHashedTables bounds what goes into a manifest, mirroring the row-count cap.
const maxHashedTables = 200

// pgTableHashQuery generates one hash statement per table and executes them.
//
// Two things it does NOT do, deliberately:
//
// It does not order by the primary key. A textual primary key orders by
// collation, which is the very dependence rule 2 exists to remove; and a table
// without one would then need a fallback that is weaker still. Ordering by the
// projected row text under an explicit `COLLATE "C"` is byte-order — the same on
// every cluster, keyed or not — and rows that tie are identical rows, so their
// order cannot change the concatenation.
//
// It does not use `t::text` for the row. Whole-row text is what rule 1 forbids,
// so the row is rebuilt from per-column projections instead, which is also what
// makes the temporal handling possible at all.
//
// `\gexec` runs each generated statement, so this is one psql round trip.
const pgTableHashQuery = `SELECT 'SELECT ''` + tableHashPrefix + `'' || current_database() || ''.' || n.nspname || '.' || c.relname || '|'' || coalesce(md5(string_agg(r, E''\n'' ORDER BY r COLLATE "C")), ''-'') FROM (SELECT concat_ws(E''\x1f'', ' || k.cols || ') AS r FROM ' || quote_ident(n.nspname) || '.' || quote_ident(c.relname) || ') s'
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
CROSS JOIN LATERAL (
  SELECT string_agg(
    CASE
      WHEN a.atttypid = ANY (ARRAY['timestamptz','timestamp','date','time','timetz']::regtype[])
        THEN 'extract(epoch from ' || quote_ident(a.attname) || ')::text'
      WHEN a.atttypid = 'money'::regtype
        THEN quote_ident(a.attname) || '::numeric::text'
      ELSE quote_ident(a.attname) || '::text'
    END, ', ' ORDER BY a.attnum) AS cols
  FROM pg_attribute a WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
) k
WHERE c.relkind = 'r' AND n.nspname NOT IN ('pg_catalog','information_schema') AND k.cols IS NOT NULL
ORDER BY 1 \gexec`

// pgHashStatementTimeout bounds the pass. A hash walks every row of every table,
// so on a large database it is the most expensive read a backup makes — and it
// is an EXTRA, not the backup. Exceeding this records nothing and says so.
const pgHashStatementTimeout = "10min"

// pgTableHashScript runs the hash pass across every database the client can
// reach, prefixing each table with its database so two schemas cannot collide.
//
// The query travels on stdin through a quoted heredoc: it is full of quotes and
// backslashes by construction, and `\gexec` is a psql meta-command that -c does
// not accept.
func pgTableHashScript(d *EmbeddedDump) string {
	opts := pgClientOpts(d)
	return "set -e; " +
		"for db in $(psql " + opts + ` -d postgres -tAqX -c "SELECT datname FROM pg_database WHERE datallowconn AND datname NOT IN ('template0','template1')"); do ` +
		`psql ` + opts + ` -d "$db" -X -q -A -t -v ON_ERROR_STOP=1 <<'DOCKBACKHASH'
SET statement_timeout = '` + pgHashStatementTimeout + `';
` + pgTableHashQuery + `
DOCKBACKHASH
done`
}

// mysqlQuotedIdent builds the identifier the CHECKSUM statement runs against:
// a backtick-quoted `schema`.`table`, with any backtick inside a name doubled —
// MySQL's own escape, applied by the server rather than by us.
//
// Every backtick is written \` because this expression is interpolated into a
// DOUBLE-QUOTED shell string, where a bare backtick opens a command
// substitution instead of reaching the server. The shell turns each \` back
// into a plain backtick before mysql ever sees it.
const mysqlQuotedIdent = "concat('\\`',replace(table_schema,'\\`','\\`\\`'),'\\`.\\`',replace(table_name,'\\`','\\`\\`'),'\\`')"

// mysqlTableHashScript asks MySQL for its own per-table checksum.
//
// CHECKSUM TABLE ... EXTENDED reads every row and is computed by the server over
// values, not over their rendered text, so it carries none of the timezone or
// collation dependence rule 1 and 2 exist to remove.
//
// The listing returns TWO columns: the display key that goes into the manifest,
// and the quoted identifier the next statement runs against. Interpolating the
// bare name was a SQL injection executed as the dump user — root whenever the
// image supplies a root password — so anyone able to CREATE TABLE in any
// application database on the server chose what the next backup would run.
func mysqlTableHashScript() string {
	return mysqlClientPreamble +
		`c() { "$CLI" -u"$U" -N -B -e "$1" 2>/dev/null; }; ` +
		`c "SELECT concat(table_schema,'.',table_name), ` + mysqlQuotedIdent +
		` FROM information_schema.tables WHERE table_type='BASE TABLE' AND table_schema NOT IN ('mysql','information_schema','performance_schema','sys')"` +
		` | while IFS="$(printf '\t')" read -r t q; do ` +
		`h=$(c "CHECKSUM TABLE $q EXTENDED" | awk '{print $2}'); ` +
		`[ -n "$h" ] && printf '` + tableHashPrefix + `%s|%s\n' "$t" "$h"; done`
}

// mysqlClientPreamble picks the client, authenticates, and leaves $CLI and $U set
// — or exits quietly when this container turns out not to be a server.
//
// Copied in shape from the dump command's own ladder, and for exactly the reason
// that ladder exists. MariaDB 11 ships NO `mysql` binary, only `mariadb`, and
// the root password lives under either the MYSQL_ or the MARIADB_ prefix
// depending on which image built the container. A hash pass that assumes `mysql`
// and MYSQL_ROOT_PASSWORD silently produces nothing — measured on
// mariadb:11.4-noble, where it recorded no baseline at all and nobody would have
// known, because an absent baseline looks exactly like a backup that has none.
//
// Exits 0 with no output when it cannot get in: a content baseline is an extra,
// and an extra must never fail a dump that already worked.
const mysqlClientPreamble = `CLI=mysql; command -v mariadb >/dev/null 2>&1 && CLI=mariadb; ` +
	`command -v "$CLI" >/dev/null 2>&1 || exit 0; ` +
	`RP="${MYSQL_ROOT_PASSWORD:-${MARIADB_ROOT_PASSWORD:-}}"; ` +
	`AU="${MYSQL_USER:-${MARIADB_USER:-}}"; AP="${MYSQL_PASSWORD:-${MARIADB_PASSWORD:-}}"; ` +
	`if [ -n "$RP" ] && MYSQL_PWD="$RP" "$CLI" -uroot -N -e "SELECT 1" >/dev/null 2>&1; then U=root; export MYSQL_PWD="$RP"; ` +
	`elif "$CLI" -uroot -N -e "SELECT 1" >/dev/null 2>&1; then U=root; ` +
	`elif [ -n "$AU" ] && MYSQL_PWD="$AP" "$CLI" -u"$AU" -N -e "SELECT 1" >/dev/null 2>&1; then U="$AU"; export MYSQL_PWD="$AP"; ` +
	`else exit 0; fi; `

// parseTableHashes reads the hash lines into table → hash.
//
// Table names may contain the separator, so the hash is taken from the RIGHT and
// the name is whatever precedes it.
func parseTableHashes(out string) map[string]string {
	hashes := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), tableHashPrefix)
		if !ok || rest == "" {
			continue
		}
		cut := strings.LastIndex(rest, "|")
		if cut <= 0 || cut == len(rest)-1 {
			continue
		}
		name, hash := rest[:cut], rest[cut+1:]
		if name == "" || hash == "" || hash == "-" {
			continue
		}
		hashes[name] = hash
	}
	return hashes
}

// tableHashScriptFor returns the pass for an engine, or false when there is
// none. Mongo and Redis have no table concept to hash.
func tableHashScriptFor(engine string, d *EmbeddedDump) (string, bool) {
	switch engine {
	case "postgres":
		return pgTableHashScript(d), true
	case "mysql":
		return mysqlTableHashScript(), true
	}
	return "", false
}

// recordTableHashes takes the content baseline beside the dump it belongs to.
//
// Never fails a backup. The hash is an EXTRA proof on top of a dump that has
// already succeeded, and refusing to save a good dump because an optional check
// timed out would trade a real backup for a nicer report.
func (e *Engine) recordTableHashes(ctx context.Context, cli *client.Client, containerID, engine string, d *EmbeddedDump, dump *DBDump, logID string, activityBefore map[string]string) {
	script, ok := tableHashScriptFor(engine, d)
	if !ok {
		e.logf(logID, "INFO", "No content baseline for %s — a restore of it is checked by the dump's own contract instead", engine)
		return
	}
	out, err := dockercli.ExecCapture(ctx, cli, containerID, []string{"/bin/sh", "-c", script})
	if err != nil {
		e.logf(logID, "INFO", "Could not take a content baseline of this database (%v) — the backup is complete, but a restore of it will be checked by row counts alone", err)
		return
	}
	hashes := parseTableHashes(string(out))
	if len(hashes) == 0 {
		return
	}
	if len(hashes) > maxHashedTables {
		e.logf(logID, "INFO", "This database has %d tables, more than the %d a content baseline records — a restore of it will be checked by row counts alone", len(hashes), maxHashedTables)
		return
	}

	// The invariant: a recorded hash describes the DUMP. A table written to
	// between the dump and this pass is in the hash and not in the dump, so
	// keeping it would guarantee a false mismatch on every restore.
	if activityBefore != nil {
		// For MySQL the checksum IS the evidence, so the baseline just computed
		// doubles as the "after" reading and costs no second pass.
		after := hashes
		if engine != "mysql" {
			after = e.tableQuiescenceEvidence(ctx, cli, containerID, engine, d)
		}
		kept, moved := quiescentTables(hashes, activityBefore, after)
		if len(moved) > 0 {
			e.logf(logID, "INFO", "%d %s written to while this backup was being taken, so %s left out of the content baseline: %s. The dump holds them as they were at its own instant; a hash taken afterwards would not match that, and would report a restore of them as wrong.",
				len(moved), plural(len(moved), "table was", "tables were"),
				plural(len(moved), "it is", "they are"), namedTables(moved))
		}
		hashes = kept
	} else if engineNeedsQuiescenceEvidence(engine) {
		// The counters could not be read, so nothing can be said about what moved.
		e.logf(logID, "INFO", "Could not tell which tables changed while this backup was being taken — no content baseline is recorded, because one that might describe a moment after the dump would report good restores as wrong")
		return
	}
	if len(hashes) == 0 {
		return
	}
	dump.TableHashes = hashes
	e.logf(logID, "INFO", "Content baseline recorded for %d %s — a restore can prove the rows came back unchanged, not just that there are the same number of them",
		len(hashes), plural(len(hashes), "table", "tables"))
}

// sortedTableNames returns hash keys in order, for stable reporting.
func sortedTableNames(hashes map[string]string) []string {
	out := make([]string, 0, len(hashes))
	for name := range hashes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Checking the baseline back, after the import (#18/#26 restore half).
//
// The comparison itself lives in dbverify.go beside the other restore contracts.
// What is here is the part that needs a daemon: re-running the capture pass
// against the freshly imported database, at the one moment the data is the
// restore's alone.

// restoreHashVerdictKey is where a restore's verdict is kept. Settings rather
// than a new column: the verdict is an artifact OF a restore, not a property of
// the backup, and it needs no migration to survive one.
func restoreHashVerdictKey(backupID string) string { return "restore.hashverdict:" + backupID }

// restoredTableHashes re-runs the capture pass against the restored database.
func (e *Engine) restoredTableHashes(ctx context.Context, cli *client.Client, containerID, engine string, d *EmbeddedDump) (map[string]string, error) {
	script, ok := tableHashScriptFor(engine, d)
	if !ok {
		return nil, nil
	}
	out, err := dockercli.ExecCapture(ctx, cli, containerID, []string{"/bin/sh", "-c", script})
	if err != nil {
		return nil, err
	}
	return parseTableHashes(string(out)), nil
}

// verifyTableHashes compares the restored database against this backup's content
// baseline, reports the verdict, persists it, and says whether the restore should
// be treated as unhealthy.
//
// FAILURE POLICY, and the one place it is decided.
//
// A durable-table mismatch is meant to fail the restore — which, through the
// existing gate, means ROLLING IT BACK to the pre-restore snapshot. That is only
// safe when "durable" can be told apart from "volatile", and volatile tables are
// declared per application. Until an application declares them, every table
// counts as durable, and a single session row written between the import and this
// check would discard an otherwise perfect restore. R4 §31 measured exactly that
// shape: 73 of 74 tables identical, the difference genuine runtime state.
//
// So the escalation is armed by the classification, not by its absence: an
// application that declares its volatile tables gets the failure, and one that
// does not gets the same named report as a loud warning. Nothing is hidden
// either way — the tables are named identically in both.
func (e *Engine) verifyTableHashes(ctx context.Context, cli *client.Client, b *store.Backup, containerID, engine string, d *EmbeddedDump, recorded map[string]string, volatile map[string]bool, classified bool) bool {
	if len(recorded) == 0 {
		return true
	}
	restored, err := e.restoredTableHashes(ctx, cli, containerID, engine, d)
	if err != nil {
		e.logf(b.ID, "INFO", "Could not check the restored data against this backup's content baseline (%v) — the import succeeded and its row counts were checked, but the rows themselves were not compared", err)
		return true
	}
	if len(restored) == 0 {
		return true
	}

	verdict := CompareTableHashes(recorded, restored)
	verdict.Phase = HashPhasePreStart
	e.persistHashVerdict(b, verdict)

	// A dump legitimately creates tables the baseline never saw — an extension's
	// own, most often. Named, never blocking.
	if len(verdict.OnlyInRestored) > 0 {
		e.logf(b.ID, "WARN", "%d %s in the restored database %s not in this backup's baseline: %s. A dump can legitimately create tables of its own (an extension's, for instance), so this is reported rather than treated as a problem.",
			len(verdict.OnlyInRestored), plural(len(verdict.OnlyInRestored), "table", "tables"),
			plural(len(verdict.OnlyInRestored), "was", "were"), namedTables(verdict.OnlyInRestored))
	}

	split := verdict.Split(volatile)
	if split.Clean() {
		e.logf(b.ID, "INFO", "Content verified before %s started: %s — the rows came back, not merely the same number of them",
			b.TargetName, split.Headline())
		if len(split.VolatileDifferent) > 0 {
			// Named, not merely counted: an operator who wants to check that a
			// table really is runtime state needs to know which one it was.
			e.logf(b.ID, "INFO", "The %s that differ %s ones %s declares it rewrites itself: %s",
				plural(len(split.VolatileDifferent), "table", "tables"),
				plural(len(split.VolatileDifferent), "is one of the", "are"),
				b.TargetName, namedTables(split.VolatileDifferent))
		}
		return true
	}

	if len(split.DurableMissing) > 0 {
		e.logf(b.ID, "ERROR", "%d durable %s this backup recorded %s not in the restored database at all: %s",
			len(split.DurableMissing), plural(len(split.DurableMissing), "table", "tables"),
			plural(len(split.DurableMissing), "is", "are"), namedTables(split.DurableMissing))
	}

	level, tail := "WARN", "Check these against what this application writes at startup before treating any of them as data loss."
	if classified {
		level = "ERROR"
		tail = "This application declares which of its tables change by themselves, and these are not among them."
	}
	e.logf(b.ID, level, "Content check before %s started: %s; %d durable %s came back with different content: %s. %s",
		b.TargetName, split.Headline(), len(split.DurableDifferent),
		plural(len(split.DurableDifferent), "table", "tables"), namedTables(split.DurableDifferent), tail)
	return !classified
}

// persistHashVerdict keeps the verdict beside the backup so a later check can be
// compared against it, and so the operator can read it after the run log has
// scrolled.
func (e *Engine) persistHashVerdict(b *store.Backup, verdict HashVerdict) {
	if e.Store == nil {
		return
	}
	raw, err := json.Marshal(verdict)
	if err != nil {
		return
	}
	_ = e.Store.SetSetting(restoreHashVerdictKey(b.ID), string(raw))
}

// Making the baseline describe the DUMP, not the database a moment later.
//
// The dump is a consistent snapshot taken at one instant; the hash pass runs
// afterwards, in its own transaction, seeing the database as it is then. Any row
// written in between is in the BASELINE and not in the DUMP — so every restore of
// that backup would report a mismatch on a table that restored perfectly.
// Databases are dumped live and never quiesced, so the window is real, and a
// false mismatch is precisely what this feature must not manufacture.
//
// pg_dumpall has no --snapshot (measured), so the dump's own snapshot cannot be
// borrowed for the hash. What CAN be done is to ask afterwards which tables were
// written to across the window, and keep the baseline only for the ones that
// were not. PostgreSQL counts exactly that per table.
//
// The result is an invariant worth more than the coverage it costs: a recorded
// hash means that table did not change between the dump and the hash, so a later
// mismatch on it is about the restore and nothing else.

// tableModPrefix tags an activity line.
const tableModPrefix = "TBLMOD|"

// pgTableActivityQuery reads each table's cumulative modification count, keyed
// exactly as the hashes are so the two line up.
const pgTableActivityQuery = `SELECT '` + tableModPrefix + `' || current_database() || '.' || schemaname || '.' || relname || '|' || ` +
	`(coalesce(n_tup_ins,0) + coalesce(n_tup_upd,0) + coalesce(n_tup_del,0)) FROM pg_stat_user_tables`

// pgTableActivityScript asks every reachable database.
func pgTableActivityScript(d *EmbeddedDump) string {
	opts := pgClientOpts(d)
	return "set -e; " +
		"for db in $(psql " + opts + ` -d postgres -tAqX -c "SELECT datname FROM pg_database WHERE datallowconn AND datname NOT IN ('template0','template1')"); do ` +
		`psql ` + opts + ` -d "$db" -tAqX -c "` + pgTableActivityQuery + `"; done`
}

// parseTableActivity reads the counters. Same right-hand split as the hashes, so
// a table name containing the separator survives.
func parseTableActivity(out string) map[string]string {
	counts := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), tableModPrefix)
		if !ok {
			continue
		}
		cut := strings.LastIndex(rest, "|")
		if cut <= 0 {
			continue
		}
		if _, err := strconv.ParseInt(rest[cut+1:], 10, 64); err != nil {
			continue
		}
		counts[rest[:cut]] = rest[cut+1:]
	}
	return counts
}

// quiescentTables keeps the hashes of tables whose modification count did not
// move across the dump-and-hash window.
//
// A table absent from either reading is dropped: a counter that cannot be
// compared is not evidence of quiescence, and the invariant is the point.
func quiescentTables(hashes, before, after map[string]string) (kept map[string]string, moved []string) {
	kept = make(map[string]string, len(hashes))
	for _, name := range sortedTableNames(hashes) {
		was, hadBefore := before[name]
		is, hadAfter := after[name]
		if !hadBefore || !hadAfter || was != is {
			moved = append(moved, name)
			continue
		}
		kept[name] = hashes[name]
	}
	return kept, moved
}

// tableQuiescenceEvidence reads, per table, something that changes if and only if
// the table was written to.
//
// Two engines, two answers, both exact:
//
//   - PostgreSQL keeps per-table modification counters, so the evidence is
//     essentially free — two catalogue queries either side of the dump.
//   - MySQL and MariaDB keep no such counter that can be relied on
//     (performance_schema is on by default in MySQL and OFF in MariaDB), so the
//     evidence is the table's own CHECKSUM. That is exact, and it is the same
//     value the baseline is made of, so the pass after the dump serves both jobs
//     and only the one before it is extra.
//
// Returns nothing when it cannot tell, which the caller treats as "record no
// baseline" rather than as "nothing moved".
func (e *Engine) tableQuiescenceEvidence(ctx context.Context, cli *client.Client, containerID, engine string, d *EmbeddedDump) map[string]string {
	var script string
	switch engine {
	case "postgres":
		script = pgTableActivityScript(d)
	case "mysql":
		script = mysqlTableHashScript()
	default:
		return nil
	}
	out, err := dockercli.ExecCapture(ctx, cli, containerID, []string{"/bin/sh", "-c", script})
	if err != nil {
		return nil
	}
	if engine == "mysql" {
		return parseTableHashes(string(out))
	}
	return parseTableActivity(string(out))
}

// engineNeedsQuiescenceEvidence reports whether a baseline for this engine is
// only trustworthy with a before-and-after reading. Both engines that HAVE a
// baseline do.
func engineNeedsQuiescenceEvidence(engine string) bool {
	return engine == "postgres" || engine == "mysql"
}
