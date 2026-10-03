package backup

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// F83 ownership precedence: Selected beats unselected, RW beats RO, ties are
// deterministic (lexicographically-first name).
func TestSharedBindOwner(t *testing.T) {
	cases := []struct {
		name  string
		cands []SharedBindMount
		want  string
	}{
		{"empty", nil, ""},
		{"selected beats unselected RW",
			[]SharedBindMount{{Container: "a", RW: true}, {Container: "b", Selected: true}}, "b"},
		{"RW beats RO when none selected",
			[]SharedBindMount{{Container: "ml", RW: false}, {Container: "server", RW: true}}, "server"},
		{"RW beats RO among selected",
			[]SharedBindMount{{Container: "a", RW: false, Selected: true}, {Container: "z", RW: true, Selected: true}}, "z"},
		{"tie goes to first name",
			[]SharedBindMount{{Container: "zeta", RW: true}, {Container: "alpha", RW: true}}, "alpha"},
		{"all equal unselected RO — first name",
			[]SharedBindMount{{Container: "b"}, {Container: "a"}, {Container: "c"}}, "a"},
	}
	for _, c := range cases {
		if got := SharedBindOwner("/srv/data", c.cands); got != c.want {
			t.Errorf("%s: owner=%q want %q", c.name, got, c.want)
		}
	}
	// Determinism: order of candidates must not change the answer.
	fwd := []SharedBindMount{{Container: "a", RW: true}, {Container: "b", RW: true}}
	rev := []SharedBindMount{{Container: "b", RW: true}, {Container: "a", RW: true}}
	if SharedBindOwner("/x", fwd) != SharedBindOwner("/x", rev) {
		t.Fatal("owner must be independent of candidate order")
	}
}

func TestCoverageMap(t *testing.T) {
	shared := "/srv/photos/upload"
	cs := []*dockercli.Container{
		{Name: "server", Mounts: []dockercli.Mount{{Type: "bind", Source: shared, Destination: "/usr/src/app/upload", RW: true}}},
		{Name: "ml", Mounts: []dockercli.Mount{{Type: "bind", Source: shared, Destination: "/usr/src/app/upload", RW: false}}},
		// Single-mounter bind: not shared, must not appear.
		{Name: "solo", Mounts: []dockercli.Mount{{Type: "bind", Source: "/srv/solo", Destination: "/data", RW: true}}},
		// Named volume: never part of bind coverage.
		{Name: "db", Mounts: []dockercli.Mount{{Type: "volume", Source: shared, Destination: "/var/lib/db", RW: true}}},
	}
	m := CoverageMap(cs, func(string) map[string]bool { return nil })
	if len(m) != 1 || m[shared] != "server" {
		t.Fatalf("want {%q: server}, got %v", shared, m)
	}

	// The ML container's stored selection ticks the bind → it becomes the owner.
	m = CoverageMap(cs, func(name string) map[string]bool {
		if name == "ml" {
			return map[string]bool{"/usr/src/app/upload": true}
		}
		return nil
	})
	if m[shared] != "ml" {
		t.Fatalf("selected mounter must own: got %v", m)
	}

	// System-path binds are never coverage candidates.
	sys := []*dockercli.Container{
		{Name: "a", Mounts: []dockercli.Mount{{Type: "bind", Source: "/etc/localtime", Destination: "/etc/localtime", RW: true}}},
		{Name: "b", Mounts: []dockercli.Mount{{Type: "bind", Source: "/etc/localtime", Destination: "/etc/localtime", RW: true}}},
	}
	if m := CoverageMap(sys, func(string) map[string]bool { return nil }); len(m) != 0 {
		t.Fatalf("system binds must be excluded, got %v", m)
	}
}

// CoveredSources: only recent successful backups of OTHER containers, via the
// manifest's recorded bind sources.
func TestCoveredSources(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &Engine{Store: st, Log: func(string, string, string) {}}

	shared := "/srv/photos/upload"
	man, _ := json.Marshal(Manifest{Volumes: []VolumeRef{
		{Destination: "/usr/src/app/upload", Type: "bind", Source: shared},
		{Name: "cfg", Destination: "/config", Type: "volume"},
	}})
	// CreateBackup inserts the row; UpdateBackup persists the manifest+status,
	// mirroring the engine's real two-phase write.
	mk := func(s *store.Store, id, target, status string, age time.Duration, manifest string) {
		b := &store.Backup{
			ID: id, NodeID: "n1", TargetName: target, Status: status,
			ManifestJSON: manifest, CreatedAt: time.Now().Add(-age).Unix(),
		}
		if err := s.CreateBackup(b); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateBackup(b); err != nil {
			t.Fatal(err)
		}
	}
	mk(st, "b1", "server", "success", time.Hour, string(man))

	got := e.CoveredSources("n1", "ml")
	if got[shared] != "server" {
		t.Fatalf("want server covering %q, got %v", shared, got)
	}
	// A container never covers itself.
	if got := e.CoveredSources("n1", "server"); got[shared] != "" {
		t.Fatalf("self-coverage must be excluded, got %v", got)
	}

	// Stale coverage (outside the window) doesn't count.
	st2, _ := store.Open(filepath.Join(t.TempDir(), "t2.db"))
	t.Cleanup(func() { st2.Close() })
	e2 := &Engine{Store: st2, Log: func(string, string, string) {}}
	mk(st2, "old", "server", "success", 15*24*time.Hour, string(man))
	if got := e2.CoveredSources("n1", "ml"); len(got) != 0 {
		t.Fatalf("stale backups must not cover, got %v", got)
	}

	// A failed backup doesn't cover either.
	mk(st, "b3", "other", "failed", time.Hour, string(man))
	if got := e.CoveredSources("n1", "x"); got[shared] != "server" {
		t.Fatalf("failed backups must not cover; server still should: %v", got)
	}
}

// A manifest whose only skips are covered is not PARTIAL; any uncovered skip is.
func TestHasUncoveredSkip(t *testing.T) {
	if (&Manifest{}).HasUncoveredSkip() {
		t.Fatal("no skips = not partial")
	}
	covered := &Manifest{SkippedMounts: []SkippedMount{{Destination: "/u", CoveredBy: "server"}}}
	if covered.HasUncoveredSkip() {
		t.Fatal("covered-only skips must not be partial")
	}
	mixed := &Manifest{SkippedMounts: []SkippedMount{{Destination: "/u", CoveredBy: "server"}, {Destination: "/media"}}}
	if !mixed.HasUncoveredSkip() {
		t.Fatal("an uncovered skip must stay partial")
	}
	var nilMan *Manifest
	if nilMan.HasUncoveredSkip() {
		t.Fatal("nil manifest = not partial")
	}
}
