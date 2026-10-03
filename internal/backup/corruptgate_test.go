package backup

import (
	"path/filepath"
	"strings"
	"testing"

	"dockback/internal/store"
)

// TestParseSQLiteFailures pins the distinction the whole gate rests on. Before
// F116 both outcomes were swallowed identically and the raw file shipped, so a
// corrupt database produced a green backup.
func TestParseSQLiteFailures(t *testing.T) {
	got := parseSQLiteFailures(
		"corrupt\t/config/app.db\t*** in database main *** Page 42 is never used\n" +
			"snapshot\t/config/locked.db\tcould not be snapshotted (locked or unreadable)\n" +
			"\n" +
			"nonsense\t/config/x.db\twhatever\n" +
			"corrupt\t\tmissing path\n" +
			"corrupt\t/config/nodetail.db\n")
	if len(got) != 3 {
		t.Fatalf("expected 3 parsed failures, got %d: %+v", len(got), got)
	}
	if got[0].kind != "corrupt" || got[0].path != "/config/app.db" || !strings.Contains(got[0].detail, "Page 42") {
		t.Errorf("first failure parsed wrong: %+v", got[0])
	}
	if got[1].kind != "snapshot" {
		t.Errorf("a snapshot failure must stay distinct from corruption: %+v", got[1])
	}
	// A verdict we don't recognise is dropped rather than guessed at — treating
	// it as corruption would fail backups on a sidecar we don't understand.
	for _, f := range got {
		if f.kind == "nonsense" {
			t.Error("an unknown verdict must be ignored, never interpreted")
		}
	}
	if got[2].path != "/config/nodetail.db" || got[2].detail != "" {
		t.Errorf("a failure with no detail must still parse: %+v", got[2])
	}
	if len(parseSQLiteFailures("")) != 0 {
		t.Error("empty input must parse to no failures")
	}
}

func gateEngine(t *testing.T, failOn string) (*Engine, *[]string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if failOn != "" {
		if err := st.SetSetting(failOnCorruptDBKey, failOn); err != nil {
			t.Fatal(err)
		}
	}
	var lines []string
	return &Engine{Store: st, Log: func(_, level, msg string) { lines = append(lines, level+" "+msg) }}, &lines
}

// TestReportSQLiteFailuresCorrupt: a measured corruption is logged at ERROR and
// recorded in the manifest, so a backup stored with the gate off still declares
// the damage on its own face instead of looking clean.
func TestReportSQLiteFailuresCorrupt(t *testing.T) {
	e, lines := gateEngine(t, "")
	man := &Manifest{}
	corrupt := e.reportSQLiteFailures(man, []sqliteFailure{
		{kind: "corrupt", path: "/config/app.db", detail: "Page 42 is never used"},
		{kind: "snapshot", path: "/config/locked.db", detail: "could not be snapshotted"},
	}, "b1")

	if len(corrupt) != 1 || !strings.Contains(corrupt[0], "/config/app.db") {
		t.Fatalf("only the corrupt database should reach the gate, got %v", corrupt)
	}
	if len(man.CorruptDatabases) != 1 || man.CorruptDatabases[0].Path != "/config/app.db" {
		t.Fatalf("corruption must be recorded in the manifest, got %+v", man.CorruptDatabases)
	}
	var sawErr, sawWarn bool
	for _, l := range *lines {
		if strings.HasPrefix(l, "ERR ") && strings.Contains(l, "app.db") {
			sawErr = true
		}
		if strings.HasPrefix(l, "WARN ") && strings.Contains(l, "locked.db") {
			sawWarn = true
		}
	}
	if !sawErr {
		t.Error("a corrupt database must be logged at ERROR")
	}
	if !sawWarn {
		t.Error("a database that could not be snapshotted must WARN, not ERROR — it says nothing about the data")
	}
	// A snapshot failure is NOT corruption and must not be recorded as such.
	for _, c := range man.CorruptDatabases {
		if c.Path == "/config/locked.db" {
			t.Error("a snapshot failure must never be recorded as corruption")
		}
	}
}

// TestFailOnCorruptDBSetting: default ON, explicitly disable-able. The escape
// hatch matters — the scan finds every SQLite file under the captured mounts,
// including abandoned caches nobody cares about.
func TestFailOnCorruptDBSetting(t *testing.T) {
	e, _ := gateEngine(t, "")
	if !e.failOnCorruptDB() {
		t.Error("the corruption gate must default to ON")
	}
	e, _ = gateEngine(t, "false")
	if e.failOnCorruptDB() {
		t.Error("the gate must be turn-off-able")
	}
	e, _ = gateEngine(t, "true")
	if !e.failOnCorruptDB() {
		t.Error("an explicit true must keep the gate on")
	}
}

// TestReportSQLiteFailuresQuietWhenClean — the overwhelming majority of runs.
func TestReportSQLiteFailuresQuietWhenClean(t *testing.T) {
	e, lines := gateEngine(t, "")
	man := &Manifest{}
	if got := e.reportSQLiteFailures(man, nil, "b1"); len(got) != 0 {
		t.Fatalf("no failures must produce no corruption list, got %v", got)
	}
	if len(man.CorruptDatabases) != 0 {
		t.Error("a clean run must record nothing")
	}
	if len(*lines) != 0 {
		t.Errorf("a clean run must log nothing here, got %v", *lines)
	}
}
