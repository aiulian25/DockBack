package api

import "testing"

// F10: the "restore as a copy" name must be a valid Docker container name — so a
// clone can't collide with a daemon/shell edge or inject.
func TestValidContainerName(t *testing.T) {
	good := []string{"paperless-restored", "app_copy", "db.test", "a", "X1", "immich-server-restored"}
	for _, n := range good {
		if !validContainerName(n) {
			t.Errorf("%q should be a valid container name", n)
		}
	}
	bad := []string{"", "-startsdash", ".startsdot", "has space", "bad/slash", "quote'", "semi;colon", "$(x)"}
	for _, n := range bad {
		if validContainerName(n) {
			t.Errorf("%q should be rejected", n)
		}
	}
}
