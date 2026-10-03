package backup

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
)

// Conditional embedded databases, and holding their import to the rows (F166,
// F168).
//
// F126 established that some applications bundle a database server inside their
// own container, and that such a server must be dumped rather than have its data
// directory copied out from under it. That declaration was unconditional: an
// image either bundles a database or it does not.
//
// Some images are both. Uptime Kuma 2 is the case that forces the distinction —
// the same image runs an embedded MariaDB or a plain SQLite file depending on
// one setting, and the MariaDB client is present either way. Handing the SQLite
// deployment a dump command produces a client that exists and a server that does
// not, which is a hard connection failure rather than the "tools are missing"
// signal that degrades gracefully. That would fail the backup of a completely
// healthy application.
//
// So the declaration can carry a probe. When it says no, nothing is lost: the
// ordinary file path finds the SQLite database and snapshots it properly, which
// for that deployment was the right method all along.
//
// The second half is about what a restore proves. The generic contract for this
// engine counts TABLES, which catches a dump that was cut short. It cannot catch
// an import that created every table and filled almost none of them. For an
// application whose entire value is rows — a history of checks, a list of
// monitors — the tables being present says very little.

// embeddedDumpApplies runs the declaration's probe, when it has one (F166).
//
// A declaration with no probe applies unconditionally, which is every case that
// existed before this. A probe that cannot RUN is read as "no", deliberately: an
// application that might be running an embedded server is better captured as
// files than not captured at all, and the file path is never wrong — only
// sometimes less good.
func (e *Engine) embeddedDumpApplies(ctx context.Context, cli *client.Client, containerID string, d *EmbeddedDump, logID, appName string) bool {
	if d == nil {
		return false
	}
	if len(d.When) == 0 {
		return true
	}
	if _, err := dockercli.ExecHook(ctx, cli, containerID, d.When, "", ""); err != nil {
		e.logf(logID, "INFO", "%s is not running its bundled %s server in this deployment — backing up its files instead, which is the right method for the other way it can be configured",
			appName, d.Engine)
		return false
	}
	return true
}

// recordAppTableCounts records the row counts of the application's own key
// tables at capture (F168), so the import can be held to them.
//
// Best-effort: a count that cannot be taken leaves the expectation empty, which
// the restore reads as "nothing to compare" rather than "expected nothing".
// Failing a backup because a supplementary count did not run would trade a real
// backup for a diagnostic.
func (e *Engine) recordAppTableCounts(ctx context.Context, cli *client.Client, containerID string, d *EmbeddedDump, dump *DBDump, logID string) {
	cmd := embeddedMySQLCountCmd(d, tablesOf(d))
	if cmd == nil {
		return
	}
	out, err := dockercli.ExecCapture(ctx, cli, containerID, cmd)
	if err != nil {
		e.logf(logID, "INFO", "Could not record this application's table counts (%v) — the dump is fine; the restore will simply have nothing extra to cross-check against", err)
		return
	}
	counts := ParseTableCounts(string(out))
	if len(counts) == 0 {
		return
	}
	dump.DumpTableRows = counts
	e.logf(logID, "INFO", "Recorded what this dump contains: %s", describeCounts(counts))
}

// tablesOf is the declared table list, or nil.
func tablesOf(d *EmbeddedDump) []string {
	if d == nil {
		return nil
	}
	return d.CountTables
}

// ParseTableCounts reads "<table>|<rows>" lines. Exported so the parse is
// testable without a container; unrecognised lines are ignored.
func ParseTableCounts(out string) map[string]int64 {
	res := map[string]int64{}
	for _, line := range strings.Split(out, "\n") {
		name, cnt, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(cnt), 10, 64)
		if err != nil || n < 0 {
			continue
		}
		res[strings.TrimSpace(name)] = n
	}
	if len(res) == 0 {
		return nil
	}
	return res
}

// describeCounts renders the counts in a stable order for a log line.
func describeCounts(counts map[string]int64) string {
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, n+" "+strconv.FormatInt(counts[n], 10))
	}
	return strings.Join(parts, ", ")
}

// verifyAppTableCounts compares what the restored database holds against what
// capture recorded (F168).
//
// Reuses shortTables, so the shortfall message reads identically to the SQLite
// one: the operator learns WHICH data is missing rather than that something is.
// Same asymmetry too — fewer rows is a failure, more is not, and anything that
// could not be counted produces no verdict at all.
func (e *Engine) verifyAppTableCounts(ctx context.Context, cli *client.Client, targetID string, d *EmbeddedDump, recorded *DBDump, logID string) error {
	if recorded == nil || len(recorded.DumpTableRows) == 0 {
		return nil
	}
	cmd := embeddedMySQLCountCmd(d, tablesOf(d))
	if cmd == nil {
		return nil
	}
	out, err := dockercli.ExecCapture(ctx, cli, targetID, cmd)
	if err != nil {
		e.logf(logID, "WARN", "Could not read the restored table counts back (%v) — the import reported success; check the application's own pages before relying on it", err)
		return nil
	}
	actual := ParseTableCounts(string(out))
	if len(actual) == 0 {
		e.logf(logID, "WARN", "The restored database returned no table counts — the import reported success; check the application's own pages before relying on it")
		return nil
	}
	if short := shortTables(recorded.DumpTableRows, actual); len(short) > 0 {
		e.logf(logID, "ERR", "Database restore verification FAILED: the imported data is short — %s", strings.Join(short, ", "))
		return fmt.Errorf("the imported database is INCOMPLETE — %s; do NOT rely on this restore", strings.Join(short, ", "))
	}
	e.logf(logID, "INFO", "Verified the imported data: %s — matches what was captured", describeCounts(actual))
	return nil
}
