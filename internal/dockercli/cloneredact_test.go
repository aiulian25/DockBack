package dockercli

import (
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/mount"
)

// A bind path is a directory on the host, possibly the original's. A redaction writes,
// so it must only ever reach what DockBack made for the copy.
func TestARedactionReachesOnlyTheCopysVolumes(t *testing.T) {
	config := cloneRedactionHostConfig([]types.MountPoint{
		{Type: mount.TypeVolume, Name: "clone-config", Destination: "/config"},
		{Type: mount.TypeBind, Source: "/srv/original/config", Destination: "/shared"},
		{Type: mount.TypeTmpfs, Destination: "/run"},
		{Type: mount.TypeVolume, Name: "", Destination: "/anonymous-without-a-name"},
	})
	if len(config.Binds) != 0 {
		t.Fatalf("binds: %v", config.Binds)
	}
	if len(config.Mounts) != 1 || config.Mounts[0].Source != "clone-config" || config.Mounts[0].Target != "/config" {
		t.Fatalf("mounts: %+v", config.Mounts)
	}
}

func TestARedactionHelperIsHeldToWhatItNeeds(t *testing.T) {
	config := cloneRedactionHostConfig(nil)
	if config.NetworkMode != cloneNetworkMode || !config.ReadonlyRootfs {
		t.Errorf("network %q, read-only root %v", config.NetworkMode, config.ReadonlyRootfs)
	}
	if len(config.CapDrop) != 1 || config.CapDrop[0] != "ALL" || len(config.CapAdd) != 1 || config.CapAdd[0] != "DAC_OVERRIDE" {
		t.Errorf("capabilities: drop %v, add %v", config.CapDrop, config.CapAdd)
	}
	if config.Privileged || config.VolumesFrom != nil {
		t.Errorf("privileged %v, volumes-from %v", config.Privileged, config.VolumesFrom)
	}
}
