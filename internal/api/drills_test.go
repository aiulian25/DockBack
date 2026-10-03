package api

import (
	"strconv"
	"testing"
	"time"

	"dockback/internal/store"
)

// TestDrillCandidatesScope covers the F12 candidate selection: "newest" keeps one
// backup per container, while "newest_per_week" widens to the newest of each ISO
// week — proving older generations still restore.
func TestDrillCandidatesScope(t *testing.T) {
	const day = int64(86400)
	base := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC).Unix() // a Monday

	var all []*store.Backup
	// Container "web": 4 weekly backups (weeks apart) + a second, older backup in
	// the newest week (so "newest" and "newest_per_week" both pick the newest one
	// for that week, but per-week yields 4 groups total).
	for i := int64(0); i < 4; i++ {
		all = append(all, &store.Backup{ID: "web-w" + strconv.FormatInt(i, 10), NodeID: "n1", TargetName: "web", Status: "success", CreatedAt: base + i*7*day})
	}
	all = append(all, &store.Backup{ID: "web-w3-old", NodeID: "n1", TargetName: "web", Status: "success", CreatedAt: base + 3*7*day - 3600})
	// A failed run must never be a candidate.
	all = append(all, &store.Backup{ID: "web-failed", NodeID: "n1", TargetName: "web", Status: "failed", CreatedAt: base + 100*day})
	// A second container with a single backup.
	all = append(all, &store.Backup{ID: "db-1", NodeID: "n1", TargetName: "db", Status: "success", CreatedAt: base})

	newest := drillCandidates(all, "newest")
	// One per container: web + db.
	if len(newest) != 2 {
		t.Fatalf("newest scope: got %d candidates, want 2", len(newest))
	}
	for _, b := range newest {
		if b.Status != "success" {
			t.Fatalf("newest scope included a non-success backup: %s", b.ID)
		}
		if b.TargetName == "web" && b.ID != "web-w3" {
			t.Fatalf("newest scope should pick the newest web backup, got %s", b.ID)
		}
	}

	perWeek := drillCandidates(all, "newest_per_week")
	// web contributes 4 weekly groups (the older duplicate in week 3 is dropped),
	// db contributes 1 → 5 total.
	if len(perWeek) != 5 {
		t.Fatalf("newest_per_week scope: got %d candidates, want 5", len(perWeek))
	}
	if len(perWeek) <= len(newest) {
		t.Fatalf("newest_per_week (%d) must yield MORE candidates than newest (%d)", len(perWeek), len(newest))
	}
	// The older duplicate in week 3 must be superseded by the newer one.
	for _, b := range perWeek {
		if b.ID == "web-w3-old" {
			t.Fatal("newest_per_week must keep only the newest backup within a week")
		}
		if b.Status != "success" {
			t.Fatalf("newest_per_week included a non-success backup: %s", b.ID)
		}
	}

	// An unknown scope falls back to newest semantics.
	if got := drillCandidates(all, "bogus"); len(got) != len(newest) {
		t.Fatalf("unknown scope should behave like newest: got %d, want %d", len(got), len(newest))
	}
}
