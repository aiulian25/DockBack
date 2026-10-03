package dockercli

import "testing"

// R4's takeaway, and the reason the fixed list was the wrong shape: "Detect the
// env-configurable model by semantics, not name: any variable pair matching
// `*UID`/`*GID` (`PUID`, `USERMAP_UID`, `UID`, `RUN_AS_UID`)."
//
// USERMAP_UID had to be added to the list by hand after a restore left a
// Synology's documents owned by a uid that did not exist on the target. The next
// convention would have cost the same discovery.
func TestRunAsIDsSemantic(t *testing.T) {
	t.Run("a convention nobody wrote down is still a pair", func(t *testing.T) {
		uid, gid, key, ok := RunAsIDs([]string{"TZ=UTC", "RUN_AS_UID=1000", "RUN_AS_GID=1000"})
		if !ok || uid != 1000 || gid != 1000 {
			t.Fatalf("got %d:%d ok=%v", uid, gid, ok)
		}
		// The run log has to name the variable it actually read, not a guess.
		if key != "RUN_AS_UID/RUN_AS_GID" {
			t.Errorf("key = %q", key)
		}
		// And the rewriter must agree with the reader, or the alignment chowns to
		// one pair while the recreate rewrites another.
		uidKey, gidKey, found := RunAsEnvPair([]string{"TZ=UTC", "RUN_AS_UID=1000", "RUN_AS_GID=1000"})
		if !found || uidKey != "RUN_AS_UID" || gidKey != "RUN_AS_GID" {
			t.Errorf("RunAsEnvPair = %q/%q ok=%v", uidKey, gidKey, found)
		}
	})

	t.Run("a lone UID-suffixed key is not an identity", func(t *testing.T) {
		// SQUID=3128 is a port. The PAIR is what makes the generalisation safe:
		// nothing else is shaped like `<prefix>UID` plus `<prefix>GID`.
		for _, env := range [][]string{
			{"SQUID=3128"},
			{"SQUID=3128", "TZ=UTC"},
			{"LIQUID=42", "PUID_SOMETHING=1"},
			{"RUN_AS_UID=1000"},                   // half a pair
			{"RUN_AS_GID=1000"},                   // the other half
			{"A_UID=1000", "B_GID=1000"},          // different prefixes are different things
			{"RUN_AS_UID=abc", "RUN_AS_GID=1000"}, // not an id
			{"RUN_AS_UID=1000", "RUN_AS_GID=-5"},  // negative
			{"RUN_AS_UID=", "RUN_AS_GID="},
		} {
			if _, _, _, ok := RunAsIDs(env); ok {
				t.Errorf("%v must yield no ids", env)
			}
			if _, _, ok := RunAsEnvPair(env); ok {
				t.Errorf("%v must yield no rewritable pair", env)
			}
		}
	})

	t.Run("an explicit pair wins over a coexisting generic one", func(t *testing.T) {
		// The step's DO-NOT. PUID is the LinuxServer convention and by far the
		// most common; a generic match must never displace it.
		env := []string{"PUID=1000", "PGID=1000", "RUN_AS_UID=1026", "RUN_AS_GID=100"}
		if _, _, key, _ := RunAsIDs(env); key != "PUID/PGID" {
			t.Errorf("the explicit pair must win, got %q", key)
		}
		if uidKey, _, _ := RunAsEnvPair(env); uidKey != "PUID" {
			t.Errorf("the rewriter must agree, got %q", uidKey)
		}
		// Every explicit convention, against a generic pair sorting BEFORE it
		// alphabetically — so the ordering is what decides, not luck.
		for _, explicit := range [][2]string{
			{"PUID", "PGID"}, {"USERMAP_UID", "USERMAP_GID"}, {"UID", "GID"}, {"USER_ID", "GROUP_ID"},
		} {
			env := []string{
				explicit[0] + "=1000", explicit[1] + "=1000",
				"AAA_UID=1026", "AAA_GID=100",
			}
			if _, _, key, _ := RunAsIDs(env); key != explicit[0]+"/"+explicit[1] {
				t.Errorf("%s must win over the generic scan, got %q", explicit[0], key)
			}
		}
	})

	t.Run("two generic pairs give the same answer every run", func(t *testing.T) {
		// Map iteration is random; a value this decides a chown from must not be.
		env := []string{"ZZZ_UID=1", "ZZZ_GID=1", "AAA_UID=2", "AAA_GID=2"}
		first, _, _, _ := RunAsIDs(env)
		for i := 0; i < 50; i++ {
			if uid, _, _, _ := RunAsIDs(env); uid != first {
				t.Fatalf("answer flapped between runs: %d then %d", first, uid)
			}
		}
	})

	t.Run("a non-numeric explicit pair falls through to a usable generic one", func(t *testing.T) {
		// The explicit loop skips a pair it cannot read as ids; the generic scan
		// then finds the one that IS readable, rather than the container being
		// treated as declaring nothing.
		uid, gid, key, ok := RunAsIDs([]string{"PUID=abc", "PGID=xyz", "RUN_AS_UID=33", "RUN_AS_GID=33"})
		if !ok || uid != 33 || gid != 33 || key != "RUN_AS_UID/RUN_AS_GID" {
			t.Fatalf("got %d:%d key=%q ok=%v", uid, gid, key, ok)
		}
	})
}
