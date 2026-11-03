package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"dockback/internal/backup"
	"dockback/internal/storage"
	"dockback/internal/store"
)

type previewedItem struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	Reason string `json:"reason"`
}

func retentionPreview(t *testing.T, s *Server) map[string]previewedItem {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleRetentionPreview(rec, httptest.NewRequest("GET", "/api/retention/preview", nil))
	var out struct {
		Targets []struct {
			Items []previewedItem `json:"items"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%v: %s", err, rec.Body.String())
	}
	items := map[string]previewedItem{}
	for _, target := range out.Targets {
		for _, item := range target.Items {
			items[item.ID] = item
		}
	}
	return items
}

func previewServer(t *testing.T) *Server {
	t.Helper()
	s := stepUpRestoreServer(t)
	local, err := storage.NewLocal(filepath.Join(t.TempDir(), "backups"))
	if err != nil {
		t.Fatal(err)
	}
	s.engine.Storage = local
	return s
}

// mkRetentionRow stores a backup of "app"; age is in days, so a bigger age is older.
func mkRetentionRow(t *testing.T, s *Server, id, verified, parent string, age int) {
	t.Helper()
	man, err := json.Marshal(backup.Manifest{BackupID: id, TargetName: "app", Parent: parent})
	if err != nil {
		t.Fatal(err)
	}
	b := &store.Backup{ID: id, NodeID: "n1", TargetName: "app", Status: "success", Verified: verified,
		CreatedAt: time.Now().AddDate(0, 0, -age).Unix(), ManifestJSON: string(man), StorageKey: "k/" + id}
	if err := s.store.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
}

// The preview is what an operator agrees to, so it must name exactly what the
// sweep deletes: it used to work out its own answer, and knew nothing of the
// chains and holds the sweep honours.
func TestRetentionPreviewMatchesWhatTheSweepDeletes(t *testing.T) {
	s := previewServer(t)
	if err := s.store.SetSetting("retention.generations", "1"); err != nil {
		t.Fatal(err)
	}
	mkRetentionRow(t, s, "old-full", "verified", "", 6)
	mkRetentionRow(t, s, "full", "verified", "", 5)
	mkRetentionRow(t, s, "delta", "verified", "full", 4)
	mkRetentionRow(t, s, "old-failure", "failed", "", 3)
	mkRetentionRow(t, s, "corrupt", "failed", "", 1)

	items := retentionPreview(t, s)

	var previewedPrune []string
	for id, item := range items {
		if item.Action == "prune" {
			previewedPrune = append(previewedPrune, id)
		}
	}
	slices.Sort(previewedPrune)
	if want := []string{"old-failure", "old-full"}; !slices.Equal(previewedPrune, want) {
		t.Errorf("previewed prune %v, want %v", previewedPrune, want)
	}
	if items["full"].Reason != backup.RetainedChain {
		t.Errorf("the preview should say why full stays: %+v", items["full"])
	}

	s.engine.PruneAll(context.Background())

	for id := range items {
		_, err := s.store.GetBackup(id)
		deleted := err != nil
		if deleted != slices.Contains(previewedPrune, id) {
			t.Errorf("%s: deleted=%v, but previewed prune %v", id, deleted, previewedPrune)
		}
	}
}

func TestRetentionPreviewShowsTheFloorAndAHold(t *testing.T) {
	s := previewServer(t)
	if err := s.store.SetSetting("retention.generations", "1"); err != nil {
		t.Fatal(err)
	}
	mkRetentionRow(t, s, "good", "verified", "", 2)
	mkRetentionRow(t, s, "pending", "unverified", "", 1)

	if got := retentionPreview(t, s)["good"]; got.Action != "keep" || got.Reason != backup.RetainedLastHealthy {
		t.Errorf("the last healthy point should be kept and say why: %+v", got)
	}

	mkRetentionRow(t, s, "v3", "verified", "", 0)
	if got := retentionPreview(t, s)["good"]; got.Action != "prune" {
		t.Errorf("with a newer verified backup it is an ordinary candidate: %+v", got)
	}
	if err := s.store.SetSetting(backup.RetentionHoldKey("n1", "app"), "suspect|1"); err != nil {
		t.Fatal(err)
	}
	if got := retentionPreview(t, s)["good"]; got.Action != "keep" {
		t.Errorf("a held target prunes nothing, so the preview must not promise it: %+v", got)
	}
}
