package backup

import (
	"strings"
	"testing"
)

// R2 §Issue 14 read ownership out of the TAR HEADERS — `1026/100` on the
// directory Docker created, `33/33` on the one the app made itself. That is a
// fact about the archive. #15's rule is that the restored tree has to be checked
// on disk: "verify by stat, never trust tar."
func TestOwnershipSweepVerdict(t *testing.T) {
	man := &Manifest{Volumes: []VolumeRef{
		{Destination: "/config", Type: "bind", Source: "/srv/app/config", Owner: "101:104"},
		{Destination: "/data", Type: "bind", Source: "/srv/app/data", Owner: "101:104"},
	}}

	t.Run("recorded 101:104, found 0:0, nothing intended", func(t *testing.T) {
		// The plan's fixture. tar restores ownership only when the extracting
		// process may set it, so root-owned data under a manifest that says
		// otherwise is the ordinary outcome, not an exotic one.
		drifted := ownershipSweepVerdict(man, map[string]string{"/config": "0:0", "/data": "101:104"}, ownershipIntent{})
		if len(drifted) != 1 {
			t.Fatalf("got %d drifts, want 1: %+v", len(drifted), drifted)
		}
		got := drifted[0]
		if got.Destination != "/config" || got.Expected != "101:104" || got.Found != "0:0" {
			t.Fatalf("wrong drift: %+v", got)
		}
		if got.Intended {
			t.Error("nothing intended this change")
		}
		// The message has to name the command, not just the problem.
		if fix := ownershipFixCommand(man, got); !strings.Contains(fix, "chown -R 101:104 /srv/app/config") {
			t.Errorf("fix = %q", fix)
		}
	})

	t.Run("a change the alignment made is silent", func(t *testing.T) {
		// F117 deliberately re-owns to the ids the target runs as. Reporting that
		// as drift would have the tool warning about its own correct work.
		intent := ownershipIntent{UID: 1000, GID: 1000, Paths: []string{"/config", "/data"}, Declared: true}
		if drifted := ownershipSweepVerdict(man, map[string]string{"/config": "1000:1000", "/data": "1000:1000"}, intent); len(drifted) != 0 {
			t.Fatalf("warned about an intended change: %+v", drifted)
		}
	})

	t.Run("an intended change that did not take is reported as such", func(t *testing.T) {
		// The chown said it failed, or took only partly. The alignment's own
		// message cannot name which paths were left behind; this can.
		intent := ownershipIntent{UID: 1000, GID: 1000, Paths: []string{"/config", "/data"}, Declared: true}
		drifted := ownershipSweepVerdict(man, map[string]string{"/config": "1000:1000", "/data": "101:104"}, intent)
		if len(drifted) != 1 || drifted[0].Destination != "/data" {
			t.Fatalf("want the untouched path only: %+v", drifted)
		}
		if !drifted[0].Intended || drifted[0].Expected != "1000:1000" {
			t.Fatalf("expected the intended ids: %+v", drifted[0])
		}
		if note := intendedNote(drifted[0].Intended); !strings.Contains(note, "did not take") {
			t.Errorf("note = %q", note)
		}
	})

	t.Run("a path outside the intent still measures against the record", func(t *testing.T) {
		// The alignment skips paths owned by service accounts inside the image
		// (alignablePaths' `leave` list). Those keep the recorded owner as their
		// expectation — the intent says nothing about them.
		intent := ownershipIntent{UID: 1000, GID: 1000, Paths: []string{"/config"}, Declared: true}
		drifted := ownershipSweepVerdict(man, map[string]string{"/config": "1000:1000", "/data": "0:0"}, intent)
		if len(drifted) != 1 || drifted[0].Destination != "/data" || drifted[0].Expected != "101:104" {
			t.Fatalf("a path the alignment left alone must be held to the record: %+v", drifted)
		}
		if drifted[0].Intended {
			t.Error("/data was not in the intent")
		}
	})

	t.Run("nothing to judge is silent", func(t *testing.T) {
		// A named volume records no owner, and neither does a pre-F81 backup.
		// Holding a path to an expectation nobody wrote down would be inventing
		// one.
		noOwner := &Manifest{Volumes: []VolumeRef{
			{Destination: "/data", Type: "volume", Name: "app-data"},
			{Destination: "/cfg", Type: "bind", Source: "/srv/cfg", Owner: "   "},
		}}
		if drifted := ownershipSweepVerdict(noOwner, map[string]string{"/data": "0:0", "/cfg": "0:0"}, ownershipIntent{}); len(drifted) != 0 {
			t.Fatalf("judged a mount with no recorded owner: %+v", drifted)
		}
		// A path that could not be stat-ed is absent, not drifted: "not there"
		// and "wrong owner" are different problems with different fixes.
		if drifted := ownershipSweepVerdict(man, map[string]string{}, ownershipIntent{}); len(drifted) != 0 {
			t.Fatalf("an unread path is not a drift: %+v", drifted)
		}
		if drifted := ownershipSweepVerdict(nil, map[string]string{"/x": "0:0"}, ownershipIntent{}); len(drifted) != 0 {
			t.Fatal("a nil manifest must judge nothing")
		}
	})

	t.Run("an undeclared intent never masks a drift", func(t *testing.T) {
		// The zero value has to mean "the alignment did not run", not "it chose
		// 0:0" — otherwise a restore where it never ran would treat root-owned
		// data as intended.
		intent := ownershipIntent{Paths: []string{"/config"}}
		drifted := ownershipSweepVerdict(man, map[string]string{"/config": "0:0"}, intent)
		if len(drifted) != 1 || drifted[0].Expected != "101:104" {
			t.Fatalf("an undeclared intent must not become the expectation: %+v", drifted)
		}
	})

	t.Run("the script reads, and only reads", func(t *testing.T) {
		script := ownershipStatScript([]string{"/config", "/data with space"})
		if !strings.Contains(script, "stat -c '%u:%g'") {
			t.Errorf("must stat the numeric pair:\n%s", script)
		}
		// Nothing that could write, create or touch a timestamp.
		for _, forbidden := range []string{"chown", "touch", "rm ", "> \"", ": >", "mkdir"} {
			if strings.Contains(script, forbidden) {
				t.Errorf("the sweep must change nothing, found %q:\n%s", forbidden, script)
			}
		}
		// A path with a space survives, and absence is stated rather than implied.
		if !strings.Contains(script, `'/data with space'`) {
			t.Errorf("a path with a space was not quoted:\n%s", script)
		}
		if !strings.Contains(script, ownerMissing) {
			t.Errorf("absence must be said out loud, not left as silence:\n%s", script)
		}
		if ownershipStatScript(nil) != "" {
			t.Error("no destinations means no script")
		}
	})

	t.Run("the parse keeps absence and ownership apart", func(t *testing.T) {
		found, missing := parseOwnershipStat("OWN|/config|101:104\nOWNMISS|/gone|\nOWN|/data|0:0\ngarbage\n")
		if len(found) != 2 || found["/config"] != "101:104" || found["/data"] != "0:0" {
			t.Fatalf("found = %v", found)
		}
		if len(missing) != 1 || missing[0] != "/gone" {
			t.Fatalf("missing = %v", missing)
		}
	})
}

// The EACCES incident's actual shape: everything agreed, and everything agreed
// on a Synology's numbering.
//
// The sweep found no drift — correctly — so the restore log said only
// "ownership verified" and the trap sprang days later, when PUID/PGID were
// edited to a local account and the linuxserver init re-owned only the
// top-level mount directories.
func TestForeignConsistentOwners(t *testing.T) {
	cases := []struct {
		name  string
		found map[string]string
		want  []string
	}{
		{
			"the incident: one Synology pair across every mount",
			map[string]string{"/config": "1026:100", "/data": "1026:100"},
			[]string{"1026:100"}, // one fact, not two
		},
		{"an ordinary desktop id is not news", map[string]string{"/config": "1000:1000"}, nil},
		{"a service account inside the image is not news", map[string]string{"/db": "999:999"}, nil},
		{"root is not news", map[string]string{"/data": "0:0"}, nil},
		{
			"Unraid's nobody/users pair",
			map[string]string{"/data": "99:100"},
			[]string{"99:100"},
		},
		{
			"a subordinate id from a user namespace",
			map[string]string{"/data": "1000000:1000000"},
			[]string{"1000000:1000000"},
		},
		{
			"two distinct foreign pairs are both named, sorted",
			map[string]string{"/a": "99:100", "/b": "1026:100", "/c": "1000:1000"},
			[]string{"1026:100", "99:100"}, // string order, and 1000:1000 excluded
		},
		{"nothing to judge", map[string]string{}, nil},
		{"a value stat could not produce", map[string]string{"/x": "notanowner"}, nil},
		{"a half pair", map[string]string{"/x": "1026"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := foreignConsistentOwners(tc.found)
			if len(got) != len(tc.want) {
				t.Fatalf("foreignConsistentOwners = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("foreignConsistentOwners = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
