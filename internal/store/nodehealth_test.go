package store

import (
	"testing"
	"time"
)

// TestNodeHealthTransitions (F40): a row is written only on a reachability change
// (not per refresh tick), NodeHealth returns newest-first, rows are per-node, and
// prune drops old rows.
func TestNodeHealthTransitions(t *testing.T) {
	st := tStore(t)
	rec := func(reachable bool, errStr string) {
		t.Helper()
		if err := st.RecordNodeHealth("n1", reachable, errStr); err != nil {
			t.Fatal(err)
		}
	}

	// First observation (unreachable) → exactly one baseline row.
	rec(false, "dial timeout")
	// Repeated identical states must NOT add rows (no churn per 30s tick).
	rec(false, "dial timeout")
	rec(false, "still down")
	if rows, _ := st.NodeHealth("n1", 0); len(rows) != 1 || rows[0].Reachable || rows[0].Error != "dial timeout" {
		t.Fatalf("after repeated-unreachable: rows=%+v, want one unreachable row", rows)
	}

	// Flip to reachable → a second row (the acceptance "exactly two rows" case).
	rec(true, "")
	rec(true, "") // still reachable → no-op
	rows, _ := st.NodeHealth("n1", 0)
	if len(rows) != 2 {
		t.Fatalf("after a flip: %d rows, want 2", len(rows))
	}
	// Newest-first, with a strictly-increasing per-node ts (same-second flips don't clash).
	if !rows[0].Reachable || rows[1].Reachable {
		t.Fatalf("order wrong: %+v (want newest reachable, then unreachable)", rows)
	}
	if rows[0].Ts <= rows[1].Ts {
		t.Fatalf("ts not strictly increasing: %d <= %d", rows[0].Ts, rows[1].Ts)
	}

	// A second flip → three rows.
	rec(false, "flap")
	if rows, _ := st.NodeHealth("n1", 0); len(rows) != 3 {
		t.Fatalf("after a second flip: %d rows, want 3", len(rows))
	}

	// Scoped per node.
	if err := st.RecordNodeHealth("n2", true, ""); err != nil {
		t.Fatal(err)
	}
	if rows, _ := st.NodeHealth("n2", 0); len(rows) != 1 {
		t.Fatalf("n2 rows = %d, want 1 (independent of n1)", len(rows))
	}
	if rows, _ := st.NodeHealth("n1", 0); len(rows) != 3 {
		t.Fatalf("n1 rows = %d, want 3 (unaffected by n2)", len(rows))
	}

	// `since` filters by timestamp.
	all, _ := st.NodeHealth("n1", 0)
	newest := all[0].Ts
	if rows, _ := st.NodeHealth("n1", newest); len(rows) != 1 || rows[0].Ts != newest {
		t.Fatalf("since=newest returned %+v, want only the newest row", rows)
	}

	// Prune drops rows older than the cutoff.
	if err := st.PruneNodeHealth(time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if rows, _ := st.NodeHealth("n1", 0); len(rows) != 0 {
		t.Fatalf("after prune: %d rows, want 0", len(rows))
	}
}
