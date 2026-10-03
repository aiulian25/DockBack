package api

import (
	"testing"
	"time"

	"dockback/internal/store"
)

// F31: the critical-DB tier knobs resolve to their shipped defaults when unset,
// honor a configured value, and self-clamp to a sane range.
func TestCriticalTierResolvers(t *testing.T) {
	s := &Server{store: testStore(t)}

	// Defaults reproduce today's hardcoded 300 / 60 / 3.
	if got := s.criticalRPOMin(); got != 300 {
		t.Errorf("default criticalRPOMin = %d, want 300", got)
	}
	if got := s.criticalTickSeconds(); got != 60 {
		t.Errorf("default criticalTickSeconds = %d, want 60", got)
	}
	if got := s.criticalFailLimit(); got != 3 {
		t.Errorf("default criticalFailLimit = %d, want 3", got)
	}

	// Configured values are honored (the acceptance: a 2-minute RPO floor).
	_ = s.store.SetSetting("critical.rpo_min_seconds", "120")
	_ = s.store.SetSetting("critical.tick_seconds", "45")
	_ = s.store.SetSetting("critical.fail_limit", "5")
	if got := s.criticalRPOMin(); got != 120 {
		t.Errorf("criticalRPOMin = %d, want 120", got)
	}
	if got := s.criticalTickSeconds(); got != 45 {
		t.Errorf("criticalTickSeconds = %d, want 45", got)
	}
	if got := s.criticalFailLimit(); got != 5 {
		t.Errorf("criticalFailLimit = %d, want 5", got)
	}

	// Self-clamping (defense in depth beyond coerceSetting).
	_ = s.store.SetSetting("critical.rpo_min_seconds", "10")
	if got := s.criticalRPOMin(); got != 60 {
		t.Errorf("criticalRPOMin low should clamp to 60, got %d", got)
	}
	_ = s.store.SetSetting("critical.rpo_min_seconds", "99999")
	if got := s.criticalRPOMin(); got != 3600 {
		t.Errorf("criticalRPOMin high should clamp to 3600, got %d", got)
	}
	_ = s.store.SetSetting("critical.tick_seconds", "5")
	if got := s.criticalTickSeconds(); got != 30 {
		t.Errorf("criticalTickSeconds low should clamp to 30, got %d", got)
	}
	_ = s.store.SetSetting("critical.fail_limit", "99")
	if got := s.criticalFailLimit(); got != 10 {
		t.Errorf("criticalFailLimit high should clamp to 10, got %d", got)
	}
}

func TestRPODue(t *testing.T) {
	now := time.Unix(10_000, 0)
	rpo := 15 * time.Minute

	cases := []struct {
		name        string
		lastPoint   time.Time
		lastEnqueue time.Time
		want        bool
	}{
		{"never backed up", time.Time{}, time.Time{}, true},
		{"fresh backup", now.Add(-5 * time.Minute), time.Time{}, false},
		{"stale backup due", now.Add(-20 * time.Minute), time.Time{}, true},
		{"stale backup but just enqueued", now.Add(-20 * time.Minute), now.Add(-1 * time.Minute), false},
		{"enqueue also stale", now.Add(-30 * time.Minute), now.Add(-16 * time.Minute), true},
		{"exactly at rpo", now.Add(-15 * time.Minute), time.Time{}, true},
	}
	for _, c := range cases {
		if got := rpoDue(c.lastPoint, c.lastEnqueue, now, rpo); got != c.want {
			t.Errorf("%s: rpoDue = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestConsecutiveVerifyFailures(t *testing.T) {
	b := func(status, verified string) *store.Backup {
		return &store.Backup{TargetName: "db", Status: status, Verified: verified}
	}
	cases := []struct {
		name      string
		backups   []*store.Backup
		wantFails int
		wantOK    bool
	}{
		{"none", nil, 0, false},
		{"all fail-verify", []*store.Backup{b("success", "failed"), b("success", "failed"), b("success", "failed")}, 3, false},
		{"verified stops streak", []*store.Backup{b("success", "failed"), b("success", "verified"), b("success", "failed")}, 1, true},
		{"backup failure counts", []*store.Backup{b("failed", ""), b("success", "failed")}, 2, false},
		{"pending ignored, then verified", []*store.Backup{b("running", ""), b("success", "verified")}, 0, true},
		{"unverified neutral before verified", []*store.Backup{b("success", "unverified"), b("success", "failed"), b("success", "verified")}, 1, true},
		{"healthy newest", []*store.Backup{b("success", "verified")}, 0, true},
	}
	for _, c := range cases {
		fails, ok := consecutiveVerifyFailures(c.backups, "db")
		if fails != c.wantFails || ok != c.wantOK {
			t.Errorf("%s: got (%d,%v), want (%d,%v)", c.name, fails, ok, c.wantFails, c.wantOK)
		}
	}
	// A different target name must not be counted.
	other := []*store.Backup{{TargetName: "other", Status: "success", Verified: "failed"}}
	if fails, _ := consecutiveVerifyFailures(other, "db"); fails != 0 {
		t.Errorf("other-target: got %d, want 0", fails)
	}
}

// newestVerifiedFor and lowRPOHealth must read a container's OWN backups, not a
// node-wide window filtered by name — otherwise a busy node's newer rows push a
// critical DB's history out of the window, faking an RPO breach (newestVerifiedFor)
// and a reset failure streak (lowRPOHealth). Step 11 / finding #5.
func TestLowRPOHelpersQueryByTarget(t *testing.T) {
	s := &Server{store: testStore(t)}

	// A noisy neighbour fills any node-wide window with newer, verified rows.
	for i := 0; i < 105; i++ {
		_ = s.store.CreateBackup(&store.Backup{
			ID: "noisy-" + itoa(i), NodeID: "n1", TargetName: "noisy",
			Status: "success", Verified: "verified", CreatedAt: int64(2000 + i),
		})
	}
	// The critical DB: an OLDER verified recovery point, then two recent failures.
	_ = s.store.CreateBackup(&store.Backup{ID: "db-ok", NodeID: "n1", TargetName: "db", Status: "success", Verified: "verified", CreatedAt: 1000})
	_ = s.store.CreateBackup(&store.Backup{ID: "db-f1", NodeID: "n1", TargetName: "db", Status: "failed", Verified: "failed", CreatedAt: 1001})
	_ = s.store.CreateBackup(&store.Backup{ID: "db-f2", NodeID: "n1", TargetName: "db", Status: "failed", Verified: "failed", CreatedAt: 1002})
	// The SAME name on another node must never affect n1's answers.
	_ = s.store.CreateBackup(&store.Backup{ID: "db-n2", NodeID: "n2", TargetName: "db", Status: "success", Verified: "verified", CreatedAt: 9999})

	// Precondition — document the bug: the OLD node-wide approach (100 newest,
	// then filter by name) sees zero of the DB's rows, so it reports a healthy DB
	// as having no verified backup and no failures.
	wide, _ := s.store.ListBackups("n1", 100)
	if f, ok := consecutiveVerifyFailures(wide, "db"); f != 0 || ok {
		t.Fatalf("precondition: node-wide window should miss the DB entirely, got fails=%d ok=%v", f, ok)
	}

	// newestVerifiedFor finds the DB's own verified recovery point (ts=1000),
	// scoped to n1 — never n2's newer 9999.
	if at, ok := s.newestVerifiedFor("n1", "db"); !ok || at != 1000 {
		t.Fatalf("newestVerifiedFor = (%d,%v), want (1000,true)", at, ok)
	}

	// lowRPOHealth counts the two failures standing before the verified point.
	if fails, ok := s.lowRPOHealth("n1", "db"); fails != 2 || !ok {
		t.Fatalf("lowRPOHealth = (%d,%v), want (2,true)", fails, ok)
	}
}

func TestCritKeyDistinct(t *testing.T) {
	// Same name on different nodes, and different names, must not collide.
	if critKey("n1", "db") == critKey("n2", "db") {
		t.Fatal("keys collide across nodes")
	}
	if critKey("n1", "a") == critKey("n1", "b") {
		t.Fatal("keys collide across names")
	}
}
