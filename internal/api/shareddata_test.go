package api

import (
	"strings"
	"testing"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
)

// sharedDataServer seeds the cached inventory the warning reads, so the logic is
// exercised without a Docker daemon.
func sharedDataServer(cs ...*dockercli.Container) *Server {
	s := &Server{stats: map[string]*nodeStat{}}
	s.stats["n1"] = &nodeStat{Containers: cs}
	return s
}

// TestSharedDataWarnings is the Calibre case generalised: one container manages a
// library another reads, and restoring only one rolls the shared data back
// underneath the other — from a restore that reports success.
func TestSharedDataWarnings(t *testing.T) {
	s := sharedDataServer(
		&dockercli.Container{Name: "Calibre", Mounts: []dockercli.Mount{
			{Type: "bind", Source: "/srv/calibre", Destination: "/config", RW: true},
		}},
		&dockercli.Container{Name: "calibre-web-automated", Mounts: []dockercli.Mount{
			{Type: "bind", Source: "/srv/calibre/Calibre Library", Destination: "/calibre-library", RW: true},
			{Type: "bind", Source: "/srv/cwa", Destination: "/config", RW: true},
		}},
	)
	man := &backup.Manifest{Volumes: []backup.VolumeRef{
		{Type: "bind", Source: "/srv/calibre/Calibre Library", Destination: "/calibre-library"},
	}}

	got := s.sharedDataWarnings("n1", man, "Calibre")
	if len(got) != 1 {
		t.Fatalf("expected one sharer warning, got %v", got)
	}
	if !strings.Contains(got[0], "calibre-web-automated") || !strings.Contains(got[0], "/srv/calibre/Calibre Library") {
		t.Errorf("the warning must name the container AND the path, got %q", got[0])
	}

	// The container being restored is never reported as sharing with itself.
	if got := s.sharedDataWarnings("n1", man, "calibre-web-automated"); len(got) != 0 {
		t.Errorf("a container must not be warned about its own mount, got %v", got)
	}
}

// TestSharedDataWarningsQuiet — the overwhelming majority of restores. A warning
// that fires on ordinary setups is one operators learn to ignore.
func TestSharedDataWarningsQuiet(t *testing.T) {
	s := sharedDataServer(
		&dockercli.Container{Name: "web", Mounts: []dockercli.Mount{
			{Type: "bind", Source: "/srv/web", Destination: "/data", RW: true},
		}},
		&dockercli.Container{Name: "other", Mounts: []dockercli.Mount{
			{Type: "bind", Source: "/srv/other", Destination: "/data", RW: true},
		}},
	)
	man := &backup.Manifest{Volumes: []backup.VolumeRef{{Type: "bind", Source: "/srv/web", Destination: "/data"}}}
	if got := s.sharedDataWarnings("n1", man, "web"); len(got) != 0 {
		t.Errorf("nothing is shared here — must be silent, got %v", got)
	}

	// A NAMED volume is reached by name, not by host path; treating the
	// daemon-managed source as "shared" would fire on ordinary setups.
	volMan := &backup.Manifest{Volumes: []backup.VolumeRef{
		{Type: "volume", Name: "app_data", Source: "/var/lib/docker/volumes/app_data/_data", Destination: "/data"},
	}}
	volSrv := sharedDataServer(
		&dockercli.Container{Name: "web", Mounts: []dockercli.Mount{{Type: "volume", Name: "app_data", Source: "/var/lib/docker/volumes/app_data/_data", Destination: "/data"}}},
		&dockercli.Container{Name: "other", Mounts: []dockercli.Mount{{Type: "volume", Name: "app_data", Source: "/var/lib/docker/volumes/app_data/_data", Destination: "/data"}}},
	)
	if got := volSrv.sharedDataWarnings("n1", volMan, "web"); len(got) != 0 {
		t.Errorf("named volumes must not produce a shared-bind warning, got %v", got)
	}

	// No inventory cached yet, no manifest, no volumes — all silent, never a panic.
	if got := (&Server{stats: map[string]*nodeStat{}}).sharedDataWarnings("n1", man, "web"); len(got) != 0 {
		t.Errorf("an uncached node must be silent, got %v", got)
	}
	if got := s.sharedDataWarnings("n1", nil, "web"); len(got) != 0 {
		t.Errorf("a nil manifest must be silent, got %v", got)
	}
	if got := s.sharedDataWarnings("n1", &backup.Manifest{}, "web"); len(got) != 0 {
		t.Errorf("a manifest with no volumes must be silent, got %v", got)
	}
}
