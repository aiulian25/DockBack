package api

import (
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"dockback/internal/store"
)

// TestStandbyOverdueSelection verifies the scheduling decision: only entries past
// their interval are due, and they come back oldest-run-first so the most-overdue
// is rehearsed first (and the loop bounds to one per cycle by taking due[0]).
func TestStandbyOverdueSelection(t *testing.T) {
	const day = int64(24 * 3600)
	now := int64(1_000_000_000)
	list := []*store.Standby{
		{NodeID: "n1", Target: "fresh", StandbyNode: "n2", IntervalDays: 7, LastRun: now - 2*day},      // not due (weekly, ran 2d ago)
		{NodeID: "n1", Target: "due-old", StandbyNode: "n2", IntervalDays: 7, LastRun: now - 30*day},   // due, oldest
		{NodeID: "n1", Target: "never", StandbyNode: "n2", IntervalDays: 7, LastRun: 0},                // due, never run
		{NodeID: "n1", Target: "due-recent", StandbyNode: "n2", IntervalDays: 7, LastRun: now - 8*day}, // due, just over interval
	}
	due := overdueStandby(list, now)
	if len(due) != 3 {
		t.Fatalf("expected 3 due, got %d", len(due))
	}
	// Oldest LastRun first: never(0) < due-old(-30d) < due-recent(-8d).
	want := []string{"never", "due-old", "due-recent"}
	for i, w := range want {
		if due[i].Target != w {
			t.Errorf("due[%d] = %q, want %q", i, due[i].Target, w)
		}
	}
	// The bounded loop rehearses only due[0] per cycle — the most overdue.
	if due[0].Target != "never" {
		t.Errorf("most-overdue picked = %q, want never", due[0].Target)
	}

	// A zero/unset interval falls back to weekly, so an entry run 3 days ago isn't due.
	none := overdueStandby([]*store.Standby{{Target: "x", LastRun: now - 3*day, IntervalDays: 0}}, now)
	if len(none) != 0 {
		t.Errorf("weekly default should not be due after 3 days: %v", none)
	}
}

func TestClampStandbyInterval(t *testing.T) {
	cases := map[int]int{0: 7, -5: 7, 1: 1, 30: 30, 400: 365}
	for in, want := range cases {
		if got := clampStandbyInterval(in); got != want {
			t.Errorf("clampStandbyInterval(%d) = %d, want %d", in, got, want)
		}
	}
}

// F75: /metrics exports standby readiness. Two standbys — one proven, one
// failing — must show configured=2, failing=1, and a non-negative oldest age.
func TestStandbyMetricsGauges(t *testing.T) {
	st := testStore(t)
	s := &Server{store: st}

	scrape := func() string {
		rec := httptest.NewRecorder()
		s.handleMetrics(rec, httptest.NewRequest("GET", "/metrics", nil))
		return rec.Body.String()
	}

	// No standbys: configured 0, failing 0, age -1.
	body := scrape()
	for _, want := range []string{"dockback_standby_configured 0", "dockback_standby_failing 0", "dockback_oldest_standby_age_seconds -1"} {
		if !strings.Contains(body, want) {
			t.Errorf("empty fleet: metrics missing %q", want)
		}
	}

	// One proven (rehearsed 1h ago), one failing (rehearsed 2h ago) — the oldest
	// age comes from the LEAST-recently rehearsed one.
	now := time.Now().Unix()
	if err := st.SetStandby("n1", "app", "n2", 7); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordStandbyResult("n1", "app", true, 1200, "ok", now-3600); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStandby("n1", "db", "n2", 7); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordStandbyResult("n1", "db", false, 0, "boot failed", now-7200); err != nil {
		t.Fatal(err)
	}

	body = scrape()
	if !strings.Contains(body, "dockback_standby_configured 2") || !strings.Contains(body, "dockback_standby_failing 1") {
		t.Fatalf("gauge math wrong:\n%s", body)
	}
	// Oldest age ≈ 7200s (the failing one is the least-recently rehearsed).
	m := regexp.MustCompile(`dockback_oldest_standby_age_seconds (\d+)`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("oldest-standby gauge missing or negative with rehearsed standbys")
	}
	age, _ := strconv.ParseInt(m[1], 10, 64)
	if age < 7200-5 || age > 7200+60 {
		t.Fatalf("oldest age = %d, want ≈7200", age)
	}
}
