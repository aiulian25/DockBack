package api

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"dockback/internal/config"
)

// Two settings key spaces grow with the OUTSIDE world rather than with the
// fleet: one lockout row per address that has ever failed a sign-in, one
// throttle stamp per alert kind per scope. Nothing removed either, so on an
// internet-adjacent deployment the table grew forever — and every row of it
// goes into every application backup.

func pruneServer(t *testing.T) *Server {
	t.Helper()
	st := testStore(t)
	return &Server{store: st, guard: newLoginGuard(st), cfg: &config.Config{}}
}

func TestPruneLockoutsKeepsWhatStillMatters(t *testing.T) {
	s := pruneServer(t)
	now := time.Now().Unix()
	old := time.Now().Add(-2 * lockoutRetention).Unix()

	write := func(key string, rec lockRecord) {
		b, _ := json.Marshal(rec)
		if err := s.store.SetSetting(key, string(b)); err != nil {
			t.Fatal(err)
		}
	}
	// Long silent — the only thing safe to drop.
	write("lockout:ip:198.51.100.1", lockRecord{Strikes: 4, Until: old, WindowStart: old})
	// Locked right now.
	write("lockout:ip:198.51.100.2", lockRecord{Strikes: 1, Until: now + 600, WindowStart: now})
	// Not locked, but its counting window is open — the run-up to a lockout.
	write("lockout:ip:198.51.100.3", lockRecord{Fails: 3, WindowStart: now, Until: 0})
	// Recently locked and released: the strike count is what makes the NEXT
	// lockout longer, so it must survive.
	write("lockout:user:admin", lockRecord{Strikes: 6, Until: now - 60, WindowStart: now - 120})
	// Unreadable: left alone rather than guessed at.
	if err := s.store.SetSetting("lockout:ip:garbage", "not json"); err != nil {
		t.Fatal(err)
	}

	s.pruneLockouts()

	gone := []string{"lockout:ip:198.51.100.1"}
	kept := []string{"lockout:ip:198.51.100.2", "lockout:ip:198.51.100.3", "lockout:user:admin", "lockout:ip:garbage"}
	for _, k := range gone {
		if v, _ := s.store.GetSetting(k, ""); v != "" {
			t.Errorf("%s has been silent for a month and should be gone", k)
		}
	}
	for _, k := range kept {
		if v, _ := s.store.GetSetting(k, ""); v == "" {
			t.Errorf("%s still matters and must be kept", k)
		}
	}
}

// The escalation added in step 6 depends on the strike count outliving the
// policy window. Pruning on the window would reset it every quarter of an hour.
func TestLockoutRetentionOutlivesThePolicyWindow(t *testing.T) {
	if lockoutRetention <= ipLockPolicy.window {
		t.Fatalf("retention %v must outlast the %v policy window, or a patient attacker gets a fresh escalation ladder",
			lockoutRetention, ipLockPolicy.window)
	}
	if lockoutRetention <= ipLockPolicy.maxLock {
		t.Fatalf("retention %v must outlast the %v maximum lock, or a live lockout could be pruned",
			lockoutRetention, ipLockPolicy.maxLock)
	}
}

func TestPruneAlertStampsDropsOnlyDeadOnes(t *testing.T) {
	s := pruneServer(t)
	fresh := strconv.FormatInt(time.Now().Unix(), 10)
	stale := strconv.FormatInt(time.Now().Add(-2*alertStampRetention).Unix(), 10)

	for k, v := range map[string]string{
		"alert.last.destination.full.d1":       fresh,
		"alert.last.auth.lockout.198.51.100.9": stale,
		"alert.last.key.unescrowed":            stale,
		"alert.last.container.crashed.web":     "not a number",
	} {
		if err := s.store.SetSetting(k, v); err != nil {
			t.Fatal(err)
		}
	}
	// A setting that merely starts similarly must not be touched.
	if err := s.store.SetSetting("alert.dest_full_pct", "90"); err != nil {
		t.Fatal(err)
	}

	s.pruneAlertStamps()

	if v, _ := s.store.GetSetting("alert.last.destination.full.d1", ""); v == "" {
		t.Error("a live cooldown must survive, or the alert fires again immediately")
	}
	for _, k := range []string{
		"alert.last.auth.lockout.198.51.100.9",
		"alert.last.key.unescrowed",
		"alert.last.container.crashed.web",
	} {
		if v, _ := s.store.GetSetting(k, ""); v != "" {
			t.Errorf("%s can no longer suppress anything and should be gone", k)
		}
	}
	if v, _ := s.store.GetSetting("alert.dest_full_pct", ""); v != "90" {
		t.Error("a configuration setting sharing the prefix must never be pruned")
	}
}

// Both retentions must outlast every cooldown, or a prune could un-suppress an
// alert that is still meant to be quiet.
func TestAlertRetentionOutlastsEveryCooldown(t *testing.T) {
	for name, cd := range map[string]time.Duration{
		"offsite": offsiteAlertCooldown, "destination full": destFullCooldown,
		"key": keyAlertCooldown, "target missing": targetMissingCooldown,
		"low RPO paused": lowRPOPausedCooldown, "forecast": destForecastCooldown,
		"anomaly": anomalyCooldown, "crash": crashAlertCooldown,
		"trust-all proxies": trustAllProxiesCooldown, "auth": authAlertCooldown,
	} {
		if alertStampRetention <= cd {
			t.Errorf("%s cooldown is %v, retention is %v — pruning would end a live suppression", name, cd, alertStampRetention)
		}
	}
}
