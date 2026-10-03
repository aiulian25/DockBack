package backup

import (
	"errors"
	"strings"
	"testing"
)

// TestDumpCommandToolGuards locks the contract that the dump scripts detect an
// ABSENT client (a container misdetected as a DB — e.g. an app image whose tag
// merely contains "postgres"/"mysql", like CommaFeed's `-postgres` tag) and exit
// 127 ("not found") so the engine falls back to a file backup, while reserving
// exit 3 for "tools present but credentials can't connect" (loud failure).
func TestDumpCommandToolGuards(t *testing.T) {
	pg := strings.Join(dbDumpCommand("postgres", nil, nil), " ")
	for _, want := range []string{"command -v psql", "command -v pg_dumpall", "exit 127", "exit 3"} {
		if !strings.Contains(pg, want) {
			t.Errorf("postgres dump script missing %q:\n%s", want, pg)
		}
	}
	my := strings.Join(dbDumpCommand("mysql", nil, nil), " ")
	for _, want := range []string{`command -v "$CLI"`, "exit 127", "exit 3"} {
		if !strings.Contains(my, want) {
			t.Errorf("mysql dump script missing %q:\n%s", want, my)
		}
	}
}

// TestDumpToolMissingContract is the crux of the regression: exit-127/"not found"
// must trigger the graceful file-backup fallback, but an exit-3 auth failure must
// NOT — a real database with bad credentials has to fail loudly, never silently
// degrade to a file copy of its raw data dir.
func TestDumpToolMissingContract(t *testing.T) {
	if !dumpToolMissing(errors.New(`command "sh -c ..." exited 127: psql: not found`)) {
		t.Error("exit 127 / not found should be treated as a missing tool (fallback to files)")
	}
	if dumpToolMissing(errors.New(`command "sh -c ..." exited 3: DockBack: could not connect to Postgres`)) {
		t.Error("exit 3 auth failure must NOT be treated as a missing tool — it must fail loudly")
	}
}
