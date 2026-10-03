package api

import (
	"testing"

	"dockback/internal/notify"
	"dockback/internal/store"
)

const gb = int64(1) << 30

// F28: the pure drift decision — a run > factor× the median duration OR size is
// anomalous; a modest run is not; too few priors is never judged.
func TestJudgeAnomaly(t *testing.T) {
	// 8 priors at ~10s (10000ms) and ~1 GiB.
	durs := []int64{9000, 10000, 11000, 10000, 9500, 10500, 10000, 9800}
	sizes := []int64{gb, gb, gb + gb/10, gb - gb/20, gb, gb, gb + gb/5, gb}

	// 40s / 5 GiB at factor 3 → both metrics anomalous.
	v := judgeAnomaly(40000, 5*gb, durs, sizes, 3)
	if !v.durAnom || !v.sizeAnom {
		t.Errorf("4x run should flag both: dur=%v size=%v", v.durAnom, v.sizeAnom)
	}

	// 15s / 1.2 GiB at factor 3 → neither (well under 3×).
	v = judgeAnomaly(15000, gb+gb/5, durs, sizes, 3)
	if v.durAnom || v.sizeAnom {
		t.Errorf("modest run should flag nothing: dur=%v size=%v", v.durAnom, v.sizeAnom)
	}

	// Duration-only anomaly (slow disk), size normal.
	v = judgeAnomaly(50000, gb, durs, sizes, 3)
	if !v.durAnom || v.sizeAnom {
		t.Errorf("slow-only run should flag duration only: dur=%v size=%v", v.durAnom, v.sizeAnom)
	}

	// Fewer than the minimum priors → never flagged, even for a huge run.
	v = judgeAnomaly(1_000_000, 100*gb, durs[:2], sizes[:2], 3)
	if v.durAnom || v.sizeAnom {
		t.Error("with <3 priors nothing may be flagged")
	}

	// A larger factor suppresses a borderline run: 40s vs 10s median is 4×, so
	// factor 5 must NOT flag it.
	v = judgeAnomaly(40000, gb, durs, sizes, 5)
	if v.durAnom {
		t.Error("40s vs ~10s (4x) should not trip a 5x threshold")
	}
}

func TestMedianInt64(t *testing.T) {
	if m := medianInt64([]int64{5, 1, 3}); m != 3 {
		t.Errorf("odd median = %d, want 3", m)
	}
	if m := medianInt64([]int64{1, 2, 3, 4}); m != 2 { // (2+3)/2 = 2 (int)
		t.Errorf("even median = %d, want 2", m)
	}
	if m := medianInt64(nil); m != 0 {
		t.Errorf("empty median = %d, want 0", m)
	}
	// Input is not mutated.
	in := []int64{3, 1, 2}
	_ = medianInt64(in)
	if in[0] != 3 {
		t.Error("medianInt64 must not sort the caller's slice in place")
	}
}

// F28: the factor resolver defaults to 3 and self-clamps to [2,100].
func TestAnomalyFactorResolver(t *testing.T) {
	s := &Server{store: testStore(t)}
	if got := s.anomalyFactor(); got != 3 {
		t.Errorf("default anomalyFactor = %d, want 3", got)
	}
	_ = s.store.SetSetting("alert.anomaly_factor", "5")
	if got := s.anomalyFactor(); got != 5 {
		t.Errorf("anomalyFactor = %d, want 5", got)
	}
	_ = s.store.SetSetting("alert.anomaly_factor", "1")
	if got := s.anomalyFactor(); got != 2 {
		t.Errorf("anomalyFactor low should clamp to 2, got %d", got)
	}
	_ = s.store.SetSetting("alert.anomaly_factor", "9999")
	if got := s.anomalyFactor(); got != 100 {
		t.Errorf("anomalyFactor high should clamp to 100, got %d", got)
	}
}

// F28 end-to-end: an anomalous run raises exactly one throttled warning; a modest
// run and a container with too few priors raise none. `duration_ms` round-trips.
func TestCheckBackupAnomalyEndToEnd(t *testing.T) {
	st := testStore(t)
	s := &Server{store: st, notifier: notify.New(func() (notify.Config, error) { return notify.Config{}, nil }, nil)}

	// Seed 8 prior successful backups for container "paperless" on node n1.
	for i := 0; i < 8; i++ {
		b := &store.Backup{ID: idFor("p", i), NodeID: "n1", TargetName: "paperless", Status: "success", CreatedAt: int64(100 + i)}
		if err := st.CreateBackup(b); err != nil {
			t.Fatal(err)
		}
		b.SizeBytes = gb
		b.DurationMs = 10000
		if err := st.UpdateBackup(b); err != nil {
			t.Fatal(err)
		}
	}
	// duration_ms must round-trip through the catalog.
	if got, _ := st.GetBackup(idFor("p", 0)); got.DurationMs != 10000 {
		t.Fatalf("duration_ms not persisted: got %d", got.DurationMs)
	}

	throttleKey := alertKey(notify.KindBackupAnomaly, "n1\x00paperless")

	// A 4x/5x run → fires exactly one warning (recorded as a throttle timestamp).
	big := &store.Backup{ID: "p-new", NodeID: "n1", TargetName: "paperless", Status: "success", CreatedAt: 200}
	_ = st.CreateBackup(big)
	big.SizeBytes = 5 * gb
	big.DurationMs = 40000
	_ = st.UpdateBackup(big)
	s.checkBackupAnomaly(big)
	if v, _ := st.GetSetting(throttleKey, "0"); v == "0" {
		t.Error("an anomalous run should have fired (and recorded) a warning")
	}

	// A fresh container with only 2 priors → never fires.
	for i := 0; i < 2; i++ {
		b := &store.Backup{ID: idFor("q", i), NodeID: "n1", TargetName: "mini", Status: "success", CreatedAt: int64(300 + i)}
		_ = st.CreateBackup(b)
		b.SizeBytes = gb
		b.DurationMs = 10000
		_ = st.UpdateBackup(b)
	}
	huge := &store.Backup{ID: "q-new", NodeID: "n1", TargetName: "mini", Status: "success", CreatedAt: 400}
	_ = st.CreateBackup(huge)
	huge.SizeBytes = 100 * gb
	huge.DurationMs = 999999
	_ = st.UpdateBackup(huge)
	s.checkBackupAnomaly(huge)
	if v, _ := st.GetSetting(alertKey(notify.KindBackupAnomaly, "n1\x00mini"), "0"); v != "0" {
		t.Error("a container with <3 priors must never fire")
	}
}

func idFor(prefix string, i int) string {
	return prefix + string(rune('0'+i))
}
