package backup

import (
	"path/filepath"
	"strings"
	"testing"

	"dockback/internal/store"
)

// TestJellyfinProfile (F133). Jellyfin's two cross-machine risks: a migrated
// database an older server cannot open, and absolute media paths in libraries
// AND playlists that only resolve if the target mounts the shares identically.
func TestJellyfinProfile(t *testing.T) {
	p := ProfileFor("jellyfin/jellyfin:latest")
	if p == nil || p.Name != "Jellyfin" {
		t.Fatalf("expected the Jellyfin profile, got %+v", p)
	}
	if p.OneWayMigration == "" {
		t.Error("a migrated Jellyfin database will not open on an older server — a downgrade must be blocked")
	}
	if v := AppVersionCompatibility(p, "10.10.3", "10.8.13"); !v.Blocking {
		t.Errorf("a Jellyfin downgrade must BLOCK, got %+v", v)
	}
	if p.PathEmbedding == "" {
		t.Error("libraries and playlists store absolute media paths — that must be declared")
	}
	if !strings.Contains(p.PathEmbedding, "playlist") {
		t.Errorf("playlists are the part people forget — the note should say so: %q", p.PathEmbedding)
	}
}

// TestJellyfinRegenerable: trickplay is 11 GB of a 12 GB archive, and it lives
// INSIDE the /config mount — so mount-level selection cannot express it and the
// only choices without this were "back up 12 GB" or "back up nothing".
func TestJellyfinRegenerable(t *testing.T) {
	regen := RegenerablePathsFor("jellyfin/jellyfin:latest")
	if len(regen) != 1 {
		t.Fatalf("expected one regenerable directory, got %+v", regen)
	}
	r := regen[0]
	if !strings.Contains(r.Path, "trickplay") {
		t.Errorf("path = %q, want the trickplay directory", r.Path)
	}
	// The cost has to be stated, or the trade is being made blind.
	if r.Label == "" || r.Cost == "" {
		t.Errorf("a regenerable path must carry a label and its regeneration cost, got %+v", r)
	}
	if !strings.HasPrefix(r.Path, "/") {
		t.Errorf("the path must be absolute inside the container, got %q", r.Path)
	}
	// Ordinary images declare none, so the option is never offered for them.
	for _, img := range []string{"nginx:alpine", "postgres:16", ""} {
		if got := RegenerablePathsFor(img); len(got) != 0 {
			t.Errorf("%q must declare nothing regenerable, got %v", img, got)
		}
	}
}

// TestExcludeRegenerableSetting: default OFF. Fidelity first — the size trade is
// the operator's explicit choice, never something that quietly happens to their
// backup because DockBack decided a directory looked disposable.
func TestExcludeRegenerableSetting(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := &Engine{Store: st, Log: func(string, string, string) {}}

	if e.ExcludeRegenerable("n1", "jellyfin") {
		t.Fatal("the default must be to capture everything")
	}
	if err := e.SetExcludeRegenerable("n1", "jellyfin", true); err != nil {
		t.Fatal(err)
	}
	if !e.ExcludeRegenerable("n1", "jellyfin") {
		t.Error("the choice must persist")
	}
	if err := e.SetExcludeRegenerable("n1", "jellyfin", false); err != nil {
		t.Fatal(err)
	}
	if e.ExcludeRegenerable("n1", "jellyfin") {
		t.Error("turning it back off must restore the full capture")
	}
	// Per container, not global.
	_ = e.SetExcludeRegenerable("n1", "jellyfin", true)
	if e.ExcludeRegenerable("n1", "plex") {
		t.Error("the setting must be per container")
	}
}

// TestExcludeSubPathsCombines: the embedded-database exclusion (F126) and the
// regenerable exclusion (F132) are independent and must both apply.
func TestExcludeSubPathsCombines(t *testing.T) {
	var none Options
	if got := none.excludeSubPaths(); len(got) != 0 {
		t.Errorf("nothing declared must exclude nothing, got %v", got)
	}
	o := Options{
		embeddedDataDir:    "/config/postgres",
		excludeRegenerable: []RegenerablePath{{Path: "/config/data/trickplay"}, {Path: ""}},
	}
	got := o.excludeSubPaths()
	if len(got) != 2 {
		t.Fatalf("expected both exclusions and no empty entry, got %v", got)
	}
	if got[0] != "/config/postgres" || got[1] != "/config/data/trickplay" {
		t.Errorf("got %v", got)
	}
}
