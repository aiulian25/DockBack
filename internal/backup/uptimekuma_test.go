package backup

import (
	"strings"
	"testing"
)

// Uptime Kuma: the conditional embedded database, the non-default socket, and
// holding an import to the rows (F166–F169).

func kumaDump(t *testing.T) *EmbeddedDump {
	t.Helper()
	p := ProfileFor("louislam/uptime-kuma:2")
	if p == nil || p.EmbeddedDump == nil {
		t.Fatal("Uptime Kuma must declare its bundled database")
	}
	return p.EmbeddedDump
}

// F166 — the probe reads the application's OWN configuration, because that is
// the only thing that knows which of the two ways this deployment runs.
func TestUptimeKumaEmbeddedProbe(t *testing.T) {
	d := kumaDump(t)
	if len(d.When) == 0 {
		t.Fatal("the declaration must be conditional — the same image runs two different databases")
	}
	probe := strings.Join(d.When, " ")
	if !strings.Contains(probe, "db-config.json") || !strings.Contains(probe, "embedded-mariadb") {
		t.Errorf("the probe should read the application's own configuration; got %q", probe)
	}
	// A missing configuration file must answer "no" rather than error out.
	if !strings.Contains(probe, "|| exit 1") {
		t.Error("a missing configuration file must be a clean no")
	}
}

// The socket. Without it every connection fails for a reason that reads like an
// authentication problem.
func TestUptimeKumaSocket(t *testing.T) {
	d := kumaDump(t)
	if d.Socket != "/app/data/run/mariadb.sock" {
		t.Errorf("the non-default socket must be declared, got %q", d.Socket)
	}
	cmd := embeddedDumpCommand(d, nil)
	script := strings.Join(cmd, " ")
	if !strings.Contains(script, `SOCK="--socket=/app/data/run/mariadb.sock"`) {
		t.Errorf("the dump must use the declared socket; got %q", script)
	}
	// Both the probe connection and the dump itself.
	if strings.Count(script, "$SOCK") < 2 {
		t.Error("the connection check and the dump must both use the socket")
	}
	// The transactional flag is what makes this safe against a live application.
	if !strings.Contains(script, "--single-transaction") {
		t.Error("the dump must be transactional — that is what removes the downtime")
	}
	// No password anywhere: the local socket is trusted, and a password on argv
	// would show in the host's process list.
	if strings.Contains(script, "-p") && !strings.Contains(script, "--socket") {
		t.Error("no password may reach argv")
	}

	// An engine with no declared socket produces no option at all, rather than an
	// empty one the client would reject.
	plain := embeddedDumpCommand(&EmbeddedDump{Engine: "mysql", DBName: "x"}, nil)
	if !strings.Contains(strings.Join(plain, " "), `SOCK=""`) {
		t.Error("an undeclared socket must yield no option")
	}
}

// F168 — the counts that say the history came back, not just the schema.
func TestUptimeKumaCountTables(t *testing.T) {
	d := kumaDump(t)
	want := map[string]bool{"monitor": true, "heartbeat": true, "notification": true, "user": true, "status_page": true, "api_key": true}
	for _, tb := range d.CountTables {
		delete(want, tb)
	}
	if len(want) != 0 {
		t.Errorf("these tables should be counted: %v", want)
	}

	cmd := embeddedMySQLCountCmd(d, d.CountTables)
	if cmd == nil {
		t.Fatal("a count command should be produced")
	}
	script := strings.Join(cmd, " ")
	for _, tb := range d.CountTables {
		if !strings.Contains(script, "`"+tb+"`") {
			t.Errorf("%q should be counted with a quoted identifier", tb)
		}
	}
	// A missing client is silence, not a failure — a supplementary count must
	// never cost a backup.
	if !strings.Contains(script, "|| exit 0") {
		t.Error("a missing client must exit cleanly")
	}
	// Nothing from the registry may become SQL of its own.
	if got := cleanSQLIdent("monitor; DROP TABLE user--"); got != "monitorDROPTABLEuser" {
		t.Errorf("identifiers must be reduced to identifier characters, got %q", got)
	}
	// An engine with nothing declared produces no command.
	if embeddedMySQLCountCmd(&EmbeddedDump{Engine: "mysql", DBName: "x"}, nil) != nil {
		t.Error("no declared tables means no command")
	}
	if embeddedMySQLCountCmd(&EmbeddedDump{Engine: "postgres", DBName: "x"}, []string{"t"}) != nil {
		t.Error("the count command is MySQL-shaped and must not be produced for another engine")
	}
}

func TestParseTableCounts(t *testing.T) {
	got := ParseTableCounts("monitor|17\nheartbeat|34671\nnotification|1\n")
	if len(got) != 3 || got["monitor"] != 17 || got["heartbeat"] != 34671 {
		t.Fatalf("got %v", got)
	}
	// Noise, negatives and malformed lines are ignored rather than becoming
	// entries — a wrong expectation is worse than a missing one.
	if got := ParseTableCounts("hello\nmonitor|\n|5\nx|-1\n"); got != nil {
		t.Errorf("malformed output must yield nothing, got %v", got)
	}
	if ParseTableCounts("") != nil {
		t.Error("empty output must yield nothing")
	}
}

// The shortfall message is the SQLite one, reused — so an operator learns WHICH
// data is missing rather than that something is.
func TestUptimeKumaShortfallNamesTheTable(t *testing.T) {
	captured := map[string]int64{"monitor": 17, "heartbeat": 34671, "notification": 1}
	restored := map[string]int64{"monitor": 17, "heartbeat": 12, "notification": 1}
	short := shortTables(captured, restored)
	if len(short) != 1 || !strings.Contains(short[0], "heartbeat") {
		t.Fatalf("the shortfall should name heartbeat, got %v", short)
	}
	// More rows than captured is not a shortfall: the application has been
	// running and recording since the import.
	more := map[string]int64{"monitor": 17, "heartbeat": 34999, "notification": 1}
	if got := shortTables(captured, more); len(got) != 0 {
		t.Errorf("a target holding more must not be a shortfall, got %v", got)
	}
}

// F169 — the rest of the profile.
func TestUptimeKumaProfile(t *testing.T) {
	p := ProfileFor("louislam/uptime-kuma:2")
	if p == nil {
		t.Fatal("no profile")
	}
	// The raw data directory is excluded by the dump, not by a NeverBackup entry
	// — and only once the dump has actually succeeded.
	if p.EmbeddedDump.DataDir != "/app/data/mariadb" {
		t.Errorf("the datadir the dump supersedes must be declared, got %q", p.EmbeddedDump.DataDir)
	}
	excluded := map[string]bool{}
	for _, nb := range p.NeverBackup {
		excluded[nb.Path] = true
	}
	for _, want := range []string{"/app/data/run", "/app/data/error.log"} {
		if !excluded[want] {
			t.Errorf("%q should be excluded", want)
		}
	}
	// The things a restore needs must NOT be excluded.
	for _, keep := range []string{"/app/data/db-config.json", "/app/data/upload", "/app/data/screenshots"} {
		if excluded[keep] {
			t.Errorf("%q must be kept", keep)
		}
	}
	// The application's own documented corruption risk.
	if !p.LocalOnlyPath("/app/data/mariadb") {
		t.Error("the data directory must be marked local-disk-only")
	}
	if p.CredentialStore == "" {
		t.Error("an archive holding every notification channel's credentials is a credential store")
	}
	// Forward migrates, backward is refused.
	if v := AppVersionCompatibility(p, "2.1.0", "2.0.0"); !v.Blocking {
		t.Error("restoring a newer backup into an older image must be blocked")
	}
	if v := AppVersionCompatibility(p, "2.0.0", "2.1.0"); v.Blocking || v.Warning == "" {
		t.Error("restoring into a newer image must warn, not block")
	}
	// The move story: monitors are unaffected, two published things are not.
	note := strings.ToLower(p.Address[0].Note)
	for _, want := range []string{"status page", "every monitor keeps checking"} {
		if !strings.Contains(note, want) {
			t.Errorf("the address note should mention %q; got %q", want, note)
		}
	}
}

// The pre-restore panel must carry the local-disk requirement, which is the one
// thing that silently corrupts.
func TestUptimeKumaPreconditions(t *testing.T) {
	pre := AppPreconditionsFor(&Manifest{Image: "louislam/uptime-kuma:2"})
	if pre == nil {
		t.Fatal("Uptime Kuma must have preconditions")
	}
	joined := strings.ToLower(strings.Join(pre.Notes, " "))
	for _, want := range []string{"local disk", "monitor"} {
		if !strings.Contains(joined, want) {
			t.Errorf("preconditions should mention %q; got %q", want, joined)
		}
	}
}
