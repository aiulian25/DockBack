package dockercli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
)

// SEC-002: a helper container has no network and only the capabilities file work needs.

func TestConfineHelperTakesAwayNetworkAndPrivileges(t *testing.T) {
	given := &container.HostConfig{VolumesFrom: []string{"target:ro"}, Binds: []string{"vol:/voldata"}}
	confined := confineHelper(given)

	if confined.NetworkMode != helperNetwork {
		t.Errorf("network = %q, want %q", confined.NetworkMode, helperNetwork)
	}
	if len(confined.CapDrop) != 1 || confined.CapDrop[0] != "ALL" {
		t.Errorf("every capability must be dropped first, got %v", confined.CapDrop)
	}
	for _, unwanted := range []string{"NET_RAW", "NET_ADMIN", "SYS_ADMIN", "SETUID", "SETGID", "SYS_CHROOT", "SYS_PTRACE"} {
		for _, added := range confined.CapAdd {
			if added == unwanted {
				t.Errorf("a helper must not hold %s", unwanted)
			}
		}
	}
	if len(confined.SecurityOpt) != 1 || confined.SecurityOpt[0] != helperNoNewPrivileges {
		t.Errorf("security options = %v", confined.SecurityOpt)
	}
	if len(confined.VolumesFrom) != 1 || len(confined.Binds) != 1 {
		t.Error("the mounts a caller asked for must be left as given")
	}
}

// unconfinedByDesign names the files that build a host configuration which is not a
// helper's, with why. Anything else that writes a HostConfig literal must pass it
// through confineHelper, so a new helper cannot quietly be given a network.
var unconfinedByDesign = map[string]string{
	"recreate.go":      "recreates the operator's own container, exactly as it was recorded",
	"verifyrestore.go": "drill and deep-verification sandboxes, which set their own isolation",
	"cloneredact.go":   "the test-clone sandbox, which sets its own isolation",
	"composecheck.go":  "runs the Compose CLI image, not the helper image",
	"machine.go":       "the hardware probe, which needs the host's device view",
	"nvidia.go":        "the GPU probe, which needs the GPU runtime",
}

// netnsProbe is the one helper that must share a container's network: it exists to
// look at that network and mounts nothing.
const netnsProbe = `NetworkMode: container.NetworkMode("container:" + targetID)`

func TestEveryHelperHostConfigIsConfined(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") || unconfinedByDesign[source] != "" {
			continue
		}
		content, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		for number, line := range strings.Split(string(content), "\n") {
			if !strings.Contains(line, "container.HostConfig{") || strings.Contains(line, netnsProbe) {
				continue
			}
			if !strings.Contains(line, "confineHelper(&container.HostConfig{") {
				t.Errorf("%s:%d builds a host configuration that is not confined: %s", source, number+1, strings.TrimSpace(line))
			}
		}
	}
}
