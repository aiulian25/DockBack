package api

import (
	"strings"
	"testing"
)

// A nas01 → server2 stack restore was refused with "path remap needs two absolute
// base directories (e.g. /opt/docker → /opt/stacks)". The base typed was
// "home/user/docker" — one missing leading slash — but the message named
// neither the field nor the value, and the SAME text was emitted for a second,
// unrelated cause (a from-base that could not be derived from the catalog). The
// operator had three things to guess between.
func TestRemapBaseErrorNamesTheField(t *testing.T) {
	t.Run("a relative base is named, quoted and corrected", func(t *testing.T) {
		_, _, err := validateRemapPathBases("/volume1/docker", "home/user/docker")
		if err == nil {
			t.Fatal("a relative base must be refused")
		}
		msg := err.Error()
		for _, want := range []string{`"To base folder"`, `"home/user/docker"`, `"/home/user/docker"`, "absolute"} {
			if !strings.Contains(msg, want) {
				t.Errorf("message is missing %q:\n%s", want, msg)
			}
		}
		// The FROM was fine; the message must not implicate it.
		if strings.Contains(msg, `"From base folder"`) {
			t.Errorf("blamed the wrong field:\n%s", msg)
		}
	})

	t.Run("the other side is named when it is the one at fault", func(t *testing.T) {
		_, _, err := validateRemapPathBases("volume1/docker", "/home/user/docker")
		if err == nil || !strings.Contains(err.Error(), `"From base folder"`) {
			t.Fatalf("want the From field named, got %v", err)
		}
	})

	t.Run("identical bases echo the value", func(t *testing.T) {
		_, _, err := validateRemapPathBases("/opt/docker", "/opt/docker/")
		if err == nil || !strings.Contains(err.Error(), "/opt/docker") {
			t.Fatalf("want the colliding base echoed, got %v", err)
		}
	})

	t.Run("a valid pair still passes, cleaned", func(t *testing.T) {
		f, to, err := validateRemapPathBases("  /volume1/docker/  ", "/home/user/docker")
		if err != nil {
			t.Fatalf("a good pair was refused: %v", err)
		}
		if f != "/volume1/docker" || to != "/home/user/docker" {
			t.Fatalf("cleaned to %q → %q", f, to)
		}
	})

	t.Run("the root refusal is unchanged", func(t *testing.T) {
		if _, _, err := validateRemapPathBases("/", "/opt/stacks"); err == nil || !strings.Contains(err.Error(), "every absolute path") {
			t.Fatalf("want the / refusal, got %v", err)
		}
	})
}
