package backup

import (
	"strings"
	"testing"
)

// TestMealieProfile (F139).
func TestMealieProfile(t *testing.T) {
	p := ProfileFor("ghcr.io/mealie-recipes/mealie:latest")
	if p == nil || p.Name != "Mealie" {
		t.Fatalf("expected the Mealie profile, got %+v", p)
	}
	if p.OneWayMigration == "" {
		t.Error("Mealie runs Alembic migrations on start — a downgrade must be blocked")
	}
	if v := AppVersionCompatibility(p, "3.22.0", "3.10.0"); !v.Blocking {
		t.Errorf("a Mealie downgrade must BLOCK, got %+v", v)
	}
	// BASE_URL is read at runtime, so it is an env change and nothing in the
	// database is rewritten — the note must say so, or an operator will look for
	// a content rewrite that does not exist.
	var env *AddressBinding
	for i := range p.Address {
		if p.Address[i].Kind == BindEnv {
			env = &p.Address[i]
		}
	}
	if env == nil || len(env.Keys) != 1 || env.Keys[0] != "BASE_URL" {
		t.Fatalf("BASE_URL must be an env binding, got %+v", env)
	}
	if !strings.Contains(env.Note, "nothing in the database needs rewriting") {
		t.Errorf("the note should rule out a data rewrite, got %q", env.Note)
	}
}

// TestMealieNeverBackup (F138): ~37 MB of a 93 MB archive was rotated logs.
func TestMealieNeverBackup(t *testing.T) {
	p := ProfileFor("mealie")
	if len(p.NeverBackup) == 0 {
		t.Fatal("Mealie's rotated logs dominated its archive — they must be excluded")
	}
	var sawLogs, sawTemp bool
	for _, nb := range p.NeverBackup {
		if nb.Why == "" {
			t.Errorf("every excluded path must say what it is, got %+v", nb)
		}
		if !strings.HasPrefix(nb.Path, "/") {
			t.Errorf("paths must be absolute inside the container, got %q", nb.Path)
		}
		if strings.Contains(nb.Path, "mealie.log") {
			sawLogs = true
		}
		if strings.Contains(nb.Path, ".temp") {
			sawTemp = true
		}
	}
	if !sawLogs || !sawTemp {
		t.Errorf("expected the rotated logs and the scratch dir, got %+v", p.NeverBackup)
	}
	// It must NOT be conflated with Regenerable: that is opt-in and defaults to
	// keeping the data, which would be wrong for logs.
	if len(p.Regenerable) != 0 {
		t.Errorf("logs are not 'regenerable data worth keeping' — they must not be offered as a toggle, got %+v", p.Regenerable)
	}
	// And ordinary apps exclude nothing implicitly.
	for _, img := range []string{"nginx:alpine", "postgres:16"} {
		if q := ProfileFor(img); q != nil && len(q.NeverBackup) > 0 {
			t.Errorf("%q must not silently exclude anything", img)
		}
	}
}

// TestNeverBackupAlwaysApplies: no toggle, and it combines with the other two
// exclusion sources rather than replacing them.
func TestNeverBackupAlwaysApplies(t *testing.T) {
	o := Options{
		embeddedDataDir:    "/config/postgres",
		excludeRegenerable: []RegenerablePath{{Path: "/config/data/trickplay"}},
		neverBackup:        []string{"/app/data/mealie.log*", "/app/data/.temp"},
	}
	got := o.excludeSubPaths()
	if len(got) != 4 {
		t.Fatalf("all three sources must combine, got %v", got)
	}
	// Junk alone, with no dump and no operator choice, still applies.
	only := Options{neverBackup: []string{"/app/data/mealie.log*"}}
	if g := only.excludeSubPaths(); len(g) != 1 || g[0] != "/app/data/mealie.log*" {
		t.Errorf("a never-backup path must apply unconditionally, got %v", g)
	}
	// Nothing declared, nothing excluded — the ordinary container's capture is
	// byte-for-byte what it was.
	var none Options
	if g := none.excludeSubPaths(); len(g) != 0 {
		t.Errorf("an ordinary container must exclude nothing, got %v", g)
	}
}

// TestMealieZipLabel records the report's step-5 decision where an operator will
// see it: the in-app ZIP is a portable extra, not the restore path.
func TestMealieZipLabel(t *testing.T) {
	labels := strings.Join(AutoHookLabels("ghcr.io/mealie-recipes/mealie:latest", false), " ")
	for _, want := range []string{"captured directly", "version-sensitive", "not the restore path"} {
		if !strings.Contains(labels, want) {
			t.Errorf("the label should say %q, got %q", want, labels)
		}
	}
	if got := AutoHookLabels("nginx:alpine", false); len(got) != 0 {
		t.Errorf("an unprofiled image must add nothing, got %v", got)
	}
}
