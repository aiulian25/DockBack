package backup

import (
	"slices"
	"testing"
)

// F4: the post-restore health gate must (a) pass a healthy restore, (b) choose
// rollback when the container is unhealthy AND a safety snapshot exists, and
// (c) report the unhealthy failure (no rollback) when no snapshot was taken —
// never silent success.
func TestDecideRestoreVerdict(t *testing.T) {
	cases := []struct {
		name        string
		healthy     bool
		hasSnapshot bool
		want        restoreVerdict
	}{
		{"healthy with snapshot", true, true, restoreHealthy},
		{"healthy without snapshot", true, false, restoreHealthy},
		{"unhealthy with snapshot rolls back", false, true, restoreRollback},
		{"unhealthy without snapshot reports failure", false, false, restoreUnhealthyNoSnapshot},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decideRestoreVerdict(c.healthy, c.hasSnapshot); got != c.want {
				t.Errorf("decideRestoreVerdict(healthy=%v, snapshot=%v) = %v, want %v", c.healthy, c.hasSnapshot, got, c.want)
			}
		})
	}
}

// The #14/#21 resolution: DockBack's one mutation on restored files refuses the
// shape R3's experiment proved dangerous.
func TestAlignSkipsDuplicateTargetConfigDirs(t *testing.T) {
	const dbDir = "/volume1/docker/nextcloud/db"
	dupes := map[string][]string{dbDir: {"/etc/mysql/conf.d", "/var/lib/mysql"}}
	paths := []string{"/etc/mysql/conf.d", "/var/lib/mysql", "/var/www/html"}

	t.Run("a doubly-mounted directory holding config is held at every destination", func(t *testing.T) {
		// Only conf.d is reported as config-bearing, but both destinations are
		// ONE directory: chowning through the datadir path changes exactly the
		// same inodes, so holding only conf.d would be a guard in name only.
		keep, held := splitHeldConfigDuplicates(paths, dupes, map[string]bool{"/etc/mysql/conf.d": true})

		if len(keep) != 1 || keep[0] != "/var/www/html" {
			t.Errorf("kept %v, want only /var/www/html — the app still needs to own its data", keep)
		}
		if len(held) != 2 {
			t.Fatalf("held %v, want both destinations of the duplicated source", held)
		}
		for _, want := range []string{"/etc/mysql/conf.d", "/var/lib/mysql"} {
			if !slices.Contains(held, want) {
				t.Errorf("%s must be held: chowning it changes the same files as conf.d", want)
			}
		}
	})

	t.Run("a doubly-mounted directory with no config is still aligned", func(t *testing.T) {
		// Refusing here would leave applications unable to write their own data,
		// which is the problem ownership alignment exists to solve.
		keep, held := splitHeldConfigDuplicates(paths, dupes, map[string]bool{})
		if len(held) != 0 {
			t.Errorf("nothing to promote, nothing to hold: %v", held)
		}
		if len(keep) != len(paths) {
			t.Errorf("kept %v, want all %v", keep, paths)
		}
	})

	t.Run("config in a singly-mounted directory is aligned as before", func(t *testing.T) {
		// The common case, and the one that must not regress: ordinary config
		// directories are most of what ownership alignment exists to fix.
		keep, held := splitHeldConfigDuplicates(
			[]string{"/config"}, map[string][]string{}, map[string]bool{"/config": true})
		if len(held) != 0 || len(keep) != 1 {
			t.Errorf("a single mount is not the dangerous shape: keep=%v held=%v", keep, held)
		}
	})
}
