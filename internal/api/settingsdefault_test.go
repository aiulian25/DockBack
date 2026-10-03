package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dockback/internal/backup"
	"dockback/internal/config"
	"dockback/internal/storage"
)

// mustLocal is a throwaway local backend: handleGetSettings reports the storage
// backend's name, so the endpoint needs one to answer at all.
func mustLocal(t *testing.T) storage.Backend {
	t.Helper()
	be, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return be
}

// Settings answered "10 generations" while the engine kept 3. An operator
// reading the page was told a policy the application was not applying, and the
// two endpoints that report retention disagreed with each other as well.
func TestRetentionDefaultsAgreeAcrossEveryEndpoint(t *testing.T) {
	st := testStore(t)
	s := &Server{
		store:  st,
		cfg:    &config.Config{DBReadyTimeout: 300},
		engine: &backup.Engine{Store: st, Storage: mustLocal(t), Log: func(string, string, string) {}},
	}

	rec := httptest.NewRecorder()
	s.handleGetSettings(rec, httptest.NewRequest("GET", "/api/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("settings: %d %s", rec.Code, rec.Body.String())
	}
	var settings map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	if got := settings["retention.generations"]; got != backup.DefaultGenerations {
		t.Errorf("Settings reports %v generations; the engine keeps %s", got, backup.DefaultGenerations)
	}

	// The policy endpoint reads the same setting and must answer the same.
	if got := s.loadPolicy().Generations; got != 3 {
		t.Errorf("the policy endpoint reports %d generations, the engine keeps %s", got, backup.DefaultGenerations)
	}

	// A value the operator actually set is reported by both, unchanged.
	if err := s.store.SetSetting("retention.generations", "9"); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	s.handleGetSettings(rec, httptest.NewRequest("GET", "/api/settings", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &settings)
	if settings["retention.generations"] != "9" || s.loadPolicy().Generations != 9 {
		t.Errorf("a configured policy must be reported as-is: settings=%v policy=%d",
			settings["retention.generations"], s.loadPolicy().Generations)
	}
}
