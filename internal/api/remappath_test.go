package api

import (
	"strings"
	"testing"
)

// F81: the remap TARGET base is validated fail-closed against the same
// protected-root list write-time validation uses.
func TestValidateRemapPathBases(t *testing.T) {
	// Valid pair passes, cleaned.
	f, to, err := validateRemapPathBases("/opt/docker/", "/opt/stacks")
	if err != nil || f != "/opt/docker" || to != "/opt/stacks" {
		t.Fatalf("valid pair rejected: %q %q %v", f, to, err)
	}

	bad := []struct {
		name, from, to, wantIn string
	}{
		{"forbidden target /etc", "/opt/docker", "/etc", "protected system path"},
		{"forbidden target under /var/lib", "/opt/docker", "/var/lib/docker", "protected system path"},
		{"relative from", "opt/docker", "/opt/stacks", "absolute"},
		{"relative to", "/opt/docker", "opt/stacks", "absolute"},
		{"empty both", "", "", "absolute"},
		{"root from", "/", "/opt/stacks", "refuses /"},
		{"root to", "/opt/docker", "/", "refuses /"},
		{"identical", "/opt/docker", "/opt/docker", "identical"},
	}
	for _, c := range bad {
		if _, _, err := validateRemapPathBases(c.from, c.to); err == nil || !strings.Contains(err.Error(), c.wantIn) {
			t.Errorf("%s: err=%v, want mention of %q", c.name, err, c.wantIn)
		}
	}

	// A forbidden FROM base is fine — it only has to match old paths, the
	// write side never touches it.
	if _, _, err := validateRemapPathBases("/var/lib/oldapp", "/opt/stacks"); err != nil {
		t.Fatalf("forbidden FROM base should be allowed: %v", err)
	}
}
