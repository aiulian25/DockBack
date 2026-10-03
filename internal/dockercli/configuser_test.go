package dockercli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
)

// R3 §Issue 24 — `user: 1026:100` on Nextcloud's redis: "Synology's uid/gid,
// hardcoded in compose. On the target, 1026 does not exist."
func TestRewriteConfigUser(t *testing.T) {
	inspect := func(user string) []byte {
		insp := types.ContainerJSON{
			ContainerJSONBase: &types.ContainerJSONBase{Name: "/Nextcloud-REDIS"},
			Config:            &container.Config{Image: "redis:7", User: user, Env: []string{"TZ=Europe/London"}},
		}
		raw, _ := json.Marshal(insp)
		return raw
	}

	t.Run("a numeric override is pointed at the pin", func(t *testing.T) {
		out, changed, err := RewriteConfigUser(inspect("1026:100"), 999, 999)
		if err != nil || !changed {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
		if got := ConfigUser(out); got != "999:999" {
			t.Fatalf("user = %q, want 999:999", got)
		}
		// Nothing else moves with it.
		if !strings.Contains(string(out), "TZ=Europe/London") {
			t.Error("unrelated configuration was disturbed")
		}
	})

	t.Run("a named user is never touched", func(t *testing.T) {
		// The step's DO-NOT: a name resolves inside the image, so it is already
		// portable — and replacing it with a number would discard the group
		// memberships the image set up for that account.
		for _, user := range []string{"redis", "postgres", "www-data", "redis:redis", "1000:users", "node:1000"} {
			before := inspect(user)
			out, changed, err := RewriteConfigUser(before, 999, 999)
			if err != nil || changed {
				t.Errorf("%q: rewrote a named user (changed=%v err=%v)", user, changed, err)
			}
			if string(out) != string(before) {
				t.Errorf("%q: document was rewritten", user)
			}
		}
	})

	t.Run("a container with no override gets none", func(t *testing.T) {
		// Adding one would introduce configuration the source does not have, and
		// would override the image's own USER — the model that is already correct
		// on every host.
		before := inspect("")
		out, changed, _ := RewriteConfigUser(before, 999, 999)
		if changed || string(out) != string(before) {
			t.Fatal("an override was invented where the source had none")
		}
	})

	t.Run("re-running is a no-op", func(t *testing.T) {
		before := inspect("999:999")
		out, changed, _ := RewriteConfigUser(before, 999, 999)
		if changed {
			t.Fatal("rewrote an override that already names the pinned ids")
		}
		if string(out) != string(before) {
			t.Fatal("document churned on a second restore")
		}
	})

	t.Run("a uid with no group half is still numeric", func(t *testing.T) {
		out, changed, _ := RewriteConfigUser(inspect("1026"), 999, 999)
		if !changed || ConfigUser(out) != "999:999" {
			t.Fatalf("bare uid not rewritten: changed=%v user=%q", changed, ConfigUser(out))
		}
	})

	t.Run("NumericUser separates the ids from the names", func(t *testing.T) {
		cases := []struct {
			in      string
			uid     int
			gid     int
			numeric bool
		}{
			{"1026:100", 1026, 100, true},
			{"999:999", 999, 999, true},
			{"0:0", 0, 0, true},
			{"1026", 1026, -1, true}, // no group stated, which is not group 0
			{" 1026:100 ", 1026, 100, true},
			{"", 0, -1, false},
			{"redis", 0, -1, false},
			{"1000:users", 0, -1, false},
			{"node:1000", 0, -1, false},
			{"1026:100:5", 0, -1, false},
			{"-1:100", 0, -1, false},
		}
		for _, tc := range cases {
			uid, gid, numeric := NumericUser(tc.in)
			if numeric != tc.numeric || uid != tc.uid || gid != tc.gid {
				t.Errorf("NumericUser(%q) = %d,%d,%v — want %d,%d,%v", tc.in, uid, gid, numeric, tc.uid, tc.gid, tc.numeric)
			}
		}
	})

	t.Run("a document that cannot be read is returned untouched", func(t *testing.T) {
		junk := []byte("{{{ not json")
		out, changed, _ := RewriteConfigUser(junk, 999, 999)
		if changed || string(out) != string(junk) {
			t.Fatal("an unreadable inspect must come back exactly as given")
		}
	})
}
