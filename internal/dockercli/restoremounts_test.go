package dockercli

import (
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/mount"
)

// A restore of a stack whose secrets file and bootstrap script were bound :ro
// failed half-way with "Read-only file system". The sidecar must get the same
// mounts, writable — and nothing the container does not mount.
func TestRestoreMountsAreWritableCopiesOfTheContainersOwn(t *testing.T) {
	points := []types.MountPoint{
		{Type: mount.TypeBind, Source: "/srv/app/secrets", Destination: "/secrets", RW: false, Mode: "ro"},
		{Type: mount.TypeBind, Source: "/srv/app/app.key", Destination: "/run/secrets/app.key", RW: false},
		{Type: mount.TypeBind, Source: "/srv/app/data", Destination: "/data", RW: true},
		{Type: mount.TypeVolume, Name: "app_db", Source: "/var/lib/docker/volumes/app_db/_data", Destination: "/var/lib/postgresql/data", RW: false},
		{Type: mount.TypeVolume, Name: "3f1c0a9e5b", Destination: "/cache", RW: true}, // anonymous volume
		{Type: mount.TypeTmpfs, Destination: "/tmp"},                                  // holds nothing to restore
		{Type: mount.TypeBind, Source: "/srv/odd:name", Destination: "/odd", RW: false},
		{Type: mount.TypeBind, Source: "/srv/x", Destination: ""},
	}
	binds, mounts := restoreMountsFor(points)

	want := []string{
		"/srv/app/secrets:/secrets:rw",
		"/srv/app/app.key:/run/secrets/app.key:rw",
		"/srv/app/data:/data:rw",
		"app_db:/var/lib/postgresql/data:rw", // a volume by NAME, not its storage path
		"3f1c0a9e5b:/cache:rw",
	}
	if len(binds) != len(want) {
		t.Fatalf("binds = %v, want %v", binds, want)
	}
	for i := range want {
		if binds[i] != want[i] {
			t.Errorf("bind %d = %q, want %q", i, binds[i], want[i])
		}
	}

	// A colon cannot be written in the -v form; it goes through the mount API,
	// still writable.
	if len(mounts) != 1 || mounts[0].Source != "/srv/odd:name" || mounts[0].Target != "/odd" || mounts[0].ReadOnly {
		t.Errorf("colon path must become one writable API mount, got %+v", mounts)
	}
}
