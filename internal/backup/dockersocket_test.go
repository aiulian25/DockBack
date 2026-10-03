package backup

import (
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
)

func inspWithMounts(mounts ...types.MountPoint) types.ContainerJSON {
	return types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{
			Name:       "/app",
			HostConfig: &container.HostConfig{},
		},
		Mounts: mounts,
		Config: &container.Config{Image: "app:1"},
	}
}

// TestDockerSocketMode (F128). Keyed on the SOURCE, not the destination: what
// matters is which host object was handed over, and an unusual in-container path
// changes nothing about the authority that grants.
func TestDockerSocketMode(t *testing.T) {
	rw := inspWithMounts(types.MountPoint{Type: "bind", Source: DockerSocketPath, Destination: DockerSocketPath, RW: true})
	if got := dockerSocketMode(rw); got != "rw" {
		t.Errorf("got %q, want rw", got)
	}
	ro := inspWithMounts(types.MountPoint{Type: "bind", Source: DockerSocketPath, Destination: DockerSocketPath, RW: false})
	if got := dockerSocketMode(ro); got != "ro" {
		t.Errorf("got %q, want ro", got)
	}
	// Mounted somewhere unusual inside the container — still the same authority.
	odd := inspWithMounts(types.MountPoint{Type: "bind", Source: DockerSocketPath, Destination: "/tmp/d.sock", RW: true})
	if got := dockerSocketMode(odd); got != "rw" {
		t.Errorf("an unusual destination must not hide the socket, got %q", got)
	}
	// The overwhelming majority of containers.
	none := inspWithMounts(types.MountPoint{Type: "bind", Source: "/srv/data", Destination: "/data", RW: true})
	if got := dockerSocketMode(none); got != "" {
		t.Errorf("an ordinary bind must report nothing, got %q", got)
	}
	if got := dockerSocketMode(inspWithMounts()); got != "" {
		t.Errorf("no mounts must report nothing, got %q", got)
	}
}

// TestDockerSocketWidened is the invariant the restore asserts. Narrowing is a
// hardening change and must never be flagged — nagging an operator who tightened
// their own deployment is how a security warning becomes noise.
func TestDockerSocketWidened(t *testing.T) {
	cases := []struct {
		recorded, now string
		widened       bool
	}{
		{"ro", "rw", true},  // the classic widening
		{"", "rw", true},    // appeared where there was none
		{"", "ro", true},    // still an addition of authority
		{"rw", "rw", false}, // unchanged
		{"ro", "ro", false},
		{"", "", false},
		{"rw", "ro", false}, // narrowed — a good thing
		{"rw", "", false},   // removed — also good
		{"ro", "", false},
	}
	for _, c := range cases {
		if got := DockerSocketWidened(c.recorded, c.now); got != c.widened {
			t.Errorf("DockerSocketWidened(%q,%q) = %v, want %v", c.recorded, c.now, got, c.widened)
		}
	}
}

// TestHostRequirementsRecordsSocket: the socket has to make it into the manifest,
// and an ordinary container must still record no requirements at all so its
// manifest is unchanged.
func TestHostRequirementsRecordsSocket(t *testing.T) {
	req := hostRequirementsOf(inspWithMounts(
		types.MountPoint{Type: "bind", Source: DockerSocketPath, Destination: DockerSocketPath, RW: true},
	))
	if req == nil || req.DockerSocket != "rw" {
		t.Fatalf("the Docker socket must be recorded as a host requirement, got %+v", req)
	}
	if hostRequirementsOf(inspWithMounts(
		types.MountPoint{Type: "bind", Source: "/srv/data", Destination: "/data", RW: true},
	)) != nil {
		t.Error("an ordinary container must still record no host requirements — its manifest must not change")
	}
	// Empty() has to know about the new field, or a socket-only container would
	// be dropped from the manifest entirely.
	if (HostRequirements{DockerSocket: "ro"}).Empty() {
		t.Error("a container whose ONLY requirement is the Docker socket must not be treated as empty")
	}
}
