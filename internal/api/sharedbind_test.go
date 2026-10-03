package api

import (
	"encoding/json"
	"testing"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
)

// F83 stack-run dedup: a shared host bind selected by two services is kept by
// its deterministic owner; the non-owner gets a ONE-RUN selection without it
// (never persisted), and the owner set drives owner-first enqueueing.
func TestSharedBindDedup(t *testing.T) {
	st := testStore(t)
	s := &Server{store: st, engine: &backup.Engine{Store: st, Log: func(string, string, string) {}}}

	shared := "/srv/photos/upload"
	members := []*dockercli.Container{
		{ID: "c1", Name: "server", Stack: "photos", Mounts: []dockercli.Mount{
			{Type: "bind", Source: shared, Destination: "/usr/src/app/upload", RW: true},
			{Type: "volume", Source: "vol1", Destination: "/config", RW: true},
		}},
		{ID: "c2", Name: "ml", Stack: "photos", Mounts: []dockercli.Mount{
			{Type: "bind", Source: shared, Destination: "/usr/src/app/upload", RW: true},
		}},
	}
	// Stored selections both tick the shared bind (the double-capture case).
	// Key format is the engine's mountSelKey: "mounts.<node>.<name>".
	selServer, _ := json.Marshal([]string{"/usr/src/app/upload", "/config"})
	selML, _ := json.Marshal([]string{"/usr/src/app/upload"})
	_ = st.SetSetting("mounts.n1.server", string(selServer))
	_ = st.SetSetting("mounts.n1.ml", string(selML))

	inc, owners, logs := s.sharedBindDedup("n1", members)

	// Both Selected + RW → deterministic lexicographic owner: "ml".
	if !owners["ml"] || owners["server"] {
		t.Fatalf("owner should be ml (lexicographic tie-break), got %v", owners)
	}
	// The non-owner's one-run selection drops the shared destination, keeps the rest.
	got, ok := inc["server"]
	if !ok {
		t.Fatalf("non-owner must get a one-run selection, got %v", inc)
	}
	if len(got) != 1 || got[0] != "/config" {
		t.Fatalf("shared dest must be dropped, /config kept: %v", got)
	}
	// The owner's selection is untouched (no entry).
	if _, ok := inc["ml"]; ok {
		t.Fatal("owner must keep its selection unchanged")
	}
	if len(logs) != 1 {
		t.Fatalf("expected one coverage log line, got %v", logs)
	}
	// The dedup must NOT have rewritten the stored selections.
	if v, _ := st.GetSetting("mounts.n1.server", ""); v != string(selServer) {
		t.Fatalf("stored selection was mutated: %s", v)
	}

	// A member WITHOUT a stored selection is left alone (its default handles it).
	membersNoSel := []*dockercli.Container{
		members[0],
		{ID: "c3", Name: "viewer", Stack: "photos", Mounts: []dockercli.Mount{
			{Type: "bind", Source: shared, Destination: "/view", RW: true},
		}},
	}
	_ = st.DeleteSetting("mounts.n1.ml")
	inc2, owners2, _ := s.sharedBindDedup("n1", membersNoSel)
	if !owners2["server"] {
		t.Fatalf("selected mounter must own over unselected: %v", owners2)
	}
	if _, ok := inc2["viewer"]; ok {
		t.Fatal("a member without a stored selection must not get a forced selection")
	}

	// No shared binds at all → nothing to do.
	solo := []*dockercli.Container{members[0]}
	if inc3, owners3, logs3 := s.sharedBindDedup("n1", solo); inc3 != nil || owners3 != nil || logs3 != nil {
		t.Fatal("single mounter must produce no dedup plan")
	}
}
