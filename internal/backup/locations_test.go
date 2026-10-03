package backup

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"dockback/internal/storage"
	"dockback/internal/store"
)

// TestMergeLocations locks the merge used by MirrorExisting: a re-mirrored
// destination replaces its old (failed) entry, the local copy and destinations
// that weren't re-mirrored are preserved, and a brand-new destination is added.
func TestMergeLocations(t *testing.T) {
	existing := []Location{
		{Kind: "local", Name: "local", Type: "local"},
		{Kind: "dest", DestID: "A", Name: "nas01", Type: "smb", Status: "failed", Detail: "timeout"},
		{Kind: "dest", DestID: "B", Name: "S3", Type: "s3"},
	}
	updated := []Location{
		{Kind: "dest", DestID: "A", Name: "nas01", Type: "smb"},  // succeeded this time
		{Kind: "dest", DestID: "C", Name: "New", Type: "webdav"}, // destination added later
	}

	got := mergeLocations(existing, updated)

	byID := map[string]Location{}
	local := 0
	for _, l := range got {
		if l.Kind == "local" {
			local++
			continue
		}
		byID[l.DestID] = l
	}
	if local != 1 {
		t.Errorf("local copies = %d, want exactly 1 preserved", local)
	}
	if byID["A"].Status != "" {
		t.Errorf("dest A should now be OK (failed entry replaced), got status %q", byID["A"].Status)
	}
	if l, ok := byID["B"]; !ok || l.Status != "" {
		t.Error("dest B was not re-mirrored and must be preserved unchanged")
	}
	if _, ok := byID["C"]; !ok {
		t.Error("newly-added dest C should be appended")
	}
	if len(got) != 4 {
		t.Errorf("total locations = %d, want 4 (local + A + B + C)", len(got))
	}
}

// TestEnforceRetentionKeepsChainAncestors locks the post-backup sweep to the
// same chain rule as PruneAll (F61): a full or delta that a kept backup depends
// on survives GFS, while an old full nothing depends on is still pruned.
func TestEnforceRetentionKeepsChainAncestors(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	be, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	e := &Engine{Store: st, Storage: be, Log: func(_, level, msg string) { logged = append(logged, level+" "+msg) }}

	// Oldest first: a stale full nobody depends on, then the baseline b0 with
	// deltas b1..b3 chained onto it.
	chain := []struct{ id, parent string }{
		{"stale", ""}, {"b0", ""}, {"b1", "b0"}, {"b2", "b1"}, {"b3", "b2"},
	}
	for i, link := range chain {
		b := &store.Backup{ID: link.id, NodeID: "n1", TargetName: "app", Status: "success", CreatedAt: int64(1000 + i)}
		if err := st.CreateBackup(b); err != nil {
			t.Fatal(err)
		}
		if link.parent == "" {
			continue
		}
		if err := st.UpdateBackupManifest(link.id, fmt.Sprintf(`{"parent":%q,"incremental":true}`, link.parent)); err != nil {
			t.Fatal(err)
		}
	}

	// Keep 2 → GFS keeps b3+b2 and would prune b1, b0 and stale.
	e.enforceRetention(context.Background(), "n1", "app", RetentionConfig{Generations: 2})

	left := map[string]bool{}
	all, err := st.ListBackups("n1", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range all {
		left[b.ID] = true
	}
	for _, id := range []string{"b0", "b1", "b2", "b3"} {
		if !left[id] {
			t.Errorf("%s was pruned but a kept delta depends on it", id)
		}
	}
	if left["stale"] {
		t.Error("the stale full has no dependents and must still be pruned")
	}
	if !strings.Contains(strings.Join(logged, "\n"), "a newer incremental backup depends on it") {
		t.Errorf("chain-protection log line missing:\n%s", strings.Join(logged, "\n"))
	}
}
