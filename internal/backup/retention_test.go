package backup

import (
	"testing"
	"time"

	"dockback/internal/store"
)

// mkBackups builds successful backups at the given day-offsets from a fixed
// reference, newest first (as ListBackups returns them).
func mkBackups(daysAgo ...int) []*store.Backup {
	ref := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	var out []*store.Backup
	for i, d := range daysAgo {
		out = append(out, &store.Backup{
			ID:        time.Duration(i).String() + "-" + time.Duration(d).String(),
			Status:    "success",
			SizeBytes: 100,
			CreatedAt: ref.AddDate(0, 0, -d).Unix(),
		})
	}
	return out
}

func ids(bs []*store.Backup) map[string]bool {
	m := map[string]bool{}
	for _, b := range bs {
		m[b.ID] = true
	}
	return m
}

func TestSelectForRetentionInactiveKeepsAll(t *testing.T) {
	bs := mkBackups(0, 1, 2, 3)
	keep, prune := SelectForRetention(bs, RetentionConfig{})
	if len(keep) != 4 || len(prune) != 0 {
		t.Fatalf("inactive config should keep all: keep=%d prune=%d", len(keep), len(prune))
	}
}

func TestSelectForRetentionGenerations(t *testing.T) {
	bs := mkBackups(0, 1, 2, 3, 4) // 5 backups on distinct days
	keep, prune := SelectForRetention(bs, RetentionConfig{Generations: 2})
	if len(keep) != 2 || len(prune) != 3 {
		t.Fatalf("generations=2: keep=%d prune=%d", len(keep), len(prune))
	}
	k := ids(keep)
	if !k[bs[0].ID] || !k[bs[1].ID] {
		t.Fatal("should keep the two newest")
	}
}

// TestSelectForRetentionPinnedNeverPruned locks in F2: a pinned backup is always
// kept even when it falls outside the policy (here it exceeds Generations=1).
func TestSelectForRetentionPinnedNeverPruned(t *testing.T) {
	bs := mkBackups(0, 1, 2, 3) // 4 backups on distinct days
	bs[2].Pinned = true         // an old one, well outside Generations=1
	keep, prune := SelectForRetention(bs, RetentionConfig{Generations: 1})
	if !ids(keep)[bs[2].ID] {
		t.Fatalf("pinned backup must be kept: keep=%v", ids(keep))
	}
	for _, p := range prune {
		if p.ID == bs[2].ID {
			t.Fatal("pinned backup must never appear in the prune set")
		}
	}
	// Sanity: without the pin it WOULD be pruned (Generations=1 keeps only newest).
	bs[2].Pinned = false
	_, prune2 := SelectForRetention(bs, RetentionConfig{Generations: 1})
	found := false
	for _, p := range prune2 {
		if p.ID == bs[2].ID {
			found = true
		}
	}
	if !found {
		t.Fatal("control: unpinned old backup should be pruned at Generations=1")
	}
}

func TestSelectForRetentionGFSDaily(t *testing.T) {
	// Two backups today, plus older days. Daily=2 keeps newest-of-day for the 2
	// most recent days; the same-day older one is pruned.
	ref := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	mk := func(id string, t time.Time) *store.Backup {
		return &store.Backup{ID: id, Status: "success", SizeBytes: 1, CreatedAt: t.Unix()}
	}
	bs := []*store.Backup{
		mk("today-late", ref),                    // day 0 (newest)
		mk("today-early", ref.Add(-2*time.Hour)), // day 0 (older same day)
		mk("yesterday", ref.AddDate(0, 0, -1)),   // day 1
		mk("old", ref.AddDate(0, 0, -10)),        // day 10
	}
	keep, prune := SelectForRetention(bs, RetentionConfig{Daily: 2})
	k := ids(keep)
	if !k["today-late"] || !k["yesterday"] {
		t.Fatalf("daily=2 should keep newest of the 2 most recent days: %v", k)
	}
	if k["today-early"] || k["old"] {
		t.Fatalf("should prune same-day-older and out-of-window: keep=%v", k)
	}
	if len(prune) != 2 {
		t.Fatalf("expected 2 pruned, got %d", len(prune))
	}
}

func TestSelectForRetentionGFSYearly(t *testing.T) {
	// Backups spanning 3 calendar years, newest-first. Yearly=2 keeps the newest
	// backup of each of the 2 most recent years; the same-year-older copy and the
	// third-year backup are pruned (F1).
	ref := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	mk := func(id string, t time.Time) *store.Backup {
		return &store.Backup{ID: id, Status: "success", SizeBytes: 1, CreatedAt: t.Unix()}
	}
	bs := []*store.Backup{
		mk("y2026-new", ref),                   // year 2026 (newest)
		mk("y2026-old", ref.AddDate(0, 0, -5)), // year 2026 (older same year)
		mk("y2025", ref.AddDate(-1, 0, 0)),     // year 2025
		mk("y2024", ref.AddDate(-2, 0, 0)),     // year 2024 (out of the 2-year window)
	}
	keep, prune := SelectForRetention(bs, RetentionConfig{Yearly: 2})
	k := ids(keep)
	if !k["y2026-new"] || !k["y2025"] {
		t.Fatalf("yearly=2 should keep newest of the 2 most recent years: %v", k)
	}
	if k["y2026-old"] || k["y2024"] {
		t.Fatalf("should prune same-year-older and out-of-window year: keep=%v", k)
	}
	if len(prune) != 2 {
		t.Fatalf("expected 2 pruned, got %d", len(prune))
	}
}

func TestSelectForRetentionAlwaysKeepsNewest(t *testing.T) {
	bs := mkBackups(0, 40, 80) // newest, then very old
	// Monthly=1 alone — newest must still always be kept (safety floor).
	keep, _ := SelectForRetention(bs, RetentionConfig{Monthly: 1})
	if !ids(keep)[bs[0].ID] {
		t.Fatal("newest backup must always be kept")
	}
}
