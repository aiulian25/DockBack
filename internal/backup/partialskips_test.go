package backup

import (
	"strings"
	"testing"
)

// A side-car that shares its app's folders was told its backup was PARTIAL,
// one line after being told each folder was captured by the app's backups. The
// grade, runbook and digest already left covered folders out; the warning now
// counts the way they do.
func TestPartialWarningCountsOnlyUncoveredMounts(t *testing.T) {
	var warnings []string
	e := &Engine{Log: func(_, level, msg string) {
		if level == "WARN" {
			warnings = append(warnings, msg)
		}
	}}

	e.logPartialSkips("b1", []SkippedMount{
		{Destination: "/var/www/html", Type: "bind", CoveredBy: "Nextcloud"},
		{Destination: "/var/www/html/data", Type: "bind", CoveredBy: "Nextcloud"},
	})
	if len(warnings) != 0 {
		t.Fatalf("every skipped mount is captured by another backup, so nothing is partial: %v", warnings)
	}

	e.logPartialSkips("b1", []SkippedMount{
		{Destination: "/var/www/html", Type: "bind", CoveredBy: "Nextcloud"},
		{Destination: "/mnt/external", Type: "bind", Reason: "larger than 5 GiB"},
	})
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "1 mount(s) NOT captured") {
		t.Fatalf("only the mount no backup captures makes it partial: %v", warnings)
	}
}
