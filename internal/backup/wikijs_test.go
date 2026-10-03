package backup

import (
	"strings"
	"testing"
)

// Wiki.js: the notes a restore cannot bring back, and what the profile claims
// (F170–F171).

// F170 — silent by default, specific when it applies. Both queries were run
// against a real Wiki.js 2.5 database.
func TestWikiJSPostRestoreNotes(t *testing.T) {
	notes := postRestoreNotesFor("ghcr.io/linuxserver/wikijs:latest")
	if len(notes) != 2 {
		t.Fatalf("Wiki.js declares two things a restore cannot bring back, got %d", len(notes))
	}
	var search, git PostRestoreNote
	for _, n := range notes {
		switch {
		case strings.Contains(n.SQL, "searchEngines"):
			search = n
		case strings.Contains(n.SQL, "storage"):
			git = n
		}
	}
	if search.SQL == "" || git.SQL == "" {
		t.Fatalf("both notes should be identifiable: %+v", notes)
	}

	// The search note must exclude the DEFAULT engine, or it fires on every
	// ordinary deployment — which is how a note stops being read.
	if !strings.Contains(search.SQL, `key <> 'db'`) {
		t.Errorf("the default database-backed search must not trigger the note: %q", search.SQL)
	}
	if !strings.Contains(search.SQL, `"isEnabled" = true`) {
		t.Error("a configured-but-disabled engine is not active and must not trigger it")
	}
	// The Git note is scoped to the Git module specifically, not any storage.
	if !strings.Contains(git.SQL, `key = 'git'`) {
		t.Errorf("the note is about the Git mirror specifically: %q", git.SQL)
	}

	// Both are SELECTs. Nothing here may write to an application's database.
	for _, n := range notes {
		up := strings.ToUpper(n.SQL)
		if !strings.HasPrefix(up, "SELECT") {
			t.Errorf("a note must be a SELECT: %q", n.SQL)
		}
		for _, forbidden := range []string{"UPDATE ", "DELETE ", "INSERT ", "DROP ", "ALTER ", ";"} {
			if strings.Contains(up, forbidden) {
				t.Errorf("a note must not contain %q: %q", forbidden, n.SQL)
			}
		}
		if n.Engine != "postgres" {
			t.Errorf("the dialect must be declared so it never runs against another engine: %q", n.Engine)
		}
		if n.Note == "" {
			t.Error("a note with nothing to say is not a note")
		}
	}
	// Each says what is missing AND the one action it needs.
	if !strings.Contains(search.Note, "Rebuild it") {
		t.Errorf("the search note should name the action; got %q", search.Note)
	}
	if !strings.Contains(git.Note, "deploy key") {
		t.Errorf("the Git note should name what to check; got %q", git.Note)
	}

	// Nothing else in the registry declares any, so no other restore gains a query.
	if got := postRestoreNotesFor("nginx:1.27"); len(got) != 0 {
		t.Errorf("an ordinary image must declare no notes, got %v", got)
	}
}

// The query is wrapped so only a COUNT crosses back — the database holds user
// accounts and application secrets, and a note needs to know only whether
// something is switched on.
func TestNoteQueryReturnsOnlyACount(t *testing.T) {
	cmd := noteQueryCmd("postgres", `SELECT 1 FROM "searchEngines" WHERE "isEnabled" = true`)
	script := strings.Join(cmd, " ")
	if !strings.Contains(script, "SELECT count(*) FROM (") {
		t.Error("the query must be wrapped in a count")
	}
	// It walks every database, for the same reason the sanity count does: the
	// application's database is one of several and its name is a deployment
	// choice.
	if !strings.Contains(script, "FROM pg_database") || !strings.Contains(script, `-d "$db"`) {
		t.Error("the note must be asked of every connectable database")
	}
	// A missing client is silence, not a failure.
	if !strings.Contains(script, "|| exit 0") {
		t.Error("a missing client must exit cleanly")
	}
	// MySQL gets its own shape; an unknown engine gets nothing at all.
	if noteQueryCmd("mysql", "SELECT 1") == nil {
		t.Error("mysql should be supported")
	}
	if noteQueryCmd("mongodb", "SELECT 1") != nil {
		t.Error("an engine with no shape must produce no command")
	}
}

// An unreadable answer is "no". A note sends somebody to do work, and inventing
// one from an answer that could not be parsed sends them to do work that is not
// needed.
func TestNoteApplies(t *testing.T) {
	cases := map[string]bool{
		"1\n": true, "3\n": true, "0\n": false, "": false,
		"  2  \n": true,
		"ERROR:  relation \"searchEngines\" does not exist\n": false,
		"psql: could not connect\n":                           false,
		"\n\n0\n":                                             false,
		"9999\n":                                              true, // above the cap, still a yes
	}
	for in, want := range cases {
		if got := noteApplies(in); got != want {
			t.Errorf("noteApplies(%q) = %v, want %v", in, got, want)
		}
	}
}

// F171 — the rest of the profile. (Its image matching and one-way migration are
// covered by TestWikiJSProfile in appprofile_test.go; this is what F171 adds.)
func TestWikiJSProfileRestoreShape(t *testing.T) {
	p := ProfileFor("ghcr.io/linuxserver/wikijs:latest")
	if p == nil {
		t.Fatal("Wiki.js must have a profile")
	}
	// Forward migrates, backward is refused — Wiki.js ships no down-migrations.
	if v := AppVersionCompatibility(p, "2.5.314", "2.5.300"); !v.Blocking {
		t.Error("restoring a newer backup into an older Wiki.js must be blocked")
	}
	if v := AppVersionCompatibility(p, "2.5.300", "2.5.314"); v.Blocking || v.Warning == "" {
		t.Error("restoring into a newer Wiki.js must warn, not block")
	}
	// The rendered cache is the only disposable thing here, and it is a CHOICE:
	// everything else — pages, history, users, and the attachments — is in the
	// database.
	if len(p.Regenerable) != 1 || p.Regenerable[0].Path != "/data/cache" {
		t.Errorf("the rendered cache should be an opt-in exclusion, got %+v", p.Regenerable)
	}
	if len(p.NeverBackup) != 0 {
		t.Error("nothing here is worthless enough to exclude without asking")
	}
	// Links are relative, so a move needs nothing — which is the point of the
	// address note, and the reason DockBack does not touch the database for it.
	note := p.Address[0].Note
	if !strings.Contains(note, "admin area") {
		t.Errorf("the address note should point at the application's own settings; got %q", note)
	}
	if p.Address[0].Kind != BindManual {
		t.Error("there is no command-line tool for this, so it must not claim to apply itself")
	}
}
