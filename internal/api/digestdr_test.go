package api

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dockback/internal/store"
)

// TestDigestDRLine covers the acceptance shape: 3 targets (2 drilled-ok, 1
// undrilled + partial), key unacknowledged -> the exact substrings appear.
func TestDigestDRLine(t *testing.T) {
	line := drConfidenceLine(drCounts{
		Total: 3, DrilledOK: 2, Undrilled: 1, Partial: 1,
		AppDrillAt: 0, KeyConfirmed: false, Now: 1_000_000,
	})
	for _, want := range []string{"2 of 3", "1 undrilled", "1 partial", "NOT confirmed", "App-backup not yet proven"} {
		if !strings.Contains(line, want) {
			t.Errorf("DR line missing %q:\n%s", want, line)
		}
	}

	// Confirmed key + a passed recent app-backup drill flip the wording.
	ok := drConfidenceLine(drCounts{Total: 1, DrilledOK: 1, AppDrillAt: 1_000_000 - 2*86400, AppDrillOK: true, KeyConfirmed: true, Now: 1_000_000})
	if !strings.Contains(ok, "Key backup: confirmed.") || !strings.Contains(ok, "App-backup proven 2d ago") {
		t.Errorf("confirmed/proven wording wrong:\n%s", ok)
	}
	// A failed app-backup drill is surfaced.
	if bad := drConfidenceLine(drCounts{AppDrillAt: 1_000_000 - 3600, Now: 1_000_000}); !strings.Contains(bad, "App-backup drill FAILED") {
		t.Errorf("failed app-backup drill not surfaced:\n%s", bad)
	}

	// F75: the standby sentence appears ONLY when standbys are configured, so a
	// fleet without them keeps the byte-identical pre-F75 body.
	if strings.Contains(line, "Standby:") {
		t.Errorf("no standbys configured — line must not mention them:\n%s", line)
	}
	withSB := drConfidenceLine(drCounts{Total: 1, DrilledOK: 1, StandbyTotal: 2, StandbyProven: 1, Now: 1_000_000})
	if !strings.Contains(withSB, "Standby: 1 of 2 proven.") {
		t.Errorf("standby sentence missing/wrong:\n%s", withSB)
	}
}

// TestDigestDRGating proves the toggle: enabled appends "DR confidence"; disabled
// yields the byte-identical pre-F60 body.
func TestDigestDRGating(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st}
	now := time.Unix(1_700_000_000, 0)

	_, off := s.composeDigest(now, false)
	_, on := s.composeDigest(now, true)

	if strings.Contains(off, "DR confidence") {
		t.Errorf("disabled digest must NOT contain the DR block:\n%s", off)
	}
	if !strings.Contains(on, "DR confidence:") {
		t.Errorf("enabled digest must contain the DR block:\n%s", on)
	}
	// The enabled body is the disabled body plus the appended DR block (byte-identical prefix).
	if !strings.HasPrefix(on, off) {
		t.Errorf("enabled body must be the disabled body plus an appended block.\n off=%q\n  on=%q", off, on)
	}
}
