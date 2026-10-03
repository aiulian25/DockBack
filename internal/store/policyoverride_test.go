package store

import "testing"

func TestPolicyOverrideRoundTrip(t *testing.T) {
	st := tStore(t)
	scope := NodeScope("n1")

	if _, ok := st.GetPolicyOverride(scope); ok {
		t.Fatal("no override should exist initially")
	}

	ov := PolicyOverride{
		OverrideRetention: true, Generations: 5, KeepDaily: 7, Autoprune: true,
	}
	if err := st.SetPolicyOverride(scope, ov); err != nil {
		t.Fatal(err)
	}
	got, ok := st.GetPolicyOverride(scope)
	if !ok || !got.OverrideRetention || got.Generations != 5 || got.KeepDaily != 7 || !got.Autoprune {
		t.Fatalf("round-trip mismatch: %+v ok=%v", got, ok)
	}
}

func TestPolicyOverrideEmptyDeletes(t *testing.T) {
	st := tStore(t)
	scope := NodeScope("n1")
	_ = st.SetPolicyOverride(scope, PolicyOverride{OverrideRetention: true, Generations: 3})
	// Saving an override with no active flags should remove the row (inherit all).
	if err := st.SetPolicyOverride(scope, PolicyOverride{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.GetPolicyOverride(scope); ok {
		t.Fatal("override with no active flags should be deleted")
	}
}

func TestPolicyOverrideDestinations(t *testing.T) {
	st := tStore(t)
	scope := NodeScope("n2")
	_ = st.SetPolicyOverride(scope, PolicyOverride{OverrideDestinations: true, Destinations: []string{"d1", "d2"}})
	got, ok := st.GetPolicyOverride(scope)
	if !ok || !got.OverrideDestinations || len(got.Destinations) != 2 {
		t.Fatalf("destinations override mismatch: %+v", got)
	}
	if err := st.DeletePolicyOverride(scope); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.GetPolicyOverride(scope); ok {
		t.Fatal("delete should remove the override")
	}
}
