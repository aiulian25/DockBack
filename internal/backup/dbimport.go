package backup

import (
	"strconv"
	"strings"
)

// Database-import error detection.
//
// A database client's EXIT CODE is not a verdict on whether the dump applied.
// `psql` without ON_ERROR_STOP keeps going after a failed statement and still
// exits 0; `mysql --force` does the same. So a dump whose
// `ALTER TABLE … ADD CONSTRAINT` statements failed would import "successfully"
// and leave a database with rows but no constraints — which surfaces much later
// as, e.g., "there is no unique constraint matching given keys for referenced
// table …" during the app's next migration.
//
// We deliberately do NOT just set ON_ERROR_STOP=1: our Postgres dumps begin with
// `pg_dumpall --globals-only`, whose CREATE ROLE statements routinely and
// harmlessly fail with "already exists" when restoring onto a cluster that
// already has those roles. Aborting there would break restores that are fine.
//
// Instead the client is allowed to run to completion and its output is CLASSIFIED:
// the benign "already exists" class is counted and ignored; anything else fails
// the restore loudly, naming the actual statements that did not apply.

// maxReportedImportErrors bounds how many failing statements are echoed into the
// error/log line — enough to diagnose, not a wall of text.
const maxReportedImportErrors = 5

// ImportOutcome is the classification of one dump import's client output.
type ImportOutcome struct {
	Fatal  []string // error lines that mean the dump did NOT fully apply
	Benign int      // ignorable "already exists" errors (roles/databases/etc.)
}

// OK reports whether every statement that matters applied.
func (o ImportOutcome) OK() bool { return len(o.Fatal) == 0 }

// Summary renders the fatal errors for a log line / returned error, capped.
func (o ImportOutcome) Summary() string {
	n := len(o.Fatal)
	if n == 0 {
		return ""
	}
	shown := o.Fatal
	if len(shown) > maxReportedImportErrors {
		shown = shown[:maxReportedImportErrors]
	}
	s := strings.Join(shown, "; ")
	if n > len(shown) {
		s += "; … and " + strconv.Itoa(n-len(shown)) + " more"
	}
	return s
}

// benignImportError reports whether an error line is one of the known-harmless
// ones a correct restore still produces. Kept deliberately NARROW: only
// "already exists" on cluster-level objects that our own dump preamble
// re-creates (roles, databases, schemas, extensions). An "already exists" on a
// TABLE is NOT benign — our dumps are --clean --if-exists, so a table that
// still exists means the drop didn't happen and the import is unsound.
func benignImportError(line string) bool {
	l := strings.ToLower(line)
	if !strings.Contains(l, "already exists") {
		return false
	}
	for _, obj := range []string{"role", "database", "schema", "extension", "language", "user"} {
		if strings.Contains(l, obj+" \"") || strings.Contains(l, obj+" '") {
			return true
		}
	}
	// Postgres phrases some as: ERROR:  role "x" already exists
	return false
}

// mongoRestoreFailure reports whether a mongorestore line describes documents
// that did NOT restore (F99).
//
// mongorestore is chatty and exits 0 even when documents fail, so the count is
// the verdict — and it must be read as a NUMBER, not a substring: every
// successful restore also prints "Failed: 0" and "0 document(s) failed to
// restore", and treating those as errors would fail every MongoDB restore.
func mongoRestoreFailure(line string) bool {
	for _, marker := range []string{"Failed:", "document(s) failed to restore", "documents failed to restore"} {
		i := strings.Index(line, marker)
		if i < 0 {
			continue
		}
		var digits string
		if strings.HasPrefix(marker, "Failed") {
			digits = leadingInt(strings.TrimSpace(line[i+len(marker):]))
		} else {
			digits = trailingInt(strings.TrimSpace(line[:i]))
		}
		if n, err := strconv.Atoi(digits); err == nil && n > 0 {
			return true
		}
	}
	return false
}

// leadingInt returns the run of digits at the start of s ("" when none).
func leadingInt(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return s[:i]
		}
	}
	return s
}

// trailingInt returns the run of digits at the end of s ("" when none).
func trailingInt(s string) string {
	for i := len(s); i > 0; i-- {
		if s[i-1] < '0' || s[i-1] > '9' {
			return s[i:]
		}
	}
	return s
}

// ClassifyImportOutput splits a database client's combined output into fatal and
// benign errors. Pure — the whole verdict is unit-testable without a container.
//
// postgres : psql prints `ERROR:  <message>` (NOTICE/WARNING/DETAIL are not errors).
// mysql    : the client prints `ERROR <code> (<state>) at line N: <message>`.
// mongodb  : mongorestore reports per-collection `Failed: N` and a closing
//
//	"N document(s) failed to restore" — both benign at zero, and both
//	a silent data loss above it, since mongorestore still exits 0 (F99).
//
// redis    : redis-cli prefixes a server rejection with `(error)` or `-ERR`.
func ClassifyImportOutput(engine, out string) ImportOutcome {
	var o ImportOutcome
	if out == "" {
		return o
	}
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		isErr := false
		switch engine {
		case "postgres":
			// "ERROR:  relation ... does not exist" — psql uses a two-space gap.
			isErr = strings.HasPrefix(line, "ERROR:") || strings.Contains(line, "psql:") && strings.Contains(line, "ERROR:")
		case "mysql":
			isErr = strings.HasPrefix(line, "ERROR ") || strings.Contains(line, "] ERROR ")
		case "mongodb":
			isErr = mongoRestoreFailure(line)
		case "redis":
			isErr = strings.HasPrefix(line, "(error)") || strings.HasPrefix(line, "-ERR") ||
				strings.Contains(line, " -ERR ")
		default:
			continue // exit-code-only engines
		}
		if !isErr {
			continue
		}
		if benignImportError(line) {
			o.Benign++
			continue
		}
		o.Fatal = append(o.Fatal, line)
	}
	return o
}
