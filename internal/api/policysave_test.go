package api

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// Saving the policy writes eleven settings rows that only make sense together.
// These lock in that every one of them is written, and that the keys the save
// uses are the keys the load reads back.
func savePolicy(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleSetPolicy(rec, httptest.NewRequest("POST", "/api/policy", strings.NewReader(body)))
	return rec
}

func TestPolicySaveWritesEverySettingItReadsBack(t *testing.T) {
	s := newTestServer(t)

	const body = `{"generations":7,"keep_daily":6,"keep_weekly":5,"keep_monthly":4,
		"keep_yearly":3,"autoprune":true,"autoclean_missing_days":9,
		"schedule":{"enabled":true,"kind":"daily","time":"04:30"},
		"prune_schedule":{"enabled":true,"kind":"weekly","time":"05:00"}}`
	if rec := savePolicy(t, s, body); rec.Code != 200 {
		t.Fatalf("save returned %d: %s", rec.Code, rec.Body.String())
	}

	// Read back through the loader, which is the real contract: the save and the
	// load must agree on every key.
	got := s.loadPolicy()
	if got.Generations != 7 || got.KeepDaily != 6 || got.KeepWeekly != 5 ||
		got.KeepMonthly != 4 || got.KeepYearly != 3 || !got.Autoprune {
		t.Errorf("retention did not round-trip: %+v", got)
	}
	if got.AutocleanMissingDays != 9 {
		t.Errorf("autoclean_missing_days = %d, want 9", got.AutocleanMissingDays)
	}
	if !got.Schedule.Enabled || got.Schedule.Kind != "daily" || got.Schedule.Time != "04:30" {
		t.Errorf("schedule did not round-trip: %+v", got.Schedule)
	}
	if !got.PruneSchedule.Enabled || got.PruneSchedule.Kind != "weekly" {
		t.Errorf("prune schedule did not round-trip: %+v", got.PruneSchedule)
	}
}

// The prune baseline is part of the same save: without it a freshly-enabled
// prune schedule runs on the next tick instead of at its window.
func TestPolicySaveSetsThePruneBaselineWithTheSchedule(t *testing.T) {
	s := newTestServer(t)

	savePolicy(t, s, `{"prune_schedule":{"enabled":true,"kind":"daily","time":"05:00"}}`)
	on, _ := s.store.GetSetting(retentionPruneLastRunKey, "0")
	if n, _ := strconv.ParseInt(on, 10, 64); n <= 0 {
		t.Errorf("enabling the prune schedule must stamp a baseline, got %q", on)
	}

	savePolicy(t, s, `{"prune_schedule":{"enabled":false}}`)
	off, _ := s.store.GetSetting(retentionPruneLastRunKey, "")
	if off != "0" {
		t.Errorf("disabling must clear the baseline, got %q", off)
	}
}

// A rejected policy must not have written anything at all.
func TestPolicySaveRejectsABadCronBeforeWritingAnything(t *testing.T) {
	s := newTestServer(t)
	savePolicy(t, s, `{"generations":5}`)

	rec := savePolicy(t, s, `{"generations":99,"schedule":{"enabled":true,"kind":"custom","cron":"not a cron"}}`)
	if rec.Code != 400 {
		t.Fatalf("a bad cron must be refused, got %d", rec.Code)
	}
	if got := s.loadPolicy(); got.Generations != 5 {
		t.Errorf("generations = %d — a refused save still changed the policy", got.Generations)
	}
}

func TestPolicySaveRejectsAMalformedBody(t *testing.T) {
	s := newTestServer(t)
	rec := savePolicy(t, s, `{"generations":`)
	if rec.Code != 400 {
		t.Errorf("malformed body returned %d, want 400", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["error"] == nil {
		t.Errorf("a refusal must say why: %s", rec.Body.String())
	}
}
