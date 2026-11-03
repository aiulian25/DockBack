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
		name                                         string
		haveSnapshot, wasRunning, canceled, replaced bool
		want                                         failedRestoreStep
	}{
		{"a snapshot exists: roll back to it", true, true, false, false, failedRestoreRollBack},
		{"a snapshot exists even if it was stopped: still roll back", true, false, false, false, failedRestoreRollBack},
		{"no snapshot, was running: start it again rather than leave it down", false, true, false, false, failedRestoreRestart},
		{"no snapshot, was not running: leave it as it was", false, false, false, false, failedRestoreLeave},
		// A cancel is the operator stopping it — never roll back behind their back.
		{"canceled with a snapshot: leave it, the operator decides", true, true, true, false, failedRestoreLeave},
		{"canceled without a snapshot: leave it", false, true, true, false, failedRestoreLeave},
		{"replaced, with a snapshot: roll back to it", true, true, false, true, failedRestoreRollBack},
		// Starting it would run the backup's image over data it did not write.
		{"replaced, no snapshot, was running: do not start it", false, true, false, true, failedRestoreLeave},
	}
	for _, tc := range cases {
		if got := failedRestoreAction(tc.haveSnapshot, tc.wasRunning, tc.canceled, tc.replaced); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

// Only a rollback over a replaced container rebuilds it from the snapshot. An
// in-place one must not: there the container's image never changed.
func TestRollbackRebuildsOnlyAReplacedContainer(t *testing.T) {
	opts := RestoreOptions{NodeID: "n1", TargetID: "cid"}
	if got := rollbackOptions(opts, "snap", true); !got.Recreate || !got.rollback || got.BackupID != "snap" || got.TargetID != "cid" {
		t.Errorf("after a replacement: %+v", got)
	}
	if got := rollbackOptions(opts, "snap", false); got.Recreate {
		t.Errorf("after an in-place restore it must not recreate: %+v", got)
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
