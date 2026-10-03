package api

import (
	"strings"
	"testing"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
)

// F144: a container recreated where something already holds its port is created
// and then fails to start, with a Docker error that names the port and nothing
// else. The whole value is naming the holder before the restore runs.

func portServer(cs ...*dockercli.Container) *Server {
	s := &Server{stats: map[string]*nodeStat{}}
	s.stats["n1"] = &nodeStat{Containers: cs}
	return s
}

func npmPorts() *backup.Manifest {
	return &backup.Manifest{
		Image: "jc21/nginx-proxy-manager:latest",
		PublishedPorts: []backup.PublishedPort{
			{HostPort: 80, Proto: "tcp"}, {HostPort: 81, Proto: "tcp"}, {HostPort: 443, Proto: "tcp"},
		},
	}
}

func TestPortConflictsNamesTheHolder(t *testing.T) {
	s := portServer(
		&dockercli.Container{Name: "caddy", State: "running", Ports: "80:80, 443:443"},
		&dockercli.Container{Name: "grafana", State: "running", Ports: "3000:3000"},
	)
	got := s.portConflicts("n1", npmPorts(), "npm")
	if len(got) != 2 {
		t.Fatalf("expected conflicts on 80 and 443, got %v", got)
	}
	if !strings.Contains(got[0], "port 80") || !strings.Contains(got[0], "caddy") {
		t.Errorf("the conflict must name the port AND the holder, got %q", got[0])
	}
	// Deterministic order, so the same list does not read differently twice.
	if !strings.Contains(got[1], "port 443") {
		t.Errorf("conflicts must be listed in port order, got %v", got)
	}
}

func TestPortConflictsQuiet(t *testing.T) {
	free := portServer(&dockercli.Container{Name: "grafana", State: "running", Ports: "3000:3000"})
	if got := free.portConflicts("n1", npmPorts(), "npm"); len(got) != 0 {
		t.Errorf("nothing is taken here — must be silent, got %v", got)
	}

	// The container being restored legitimately holds its own ports.
	self := portServer(&dockercli.Container{Name: "npm", State: "running", Ports: "80:80, 443:443, 81:81"})
	if got := self.portConflicts("n1", npmPorts(), "npm"); len(got) != 0 {
		t.Errorf("a container must not conflict with itself, got %v", got)
	}

	// A stopped container holds no binding — the ports are recorded, but nothing
	// is listening, so reporting it as a conflict would be wrong.
	stopped := portServer(&dockercli.Container{Name: "old-proxy", State: "exited", Ports: "80:80"})
	if got := stopped.portConflicts("n1", npmPorts(), "npm"); len(got) != 0 {
		t.Errorf("a stopped container holds nothing, got %v", got)
	}

	// Backups taken before ports were recorded, an uncached node and a nil
	// manifest are all silent rather than guessing.
	if got := free.portConflicts("n1", &backup.Manifest{}, "npm"); len(got) != 0 {
		t.Errorf("a backup with no recorded ports must be silent, got %v", got)
	}
	if got := (&Server{stats: map[string]*nodeStat{}}).portConflicts("n1", npmPorts(), "npm"); len(got) != 0 {
		t.Errorf("an uncached node must be silent, got %v", got)
	}
	if got := free.portConflicts("n1", nil, "npm"); len(got) != 0 {
		t.Errorf("a nil manifest must be silent, got %v", got)
	}
}

func TestPublishedHostPortsParsing(t *testing.T) {
	got := dockercli.PublishedHostPorts("80:80, 443:443, 3000, 8080:80")
	want := []int{80, 443, 8080}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	// An unpublished port has no host side and can never conflict.
	if p := dockercli.PublishedHostPorts("3000, 9000"); len(p) != 0 {
		t.Errorf("unpublished ports must not be reported as held, got %v", p)
	}
	if p := dockercli.PublishedHostPorts(""); len(p) != 0 {
		t.Errorf("an empty port string must yield nothing, got %v", p)
	}
}
