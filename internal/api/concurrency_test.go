package api

import (
	"testing"

	"dockback/internal/config"
)

// TestPerNodeSemaphore locks in PLAN §4.13: each node gets its own capped
// semaphore, reused per node, so one busy node can't exceed its share.
func TestPerNodeSemaphore(t *testing.T) {
	s := &Server{perNodeCap: 2, nodeSems: map[string]*dynSem{}}

	sem := s.nodeSem("n1")
	if sem.currentLimit() != 2 {
		t.Fatalf("per-node cap = %d, want 2", sem.currentLimit())
	}
	if s.nodeSem("n1") != sem {
		t.Fatal("same node should reuse its semaphore")
	}
	if s.nodeSem("n2") == sem {
		t.Fatal("a different node must get its own semaphore")
	}

	// Fill n1's two slots; a third acquire must not succeed.
	if !sem.tryAcquire() || !sem.tryAcquire() {
		t.Fatal("first two acquisitions on a cap-2 node should succeed")
	}
	if sem.tryAcquire() {
		t.Fatal("third concurrent backup on one node should be refused (cap 2)")
	}
	if sem.hasSlot() {
		t.Fatal("hasSlot must be false when full")
	}
	// A different node is unaffected.
	if !s.nodeSem("n2").tryAcquire() {
		t.Fatal("a different node should still have free capacity")
	}
}

// TestPerNodeSemaphoreDefaultsToOne guards against a zero cap deadlocking.
func TestPerNodeSemaphoreDefaultsToOne(t *testing.T) {
	s := &Server{perNodeCap: 0, nodeSems: map[string]*dynSem{}}
	if s.nodeSem("n1").currentLimit() < 1 {
		t.Fatal("per-node cap must be at least 1")
	}
}

// TestDynSemResize is the F29 core: a running semaphore's limit changes live —
// shrinking blocks new work without preempting the in-flight slot, growing lets
// more start immediately.
func TestDynSemResize(t *testing.T) {
	sem := newDynSem(2)
	if !sem.tryAcquire() || !sem.tryAcquire() {
		t.Fatal("two slots at cap 2")
	}
	if sem.tryAcquire() {
		t.Fatal("third refused at cap 2")
	}

	// Grow to 3 → one more slot becomes available immediately, no restart.
	sem.setLimit(3)
	if !sem.tryAcquire() {
		t.Fatal("growing the cap to 3 should free a slot")
	}
	if sem.tryAcquire() {
		t.Fatal("still capped at 3")
	}

	// Shrink to 1 while 3 are in use: no preemption, but no NEW slot until it drains.
	sem.setLimit(1)
	if sem.tryAcquire() {
		t.Fatal("shrinking below in-use must not grant a slot")
	}
	sem.release()
	sem.release() // 1 still in use, limit 1
	if sem.tryAcquire() {
		t.Fatal("still at the in-use==limit boundary after two releases")
	}
	sem.release() // 0 in use, limit 1
	if !sem.tryAcquire() {
		t.Fatal("a slot should be free once usage drops below the shrunk limit")
	}
	// A blank/zero limit is clamped to 1, never a deadlock.
	sem.setLimit(0)
	if sem.currentLimit() != 1 {
		t.Fatalf("limit clamped to 1, got %d", sem.currentLimit())
	}
}

// TestSetMaxConcurrentLiveResize covers the Server-level live-apply: a global cap
// change resizes backupSem, and a per-node change resizes every existing node sem.
func TestSetMaxConcurrentLiveResize(t *testing.T) {
	s := &Server{
		cfg:        &config.Config{MaxConcurrentBackups: 3, MaxConcurrentPerNode: 2},
		backupSem:  newDynSem(3),
		perNodeCap: 2,
		nodeSems:   map[string]*dynSem{},
	}
	a := s.nodeSem("n1")
	b := s.nodeSem("n2")

	// Global: serialize to 1.
	s.setMaxConcurrent(1)
	if s.backupSem.currentLimit() != 1 {
		t.Errorf("global cap = %d, want 1", s.backupSem.currentLimit())
	}
	if !s.backupSem.tryAcquire() || s.backupSem.tryAcquire() {
		t.Error("global cap 1 should permit exactly one concurrent backup")
	}

	// Per-node: raise to 4, applies to EXISTING nodes and future ones.
	s.setMaxConcurrentPerNode(4)
	if a.currentLimit() != 4 || b.currentLimit() != 4 {
		t.Errorf("existing node caps not resized: n1=%d n2=%d, want 4", a.currentLimit(), b.currentLimit())
	}
	if s.nodeSem("n3").currentLimit() != 4 {
		t.Error("a newly-seen node should get the updated per-node cap")
	}
}
