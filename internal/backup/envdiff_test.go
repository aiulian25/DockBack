package backup

import (
	"strings"
	"testing"
)

// R5 §4's secret with metacharacters in it: 128 bytes that must never reach a
// log line, and must still compare exactly.
const gnarlySecret = `p@$$w'"|&;()<>{}[]*?~#!\` + "`" + `%^ 128-byte-ish value with everything in it 0123456789abcdefghijklmnopqrstuvwxyz`

func TestEnvDelta(t *testing.T) {
	baked := []string{"PATH", "LANG", "GOSU_VERSION"}
	captured := []string{
		"PATH=/usr/local/sbin:/usr/local/bin",
		"LANG=C.UTF-8",
		"TZ=Europe/London",
		"MPLCONFIGDIR=/matplotlib",
		"DB_PASSWORD=" + gnarlySecret,
		"IMMICH_VERSION=v1.119.0",
	}

	t.Run("identical passes", func(t *testing.T) {
		delta := CompareEnv(captured, captured, baked, nil)
		if !delta.Faithful() {
			t.Fatalf("got %+v", delta)
		}
		if len(delta.Extra) != 0 {
			t.Errorf("extra = %v", delta.Extra)
		}
	})

	t.Run("baked-in noise is excluded from both sides", func(t *testing.T) {
		// R5's audit compared "both diffed against their image's baked-in env".
		// Without that, PATH and LANG drown every comparison.
		live := []string{
			"PATH=/completely/different",
			"LANG=en_GB.UTF-8",
			"TZ=Europe/London",
			"MPLCONFIGDIR=/matplotlib",
			"DB_PASSWORD=" + gnarlySecret,
			"IMMICH_VERSION=v1.119.0",
		}
		if delta := CompareEnv(captured, live, baked, nil); !delta.Faithful() {
			t.Errorf("PATH and LANG are the image's, not the container's: %+v", delta)
		}
	})

	t.Run("a key the restore rewrote passes, and is noted", func(t *testing.T) {
		live := []string{
			"PATH=/usr/local/sbin:/usr/local/bin", "LANG=C.UTF-8",
			"TZ=Europe/London", "MPLCONFIGDIR=/matplotlib",
			"DB_PASSWORD=" + gnarlySecret,
			"IMMICH_VERSION=v1.119.0",
			"IMMICH_SERVER_URL=http://newhost:2283",
		}
		captured2 := append(append([]string(nil), captured...), "IMMICH_SERVER_URL=http://oldhost:2283")
		delta := CompareEnv(captured2, live, baked, []string{"IMMICH_SERVER_URL"})
		if !delta.Faithful() {
			t.Fatalf("a deviation the restore performed is not a defect: %+v", delta)
		}
		if len(delta.Explained) != 1 || delta.Explained[0] != "IMMICH_SERVER_URL" {
			t.Errorf("explained = %v — the report must show what it changed", delta.Explained)
		}
	})

	t.Run("a dropped key fails and is named", func(t *testing.T) {
		// R5 §4.2: seventeen variables missing, including the one that was "the
		// entire reason the /matplotlib bind exists".
		live := []string{"PATH=/usr/local/sbin", "LANG=C.UTF-8", "TZ=Europe/London",
			"DB_PASSWORD=" + gnarlySecret, "IMMICH_VERSION=v1.119.0"}
		delta := CompareEnv(captured, live, baked, nil)
		if delta.Faithful() {
			t.Fatal("an omission is as much a deviation as an addition")
		}
		if len(delta.Missing) != 1 || delta.Missing[0] != "MPLCONFIGDIR" {
			t.Errorf("missing = %v", delta.Missing)
		}
	})

	t.Run("value drift fails with hash prefixes, never values", func(t *testing.T) {
		live := []string{"PATH=/usr/local/sbin", "LANG=C.UTF-8",
			"TZ=Etc/UTC", // R5 §4.1's exact defect, from the other direction
			"MPLCONFIGDIR=/matplotlib",
			"DB_PASSWORD=" + gnarlySecret + "-rotated",
			"IMMICH_VERSION=v1.119.0"}
		delta := CompareEnv(captured, live, baked, nil)
		if delta.Faithful() {
			t.Fatal("a changed value is a changed environment")
		}
		if len(delta.Changed) != 2 {
			t.Fatalf("changed = %+v", delta.Changed)
		}
		rendered := describeEnvChanges(delta.Changed)
		for _, key := range []string{"DB_PASSWORD", "TZ"} {
			if !strings.Contains(rendered, key) {
				t.Errorf("must name %s: %s", key, rendered)
			}
		}
		// The whole point: the names travel, the values never do.
		for _, secret := range []string{gnarlySecret, "Europe/London", "Etc/UTC"} {
			if strings.Contains(rendered, secret) {
				t.Errorf("a VALUE reached the report: %s", rendered)
			}
		}
		for _, c := range delta.Changed {
			if len(c.Was) != envHashLen || c.Was == c.Now {
				t.Errorf("both sides must be distinct short digests: %+v", c)
			}
		}
	})

	t.Run("extra keys are reported but never fail", func(t *testing.T) {
		// A newer image's own defaults arrive this way when the digest could not
		// be pinned; F95's image-drift report is what speaks to that.
		live := append(append([]string(nil), captured...), "NEW_IMAGE_DEFAULT=1")
		delta := CompareEnv(captured, live, baked, nil)
		if !delta.Faithful() {
			t.Errorf("an added key is not a restore defect: %+v", delta)
		}
		if len(delta.Extra) != 1 || delta.Extra[0] != "NEW_IMAGE_DEFAULT" {
			t.Errorf("extra = %v", delta.Extra)
		}
	})

	t.Run("an empty value is distinguishable from a missing one", func(t *testing.T) {
		delta := CompareEnv([]string{"K=value"}, []string{"K="}, nil, nil)
		if len(delta.Changed) != 1 || delta.Changed[0].Now != "(empty)" {
			t.Errorf("got %+v", delta.Changed)
		}
		if len(delta.Missing) != 0 {
			t.Error("set-but-empty is not absent")
		}
	})

	t.Run("the restore's own rewrites are discovered by diffing the inspect", func(t *testing.T) {
		// No pass has to report what it changed: comparing the document before and
		// after them covers all of them, including the remaps, which can touch any
		// key's value and never knew which.
		before := []string{"A=1", "B=2", "C=3"}
		after := []string{"A=1", "B=changed", "D=new"}
		got := changedEnvKeys(before, after)
		want := []string{"B", "C", "D"}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("got %v, want %v (sorted)", got, want)
			}
		}
	})
}
