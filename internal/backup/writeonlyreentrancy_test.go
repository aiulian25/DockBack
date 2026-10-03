package backup

import (
	"sync"
	"testing"
	"time"
)

// F209 — the offline private key can be read back by the restore that installed
// it.
//
// This is a regression test for a PERMANENT HANG, so it is written to time out
// rather than to fail an assertion. Restore installs the key and holds privGate
// for its whole duration (restore.go), then reads it back through
// restorePrivFor() — archiveKey does that on the very first archive read, and
// Verify does it again inside a pre-restore safety snapshot. When the gate and
// the value were one sync.Mutex, that second read never returned: Go mutexes are
// not reentrant, so the restore goroutine parked forever, its deferred unlocks
// and lock releases never ran, and the stack stayed restore-locked until the
// process was restarted. Every write-only restore did this — the mode could
// create backups it could never restore.

// withinDeadline runs fn and reports whether it finished. A hang is the failure
// being tested for, so it must never be allowed to hang the suite.
func withinDeadline(t *testing.T, d time.Duration, fn func()) bool {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// The exact shape of the bug: hold the gate as Restore does, then read the key
// back as archiveKey does.
func TestRestoreCanReadBackItsOwnPrivateKey(t *testing.T) {
	e := &Engine{}
	key := "an-offline-private-key"

	e.privGate.Lock()
	e.restorePriv.Store(&key)
	defer func() {
		e.restorePriv.Store(nil)
		e.privGate.Unlock()
	}()

	var got string
	if !withinDeadline(t, 3*time.Second, func() { got = e.restorePrivFor() }) {
		t.Fatal("restorePrivFor deadlocked while the restore held privGate — every write-only restore hangs")
	}
	if got != key {
		t.Errorf("the restore must read back the key it installed, got %q", got)
	}
}

// And through the real consumer, which is where the hang actually happened: a
// write-only manifest sends archiveKey straight to restorePrivFor.
func TestArchiveKeyDoesNotDeadlockInsideARestore(t *testing.T) {
	e := &Engine{}

	// No key installed: archiveKey must RETURN the "supply the key" error rather
	// than block, so a missing key is a clear refusal and not a hung stack.
	if !withinDeadline(t, 3*time.Second, func() {
		if _, err := e.archiveKey(&Manifest{WrappedKeyPub: "sealed"}); err == nil {
			t.Error("a write-only manifest with no key must error")
		}
	}) {
		t.Fatal("archiveKey hung with no key installed")
	}

	// Key installed and the gate held, exactly as during a restore.
	key := "an-offline-private-key"
	e.privGate.Lock()
	e.restorePriv.Store(&key)
	defer func() {
		e.restorePriv.Store(nil)
		e.privGate.Unlock()
	}()
	if !withinDeadline(t, 3*time.Second, func() {
		// It fails on the fingerprint (this is not a real keypair) — a returned
		// error is the pass condition here; hanging is the failure.
		_, _ = e.archiveKey(&Manifest{WrappedKeyPub: "sealed", BackupPubFP: "deadbeefdeadbeef"})
	}) {
		t.Fatal("archiveKey deadlocked on the gate its own restore holds")
	}
}

// The key is cleared on the way out, so nothing after the restore can read it.
func TestPrivateKeyIsClearedAfterTheRestore(t *testing.T) {
	e := &Engine{}
	key := "an-offline-private-key"
	func() {
		e.privGate.Lock()
		e.restorePriv.Store(&key)
		defer func() {
			e.restorePriv.Store(nil)
			e.privGate.Unlock()
		}()
		if e.restorePrivFor() == "" {
			t.Fatal("installed, so it must read back")
		}
	}()
	if got := e.restorePrivFor(); got != "" {
		t.Errorf("the key must not outlive its restore, got %q", got)
	}
}

// The gate still does its one job: only one key-bearing restore installs a key
// at a time, so two concurrent restores cannot see each other's key.
func TestPrivGateStillSerializesKeyBearingRestores(t *testing.T) {
	e := &Engine{}
	var wg sync.WaitGroup
	seen := make(chan string, 2)

	install := func(k string) {
		defer wg.Done()
		e.privGate.Lock()
		key := k
		e.restorePriv.Store(&key)
		// While the gate is held, what is installed is this restore's own key.
		seen <- e.restorePrivFor()
		e.restorePriv.Store(nil)
		e.privGate.Unlock()
	}
	wg.Add(2)
	go install("key-a")
	go install("key-b")

	if !withinDeadline(t, 5*time.Second, wg.Wait) {
		t.Fatal("two concurrent key-bearing restores deadlocked")
	}
	close(seen)
	got := map[string]bool{}
	for k := range seen {
		got[k] = true
	}
	if !got["key-a"] || !got["key-b"] {
		t.Errorf("each restore must see its OWN key, got %v", got)
	}
}
