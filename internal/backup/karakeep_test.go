package backup

import (
	"strings"
	"testing"

	"dockback/internal/dockercli"
)

// TestKarakeepProfile (F135). Both image names are in circulation — the project
// renamed from Hoarder — and a profile that matched only one would silently skip
// the guard for half the deployments.
func TestKarakeepProfile(t *testing.T) {
	for _, img := range []string{
		"ghcr.io/karakeep-app/karakeep:0.32.0",
		"ghcr.io/hoarder-app/hoarder:0.23.0",
	} {
		p := ProfileFor(img)
		if p == nil || p.Name != "Karakeep" {
			t.Fatalf("%q should match the Karakeep profile, got %+v", img, p)
		}
		if p.OneWayMigration == "" {
			t.Error("Karakeep migrates on start — a downgrade must be blocked")
		}
		if p.DerivedServices == "" {
			t.Error("the Meilisearch index is derived from the database — that must be declared")
		}
	}
	if v := AppVersionCompatibility(ProfileFor("karakeep"), "0.32.0", "0.25.0"); !v.Blocking {
		t.Errorf("a Karakeep downgrade must BLOCK, got %+v", v)
	}
}

// TestKarakeepDerivedServicesSurfaced: said at BACKUP time, because that is where
// the service selection is made — and at restore time, so a Meilisearch that
// comes back empty reads as correct rather than as a gap.
func TestKarakeepDerivedServicesSurfaced(t *testing.T) {
	labels := strings.Join(AutoHookLabels("ghcr.io/karakeep-app/karakeep:0.32.0", false), " ")
	for _, want := range []string{"Meilisearch", "Reindex", "stateless"} {
		if !strings.Contains(labels, want) {
			t.Errorf("the backup-time note should mention %q, got %q", want, labels)
		}
	}
	pre := AppPreconditionsFor(&Manifest{Image: "ghcr.io/karakeep-app/karakeep:0.32.0"})
	if pre == nil || !strings.Contains(strings.Join(pre.Notes, " "), "Meilisearch") {
		t.Errorf("the restore panel must explain the empty search index, got %+v", pre)
	}
	// Ordinary apps gain nothing.
	if got := AutoHookLabels("nginx:alpine", false); len(got) != 0 {
		t.Errorf("an unprofiled image must add nothing, got %v", got)
	}
}

// TestByteIdenticalRestoreVerification (F134) is the strongest claim available,
// and the only one that can prove Karakeep's AI-vs-human tag attribution
// survived: that distinction is a COLUMN VALUE, so a row count sees nothing
// whether it is intact or destroyed.
func TestByteIdenticalRestoreVerification(t *testing.T) {
	var lines []string
	e := &Engine{Log: captureLog(&lines)}
	const good = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	man := &Manifest{SQLiteDumps: []SQLiteRef{{
		Source: "/data/db.db", SHA256: good, Tables: 20, Rows: 8105,
	}}}

	// Byte-identical: passes, and says so in terms an operator can rely on.
	if err := e.assertSQLiteRestored("b1", man, []dockercli.SQLiteRestoreCheck{{
		Path: "/data/db.db", Integrity: "ok", Tables: 20, Rows: 8105, RowsKnown: true, SHA256: good,
	}}); err != nil {
		t.Fatalf("an identical database must pass, got %v", err)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "byte-identical") {
		t.Errorf("the strongest verification should say what it proved, got %v", lines)
	}

	// A different database with the SAME row counts — exactly the shape of an
	// attribution change — must still fail.
	lines = nil
	err := e.assertSQLiteRestored("b1", man, []dockercli.SQLiteRestoreCheck{{
		Path: "/data/db.db", Integrity: "ok", Tables: 20, Rows: 8105, RowsKnown: true,
		SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}})
	if err == nil {
		t.Fatal("a checksum mismatch must fail even when every count matches — that is the whole point")
	}
	if !strings.Contains(err.Error(), "/data/db.db") {
		t.Errorf("the failure must name the database, got %v", err)
	}
}

// TestByteIdenticalFallsBackToCounts: a sidecar that could not hash, or a
// pre-F134 backup, must fall through to the count comparison rather than
// silently skipping verification altogether.
func TestByteIdenticalFallsBackToCounts(t *testing.T) {
	var lines []string
	e := &Engine{Log: captureLog(&lines)}
	man := &Manifest{SQLiteDumps: []SQLiteRef{{Source: "/data/db.db", SHA256: "abc", Tables: 20, Rows: 8105}}}

	// No hash from the sidecar → counts still checked, and a shortfall still fails.
	if err := e.assertSQLiteRestored("b1", man, []dockercli.SQLiteRestoreCheck{{
		Path: "/data/db.db", Integrity: "ok", Tables: 20, Rows: 12, RowsKnown: true,
	}}); err == nil {
		t.Fatal("with no checksum available the count check must still catch a shortfall")
	}
	// A pre-F134 backup (no recorded hash) behaves exactly as before.
	old := &Manifest{SQLiteDumps: []SQLiteRef{{Source: "/data/db.db", Tables: 20, Rows: 8105}}}
	if err := e.assertSQLiteRestored("b1", old, []dockercli.SQLiteRestoreCheck{{
		Path: "/data/db.db", Integrity: "ok", Tables: 20, Rows: 8105, RowsKnown: true, SHA256: "whatever",
	}}); err != nil {
		t.Fatalf("a backup with no recorded checksum must not fail, got %v", err)
	}
}
