package store

import "testing"

func TestQueuedJobRoundTrip(t *testing.T) {
	st := tStore(t)
	rows := []QueuedJobRow{
		{ID: "j2", NodeID: "n1", NodeName: "Razer", OptsJSON: `{"NodeID":"n1"}`, Priority: 0, Seq: 2, NotBefore: 100},
		{ID: "j1", NodeID: "n1", NodeName: "Razer", OptsJSON: `{"NodeID":"n1"}`, Priority: 10, Seq: 1, NotBefore: 0},
	}
	for _, r := range rows {
		if err := st.SaveQueuedJob(r); err != nil {
			t.Fatalf("save %s: %v", r.ID, err)
		}
	}

	got, err := st.ListQueuedJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	// Ordered by seq ascending.
	if got[0].ID != "j1" || got[1].ID != "j2" {
		t.Fatalf("order = %s,%s; want j1,j2 (by seq)", got[0].ID, got[1].ID)
	}
	if got[0].Priority != 10 || got[0].NodeName != "Razer" {
		t.Errorf("unexpected row: %+v", got[0])
	}

	// Delete is idempotent and removes only the named row.
	if err := st.DeleteQueuedJob("j1"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteQueuedJob("j1"); err != nil {
		t.Fatalf("second delete should be a no-op, got %v", err)
	}
	got, _ = st.ListQueuedJobs()
	if len(got) != 1 || got[0].ID != "j2" {
		t.Fatalf("after delete: %+v", got)
	}
}

func TestSaveQueuedJobUpsert(t *testing.T) {
	st := tStore(t)
	_ = st.SaveQueuedJob(QueuedJobRow{ID: "j1", NodeID: "n1", NodeName: "a", OptsJSON: "{}", Seq: 1})
	if err := st.SaveQueuedJob(QueuedJobRow{ID: "j1", NodeID: "n2", NodeName: "b", OptsJSON: "{}", Seq: 5}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.ListQueuedJobs()
	if len(got) != 1 {
		t.Fatalf("upsert should not duplicate, got %d", len(got))
	}
	if got[0].NodeID != "n2" || got[0].Seq != 5 {
		t.Fatalf("upsert did not replace: %+v", got[0])
	}
}
