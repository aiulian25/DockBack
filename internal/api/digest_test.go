package api

import (
	"testing"
	"time"

	"dockback/internal/config"
	"dockback/internal/notify"
	"dockback/internal/store"
)

// notifyDigest builds a notify.Config with the digest enabled in the given
// success-mode, for the per-backup-gate tests.
func notifyDigest(mode string) notify.Config {
	return notify.Config{Digest: notify.DigestConfig{Enabled: true, Time: "09:00", SuccessMode: mode}}
}

// F15: the 24h aggregation must match the Insights catalog rules — count finished
// backups in the window, split verified/failed, and ignore running + older ones.
func TestDigestAggregateBackupCounts(t *testing.T) {
	now := time.Now()
	within := now.Add(-2 * time.Hour).Unix()
	old := now.Add(-30 * time.Hour).Unix()
	since := now.Add(-24 * time.Hour).Unix()

	list := []*store.Backup{
		{Status: "success", Verified: "verified", CreatedAt: within},
		{Status: "success", Verified: "verified", CreatedAt: within},
		{Status: "failed", Verified: "failed", CreatedAt: within},
		{Status: "success", Verified: "unverified", CreatedAt: within}, // counts to total, not verified
		{Status: "running", CreatedAt: within},                         // in-flight → skipped
		{Status: "success", Verified: "verified", CreatedAt: old},      // outside window → skipped
	}
	total, verified, failed := aggregateBackupCounts(list, since)
	if total != 4 {
		t.Errorf("total = %d, want 4 (running + out-of-window excluded)", total)
	}
	if verified != 2 {
		t.Errorf("verified = %d, want 2", verified)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1", failed)
	}
}

// F15: the daily fire logic — baseline established without an immediate send, then
// fires once per day at/after the configured time and not again that day.
func TestDigestDue(t *testing.T) {
	loc := time.UTC
	day := func(h, m int) time.Time { return time.Date(2026, 7, 12, h, m, 0, 0, loc) }

	// First evaluation: never fires, just establishes a baseline at `now`.
	fire, newLast := digestDue("09:00", time.Time{}, day(10, 0))
	if fire {
		t.Error("first evaluation must not fire (no backfill)")
	}
	if !newLast.Equal(day(10, 0)) {
		t.Errorf("baseline = %v, want now", newLast)
	}

	// Baseline set yesterday morning; today at 09:00 the window fires once.
	yesterday := day(9, 0).Add(-24 * time.Hour)
	fire, newLast = digestDue("09:00", yesterday, day(9, 0))
	if !fire {
		t.Error("should fire at the scheduled window when last send was before it")
	}
	if !newLast.Equal(day(9, 0)) {
		t.Errorf("newLast = %v, want the fire time", newLast)
	}

	// Already sent for today's window → does not fire again the same day.
	fire, _ = digestDue("09:00", day(9, 0), day(14, 0))
	if fire {
		t.Error("must not fire twice in the same day's window")
	}

	// Before today's window → not yet.
	fire, keep := digestDue("09:00", yesterday, day(8, 59))
	if fire {
		t.Error("must not fire before the scheduled time")
	}
	if !keep.Equal(yesterday) {
		t.Errorf("lastSent should be unchanged before the window, got %v", keep)
	}

	// Unparseable time falls back to 09:00 (so it still fires around 9am).
	if fire, _ := digestDue("nonsense", yesterday, day(9, 30)); !fire {
		t.Error("a malformed time should fall back to 09:00 and still fire")
	}
}

// F15: with the digest set to REPLACE per-backup success, the engine's per-backup
// success ping is suppressed while failures/criticals still pass through.
func TestDigestReplacesPerBackupGate(t *testing.T) {
	st := testStore(t)
	s := &Server{store: st, cfg: &config.Config{EncryptionKey: make([]byte, 32)}}

	// Default (no digest configured): per-backup success is NOT suppressed.
	if s.digestReplacesPerBackup() {
		t.Error("with no digest configured, per-backup success must still send")
	}

	// Enable digest in replace mode.
	if err := s.saveNotifyConfig(notifyDigest("digest")); err != nil {
		t.Fatal(err)
	}
	if !s.digestReplacesPerBackup() {
		t.Error("digest replace mode should suppress per-backup success")
	}

	// "both" keeps per-backup pings.
	_ = s.saveNotifyConfig(notifyDigest("both"))
	if s.digestReplacesPerBackup() {
		t.Error("success-mode both must keep per-backup pings")
	}

	// Enabled but "per_backup" mode also keeps pings.
	_ = s.saveNotifyConfig(notifyDigest("per_backup"))
	if s.digestReplacesPerBackup() {
		t.Error("success-mode per_backup must keep per-backup pings")
	}
}
