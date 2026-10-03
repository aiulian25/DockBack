package api

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"dockback/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestLoginGuardLockoutAndReset(t *testing.T) {
	g := newLoginGuard(testStore(t))
	ip, user := "203.0.113.7", "admin"

	if d := g.blocked(ip, user); d != 0 {
		t.Fatalf("fresh client should not be blocked, got %v", d)
	}
	// IP policy locks at 5 failures within the window.
	for i := 0; i < ipLockPolicy.max; i++ {
		g.recordFail(ip, user)
	}
	if d := g.blocked(ip, user); d <= 0 {
		t.Fatalf("expected lockout after %d failures", ipLockPolicy.max)
	}
	// A successful sign-in clears the lock for that IP + account.
	g.reset(ip, user)
	if d := g.blocked(ip, user); d != 0 {
		t.Fatalf("reset should clear the lock, got %v", d)
	}
	// A different IP is unaffected by another IP's lockout.
	for i := 0; i < ipLockPolicy.max; i++ {
		g.recordFail("198.51.100.9", user)
	}
	if d := g.blocked("203.0.113.7", "other-user"); d != 0 {
		t.Fatalf("unrelated IP/account should not be blocked, got %v", d)
	}
}

func TestArgonSemaphoreSheds(t *testing.T) {
	g := newLoginGuard(testStore(t))
	for i := 0; i < argonMaxConcurrent; i++ {
		if !g.acquireArgon(context.Background()) {
			t.Fatalf("should acquire slot %d", i)
		}
	}
	// Pool is full: a canceled context must not block/acquire.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if g.acquireArgon(ctx) {
		t.Fatal("should not acquire when pool is saturated")
	}
	for i := 0; i < argonMaxConcurrent; i++ {
		g.releaseArgon()
	}
	if !g.acquireArgon(context.Background()) {
		t.Fatal("should acquire after release")
	}
	g.releaseArgon()
}

// TestLockoutStrikeOverflow: the escalation doubles the lock on every repeat, so
// a long-lived record eventually shifts past the width of an int64 — where the
// duration turns negative and then zero, expiring the lock in the past. The one
// address patient enough to keep tripping the lockout would be the one it stops
// applying to, which is exactly backwards.
func TestLockoutStrikeOverflow(t *testing.T) {
	// Every strike count that used to break, plus the boundaries around the cap.
	for _, strikes := range []int{1, 2, 20, 21, 62, 63, 64, 65, 100, 1000} {
		g := newLoginGuard(testStore(t))
		key := lockKey("ip", "203.0.113.7")
		g.save(key, lockRecord{Strikes: strikes, Fails: ipLockPolicy.max - 1, WindowStart: time.Now().Unix()})

		_, lockedFor := g.bump(key, ipLockPolicy)
		if lockedFor <= 0 {
			t.Errorf("strike %d: crossing the threshold must lock, got %v", strikes, lockedFor)
		}
		if rec := g.load(key); rec.Until <= time.Now().Unix() {
			t.Errorf("strike %d: lock expires at %d, which is not in the future", strikes, rec.Until)
		}
		// Anything past the first couple of strikes is clamped to the policy cap.
		if strikes >= 3 && lockedFor != ipLockPolicy.maxLock {
			t.Errorf("strike %d: locked for %v, want the %v cap", strikes, lockedFor, ipLockPolicy.maxLock)
		}
	}
}

// The escalation itself must keep working: a second lockout lasts longer than
// the first, up to the cap. This is the behaviour README calls an escalating
// lockout, and the bound above must not flatten it.
func TestLockoutEscalatesUpToTheCap(t *testing.T) {
	g := newLoginGuard(testStore(t))
	key := lockKey("ip", "203.0.113.8")

	var durations []time.Duration
	for strike := 1; strike <= 4; strike++ {
		rec := g.load(key)
		rec.Fails = ipLockPolicy.max - 1
		rec.WindowStart = time.Now().Unix()
		rec.Until = 0 // the previous lock has expired
		g.save(key, rec)

		_, lockedFor := g.bump(key, ipLockPolicy)
		durations = append(durations, lockedFor)
	}
	if durations[0] != ipLockPolicy.base {
		t.Errorf("first lockout = %v, want the %v base", durations[0], ipLockPolicy.base)
	}
	if durations[1] <= durations[0] {
		t.Errorf("a repeat lockout must last longer: %v then %v", durations[0], durations[1])
	}
	for i, d := range durations {
		if d > ipLockPolicy.maxLock {
			t.Errorf("lockout %d = %v, past the %v cap", i+1, d, ipLockPolicy.maxLock)
		}
	}
}
