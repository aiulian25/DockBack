package backup

import (
	"strings"
	"testing"
	"time"

	"dockback/internal/dockercli"
)

// Radicale: interrupted-write leftovers and the storage-compatibility statement
// (F157–F158).

func radicalePattern(t *testing.T) StagingDebrisPattern {
	t.Helper()
	p := ProfileFor("tomsquest/docker-radicale:latest")
	if p == nil || len(p.StagingDebris) == 0 {
		t.Fatal("Radicale must declare a staging-debris pattern")
	}
	return p.StagingDebris[0]
}

// F157 — the finding names the paths, what they cost, and how to remove them.
func TestStagingDebrisFindings(t *testing.T) {
	pat := radicalePattern(t)
	old := time.Date(2025, 7, 1, 12, 0, 0, 0, time.UTC)
	found := []dockercli.StaleDir{
		{Path: "/data/collections/collection-root/alice/.Radicale.tmp-lav1yxff", ModTime: old, Bytes: 3_400_000},
		{Path: "/data/collections/collection-root/alice/.Radicale.tmp-upk0gs4m", ModTime: old.Add(time.Hour), Bytes: 3_100_000},
	}
	lines := stagingDebrisFindings(found, pat)
	if len(lines) != 1 {
		t.Fatalf("one finding for the set, not one per directory: %v", lines)
	}
	msg := lines[0]
	for _, want := range []string{
		"2 leftover staging directories",
		".Radicale.tmp-lav1yxff",
		".Radicale.tmp-upk0gs4m",
		"1 July 2025",   // the oldest, so the age is legible
		"never deletes", // the promise this feature rests on
		"verified",      // and the precondition for doing it by hand
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the finding should contain %q; got %q", want, msg)
		}
	}
	// The size is what makes it actionable — it is being paid on every backup.
	if !strings.Contains(msg, "MB") {
		t.Errorf("the finding should say what it costs; got %q", msg)
	}

	// Nothing found says nothing at all.
	if got := stagingDebrisFindings(nil, pat); len(got) != 0 {
		t.Errorf("a clean tree must be silent, got %v", got)
	}
}

// A long list is summarised rather than scrolled — a warning nobody finishes
// reading is not a warning.
func TestStagingDebrisFindingsBounded(t *testing.T) {
	var found []dockercli.StaleDir
	for i := 0; i < 12; i++ {
		found = append(found, dockercli.StaleDir{Path: "/data/.Radicale.tmp-" + strings.Repeat("x", i+1), Bytes: 1024})
	}
	msg := stagingDebrisFindings(found, radicalePattern(t))[0]
	if !strings.Contains(msg, "12 leftover staging directories") || !strings.Contains(msg, "and 8 more") {
		t.Errorf("the count should be exact and the list bounded; got %q", msg)
	}
}

// What the archive records: absolute times, not ages — an age written into an
// archive is wrong the moment the archive is a day old.
func TestStaleStagingRecord(t *testing.T) {
	pat := radicalePattern(t)
	mod := time.Date(2025, 7, 1, 12, 0, 0, 0, time.UTC)
	got := staleStagingOf([]dockercli.StaleDir{{Path: "/data/x/.Radicale.tmp-a", ModTime: mod, Bytes: 2048}}, pat)
	if len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	if got[0].ModifiedAt != "2025-07-01T12:00:00Z" {
		t.Errorf("an absolute RFC3339 time is recorded, got %q", got[0].ModifiedAt)
	}
	if got[0].Bytes != 2048 || got[0].What == "" {
		t.Errorf("size and description must be carried: %+v", got[0])
	}
	// An unreadable time is left empty rather than defaulted to the epoch, which
	// would read as a directory from 1970.
	if g := staleStagingOf([]dockercli.StaleDir{{Path: "/x"}}, pat); g[0].ModifiedAt != "" {
		t.Errorf("an unknown time must stay unknown, got %q", g[0].ModifiedAt)
	}
}

// The backup-level view, for the list and the runbook.
func TestStaleStagingFindings(t *testing.T) {
	man := &Manifest{StaleStaging: []StaleStagingDir{
		{Path: "/data/x/.Radicale.tmp-a", Bytes: 1_000_000, What: "leftover staging directories from writes that were interrupted"},
		{Path: "/data/x/.Radicale.tmp-b", Bytes: 2_000_000},
	}}
	got := StaleStagingFindings(man)
	if len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	for _, want := range []string{"carries 2", "the backup is sound", "smaller"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("should contain %q; got %q", want, got[0])
		}
	}
	// Nothing recorded, nothing said — which is every other backup.
	if got := StaleStagingFindings(&Manifest{}); len(got) != 0 {
		t.Errorf("a clean backup must be silent, got %v", got)
	}
	if got := StaleStagingFindings(nil); len(got) != 0 {
		t.Errorf("a nil manifest must be silent, got %v", got)
	}
}

// The age threshold is the whole reason this is a finding rather than noise: a
// staging directory made moments ago is a write in progress.
func TestStagingDebrisThreshold(t *testing.T) {
	pat := radicalePattern(t)
	if pat.MinAge < time.Hour {
		t.Errorf("the threshold must be long enough that an active write is never reported, got %v", pat.MinAge)
	}
	if pat.Glob != ".Radicale.tmp-*" {
		t.Errorf("unexpected glob %q", pat.Glob)
	}
	if pat.Cleanup == "" || pat.What == "" {
		t.Error("a finding has to say what it is and what to do about it")
	}
}

func TestParseStaleDirs(t *testing.T) {
	// Exactly what a real busybox sidecar printed.
	out := "STALE\t/data/collections/collection-root/user/.Radicale.tmp-old\t1785356808\t4\n"
	got := dockercli.ParseStaleDirs(out)
	if len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	if got[0].Path != "/data/collections/collection-root/user/.Radicale.tmp-old" {
		t.Errorf("path: %q", got[0].Path)
	}
	if got[0].Bytes != 4096 {
		t.Errorf("du reports kibibytes; expected 4096 bytes, got %d", got[0].Bytes)
	}
	if got[0].ModTime.IsZero() {
		t.Error("the modification time should be parsed")
	}
	// Noise and malformed lines are ignored rather than becoming entries.
	if got := dockercli.ParseStaleDirs("hello\nSTALE\tonly-two\nSTALE\t\t1\t2\n"); len(got) != 0 {
		t.Errorf("malformed output must yield nothing, got %v", got)
	}
}

// F158 — the compatibility statement, and where it surfaces.
func TestRadicaleStorageCompat(t *testing.T) {
	compat := StorageCompatFor("tomsquest/docker-radicale:latest")
	if compat == "" {
		t.Fatal("Radicale must state where its format compatibility ends")
	}
	for _, want := range []string{"3", "2"} {
		if !strings.Contains(compat, want) {
			t.Errorf("it should name both the safe range and the edge; got %q", compat)
		}
	}
	// An application with nothing to say says nothing, so the drift note is
	// unchanged for everything else.
	if got := StorageCompatFor("nginx:1.27"); got != "" {
		t.Errorf("an ordinary image must declare none, got %q", got)
	}
	// And it reaches the pre-restore panel.
	pre := AppPreconditionsFor(&Manifest{Image: "tomsquest/docker-radicale:latest"})
	if pre == nil || !strings.Contains(strings.Join(pre.Notes, " "), "stable across all of version 3") {
		t.Errorf("the compatibility statement must reach the pre-restore notes: %+v", pre)
	}
}

// The profile's other claims: what the archive holds, and what a move costs.
func TestRadicaleProfile(t *testing.T) {
	p := ProfileFor("tomsquest/docker-radicale:latest")
	if p == nil {
		t.Fatal("Radicale must have a profile")
	}
	if p.CredentialStore == "" {
		t.Error("an archive holding calendars and password hashes is a credential store")
	}
	// The password-hash file's permissions are audited (F147).
	var users *SecretFile
	for i := range p.SecretFiles {
		if strings.HasSuffix(p.SecretFiles[i].Path, "/users") {
			users = &p.SecretFiles[i]
		}
	}
	if users == nil {
		t.Fatal("the password-hash file's permissions must be audited")
	}
	if users.MaxMode != 0o640 {
		t.Errorf("a file of hashes should not be world-readable, got max mode %o", users.MaxMode)
	}
	// Radicale 3.x restores in both directions, so nothing may be blocked.
	if p.OneWayMigration != "" {
		t.Error("Radicale's format is stable across 3.x — a downgrade must not be blocked")
	}
	if v := AppVersionCompatibility(p, "3.7.6", "3.7.1"); v.Blocking {
		t.Error("restoring 3.7.6 into 3.7.1 is format-compatible and must not be blocked")
	}
	// The move story: a domain follows, an IP does not.
	joined := strings.ToLower(p.Address[0].Note + " " + p.Address[0].Symptom)
	for _, want := range []string{"domain", "ip address", "paths come back"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the address note should mention %q; got %q", want, joined)
		}
	}
}

// The backup page says how this application is captured, before the choice is
// made.
func TestRadicaleAdvertisesStagingBehaviour(t *testing.T) {
	joined := strings.Join(AutoHookLabels("tomsquest/docker-radicale:latest", false), " | ")
	for _, want := range []string{"staging", "paused copy", "never removes"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the label should mention %q; got %q", want, joined)
		}
	}
	// Nothing is advertised for an image that does not stage.
	if got := strings.Join(AutoHookLabels("nginx:1.27", false), " | "); strings.Contains(got, "staging") {
		t.Errorf("an ordinary image must not get this label, got %q", got)
	}
}
