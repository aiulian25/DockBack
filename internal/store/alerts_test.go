package store

import "testing"

func TestAlertRoundTrip(t *testing.T) {
	st := tStore(t)

	// Insert two warning + one critical alert.
	a1 := &Alert{TS: 100, Kind: "destination.full", Severity: "warning", Title: "NAS almost full", Message: "95%", Dedup: "destination.full:d1"}
	a2 := &Alert{TS: 200, Kind: "verify.failed", Severity: "critical", Title: "Verify failed", Message: "db", Dedup: "verify.failed"}
	a3 := &Alert{TS: 300, Kind: "destination.full", Severity: "warning", Title: "NAS full again", Message: "97%", Dedup: "destination.full:d1"}
	for _, a := range []*Alert{a1, a2, a3} {
		if err := st.InsertAlert(a); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if a.ID == 0 {
			t.Fatal("InsertAlert must set the id")
		}
	}

	// Count unacked.
	if n, _ := st.CountUnacked(); n != 3 {
		t.Fatalf("unacked = %d, want 3", n)
	}

	// Newest-first ordering.
	all, err := st.ListAlerts(false, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].TS != 300 || all[2].TS != 100 {
		t.Fatalf("order wrong: %+v", all)
	}

	// LastAlertForDedup returns the newest for a dedup key.
	last, err := st.LastAlertForDedup("destination.full:d1")
	if err != nil {
		t.Fatal(err)
	}
	if last == nil || last.TS != 300 {
		t.Fatalf("LastAlertForDedup = %+v, want ts 300", last)
	}
	if none, _ := st.LastAlertForDedup("nope"); none != nil {
		t.Fatalf("unknown dedup must be nil, got %+v", none)
	}

	// Ack one → unacked drops; only-unacked list excludes it.
	if err := st.AckAlert(a2.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountUnacked(); n != 2 {
		t.Fatalf("after ack one: unacked = %d, want 2", n)
	}
	unacked, _ := st.ListAlerts(true, 100, 0)
	for _, a := range unacked {
		if a.ID == a2.ID {
			t.Fatal("acked alert must not appear in unacked list")
		}
	}

	// Ack all → count 0.
	if err := st.AckAllAlerts(); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountUnacked(); n != 0 {
		t.Fatalf("after ack-all: unacked = %d, want 0", n)
	}

	// Prune keeps the newest N.
	if err := st.PruneAlerts(2); err != nil {
		t.Fatal(err)
	}
	remaining, _ := st.ListAlerts(false, 100, 0)
	if len(remaining) != 2 || remaining[0].TS != 300 || remaining[1].TS != 200 {
		t.Fatalf("prune should keep newest 2 (300,200), got %+v", remaining)
	}
}
