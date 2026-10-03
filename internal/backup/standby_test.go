package backup

import (
	"errors"
	"testing"
	"time"
)

// TestRehearseStandbyTeardownAlways is the acceptance property: whatever the
// outcome — restore error, unhealthy clone, or a healthy success — the clone is
// ALWAYS torn down, so a rehearsal never leaves a container/volumes behind.
func TestRehearseStandbyTeardownAlways(t *testing.T) {
	const to = 30 * time.Second

	// 1) Health check fails → not ok, but teardown still runs.
	torn := 0
	ok, boot, detail := rehearseStandby("app-standby", to,
		func() error { return nil },
		func() (bool, int64) { return false, 1234 },
		func() { torn++ })
	if ok {
		t.Error("expected not-ok when health fails")
	}
	if torn != 1 {
		t.Fatalf("teardown must run on health failure (got %d)", torn)
	}
	if boot != 1234 || detail == "" {
		t.Errorf("failure path should report boot time + detail: boot=%d detail=%q", boot, detail)
	}

	// 2) Restore itself errors → not ok, teardown still runs (clone may be partial).
	torn = 0
	ok, _, _ = rehearseStandby("app-standby", to,
		func() error { return errors.New("boom") },
		func() (bool, int64) { t.Fatal("health must not run when restore errors"); return true, 0 },
		func() { torn++ })
	if ok || torn != 1 {
		t.Fatalf("teardown must run on restore error: ok=%v torn=%d", ok, torn)
	}

	// 3) Healthy success → ok with boot time, and teardown STILL runs (rehearsal
	//    always cleans up its throwaway clone).
	torn = 0
	ok, boot, _ = rehearseStandby("app-standby", to,
		func() error { return nil },
		func() (bool, int64) { return true, 4100 },
		func() { torn++ })
	if !ok || boot != 4100 || torn != 1 {
		t.Fatalf("success path wrong: ok=%v boot=%d torn=%d", ok, boot, torn)
	}
}
