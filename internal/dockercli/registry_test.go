package dockercli

import (
	"errors"
	"testing"
	"time"
)

func TestBackoffGrowsAndClears(t *testing.T) {
	r := NewRegistry()
	const id = "node1"

	// No failures yet: not backed off.
	if backed, _ := r.Backoff(id); backed {
		t.Fatal("fresh node should not be in back-off")
	}

	// First failure: backed off, and the last error is reported.
	r.RecordHealth(id, errors.New("dial timeout"))
	backed, lastErr := r.Backoff(id)
	if !backed {
		t.Fatal("node should be in back-off after a failure")
	}
	if lastErr != "dial timeout" {
		t.Fatalf("lastErr = %q, want %q", lastErr, "dial timeout")
	}

	// Cooldown grows with consecutive failures (exponential).
	r.mu.Lock()
	d1 := time.Until(r.health[id].until)
	r.mu.Unlock()
	r.RecordHealth(id, errors.New("dial timeout"))
	r.mu.Lock()
	d2 := time.Until(r.health[id].until)
	fails := r.health[id].fails
	r.mu.Unlock()
	if d2 <= d1 {
		t.Errorf("cooldown should grow: d1=%v d2=%v", d1, d2)
	}
	if fails != 2 {
		t.Errorf("fails = %d, want 2", fails)
	}

	// Success clears back-off entirely.
	r.RecordHealth(id, nil)
	if backed, _ := r.Backoff(id); backed {
		t.Fatal("success should clear back-off")
	}
	r.mu.Lock()
	_, present := r.health[id]
	r.mu.Unlock()
	if present {
		t.Error("health entry should be deleted after success")
	}
}

func TestBackoffCappedAtMax(t *testing.T) {
	r := NewRegistry()
	const id = "node2"
	for i := 0; i < 20; i++ {
		r.RecordHealth(id, errors.New("down"))
	}
	r.mu.Lock()
	d := time.Until(r.health[id].until)
	r.mu.Unlock()
	if d > backoffMax+time.Second {
		t.Errorf("cooldown %v exceeds cap %v", d, backoffMax)
	}
}

func TestSetAndRemoveResetBackoff(t *testing.T) {
	r := NewRegistry()
	const id = "node3"
	r.RecordHealth(id, errors.New("down"))

	// Reconfiguring the node clears back-off so it is probed immediately.
	r.Set(NodeConn{ID: id, Transport: TransportTCPProxy, Address: "tcp://x:2375"})
	if backed, _ := r.Backoff(id); backed {
		t.Error("Set should reset back-off")
	}

	r.RecordHealth(id, errors.New("down again"))
	r.Remove(id)
	if backed, _ := r.Backoff(id); backed {
		t.Error("Remove should clear back-off")
	}
}

func TestExpiredBackoffAllowsReprobe(t *testing.T) {
	r := NewRegistry()
	const id = "node4"
	r.RecordHealth(id, errors.New("down"))
	// Force the cooldown into the past: the next refresh cycle should re-probe.
	r.mu.Lock()
	r.health[id].until = time.Now().Add(-time.Second)
	r.mu.Unlock()
	if backed, _ := r.Backoff(id); backed {
		t.Error("an expired cooldown should permit a re-probe")
	}
}
