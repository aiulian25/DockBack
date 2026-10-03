package backup

import (
	"encoding/json"
	"strings"
	"testing"
)

// The parse decides what gets written into a cluster at initdb, where several
// settings can never be corrected afterwards — so every shape it can be handed
// is pinned, and anything it cannot read must produce no value rather than a
// plausible one.
func TestPgClusterParse(t *testing.T) {
	out := strings.Join([]string{
		"S=timezone=Europe/London",
		"S=lc_collate=en_US.utf8",
		"S=max_connections=500",
		// A value containing '=' — split on the FIRST one, never the last.
		"S=search_path=\"$user\", public=extra",
		// Empty value: the setting is set, to nothing. Recorded as such.
		"S=log_line_prefix=",
		"D=UTF8|en_US.utf8|en_US.utf8|postgres",
		"D=UTF8|C|C|webapp",
		// Unreadable lines: a bare row, an unknown tag, a truncated locale row.
		"random noise",
		"X=not-a-tag",
		"D=UTF8|en_US.utf8",
		"",
	}, "\n")

	cfg := ParsePGClusterConfig(out)
	if cfg == nil {
		t.Fatal("well-formed output must produce a config")
	}
	for k, want := range map[string]string{
		"timezone":        "Europe/London",
		"lc_collate":      "en_US.utf8",
		"max_connections": "500",
		"search_path":     `"$user", public=extra`,
		"log_line_prefix": "",
	} {
		got, ok := cfg.Settings[k]
		if !ok || got != want {
			t.Errorf("%s = %q (present=%v), want %q", k, got, ok, want)
		}
	}
	if len(cfg.Settings) != 5 {
		t.Errorf("unreadable lines must contribute nothing, got %d settings: %v", len(cfg.Settings), cfg.Settings)
	}
	if len(cfg.Databases) != 2 {
		t.Fatalf("databases = %+v, want 2 (the truncated row dropped)", cfg.Databases)
	}
	if cfg.Databases[1] != (DBLocale{Name: "webapp", Encoding: "UTF8", Collate: "C", Ctype: "C"}) {
		t.Errorf("locale row parsed wrong: %+v", cfg.Databases[1])
	}
}

// A database name may contain the separator; encoding, collate and ctype cannot.
// That is why the name is emitted LAST and cut from the front.
func TestPgClusterParseNameContainingSeparator(t *testing.T) {
	cfg := ParsePGClusterConfig("D=UTF8|C|C|odd|name")
	if cfg == nil || len(cfg.Databases) != 1 {
		t.Fatalf("want one database, got %+v", cfg)
	}
	if cfg.Databases[0].Name != "odd|name" {
		t.Errorf("name = %q, want the whole remainder %q", cfg.Databases[0].Name, "odd|name")
	}
}

// Nothing understood must read as "nothing recorded", never as "the source had
// nothing set" — the two mean opposite things to a restore.
func TestPgClusterParseNothingUnderstood(t *testing.T) {
	for _, out := range []string{"", "   ", "\n\n", "psql: error: connection to server failed", "S=", "S==novalue"} {
		if cfg := ParsePGClusterConfig(out); cfg != nil {
			t.Errorf("%q must produce no config, got %+v", out, cfg)
		}
	}
}

// The manifest sidecar travels to every destination in plaintext. A replica's
// primary_conninfo literally contains a password, and the *_command settings are
// shell commands that routinely embed one — so their values must never reach it.
func TestPgClusterRedactsCredentialSettings(t *testing.T) {
	cfg := ParsePGClusterConfig(strings.Join([]string{
		"S=primary_conninfo=host=10.0.0.5 user=repl password=hunter2",
		"S=archive_command=wal-g wal-push %p --key AKIAsecret",
		"S=ssl_passphrase_command=/bin/get-key --token abc123",
		"S=restore_command=cp /wal/%f %p",
		"S=timezone=Europe/London",
	}, "\n"))
	if cfg == nil {
		t.Fatal("want a config")
	}
	blob, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"hunter2", "AKIAsecret", "abc123", "10.0.0.5"} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("%q reached the manifest: %s", secret, blob)
		}
	}
	if len(cfg.Settings) != 1 || cfg.Settings["timezone"] != "Europe/London" {
		t.Errorf("only the safe setting may be kept, got %v", cfg.Settings)
	}
	// The NAMES are kept: their presence is worth knowing and is not the secret.
	want := []string{"archive_command", "primary_conninfo", "restore_command", "ssl_passphrase_command"}
	if strings.Join(cfg.Redacted, ",") != strings.Join(want, ",") {
		t.Errorf("redacted = %v, want %v (sorted)", cfg.Redacted, want)
	}
}

func TestRedactedPGSetting(t *testing.T) {
	for _, name := range []string{"primary_conninfo", "archive_command", "ssl_passphrase_command", "restore_command", "PASSWORD_ENCRYPTION"} {
		if !redactedPGSetting(name) {
			t.Errorf("%q must be withheld", name)
		}
	}
	for _, name := range []string{"timezone", "lc_collate", "lc_ctype", "shared_preload_libraries", "max_connections", "data_checksums"} {
		if redactedPGSetting(name) {
			t.Errorf("%q is cluster semantics and must be recorded", name)
		}
	}
}

// The query must ask only for what the source actually SET. Recording a default
// is writing back configuration the source does not have, which is the same
// error as omitting one it does.
func TestPgClusterQueryExcludesDefaultsAndSessionNoise(t *testing.T) {
	for _, src := range []string{"'default'", "'client'", "'session'", "'override'"} {
		if !strings.Contains(pgClusterQuery, src) {
			t.Errorf("query must exclude source %s", src)
		}
	}
	if !strings.Contains(pgClusterQuery, "ORDER BY 1") {
		t.Error("output must be ordered, or the manifest reorders itself between backups")
	}
	if !strings.Contains(pgClusterQuery, "coalesce(setting,'')") {
		t.Error("a NULL setting must not swallow its row")
	}
	if strings.Contains(pgClusterQuery, "`") {
		t.Error("a backtick would terminate the Go raw string this is built from")
	}
}

// The script reaches the server the same way the dump and the import do, so an
// embedded server on its own socket is not silently addressed over TCP.
func TestPgClusterScriptSharesTheClientOptions(t *testing.T) {
	embedded := pgClusterScript(&EmbeddedDump{Engine: "postgres", User: "webapp", Socket: "/tmp"})
	if !strings.Contains(embedded, "-h '/tmp'") || !strings.Contains(embedded, "-U 'webapp'") {
		t.Errorf("must reuse pgClientOpts for an embedded server, got: %s", embedded)
	}
	standalone := pgClusterScript(nil)
	if !strings.Contains(standalone, "-h 127.0.0.1") {
		t.Errorf("a standalone server is reached over TCP, got: %s", standalone)
	}
	for _, flag := range []string{" -X ", " -q ", " -A ", " -t "} {
		if !strings.Contains(embedded, flag) {
			t.Errorf("missing %q — psql must not add output of its own, got: %s", flag, embedded)
		}
	}
}

// ALTER SYSTEM refuses to run inside a transaction block, so each setting is its
// own statement — and each carries a value that came from another machine's
// manifest into a shell and then into SQL. Both layers are pinned here.
func TestClusterReplayStatements(t *testing.T) {
	for _, c := range []struct{ name, value, want string }{
		{"TimeZone", "Europe/London", `ALTER SYSTEM SET TimeZone = 'Europe/London'`},
		{"lc_numeric", "en_US.utf8", `ALTER SYSTEM SET lc_numeric = 'en_US.utf8'`},
		// A numeric setting is still written quoted: PostgreSQL accepts it, and
		// one rule for every value is one rule to get right.
		{"max_connections", "500", `ALTER SYSTEM SET max_connections = '500'`},
		// A value carrying a quote is escaped for SQL by doubling it.
		{"DateStyle", "ISO, 'MDY'", `ALTER SYSTEM SET DateStyle = 'ISO, ''MDY'''`},
		// A namespaced GUC is a legitimate name.
		{"plpgsql.check_asserts", "on", `ALTER SYSTEM SET plpgsql.check_asserts = 'on'`},
	} {
		got, ok := alterSystemStatement(c.name, c.value)
		if !ok || got != c.want {
			t.Errorf("alterSystemStatement(%q,%q) = %q (ok=%v)\n want %q", c.name, c.value, got, ok, c.want)
		}
	}

	// The NAME is validated, never escaped: there is no value in accepting one
	// PostgreSQL could not have produced, and a name is not a place a quote can
	// be made safe.
	for _, name := range []string{
		"", "time zone", "time;DROP", "timezone--", `tz"x`, "tz'x", "1timezone",
		"a.b.c", "$(id)", "tz\nname",
	} {
		if _, ok := alterSystemStatement(name, "x"); ok {
			t.Errorf("%q must be refused as a setting name", name)
		}
	}

	// A crafted value must not escape the SQL literal NOR the shell. The script
	// single-quotes the statement, so a value full of $ and % — which
	// log_line_prefix legitimately is — cannot be expanded.
	stmt, ok := alterSystemStatement("log_line_prefix", `%m [%p] $(whoami) '; DROP DATABASE x; --`)
	if !ok {
		t.Fatal("a legitimate log_line_prefix must be accepted")
	}
	script := pgReplayScript(nil, stmt)
	if strings.Contains(script, "; DROP DATABASE x") && !strings.Contains(script, `'\''`) {
		t.Errorf("the value escaped its quoting: %s", script)
	}
	// Exactly one shell-quoted argument follows -c, and the injected quote is
	// neutralised by the shell escape rather than closing the string.
	if !strings.Contains(script, `-c '`) || !strings.HasSuffix(script, "'") {
		t.Errorf("statement must be one single-quoted shell argument: %s", script)
	}
}

// Replaying everything non-default is unsafe: most of it is what initdb wrote
// for the SOURCE machine, and some of it would break this restore mid-flight.
func TestPgReplayAllowList(t *testing.T) {
	for _, name := range []string{"TimeZone", "log_timezone", "DateStyle", "lc_numeric", "default_text_search_config"} {
		if !pgReplaySettings[strings.ToLower(name)] {
			t.Errorf("%q changes how identical data renders or sorts and must be replayed", name)
		}
	}
	for _, name := range []string{
		"shared_buffers",          // sized for the source's memory
		"max_connections",         // can exceed the target's shared memory
		"listen_addresses",        // the target's networking, not the source's
		"unix_socket_directories", // would move the socket this import connects through
		"dynamic_shared_memory_type",
		"data_checksums",           // read-only: fixed at initdb
		"shared_preload_libraries", // a bad value stops the server starting, and
		// ALTER SYSTEM RESET needs a RUNNING server — the retraction is
		// impossible in exactly the case it would be needed.
	} {
		if pgReplaySettings[strings.ToLower(name)] {
			t.Errorf("%q must not be replayed onto another machine", name)
		}
	}
}

// Encoding, collation and ctype are fixed when a database is created. The
// restore cannot fix a mismatch, so its whole job is to say so precisely.
func TestLocaleMismatch(t *testing.T) {
	captured := []DBLocale{
		{Name: "template1", Encoding: "UTF8", Collate: "en_US.utf8", Ctype: "en_US.utf8"},
		{Name: "app", Encoding: "UTF8", Collate: "en_US.utf8", Ctype: "en_US.utf8"},
	}
	// Same identity: nothing to report.
	if got := localeMismatch(captured, DBLocale{Encoding: "UTF8", Collate: "en_US.utf8", Ctype: "en_US.utf8"}); len(got) != 0 {
		t.Errorf("a matching cluster must report nothing, got %v", got)
	}
	// Collation differs — the one that silently reorders text indexes.
	got := localeMismatch(captured, DBLocale{Encoding: "UTF8", Collate: "C", Ctype: "C"})
	if len(got) != 2 {
		t.Fatalf("want collation and ctype reported for the user database only, got %v", got)
	}
	for _, want := range []string{"app", "collation", "en_US.utf8", "C"} {
		if !strings.Contains(strings.Join(got, " "), want) {
			t.Errorf("the report must name %q: %v", want, got)
		}
	}
	// The cluster's own templates are not user data and are not compared.
	if strings.Contains(strings.Join(got, " "), "template1") {
		t.Errorf("template databases must not be reported: %v", got)
	}
	// A comparison that could not be made must not read as one that passed.
	if got := localeMismatch(captured, DBLocale{}); got != nil {
		t.Errorf("an unknown target locale must produce no verdict, got %v", got)
	}
}

// The replay reads what the capture wrote, through the same parser, so the two
// halves of this feature cannot disagree about the format.
func TestPgClusterConfigForFindsThePostgresDump(t *testing.T) {
	cfg := &ClusterConfig{Settings: map[string]string{"TimeZone": "Europe/London"}}
	man := &Manifest{Databases: []DBDump{
		{Engine: "mysql"},
		{Engine: "postgres", ClusterConfig: cfg},
	}}
	if got := pgClusterConfigFor(man); got != cfg {
		t.Errorf("must find the postgres dump's config, got %+v", got)
	}
	// A backup taken before the capture existed carries none, which a restore
	// must read as "nothing to replay", never as "the source had nothing set".
	if got := pgClusterConfigFor(&Manifest{Databases: []DBDump{{Engine: "postgres"}}}); got != nil {
		t.Errorf("a pre-capture backup has no config, got %+v", got)
	}
	if got := pgClusterConfigFor(nil); got != nil {
		t.Errorf("a nil manifest has no config, got %+v", got)
	}
}

func TestPGPreloadLibsSplitsQuotedAndSpacedEntries(t *testing.T) {
	got := pgPreloadLibs(` pg_stat_statements, "auto_explain" ,'vectors', `)
	want := []string{"pg_stat_statements", "auto_explain", "vectors"}
	if len(got) != len(want) {
		t.Fatalf("split %d entries, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
	if len(pgPreloadLibs("")) != 0 || len(pgPreloadLibs("  ,  ")) != 0 {
		t.Error("an empty or all-separator value must yield no libraries, or a restore would set an empty preload list")
	}
}

func TestPGLibMissingSeparatesAnUnusableFileFromARefusalToLoad(t *testing.T) {
	// Every one of these means preloading it would stop the server starting.
	for _, out := range []string{
		`ERROR:  could not access file "vectors": No such file or directory`,
		`ERROR:  incompatible library "/usr/lib/postgresql/16/lib/foo.so": version mismatch`,
		`ERROR:  could not load library "x.so": undefined symbol: pg_finfo`,
	} {
		if !pgLibMissing(out) {
			t.Errorf("must treat as unloadable, so it is never preloaded: %q", out)
		}
	}
	// These mean the library IS here — it loaded far enough to object. Treating
	// them as missing would silently drop a library the target can preload fine.
	for _, out := range []string{
		"",
		"LOAD",
		`ERROR:  pg_stat_statements must be loaded via "shared_preload_libraries"`,
	} {
		if pgLibMissing(out) {
			t.Errorf("must treat as present: %q", out)
		}
	}
}

func TestPGLibNameRejectsAnythingThatCouldEscapeTheProbe(t *testing.T) {
	for _, ok := range []string{"pg_stat_statements", "$libdir/vectors", "timescaledb-2.14", "auto_explain"} {
		if !pgLibName.MatchString(ok) {
			t.Errorf("real library name rejected: %q", ok)
		}
	}
	for _, bad := range []string{
		`x'; CREATE TABLE pwned(i int); LOAD 'y`, // the manifest came from another machine
		`a b`, `foo;bar`, `$(id)`, "`id`", `../../etc/passwd`, `'`, ``,
	} {
		if pgLibName.MatchString(bad) {
			t.Errorf("must never reach a LOAD statement: %q", bad)
		}
	}
}

func TestPGDataDirPrefersTheApplicationsOwnDirectory(t *testing.T) {
	if got := pgDataDir(&EmbeddedDump{DataDir: "/config/database"}); got != "/config/database" {
		t.Errorf("embedded cluster data dir = %q, want the profile's", got)
	}
	// The standalone restore path passes nil; the rescue still has to find
	// postgresql.auto.conf or it cannot un-strand the container.
	if got := pgDataDir(nil); got != "/var/lib/postgresql/data" {
		t.Errorf("standalone data dir = %q, want the engine default", got)
	}
}
