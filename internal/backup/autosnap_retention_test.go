package backup

import (
	"reflect"
	"testing"

	"dockback/internal/store"
)

// ids returns the ordered id list of a backup slice, for compact assertions.
func idList(bs []*store.Backup) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.ID
	}
	return out
}

// mkAuto / mkNorm build newest-first fixtures. CreatedAt descending is caller's job.
func mkRow(id string, created int64, label string, pinned bool) *store.Backup {
	return &store.Backup{ID: id, Status: "success", CreatedAt: created, Label: label, Pinned: pinned}
}

func TestAutoSnapPartition(t *testing.T) {
	// 5 auto + 5 scheduled, newest-first interleaved. keep=2 auto.
	rows := []*store.Backup{
		mkRow("a5", 100, "auto: pre-change", false),
		mkRow("s5", 99, "", false),
		mkRow("a4", 98, "auto: pre-change", false),
		mkRow("s4", 97, "", false),
		mkRow("a3", 96, "auto: pre-change", false),
		mkRow("s3", 95, "", false),
		mkRow("a2", 94, "auto: pre-change", false),
		mkRow("s2", 93, "", false),
		mkRow("a1", 92, "auto: pre-change", false),
		mkRow("s1", 91, "", false),
	}
	cfg := RetentionConfig{Generations: 10, AutoKeep: 2} // keep all scheduled via generations

	keep, prune := SelectForRetention(rows, cfg)
	// Kept: 5 scheduled + 2 newest auto (a5,a4). Pruned: a3,a2,a1.
	if len(keep) != 7 {
		t.Fatalf("keep=%d want 7: %v", len(keep), idList(keep))
	}
	wantPrune := map[string]bool{"a3": true, "a2": true, "a1": true}
	if len(prune) != 3 {
		t.Fatalf("prune=%d want 3: %v", len(prune), idList(prune))
	}
	for _, b := range prune {
		if !wantPrune[b.ID] {
			t.Errorf("unexpected prune %s (must be an OLD auto snapshot)", b.ID)
		}
	}
	// No scheduled backup may be pruned by the auto budget.
	for _, b := range prune {
		if b.Label == "" {
			t.Errorf("scheduled backup %s must never be pruned by the auto budget", b.ID)
		}
	}
}

func TestAutoSnapPinnedAlwaysKept(t *testing.T) {
	rows := []*store.Backup{
		mkRow("a3", 100, "auto: pre-change", false),
		mkRow("a2", 99, "auto: pre-change", true), // pinned — kept, off-quota
		mkRow("a1", 98, "auto: pre-change", false),
	}
	keep, prune := SelectForRetention(rows, RetentionConfig{AutoKeep: 1})
	// keep newest 1 non-pinned (a3) + pinned a2. Prune a1.
	if !reflect.DeepEqual(idList(prune), []string{"a1"}) {
		t.Fatalf("prune=%v want [a1]", idList(prune))
	}
	keptIDs := map[string]bool{}
	for _, b := range keep {
		keptIDs[b.ID] = true
	}
	if !keptIDs["a2"] || !keptIDs["a3"] {
		t.Fatalf("pinned a2 and newest a3 must be kept: %v", idList(keep))
	}
}

func TestAutoSnapKeepZeroIsRegression(t *testing.T) {
	// AutoKeep==0 must reproduce the pre-change selection EXACTLY — i.e. selectGFS
	// over the whole list, auto snapshots treated like any other backup.
	rows := []*store.Backup{
		mkRow("a3", 100, "auto: pre-change", false),
		mkRow("s2", 99, "", false),
		mkRow("a2", 98, "auto: pre-change", false),
		mkRow("s1", 97, "", false),
		mkRow("a1", 96, "auto: pre-change", false),
	}
	cfg := RetentionConfig{Generations: 2, AutoKeep: 0}

	gotKeep, gotPrune := SelectForRetention(rows, cfg)
	wantKeep, wantPrune := selectGFS(rows, cfg) // the pre-change function, unchanged
	if !reflect.DeepEqual(idList(gotKeep), idList(wantKeep)) || !reflect.DeepEqual(idList(gotPrune), idList(wantPrune)) {
		t.Fatalf("AutoKeep=0 must equal pre-change output.\n got keep=%v prune=%v\nwant keep=%v prune=%v",
			idList(gotKeep), idList(gotPrune), idList(wantKeep), idList(wantPrune))
	}
	// Sanity: generations=2 keeps a3,s2 (newest 2) → prunes a2,s1,a1.
	if len(gotKeep) != 2 {
		t.Fatalf("generations=2 should keep 2, got %v", idList(gotKeep))
	}
}
