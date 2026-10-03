package store

import "testing"

func TestNodeEventsIncrAndRollover(t *testing.T) {
	st := tStore(t)

	// No events yet.
	total, today, err := st.GetNodeEvents("n1", "2026-06-30")
	if err != nil || total != 0 || today != 0 {
		t.Fatalf("empty = %d/%d err=%v, want 0/0", total, today, err)
	}

	// Three events on day 1.
	for i := 0; i < 3; i++ {
		if err := st.IncrNodeEvents("n1", "2026-06-30"); err != nil {
			t.Fatal(err)
		}
	}
	total, today, _ = st.GetNodeEvents("n1", "2026-06-30")
	if total != 3 || today != 3 {
		t.Fatalf("day1 = %d/%d, want 3/3", total, today)
	}

	// New day: total keeps climbing, today resets to 1.
	if err := st.IncrNodeEvents("n1", "2026-07-01"); err != nil {
		t.Fatal(err)
	}
	total, today, _ = st.GetNodeEvents("n1", "2026-07-01")
	if total != 4 || today != 1 {
		t.Fatalf("day2 = %d/%d, want 4/1", total, today)
	}

	// Reading for a different day than stored shows 0 today, total unchanged.
	total, today, _ = st.GetNodeEvents("n1", "2026-07-02")
	if total != 4 || today != 0 {
		t.Fatalf("day3 read = %d/%d, want 4/0", total, today)
	}

	// Counters are per node.
	if total, today, _ = st.GetNodeEvents("n2", "2026-07-01"); total != 0 || today != 0 {
		t.Fatalf("other node = %d/%d, want 0/0", total, today)
	}
}

func TestNodeEventsDelete(t *testing.T) {
	st := tStore(t)
	_ = st.IncrNodeEvents("n1", "2026-06-30")
	if err := st.DeleteNodeEvents("n1"); err != nil {
		t.Fatal(err)
	}
	total, _, _ := st.GetNodeEvents("n1", "2026-06-30")
	if total != 0 {
		t.Fatalf("after delete total=%d, want 0", total)
	}
}
