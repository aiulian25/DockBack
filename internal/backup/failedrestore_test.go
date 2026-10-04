package backup

import (
	"path/filepath"
	"strings"
	"testing"

	"dockback/internal/store"
)

// A restore in the 2026-10-04 recovery died half-way through a stack's files and left
// the container stopped on partly-overwritten data, saying nothing. What a
// failed restore leaves behind is now decided on purpose.
func TestWhatAFailedRestoreLeavesBehind(t *testing.T) {
	cases := []struct {
		name                               string
		haveSnapshot, wasRunning, canceled bool
		want                               failedRestoreStep
	}{
		{"a snapshot exists: roll back to it", true, true, false, failedRestoreRollBack},
		{"a snapshot exists even if it was stopped: still roll back", true, false, false, failedRestoreRollBack},
		{"no snapshot, was running: start it again rather than leave it down", false, true, false, failedRestoreRestart},
		{"no snapshot, was not running: leave it as it was", false, false, false, failedRestoreLeave},
		// A cancel is the operator stopping it — never roll back behind their back.
		{"canceled with a snapshot: leave it, the operator decides", true, true, true, failedRestoreLeave},
		{"canceled without a snapshot: leave it", false, true, true, failedRestoreLeave},
	}
	for _, tc := range cases {
		if got := failedRestoreAction(tc.haveSnapshot, tc.wasRunning, tc.canceled); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

// Step 27: a move ends with what Docker cannot see that may still point at the
// old machine, and the variables in this container that hold addresses.
func TestMovedChecklistNamesWhatDockerCannotSee(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, n := range []*store.Node{{ID: "old", Name: "old-host"}, {ID: "new", Name: "new-host"}} {
		if err := st.UpsertNode(n); err != nil {
			t.Fatal(err)
		}
	}
	var lines []string
	e := &Engine{Store: st, Log: func(_, _, msg string) { lines = append(lines, msg) }}
	e.logMovedChecklist("b1", "old", "new", []string{"APP_URL", "TS_IP"})
	all := strings.Join(lines, "\n")
	for _, want := range []string{"Moved from old-host to new-host", "Nginx Proxy Manager", "Pi-hole", "cron", "Uptime Kuma", "APP_URL, TS_IP"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
}
