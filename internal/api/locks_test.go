package api

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

// TestNotifyThrottledCooldown verifies the recurring-warning cooldown: a second
// call within the window is suppressed, and a lapsed window fires again (PLAN §9.15).
func TestNotifyThrottledCooldown(t *testing.T) {
	st := testStore(t)
	s := &Server{store: st}
	// Assert on the persisted timestamp gate — the mechanism that enforces the
	// throttle — so no live notifier is needed.
	key := alertKey("destination.full", "dest1")
	// First call: no prior timestamp → should record one (not suppressed).
	if raw, _ := st.GetSetting(key, "0"); raw != "0" {
		t.Fatalf("precondition: expected no timestamp, got %q", raw)
	}
	s.recordAlert(key)
	raw, _ := st.GetSetting(key, "0")
	if raw == "0" {
		t.Fatal("first alert should record a timestamp")
	}
	// Within cooldown → suppressed.
	if !s.alertSuppressed(key, time.Hour) {
		t.Fatal("second alert within cooldown must be suppressed")
	}
	// Simulate the window lapsing.
	st.SetSetting(key, strconv.FormatInt(time.Now().Add(-2*time.Hour).Unix(), 10))
	if s.alertSuppressed(key, time.Hour) {
		t.Fatal("alert after cooldown must NOT be suppressed")
	}
}

func TestOpLocksBackupVsRestore(t *testing.T) {
	l := newOpLocks()
	sk := stackKey("n1", "web", "")
	c1, c2 := containerKey("n1", "app"), containerKey("n1", "db")

	// Two services of the same stack can back up concurrently (shared readers).
	if !l.acquireBackup(sk, c1) {
		t.Fatal("first service backup should acquire")
	}
	if !l.acquireBackup(sk, c2) {
		t.Fatal("second service of same stack should also back up (shared)")
	}
	// A restore of that stack is blocked while backups are in flight.
	if l.acquireRestore(sk) {
		t.Fatal("restore must be refused while a backup of the stack runs")
	}
	// Drain the backups; now the restore can take the stack exclusively.
	l.releaseBackup(sk, c1)
	l.releaseBackup(sk, c2)
	if !l.acquireRestore(sk) {
		t.Fatal("restore should acquire once all backups finished")
	}
	// While restoring, no backup of the stack may start.
	if l.canBackup(sk, c1) || l.acquireBackup(sk, c1) {
		t.Fatal("backup must be blocked during a restore of the stack")
	}
	// A second restore of the same stack is refused.
	if l.acquireRestore(sk) {
		t.Fatal("second concurrent restore of a stack must be refused")
	}
	l.releaseRestore(sk)
	if !l.acquireBackup(sk, c1) {
		t.Fatal("backup should resume after the restore released the stack")
	}
}

func TestOpLocksSameContainerDedup(t *testing.T) {
	l := newOpLocks()
	sk := stackKey("n1", "", "solo") // standalone container, keyed by name
	ck := containerKey("n1", "solo")
	if !l.acquireBackup(sk, ck) {
		t.Fatal("first backup should acquire")
	}
	if l.canBackup(sk, ck) || l.acquireBackup(sk, ck) {
		t.Fatal("a second backup of the same container must be refused")
	}
	l.releaseBackup(sk, ck)
	if !l.acquireBackup(sk, ck) {
		t.Fatal("backup should acquire again after release")
	}
}

// releaseRestoreAndDispatch must do BOTH things a queued backup depends on when
// a restore of its stack finishes: release the exclusive lock, and wake the
// dispatcher. Missing the wake is the Step 7 bug — a job blocked by canBackup()
// has no other event coming and would wait for an unrelated enqueue.
func TestReleaseRestoreAndDispatchWakesQueue(t *testing.T) {
	s := &Server{locks: newOpLocks(), queueSig: make(chan struct{}, 1)}
	sk := stackKey("n1", "myblog", "")
	ck := containerKey("n1", "web")

	// A restore holds the stack, so a backup of a container in it can't start.
	if !s.locks.acquireRestore(sk) {
		t.Fatal("acquireRestore should succeed on a free stack")
	}
	if s.locks.canBackup(sk, ck) {
		t.Fatal("canBackup must be false while a restore holds the stack")
	}

	// Drain any pending signal so we observe only the wake this call produces.
	select {
	case <-s.queueSig:
	default:
	}

	s.releaseRestoreAndDispatch(sk)

	// (a) the lock is released — the backup can now run…
	if !s.locks.canBackup(sk, ck) {
		t.Fatal("canBackup must be true after releaseRestoreAndDispatch")
	}
	// (b) …and the dispatcher was woken, so a parked dispatchLoop re-evaluates.
	select {
	case <-s.queueSig:
	default:
		t.Fatal("releaseRestoreAndDispatch must signal the queue")
	}
}

// signalQueue is coalesced (buffered cap 1): the node-restore unwind releases
// several locks in a loop, and the resulting burst of wakes must not block or
// panic — it collapses to a single pending signal.
func TestReleaseRestoreAndDispatchCoalesces(t *testing.T) {
	s := &Server{locks: newOpLocks(), queueSig: make(chan struct{}, 1)}
	keys := []string{stackKey("n1", "a", ""), stackKey("n1", "b", ""), stackKey("n1", "c", "")}
	for _, k := range keys {
		if !s.locks.acquireRestore(k) {
			t.Fatalf("acquireRestore(%s)", k)
		}
	}
	// Release all three in a loop, as noderestore.go does — must not block.
	for _, k := range keys {
		s.releaseRestoreAndDispatch(k)
	}
	// Exactly one pending wake survives (coalesced), and every lock is released.
	got := 0
	for {
		select {
		case <-s.queueSig:
			got++
			continue
		default:
		}
		break
	}
	if got != 1 {
		t.Fatalf("coalesced queue signal want 1, got %d", got)
	}
	for _, k := range keys {
		if !s.locks.canBackup(k, containerKey("n1", "x")) {
			t.Fatalf("lock %s not released", k)
		}
	}
}

func TestStackKeyDistinct(t *testing.T) {
	// Same stack name on different nodes must not cross-lock.
	if stackKey("n1", "web", "") == stackKey("n2", "web", "") {
		t.Fatal("stack keys collide across nodes")
	}
	// A backup keyed by stack and a restore keyed by the same stack must match.
	if stackKey("n1", "web", "app") != stackKey("n1", "web", "") {
		t.Fatal("stack-scoped key must ignore container name when a stack is present")
	}
	// Standalone (no stack) keys fall back to the name and stay distinct.
	if stackKey("n1", "", "a") == stackKey("n1", "", "b") {
		t.Fatal("standalone keys collide across names")
	}
}

// TestHandleRestoreReleasesLockOnValidationError: six refusals sit between the
// acquire and the goroutine that takes ownership of the lock, and opLocks has no
// expiry. A leaked lock blocks every later backup and restore of that container
// until a restart, so each refusal is checked for the lock it must hand back.
func TestHandleRestoreReleasesLockOnValidationError(t *testing.T) {
	refusals := []struct {
		name string
		body map[string]any
	}{
		{"host base directory", map[string]any{"reconstruct_host": true, "host_base_dir": "relative"}},
		{"IP remap", map[string]any{"remap_ip": true, "remap_from_ip": "not-an-ip", "remap_to_ip": "10.0.0.2"}},
		{"domain remap", map[string]any{"remap_domain": true, "remap_from_domain": "old site!", "remap_to_domain": "new.example.com"}},
	}
	for _, refusal := range refusals {
		t.Run(refusal.name, func(t *testing.T) {
			s := stepUpRestoreServer(t)
			mkRestorable(t, s, "b1", "n1", "plain-app")

			body := map[string]any{"node_id": "n1", "target_id": "cid", "volumes": true, "confirm": true}
			for k, v := range refusal.body {
				body[k] = v
			}
			rec := postRestore(s, "b1", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			lockKey := stackKey("n1", "", "plain-app")
			if !s.locks.acquireRestore(lockKey) {
				t.Fatal("the stack is still locked after a refused restore — every later backup and restore of it would be blocked until a restart")
			}
			s.locks.releaseRestore(lockKey)
		})
	}
}
