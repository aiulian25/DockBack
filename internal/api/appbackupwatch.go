package api

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"dockback/internal/appbackup"
	"dockback/internal/notify"
	"dockback/internal/store"
)

// Watching DockBack's own backup. Weekly is the floor; a configuration change
// is backed up once it has settled, and the hourly alert check says when the
// newest app backup is too old or missing. The 2026-10-04 recovery found it five weeks
// old, with nothing having said so.

const (
	// appBackupConfigMarkKey is the newest audit row an app backup covers.
	appBackupConfigMarkKey = "appbackup.config_mark"
	// appBackupConfigQuiet is how long the configuration must stay unchanged
	// before a change is backed up, so a burst of edits makes one backup.
	appBackupConfigQuiet = 10 * time.Minute
	// appBackupStaleAfter is how old the newest app backup may be before the
	// alert check says so: a missed weekly run, plus a day.
	appBackupStaleAfter = 8 * 24 * time.Hour
	// appBackupAlertCooldown repeats a standing app-backup alert once a day.
	appBackupAlertCooldown = 24 * time.Hour
)

// configChangePrefixes are the audited actions that change what an app backup
// holds: nodes, schedules, destinations, policies, accounts and settings.
var configChangePrefixes = []string{
	"admin.", "app.dest.", "appbackup.schedule.", "autosnap.", "bind_threshold.", "cluster.",
	"container.", "critical.", "destination.", "egress.audit.o", "export.cleanup.", "export_profile.",
	"exportpreset.", "hooks.", "key.escrow.", "node.add", "node.delete", "node.hostkey.", "node.policy.",
	"node.sidecar.", "node.update", "notify.update", "password.changed", "pause_mode.", "policy.",
	"redis.auth.", "regenerable.", "restore.ownership.", "restore.stepup.", "restore_timeout.",
	"schedule.create", "schedule.delete", "schedule.update", "security.key.rotate", "settings.",
	"stack.protect", "standby.delete", "standby.set", "token.create", "token.delete", "writeonly.",
}

// isConfigChange reports whether an audited action changed the configuration.
func isConfigChange(action string) bool {
	return slices.ContainsFunc(configChangePrefixes, func(prefix string) bool { return strings.HasPrefix(action, prefix) })
}

func (s *Server) setAppBackupConfigMark(auditID int64) {
	_ = s.store.SetSetting(appBackupConfigMarkKey, strconv.FormatInt(auditID, 10))
}

// appBackupAfterConfigChange takes an app backup once the configuration has
// changed since the last one and has then stayed unchanged for a quiet period.
// A stretch of audit rows with no configuration change moves the mark past
// them, so each check reads only what is new.
func (s *Server) appBackupAfterConfigChange(a appBackupSchedule, now time.Time) {
	raw, _ := s.store.GetSetting(appBackupConfigMarkKey, "")
	mark, err := strconv.ParseInt(raw, 10, 64)
	if raw == "" || err != nil {
		if newest, merr := s.store.MaxAuditID(); merr == nil {
			s.setAppBackupConfigMark(newest)
		}
		return
	}
	var lastSeen, lastChange int64
	_ = s.store.WalkAuditSince(mark, func(row *store.AuditChainRow) bool {
		lastSeen = row.ID
		if isConfigChange(row.Action) {
			lastChange = row.TS
		}
		return true
	})
	if lastChange == 0 {
		if lastSeen > mark {
			s.setAppBackupConfigMark(lastSeen)
		}
		return
	}
	if now.Sub(time.Unix(lastChange, 0)) < appBackupConfigQuiet {
		return
	}
	s.logSink("app-backup", "INFO", "The configuration changed — taking an application backup")
	s.runAppBackup(a)
}

// checkAppBackupAlert runs on the hourly alert tick. Silent until there is
// something to lose: with no container backups at all, DockBack's own catalog
// holds nothing worth an alert yet.
func (s *Server) checkAppBackupAlert(now time.Time) {
	if n, err := s.store.CountBackups(); err != nil || n == 0 {
		return
	}
	list, err := appbackup.List(s.appBackupDir())
	if err != nil {
		return
	}
	fix := "Turn on the schedule under Settings → Advanced → Application Backup & Restore, and give it an external destination so a copy leaves this machine."
	if len(list) == 0 {
		s.notifyThrottled(notify.KindAppBackupStale, "", "DockBack has no backup of itself",
			"There is no application backup, so losing DockBack's own data loses the catalog of every backup, the nodes and the settings. "+fix,
			appBackupAlertCooldown)
		return
	}
	age := now.Sub(time.Unix(list[0].CreatedAt, 0))
	if age <= appBackupStaleAfter {
		return
	}
	days := int(age.Hours() / 24)
	s.notifyThrottled(notify.KindAppBackupStale, "", fmt.Sprintf("DockBack's own backup is %d days old", days),
		fmt.Sprintf("The newest application backup is %s, from %d days ago. Anything changed since then would be lost with DockBack's data. %s", list[0].File, days, fix),
		appBackupAlertCooldown)
}

// failedDestinations renders "name: reason" pairs, sorted.
func failedDestinations(failed map[string]string) string {
	parts := make([]string, 0, len(failed))
	for name, reason := range failed {
		parts = append(parts, name+": "+reason)
	}
	slices.Sort(parts)
	return strings.Join(parts, "; ")
}
