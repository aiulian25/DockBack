package backup

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	// The SQLite engine DockBack carries for its own catalog.
	_ "modernc.org/sqlite"

	"dockback/internal/dockercli"
)

// A consistent SQLite snapshot taken by DockBack itself. The shipped sidecar
// image has no sqlite3, so by default no database was ever snapshotted,
// integrity-checked or counted (F22, F109, F116, F124) — every one went out as
// a raw copy. When the sidecar lacks sqlite3 it hands back each database's raw
// files instead, copied while the app is held quiesced, and this turns them into
// the same snapshot with the engine DockBack already carries. It writes the same
// index, stats, rows and failure files the sidecar's sqlite3 writes, so
// everything downstream reads one format. Per-table content hashes (#18) stay
// with the sidecar's sqlite3.

// sqliteRawName matches the raw database files the sidecar hands back, and
// sqliteRawIndexNumber the set numbers raw-index.txt maps.
var (
	sqliteRawName        = regexp.MustCompile(`^raw-[0-9]+\.db(-wal|-shm|-journal)?$`)
	sqliteRawIndexNumber = regexp.MustCompile(`^[0-9]+$`)
)

const (
	// sqliteRawIndex maps "<n>\t<source path>" for each raw database set.
	sqliteRawIndex = "raw-index.txt"
	// sqliteBusyTimeoutMillis is how long a staged copy waits on its own lock.
	sqliteBusyTimeoutMillis = 5000
	// sqliteIntegrityOK is the whole answer of a clean PRAGMA integrity_check.
	sqliteIntegrityOK = "ok"
)

// snapshotSQLiteLocally snapshots every raw database set the sidecar handed back
// into dir. quiesced says the files were copied while the app could not write:
// only then does a failed integrity check mean the database itself is corrupt.
// A copy taken while the app was writing can fail it because the copy was torn.
func snapshotSQLiteLocally(dir string, quiesced bool) {
	sets := readFileString(filepath.Join(dir, sqliteRawIndex))
	_ = os.Remove(filepath.Join(dir, sqliteRawIndex))
	for _, line := range strings.Split(strings.TrimSpace(sets), "\n") {
		idx, source, ok := strings.Cut(line, "\t")
		if !ok || !sqliteRawIndexNumber.MatchString(idx) {
			continue
		}
		snapshotOneLocally(dir, idx, strings.TrimSpace(source), quiesced)
	}
}

// snapshotOneLocally turns one raw database set into <idx>.dbk with its index,
// stats and per-table rows, or into a line in failed.txt.
func snapshotOneLocally(dir, idx, source string, quiesced bool) {
	stage := filepath.Join(dir, "stage-"+idx)
	defer os.RemoveAll(stage)
	staged, err := stageRawSQLite(dir, idx, stage)
	if err != nil {
		appendSQLiteLine(dir, "failed.txt", "snapshot", source, "could not be staged: "+err.Error())
		return
	}
	dbk := filepath.Join(dir, idx+".dbk")
	if err := vacuumInto(staged, dbk); err != nil {
		_ = os.Remove(dbk)
		appendSQLiteLine(dir, "failed.txt", "snapshot", source, "could not be snapshotted (locked or unreadable)")
		return
	}
	if verdict := sqliteIntegrity(dbk); verdict != sqliteIntegrityOK {
		_ = os.Remove(dbk)
		appendSQLiteLine(dir, "failed.txt", integrityFailureKind(quiesced), source, integrityFailureDetail(verdict, quiesced))
		return
	}
	appendSQLiteLine(dir, "index.txt", idx, source)
	tables, rows, perTable := countSQLite(dbk)
	appendSQLiteLine(dir, "stats.txt", idx, fmt.Sprint(tables), fmt.Sprint(rows))
	_ = os.WriteFile(filepath.Join(dir, "rows-"+idx+".txt"), []byte(perTable), 0o600)
}

// stageRawSQLite moves a raw set into its own folder under the names SQLite
// looks for — the database and its -wal, -shm or -journal side by side — and
// returns the database's path.
func stageRawSQLite(dir, idx, stage string) (string, error) {
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return "", err
	}
	db := filepath.Join(stage, "d")
	if err := os.Rename(filepath.Join(dir, "raw-"+idx+".db"), db); err != nil {
		return "", err
	}
	for _, side := range []string{"-wal", "-shm", "-journal"} {
		_ = os.Rename(filepath.Join(dir, "raw-"+idx+".db"+side), db+side)
	}
	return db, nil
}

// vacuumInto writes one consistent copy of the database at src to dst, reading
// through its journal as SQLite does when it opens a copy after a crash.
func vacuumInto(src, dst string) error {
	db, err := sql.Open("sqlite", fmt.Sprintf("%s?_pragma=busy_timeout(%d)", src, sqliteBusyTimeoutMillis))
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec("VACUUM INTO " + sqliteStringLiteral(dst))
	return err
}

// sqliteIntegrity is the first line of PRAGMA integrity_check on path: "ok" for
// a clean database, the first problem otherwise.
func sqliteIntegrity(path string) string {
	db, err := openSnapshot(path)
	if err != nil {
		return err.Error()
	}
	defer db.Close()
	var verdict string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&verdict); err != nil {
		return err.Error()
	}
	return verdict
}

// countSQLite measures a snapshot the way the sidecar's sqlite3 does: user
// tables, their total rows (-1 above the size cap or when unreadable), and one
// "TBLROW\t<quoted name>\t<rows>" line per table when there are few enough.
func countSQLite(path string) (tables int, rows int64, perTable string) {
	tables, rows = -1, -1
	db, err := openSnapshot(path)
	if err != nil {
		return tables, rows, ""
	}
	defer db.Close()
	names, quoted, err := userTables(db)
	if err != nil {
		return tables, rows, ""
	}
	tables = len(names)
	fi, err := os.Stat(path)
	if err != nil || fi.Size() <= 0 || fi.Size() > dockercli.MaxSQLiteRowCountBytes {
		return tables, rows, ""
	}
	counts := make([]int64, len(names))
	rows = 0
	for i, name := range names {
		if err := db.QueryRow(`SELECT count(*) FROM "` + strings.ReplaceAll(name, `"`, `""`) + `"`).Scan(&counts[i]); err != nil {
			return tables, -1, ""
		}
		rows += counts[i]
	}
	if tables == 0 || tables > dockercli.MaxSQLiteTables {
		return tables, rows, ""
	}
	var lines strings.Builder
	for i := range names {
		fmt.Fprintf(&lines, "TBLROW\t%s\t%d\n", quoted[i], counts[i])
	}
	return tables, rows, lines.String()
}

// userTables lists a database's own tables, with each name as SQLite's quote()
// writes it — the form the per-table counts are keyed by on both sides.
func userTables(db *sql.DB) (names, quoted []string, err error) {
	rs, err := db.Query(`SELECT name, quote(name) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, nil, err
	}
	defer rs.Close()
	for rs.Next() {
		var name, q string
		if err := rs.Scan(&name, &q); err != nil {
			return nil, nil, err
		}
		names, quoted = append(names, name), append(quoted, q)
	}
	return names, quoted, rs.Err()
}

// openSnapshot opens a snapshot for reading only, so measuring it can never
// change the bytes whose checksum the backup records.
func openSnapshot(path string) (*sql.DB, error) {
	return sql.Open("sqlite", path+"?_pragma=query_only(1)")
}

// integrityFailureKind says what a failed integrity check means: corruption of
// the database itself when nothing could write during the copy, otherwise only
// that this copy could not be snapshotted. Pure.
func integrityFailureKind(quiesced bool) string {
	if quiesced {
		return "corrupt"
	}
	return "snapshot"
}

// integrityFailureDetail is what failed.txt says about a failed check. Pure.
func integrityFailureDetail(verdict string, quiesced bool) string {
	if quiesced {
		return verdict
	}
	return "the copy taken while the app was writing did not check clean, which says nothing about the database itself"
}

// sqliteStringLiteral quotes s as an SQL string literal. Pure.
func sqliteStringLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// appendSQLiteLine adds one tab-separated line to a file in dir.
func appendSQLiteLine(dir, file string, fields ...string) {
	f, err := os.OpenFile(filepath.Join(dir, file), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(strings.Join(fields, "\t") + "\n")
}
