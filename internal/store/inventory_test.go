package store

import "testing"

func TestNodeInventoryRoundTrip(t *testing.T) {
	st := tStore(t)
	payload := []byte(`{"summary":{"running":3},"containers":[],"stacks":[]}`)

	if err := st.SaveNodeInventory("n1", payload, true, "", 1000); err != nil {
		t.Fatalf("save: %v", err)
	}

	rows, err := st.ListNodeInventories()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.NodeID != "n1" || !got.Reachable || got.UpdatedAt != 1000 || string(got.Payload) != string(payload) {
		t.Fatalf("unexpected row: %+v", got)
	}
}

func TestNodeInventoryUpsertAndError(t *testing.T) {
	st := tStore(t)
	_ = st.SaveNodeInventory("n1", []byte("{}"), true, "", 1)
	// Overwrite with an offline snapshot + error.
	if err := st.SaveNodeInventory("n1", []byte(`{"x":1}`), false, "dial timeout", 2); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rows, _ := st.ListNodeInventories()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (upsert, not insert)", len(rows))
	}
	if rows[0].Reachable || rows[0].Error != "dial timeout" || rows[0].UpdatedAt != 2 {
		t.Fatalf("upsert did not replace: %+v", rows[0])
	}
}

func TestNodeInventoryDelete(t *testing.T) {
	st := tStore(t)
	_ = st.SaveNodeInventory("n1", []byte("{}"), true, "", 1)
	if err := st.DeleteNodeInventory("n1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rows, _ := st.ListNodeInventories()
	if len(rows) != 0 {
		t.Fatalf("rows = %d, want 0 after delete", len(rows))
	}
}
