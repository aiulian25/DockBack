package backup

import (
	"strings"
	"testing"
)

func TestStatefulMarkers(t *testing.T) {
	t.Run("the probe looks for every marker at every destination, depth one", func(t *testing.T) {
		script := statefulMarkerScript([]string{"/config", "/data"})
		for _, want := range []string{"'/config'", "'/data'", "'PG_VERSION'", "'ibdata1'", "'dump.rdb'", "'appendonlydir'", "'mongod.lock'"} {
			if !strings.Contains(script, want) {
				t.Errorf("script must look for %s: %s", want, script)
			}
		}
		// A recursive search would walk a media library to find nothing.
		if strings.Contains(script, "find ") {
			t.Error("the markers sit at the top of a data directory; this must not recurse")
		}
	})

	t.Run("parse", func(t *testing.T) {
		got := parseStatefulMarkers("MARKER|PG_VERSION|/var/lib/webapp/pgdata\nMARKER|ibdata1|/db\nnoise\n")
		if got["/var/lib/webapp/pgdata"] != "postgres" {
			t.Errorf("PG_VERSION must read as postgres, got %q", got["/var/lib/webapp/pgdata"])
		}
		if got["/db"] != "mysql" {
			t.Errorf("ibdata1 must read as mysql, got %q", got["/db"])
		}
		if len(got) != 2 {
			t.Errorf("noise must be ignored: %v", got)
		}
		// Two markers at one destination must resolve the same way every run, or
		// the finding's wording changes between identical backups.
		twice := parseStatefulMarkers("MARKER|appendonlydir|/data\nMARKER|dump.rdb|/data\n")
		if twice["/data"] != "redis" {
			t.Errorf("both Redis layouts must read as redis, got %q", twice["/data"])
		}
		if len(parseStatefulMarkers("MARKER|not-a-marker|/data\n")) != 0 {
			t.Error("an unknown marker name must be ignored, not invented into an engine")
		}
	})

	t.Run("decision table", func(t *testing.T) {
		pg := map[string]string{"/pgdata": "postgres"}
		for _, tc := range []struct {
			name         string
			found        map[string]string
			engineKind   string
			pauseMode    string
			byOperator   bool
			wantStop     bool
			wantSeverity string
		}{
			{
				// The webapp shape: markers present, image unrecognised, default
				// pause mode. The copy window gets quiesced and the finding is loud.
				name:  "marker present, unknown image, default pause -> stop and danger",
				found: pg, engineKind: "", pauseMode: PausePause, byOperator: false,
				wantStop: true, wantSeverity: FindingDanger,
			},
			{
				name:  "marker present, unknown image, live copy -> stop and danger",
				found: pg, engineKind: "", pauseMode: PauseNone, byOperator: false,
				wantStop: true, wantSeverity: FindingDanger,
			},
			{
				// Already quiescing; nothing to force, but the operator still has an
				// unrecognised database being file-copied instead of dumped.
				name:  "marker present, already stopping -> no change, still danger",
				found: pg, engineKind: "", pauseMode: PauseStop, byOperator: false,
				wantStop: false, wantSeverity: FindingDanger,
			},
			{
				// An explicit instruction is not overridden. The finding stays, at
				// warn: acknowledged, not resolved.
				name:  "the operator's own pause choice stands, and downgrades the finding",
				found: pg, engineKind: "", pauseMode: PauseNone, byOperator: true,
				wantStop: false, wantSeverity: FindingWarn,
			},
			{
				// The dump path already takes a consistent logical copy.
				name:  "marker present but the engine was recognised -> nothing to add",
				found: pg, engineKind: "postgres", pauseMode: PausePause, byOperator: false,
				wantStop: false, wantSeverity: "",
			},
			{
				name:  "no marker -> no change",
				found: map[string]string{}, engineKind: "", pauseMode: PauseNone, byOperator: false,
				wantStop: false, wantSeverity: "",
			},
			{
				// A probe that could not run returns nothing, and nothing must never
				// stop a container: that would be an outage caused by the backup.
				name:  "an unreadable probe is not a stateful verdict",
				found: nil, engineKind: "", pauseMode: PausePause, byOperator: false,
				wantStop: false, wantSeverity: "",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				stop, severity := decideStatefulVolumes(tc.found, tc.engineKind, tc.pauseMode, tc.byOperator)
				if stop != tc.wantStop {
					t.Errorf("forceStop = %v, want %v", stop, tc.wantStop)
				}
				if severity != tc.wantSeverity {
					t.Errorf("severity = %q, want %q", severity, tc.wantSeverity)
				}
			})
		}
	})

	t.Run("the description names every destination in a stable order", func(t *testing.T) {
		got := describeStatefulMarkers(map[string]string{"/b": "mysql", "/a": "postgres"})
		if got != "/a (postgres), /b (mysql)" {
			t.Errorf("got %q", got)
		}
	})
}
