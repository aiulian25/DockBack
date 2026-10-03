package storage

import "testing"

func TestValidateKey(t *testing.T) {
	ok := []string{
		"razer/portainer/20260628-abc.dback",
		"node/stack/file.dback.manifest.json",
		".dockback-probe-123",
		"a/b/c/d.bin",
	}
	for _, k := range ok {
		if err := validateKey(k); err != nil {
			t.Errorf("expected %q to be valid, got %v", k, err)
		}
	}

	bad := []string{
		"",                  // empty → could target folder root
		"/",                 // root
		"..",                // escape
		"../etc/passwd",     // traversal out of sub-path
		"node/../../secret", // traversal mid-key
		"node/./file",       // dot segment
		"a//b",              // empty segment
		"node/..",           // trailing escape
	}
	for _, k := range bad {
		if err := validateKey(k); err == nil {
			t.Errorf("expected %q to be rejected, but it passed", k)
		}
	}
}
