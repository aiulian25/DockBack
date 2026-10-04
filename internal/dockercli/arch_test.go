package dockercli

import "testing"

// Step 27: an image is checked for a build matching the machine it will run on.
func TestImageArchitectureMatch(t *testing.T) {
	for machine, want := range map[string]string{"x86_64": "amd64", "aarch64": "arm64", "armv7l": "arm", "i686": "386", "riscv64": "riscv64"} {
		if got := dockerArch(machine); got != want {
			t.Errorf("dockerArch(%q) = %q, want %q", machine, got, want)
		}
	}
	if !platformOffered([]string{"amd64", "arm64"}, "arm64") {
		t.Error("a multi-arch image with an arm64 build runs on a Raspberry Pi")
	}
	if platformOffered([]string{"amd64"}, "arm64") {
		t.Error("an amd64-only image must not read as ready for an arm64 node")
	}
	if !platformOffered(nil, "arm64") {
		t.Error("a registry that lists no platforms is nothing to compare, not a refusal")
	}
}
