package api

import (
	"dockback/internal/backup"
	"dockback/internal/storage"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

// A pass recorded for a backup the drill had nothing to restore from proved nothing.
func TestAnUntestedDrillPassEarnsNoGrade(t *testing.T) {
	b := &store.Backup{Verified: "verified"}
	man := &backup.Manifest{}
	locs := []backup.Location{{Kind: "local"}, {Kind: "dest", DestID: "d1"}}
	legacy := &store.Drill{OK: true, Detail: backup.LegacyUntestedDrillDetail}
	real := &store.Drill{OK: true, Detail: "volumes: booted"}
	legacyGrade, reasons := gradeBackup(b, man, locs, legacy, nil)
	realGrade, _ := gradeBackup(b, man, locs, real, nil)
	if legacyGrade == realGrade || !strings.Contains(strings.Join(reasons, " "), "never drilled") {
		t.Fatalf("legacy %s (%v), real %s", legacyGrade, reasons, realGrade)
	}
}

func drillTestServer(t *testing.T) *Server {
	t.Helper()
	st := testStore(t)
	be, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Server{store: st, engine: &backup.Engine{Store: st, Storage: be, WorkDir: t.TempDir(), Log: func(string, string, string) {}}}
}

func drillRow(t *testing.T, st *store.Store, id, target string, man backup.Manifest, createdAt int64) {
	t.Helper()
	b := &store.Backup{ID: id, NodeID: "n1", TargetName: target, Status: "success", CreatedAt: createdAt}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	mj, _ := json.Marshal(&man)
	b.ManifestJSON = string(mj)
	b.StorageKey = "n1/" + id + ".dback"
	b.LocationsJSON = `[{"kind":"local","name":"local","type":"local"}]`
	if err := st.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
}

// A backup a drill cannot test is never recorded, so it stays the most overdue. It
// must not use up the cycle and stop every other drill.
func TestTheSweepMovesPastWhatItCannotDrill(t *testing.T) {
	s := drillTestServer(t)
	if err := s.store.SetSetting(drillIntervalKey, "1"); err != nil {
		t.Fatal(err)
	}
	drillRow(t, s.store, "exportonly", "paperless", backup.Manifest{AppExport: &backup.AppExportRef{Tool: "paperless"}}, 1)
	drillRow(t, s.store, "drillable", "app", backup.Manifest{Volumes: []backup.VolumeRef{{Destination: "/data"}}}, 2)
	// Drilled long ago, so it is overdue but less so than the never-drilled export.
	const longAgo = 1
	if err := s.store.UpsertDrill("drillable", true, "volumes: booted", longAgo); err != nil {
		t.Fatal(err)
	}

	s.drillTick()
	if d, err := s.store.GetDrill("drillable"); err != nil || d.RanAt == longAgo {
		t.Fatalf("the drillable backup was never reached: %+v %v", d, err)
	}
	if _, err := s.store.GetDrill("exportonly"); err == nil {
		t.Fatal("a backup a drill cannot test must not be recorded as drilled")
	}
}

func TestADrillAlreadyRequestedIsNotStartedTwice(t *testing.T) {
	s := drillTestServer(t)
	drillRow(t, s.store, "b1", "app", backup.Manifest{Volumes: []backup.VolumeRef{{Destination: "/data"}}}, 1)
	s.drillsInFlight.Store("b1", struct{}{})
	r := httptest.NewRequest(http.MethodPost, "/api/backups/b1/drill", nil)
	r.SetPathValue("id", "b1")
	rec := httptest.NewRecorder()
	s.handleDrillBackup(rec, r)
	if rec.Code != http.StatusConflict {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestAnExportOnlyBackupCannotBeDrilledOnRequest(t *testing.T) {
	s := drillTestServer(t)
	drillRow(t, s.store, "b1", "paperless", backup.Manifest{AppExport: &backup.AppExportRef{Tool: "paperless"}}, 1)
	r := httptest.NewRequest(http.MethodPost, "/api/backups/b1/drill", nil)
	r.SetPathValue("id", "b1")
	rec := httptest.NewRecorder()
	s.handleDrillBackup(rec, r)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "export") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}
