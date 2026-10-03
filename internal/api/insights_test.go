package api

import (
	"testing"

	"dockback/internal/store"
)

const gib = int64(1) << 30

// TestLinearGrowthPerMonth covers the size-trend fit (F11): a steady climb reads
// as a positive per-month figure, a flat series is stable, and too-few/too-short
// series read 0.
func TestLinearGrowthPerMonth(t *testing.T) {
	const day = int64(86400)

	// 8 daily points growing 1 GiB/day ⇒ ~30 GiB/month.
	var up []sizePoint
	for i := int64(0); i < 8; i++ {
		up = append(up, sizePoint{Ts: i * day, Bytes: i * gib})
	}
	perMonth := float64(linearGrowthPerMonth(up)) / float64(gib)
	if perMonth < 29 || perMonth > 31 {
		t.Errorf("growth/month = %.1f GiB, want ~30", perMonth)
	}

	// Flat sizes ⇒ stable (0).
	flat := []sizePoint{{Ts: 0, Bytes: 5 * gib}, {Ts: 3 * day, Bytes: 5 * gib}, {Ts: 6 * day, Bytes: 5 * gib}}
	if g := linearGrowthPerMonth(flat); g != 0 {
		t.Errorf("flat series growth = %d, want 0", g)
	}

	// A single point can't trend.
	if g := linearGrowthPerMonth([]sizePoint{{Ts: 0, Bytes: gib}}); g != 0 {
		t.Errorf("single point growth = %d, want 0", g)
	}

	// Two points inside the minimum span aren't trusted.
	if g := linearGrowthPerMonth([]sizePoint{{Ts: 0, Bytes: gib}, {Ts: 3600, Bytes: 4 * gib}}); g != 0 {
		t.Errorf("sub-span growth = %d, want 0", g)
	}
}

// TestTopGrowthFromBackups covers the Insights assembly (F11): only successful
// backups count, they're grouped per (node, target), ranked fastest-growing
// first, capped to the limit, and the newest ~12 points are kept oldest→newest.
func TestTopGrowthFromBackups(t *testing.T) {
	const day = int64(86400)
	id := func(s string) string { return "node-" + s } // name resolver

	var list []*store.Backup
	add := func(node, target string, ts, bytes int64, status string) {
		list = append(list, &store.Backup{NodeID: node, TargetName: target, Status: status, CreatedAt: ts, SizeBytes: bytes})
	}
	// "media" grows fast (1 GiB/day over 6 days). List order is newest-first, like
	// ListBackups, to prove the function sorts internally.
	for i := int64(6); i >= 0; i-- {
		add("n1", "media", i*day, i*gib, "success")
	}
	// "config" is small and flat.
	add("n1", "config", 5*day, 10*1024*1024, "success")
	add("n1", "config", 0, 10*1024*1024, "success")
	// A failed run must be ignored entirely.
	add("n1", "broken", 2*day, 99*gib, "failed")

	out := topGrowthFromBackups(list, id, 8)

	if len(out) != 2 {
		t.Fatalf("expected 2 containers (failed-only excluded), got %d", len(out))
	}
	// Fastest-growing (media) ranks first.
	if out[0].Container != "media" {
		t.Fatalf("expected media first (fastest-growing), got %q", out[0].Container)
	}
	if out[0].Node != "node-n1" {
		t.Errorf("node name not resolved: %q", out[0].Node)
	}
	if out[0].GrowthPerMonth <= 0 {
		t.Errorf("media growth should be positive, got %d", out[0].GrowthPerMonth)
	}
	if out[0].LatestBytes != 6*gib {
		t.Errorf("media latest = %d, want %d", out[0].LatestBytes, 6*gib)
	}
	// Points are oldest→newest.
	if out[0].Points[0].Ts != 0 || out[0].Points[len(out[0].Points)-1].Ts != 6*day {
		t.Errorf("media points not sorted oldest→newest: %+v", out[0].Points)
	}
	if out[1].Container != "config" || out[1].GrowthPerMonth != 0 {
		t.Errorf("config should be second and stable, got %q growth=%d", out[1].Container, out[1].GrowthPerMonth)
	}

	// The limit caps the result set.
	if capped := topGrowthFromBackups(list, id, 1); len(capped) != 1 || capped[0].Container != "media" {
		t.Fatalf("limit=1 should keep only media, got %+v", capped)
	}
}

// TestTopGrowthKeepsLast12 confirms only the most recent 12 points are retained
// for a very long history (bounded payload).
func TestTopGrowthKeepsLast12(t *testing.T) {
	const day = int64(86400)
	var list []*store.Backup
	for i := int64(0); i < 30; i++ {
		list = append(list, &store.Backup{NodeID: "n1", TargetName: "web", Status: "success", CreatedAt: i * day, SizeBytes: i * gib})
	}
	out := topGrowthFromBackups(list, func(s string) string { return s }, 8)
	if len(out) != 1 {
		t.Fatalf("expected 1 container, got %d", len(out))
	}
	if len(out[0].Points) != 12 {
		t.Fatalf("expected the newest 12 points, got %d", len(out[0].Points))
	}
	if out[0].Points[0].Ts != 18*day || out[0].Points[11].Ts != 29*day {
		t.Errorf("wrong 12-point window: first=%d last=%d", out[0].Points[0].Ts, out[0].Points[11].Ts)
	}
}
