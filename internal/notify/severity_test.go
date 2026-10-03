package notify

import "testing"

// Restore outcomes used to fire as scrub.failed, so severity routing and the
// per-channel MinSeverity filters could not tell "a stored backup rotted
// overnight" (hygiene) from "the restore I am running right now failed"
// (an incident). These tests pin the routing that fixes it.

func TestSeverityOfRestoreKinds(t *testing.T) {
	critical := []string{KindRestoreFailed, KindRestoreRolledBack}
	for _, k := range critical {
		if got := SeverityOf(k); got != SevCritical {
			t.Errorf("%s must be critical — a recovery that did not work; got %v", k, got)
		}
	}
	// A cancel is the operator's own choice, so it is not an emergency — but a
	// destructive operation was interrupted, so it is not silent either.
	if got := SeverityOf(KindRestoreCanceled); got != SevWarning {
		t.Errorf("%s must be a warning, got %v", KindRestoreCanceled, got)
	}
}

// The new kinds must be distinct strings, or filtering by kind cannot work.
func TestRestoreKindsAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range []string{
		KindBackupSuccess, KindBackupFailed, KindVerifyFailed, KindScrubFailed,
		KindNoOffsite, KindDestFull, KindDestForecast, KindMissedSchedule,
		KindTargetMissing, KindLowRPOPaused, KindKeyUnescrowed, KindBackupAnomaly,
		KindContainerCrashed, KindContainerOOM, KindHostKeyChanged, KindSidecarChanged,
		KindRestoreFailed, KindRestoreRolledBack, KindRestoreCanceled,
	} {
		if k == "" {
			t.Fatal("an event kind must never be empty")
		}
		if seen[k] {
			t.Fatalf("duplicate event kind %q — filtering by kind would merge two events", k)
		}
		seen[k] = true
	}
	// A restore kind must not collide with the scrub kind it replaced.
	if KindRestoreFailed == KindScrubFailed {
		t.Fatal("restore.failed must be distinct from scrub.failed — that separation is the whole feature")
	}
}

// Existing routing must be untouched: this feature adds kinds, it does not
// re-band anything that already worked.
func TestSeverityOfExistingKindsUnchanged(t *testing.T) {
	cases := map[string]Severity{
		KindBackupSuccess:    SevInfo,
		KindBackupFailed:     SevCritical,
		KindVerifyFailed:     SevCritical,
		KindScrubFailed:      SevCritical,
		KindHostKeyChanged:   SevCritical,
		KindSidecarChanged:   SevCritical,
		KindContainerCrashed: SevWarning,
		KindContainerOOM:     SevWarning,
		KindNoOffsite:        SevWarning,
		KindMissedSchedule:   SevWarning,
	}
	for k, want := range cases {
		if got := SeverityOf(k); got != want {
			t.Errorf("%s severity changed: got %v, want %v", k, got, want)
		}
	}
	// An unknown kind must default to warning — never silently "info", which
	// would drop a new alert below every channel's threshold.
	if got := SeverityOf("something.new"); got != SevWarning {
		t.Errorf("an unknown kind must default to warning, got %v", got)
	}
}
