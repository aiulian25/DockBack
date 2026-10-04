package api

import (
	"strings"
	"testing"
)

// A fresh install starts protected: a weekly backup of this machine, a weekly
// scrub, a monthly drill, and retention that prunes. main.go calls this only
// when it has just created the database, so an existing install never sees it.
func TestApplyFreshInstallDefaults(t *testing.T) {
	st := testStore(t)
	summary, err := ApplyFreshInstallDefaults(st, true)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		scrubIntervalKey: "7", drillIntervalKey: "30",
		retentionDailyKey: "7", retentionWeeklyKey: "4", retentionMonthlyKey: "3", retentionAutopruneKey: "true",
	} {
		if got, _ := st.GetSetting(key, ""); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	rows, err := st.ListSchedules()
	if err != nil || len(rows) != 1 {
		t.Fatalf("one schedule for this machine, got %d (%v)", len(rows), err)
	}
	sc := scheduleFromRow(rows[0])
	if !sc.Enabled || sc.Kind != "weekly" || len(sc.Targets) != 1 || sc.Targets[0].NodeID != localNodeID || sc.Targets[0].ContainerName != "" {
		t.Errorf("a weekly whole-node schedule for the local node, got %+v", sc)
	}
	if !strings.Contains(summary, "weekly backup of this machine") {
		t.Errorf("the startup log says what was turned on: %q", summary)
	}

	remoteOnly := testStore(t)
	if _, err := ApplyFreshInstallDefaults(remoteOnly, false); err != nil {
		t.Fatal(err)
	}
	if rows, _ := remoteOnly.ListSchedules(); len(rows) != 0 {
		t.Errorf("with no local node there is nothing to schedule yet, got %d schedule(s)", len(rows))
	}
}
