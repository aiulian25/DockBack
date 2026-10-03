package api

import (
	"testing"

	"dockback/internal/store"
)

// TestStoreAlertDecision covers shouldStoreAlert (F46): the persistence decision
// that keeps the inbox meaningful while letting a resolved-then-recurring condition
// resurface.
func TestStoreAlertDecision(t *testing.T) {
	unacked := &store.Alert{Acked: false}
	acked := &store.Alert{Acked: true}

	cases := []struct {
		name           string
		last           *store.Alert
		cooldownActive bool
		want           bool
	}{
		// Outside cooldown: always record a fresh occurrence.
		{"fresh window, no prior", nil, false, true},
		{"fresh window, prior unacked", unacked, false, true},
		{"fresh window, prior acked", acked, false, true},
		// Inside cooldown: only resurface once the prior alert was acknowledged. With
		// no prior row (only reachable post-prune, since the first fire is always
		// outside cooldown and stores a row) there's no pending alert to resurface.
		{"cooldown, no prior", nil, true, false},
		{"cooldown, prior unacked -> suppress duplicate", unacked, true, false},
		{"cooldown, prior acked -> resurface", acked, true, true},
	}
	for _, c := range cases {
		if got := shouldStoreAlert(c.last, c.cooldownActive); got != c.want {
			t.Errorf("%s: shouldStoreAlert=%v want %v", c.name, got, c.want)
		}
	}
}

// TestStoreAlertDedupScenario models the acceptance flow: two throttled calls in a
// cooldown store exactly one unacked alert; acking then re-firing stores a fresh one.
func TestStoreAlertDedupScenario(t *testing.T) {
	// Simulate the store's "last alert for dedup" across the sequence.
	var last *store.Alert
	stored := 0
	fire := func(cooldownActive bool) {
		if shouldStoreAlert(last, cooldownActive) {
			stored++
			last = &store.Alert{Acked: false} // a new unacked row is now the latest
		}
	}

	fire(false) // first occurrence — outside cooldown
	fire(true)  // second within cooldown — suppressed (prior unacked)
	if stored != 1 {
		t.Fatalf("two calls within cooldown must store exactly one alert, got %d", stored)
	}
	last.Acked = true // operator acknowledges it
	fire(true)        // still firing within cooldown -> resurfaces
	if stored != 2 {
		t.Fatalf("after ack, a still-firing condition must store a fresh alert, got %d", stored)
	}
}
