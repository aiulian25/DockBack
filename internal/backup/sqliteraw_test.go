package backup

import (
	"strings"
	"testing"
)

// A Git server was paused for its copy, so its database and journal froze at
// one moment — and the log still said the copy "can be torn", and the backup
// was graded down. A frozen copy is crash-consistent; only a copy taken while
// the app ran can be torn.
func TestRawSQLiteCopyIsJudgedByWhetherTheAppCouldWrite(t *testing.T) {
	var lines []string
	e := &Engine{Log: func(_, level, msg string) { lines = append(lines, level+" "+msg) }}

	frozen := &Manifest{}
	e.recordRawSQLite(frozen, 1, false, true, "b1")
	if frozen.SQLiteFallback != "" || frozen.SQLiteCrashConsistent != 1 {
		t.Fatalf("a copy made while the app could not write is no fallback: %+v", frozen)
	}
	if !strings.HasPrefix(lines[0], "INFO ") || strings.Contains(lines[0], "torn") {
		t.Errorf("a frozen copy is noted, not warned about: %q", lines[0])
	}

	live := &Manifest{}
	e.recordRawSQLite(live, 2, false, false, "b2")
	if live.SQLiteFallback == "" || live.SQLiteFallbackCount != 2 || live.SQLiteCrashConsistent != 0 {
		t.Fatalf("a copy made while the app ran is a fallback: %+v", live)
	}
	if !strings.HasPrefix(lines[1], "WARN ") || !strings.Contains(lines[1], "pause the container") {
		t.Errorf("a live copy warns and names the fix: %q", lines[1])
	}

	failed := &Manifest{}
	e.recordRawSQLite(failed, 1, true, true, "b3")
	if failed.SQLiteFallback == "" || failed.SQLiteCrashConsistent != 0 {
		t.Errorf("a snapshot that failed stays a fallback, frozen or not: %+v", failed)
	}
}
