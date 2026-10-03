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

// F69 ransomware tripwire: pure delta analysis + the retention hold.

func idx(paths ...string) VolIndex {
	v := VolIndex{}
	for _, p := range paths {
		v.Entries = append(v.Entries, FileEntry{Path: p, Size: 1, MtimeUnix: 1})
	}
	return v
}

func TestDeltaStatsAnalyze(t *testing.T) {
	prev := idx("docs/a.txt", "docs/b.txt", "img/c.jpg", "raw/noext")
	changed := []string{"docs/a.txt.lock", "img/c.jpg.lock", "docs/b.txt", "raw/other"}
	deleted := []string{"raw/noext"}

	st := AnalyzeDelta(prev, idx(), changed, deleted)
	if st.Indexed != 4 || st.Changed != 4 || st.Deleted != 1 {
		t.Fatalf("counts wrong: %+v", st)
	}
	// "lock" never appeared in prev — two changed files carry it; "txt" existed
	// before so b.txt doesn't count; extensionless paths never count.
	if st.NewExtTop != "lock" || st.NewExtCount != 2 {
		t.Fatalf("extension churn wrong: top=%q count=%d", st.NewExtTop, st.NewExtCount)
	}

	// Deterministic tie-break: two new extensions with equal counts pick the
	// lexicographically smaller.
	st2 := AnalyzeDelta(prev, idx(), []string{"a.aaa", "b.bbb"}, nil)
	if st2.NewExtTop != "aaa" || st2.NewExtCount != 1 {
		t.Fatalf("tie-break wrong: %+v", st2)
	}
}

func TestSuspectDelta(t *testing.T) {
	const minFiles, chPct, delPct = 200, 60, 40

	// 100% of 300 indexed files changed → suspect via the changed rule.
	if ok, reason := SuspectDelta(DeltaStats{Indexed: 300, Changed: 300}, minFiles, chPct, delPct); !ok || !strings.Contains(reason, "changed") {
		t.Fatalf("100%% changed must trip: ok=%v reason=%q", ok, reason)
	}
	// 5% changed → clean.
	if ok, _ := SuspectDelta(DeltaStats{Indexed: 10000, Changed: 500}, minFiles, chPct, delPct); ok {
		t.Fatal("5%% changed must not trip")
	}
	// Deleted-heavy: 45% of 1000 deleted → suspect via the deleted rule.
	if ok, reason := SuspectDelta(DeltaStats{Indexed: 1000, Deleted: 450}, minFiles, chPct, delPct); !ok || !strings.Contains(reason, "deleted") {
		t.Fatalf("45%% deleted must trip: ok=%v reason=%q", ok, reason)
	}
	// Sub-min-files never trips, no matter the percentage (90% of 100).
	if ok, _ := SuspectDelta(DeltaStats{Indexed: 100, Changed: 90, Deleted: 90}, minFiles, chPct, delPct); ok {
		t.Fatal("below min_files must never trip")
	}
	// An empty parent index (first baseline) never trips.
	if ok, _ := SuspectDelta(DeltaStats{Indexed: 0, Changed: 5000}, minFiles, chPct, delPct); ok {
		t.Fatal("empty parent index must never trip")
	}
	// Extension churn strengthens the reason text.
	stats := DeltaStats{Indexed: 300, Changed: 300, NewExtTop: "locked", NewExtCount: 290}
	if _, reason := SuspectDelta(stats, minFiles, chPct, delPct); !strings.Contains(reason, `previously-unseen extension ".locked"`) {
		t.Fatalf("extension note missing: %q", reason)
	}
	// …but a marginal new-ext share (<25%% of changed) stays out of the reason.
	stats.NewExtCount = 10
	if _, reason := SuspectDelta(stats, minFiles, chPct, delPct); strings.Contains(reason, "previously-unseen") {
		t.Fatalf("marginal extension churn must not be named: %q", reason)
	}
}

// TestTripwireHoldBlocksPruneAll: with a hold key set, PruneAll leaves that
// (node, target) untouched while other targets prune normally; clearing the
// hold resumes pruning.
func TestTripwireHoldBlocksPruneAll(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	be, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Store: st, Storage: be, Log: func(string, string, string) {}}
	_ = st.SetSetting("retention.generations", "1")

	for i := 0; i < 3; i++ {
		for _, target := range []string{"held-app", "free-app"} {
			b := &store.Backup{ID: fmt.Sprintf("%s-%d", target, i), NodeID: "n1", TargetName: target,
				Status: "success", CreatedAt: int64(1000 + i)}
			if err := st.CreateBackup(b); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The tripwire froze held-app.
	_ = st.SetSetting(RetentionHoldKey("n1", "held-app"), "97% changed in a single run|1000")

	count := func(target string) int {
		all, _ := st.ListBackups("", 100)
		n := 0
		for _, b := range all {
			if b.TargetName == target {
				n++
			}
		}
		return n
	}

	e.PruneAll(context.Background())
	if got := count("held-app"); got != 3 {
		t.Fatalf("held target must not be pruned: %d rows left, want 3", got)
	}
	if got := count("free-app"); got != 1 {
		t.Fatalf("unheld target must prune normally: %d rows left, want 1", got)
	}

	// Clearing the hold resumes pruning.
	_ = st.DeleteSetting(RetentionHoldKey("n1", "held-app"))
	e.PruneAll(context.Background())
	if got := count("held-app"); got != 1 {
		t.Fatalf("after clearing the hold pruning must resume: %d rows left, want 1", got)
	}
}
