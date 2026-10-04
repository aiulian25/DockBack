package api

import (
	"strconv"
	"strings"

	"dockback/internal/store"
)

// Defaults for a fresh install. In the 2026-10-04 recovery seven stacks had never
// been backed up, scrub and drills were off, and retention kept everything,
// because every one of those started switched off. They are chosen once, when
// DockBack creates its database; an existing installation is never changed.
const (
	freshScheduleName      = "This machine — weekly"
	freshScheduleTime      = "03:00"
	freshScheduleWeekday   = 0 // Sunday
	freshScrubIntervalDays = 7
	freshDrillIntervalDays = 30
	freshKeepDaily         = 7
	freshKeepWeekly        = 4
	freshKeepMonthly       = 3
	freshScheduleIDLength  = 12
)

// ApplyFreshInstallDefaults turns on a weekly backup of the local node (when
// there is one), a weekly scrub, a monthly restore drill, and retention of
// 7 daily, 4 weekly and 3 monthly with auto-prune. The caller decides that this
// is a fresh install; it returns what it turned on, for the startup log.
func ApplyFreshInstallDefaults(st *store.Store, localNode bool) (string, error) {
	settings := map[string]string{
		scrubIntervalKey:      strconv.Itoa(freshScrubIntervalDays),
		drillIntervalKey:      strconv.Itoa(freshDrillIntervalDays),
		retentionDailyKey:     strconv.Itoa(freshKeepDaily),
		retentionWeeklyKey:    strconv.Itoa(freshKeepWeekly),
		retentionMonthlyKey:   strconv.Itoa(freshKeepMonthly),
		retentionAutopruneKey: strconv.FormatBool(true),
	}
	for key, value := range settings {
		if err := st.SetSetting(key, value); err != nil {
			return "", err
		}
	}
	turnedOn := []string{"a weekly scrub", "a monthly restore drill", "retention of 7 daily, 4 weekly and 3 monthly backups with auto-prune"}
	if localNode {
		sc := Schedule{
			ID: randToken()[:freshScheduleIDLength], Name: freshScheduleName, Enabled: true,
			Kind: "weekly", Time: freshScheduleTime, Weekday: freshScheduleWeekday,
			Targets: []ScheduleTarget{{NodeID: localNodeID}},
		}
		row, err := sc.toRow()
		if err != nil {
			return "", err
		}
		if err := st.UpsertSchedule(row); err != nil {
			return "", err
		}
		turnedOn = append([]string{"a weekly backup of this machine (Sunday " + freshScheduleTime + ")"}, turnedOn...)
	}
	return "turned on " + strings.Join(turnedOn, ", ") + " — change any of it in Settings", nil
}
