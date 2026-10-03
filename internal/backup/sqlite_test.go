package backup

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSQLiteBackupCmd locks in the shape of the consistent-snapshot command (F22):
// sqlite3 <db> with a busy timeout and a .backup dot-command targeting <db>.dbk,
// with the destination quoted for sqlite3's own parser.
func TestSQLiteBackupCmd(t *testing.T) {
	got := SQLiteBackupCmd("/data/app.db")
	if len(got) != 4 || got[0] != "sqlite3" || got[1] != "/data/app.db" {
		t.Fatalf("unexpected command: %#v", got)
	}
	if got[2] != ".timeout 5000" {
		t.Errorf("expected a busy timeout, got %q", got[2])
	}
	if got[3] != ".backup '/data/app.db.dbk'" {
		t.Errorf("expected a quoted .backup to <db>.dbk, got %q", got[3])
	}

	// A path containing a single quote must stay a single, safe shell literal.
	q := SQLiteBackupCmd("/da'ta/x.db")
	if !strings.Contains(q[3], `'\''`) {
		t.Errorf("single quote not escaped for the shell literal: %q", q[3])
	}
}

// TestSQLiteRestoreCmd is the inverse .restore shape.
func TestSQLiteRestoreCmd(t *testing.T) {
	got := SQLiteRestoreCmd("/data/app.db")
	if len(got) != 4 || got[0] != "sqlite3" || got[3] != ".restore '/data/app.db.dbk'" {
		t.Fatalf("unexpected restore command: %#v", got)
	}
}

// TestSQLiteManifestRoundTrip verifies SQLiteDumps survives a manifest JSON
// round-trip under the documented `sqlite_dumps` key, and is omitted when empty.
func TestSQLiteManifestRoundTrip(t *testing.T) {
	m := Manifest{
		Version:    ManifestVersion,
		TargetName: "sonarr",
		SQLiteDumps: []SQLiteRef{
			{Source: "/config/sonarr.db", Archive: "sqlite/1.dbk", Bytes: 4096},
			{Source: "/config/logs.db", Archive: "sqlite/2.dbk", Bytes: 8192},
		},
	}
	b, err := json.Marshal(&m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"sqlite_dumps"`) {
		t.Fatalf("expected sqlite_dumps key in JSON: %s", b)
	}
	var back Manifest
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.SQLiteDumps) != 2 ||
		back.SQLiteDumps[0].Source != "/config/sonarr.db" ||
		back.SQLiteDumps[0].Archive != "sqlite/1.dbk" ||
		back.SQLiteDumps[1].Bytes != 8192 {
		t.Fatalf("round-trip mismatch: %#v", back.SQLiteDumps)
	}

	// Empty must be omitted so old-reader/no-SQLite backups stay byte-clean.
	empty, _ := json.Marshal(&Manifest{Version: ManifestVersion})
	if strings.Contains(string(empty), "sqlite_dumps") {
		t.Errorf("empty SQLiteDumps must be omitted, got: %s", empty)
	}
}
