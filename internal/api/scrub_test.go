package api

import (
	"testing"

	"dockback/internal/store"
)

func TestSelectScrubBatch(t *testing.T) {
	all := []*store.Backup{
		{ID: "fresh", Status: "success", LastVerifiedAt: 1000}, // verified recently → not due
		{ID: "stale", Status: "success", LastVerifiedAt: 100},  // due (oldest)
		{ID: "never", Status: "success", LastVerifiedAt: 0},    // never verified → most overdue
		{ID: "failed", Status: "failed", LastVerifiedAt: 0},    // not a success → skip
		{ID: "mid", Status: "success", LastVerifiedAt: 200},    // due
	}
	cutoff := int64(500)

	got := selectScrubBatch(all, cutoff, 10)
	if len(got) != 3 {
		t.Fatalf("due count = %d, want 3 (stale, never, mid)", len(got))
	}
	// Legacy rows (no locations) scrub their local copy on the global clock, so
	// order is oldest-verified first: never(0) < stale(100) < mid(200).
	if got[0].Backup.ID != "never" || got[1].Backup.ID != "stale" || got[2].Backup.ID != "mid" {
		t.Fatalf("order = %s,%s,%s; want never,stale,mid", got[0].Backup.ID, got[1].Backup.ID, got[2].Backup.ID)
	}
	// "fresh" (verified after cutoff) and "failed" are excluded.
	for _, t2 := range got {
		if t2.Backup.ID == "fresh" || t2.Backup.ID == "failed" {
			t.Fatalf("unexpected %s in batch", t2.Backup.ID)
		}
		if t2.Source != "local" {
			t.Fatalf("legacy rows scrub local, got source %q", t2.Source)
		}
	}
}

func TestSelectScrubBatchCap(t *testing.T) {
	all := []*store.Backup{
		{ID: "a", Status: "success", LastVerifiedAt: 1},
		{ID: "b", Status: "success", LastVerifiedAt: 2},
		{ID: "c", Status: "success", LastVerifiedAt: 3},
	}
	got := selectScrubBatch(all, 100, 2)
	if len(got) != 2 || got[0].Backup.ID != "a" || got[1].Backup.ID != "b" {
		t.Fatalf("cap not honored / wrong order: %+v", got)
	}
}
