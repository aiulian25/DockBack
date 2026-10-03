package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"dockback/internal/backup"
	"dockback/internal/storage"
	"dockback/internal/store"
)

// TestBulkDeleteBackups verifies the multi-select delete endpoint deletes each
// requested backup through the shared audited path, reports per-id failures
// (e.g. an unknown id) instead of aborting the batch, leaves un-requested
// backups intact, and rejects an empty request.
func TestBulkDeleteBackups(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	be, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eng := &backup.Engine{Store: st, Storage: be, Key: make([]byte, 32), Log: func(string, string, string) {}}
	s := &Server{store: st, engine: eng}

	for _, id := range []string{"a", "b", "c"} {
		if err := st.CreateBackup(&store.Backup{ID: id, NodeID: "n", TargetName: "svc-" + id, Status: "success", CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}

	body, _ := json.Marshal(map[string]any{"ids": []string{"a", "b", "missing"}})
	rec := httptest.NewRecorder()
	s.handleBulkDeleteBackup(rec, httptest.NewRequest("POST", "/api/backups/delete", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var res struct {
		Deleted int `json:"deleted"`
		Failed  []struct {
			ID    string `json:"id"`
			Error string `json:"error"`
		} `json:"failed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 2 {
		t.Errorf("deleted = %d, want 2", res.Deleted)
	}
	if len(res.Failed) != 1 || res.Failed[0].ID != "missing" {
		t.Errorf("failed = %+v, want one entry for \"missing\"", res.Failed)
	}
	if _, err := st.GetBackup("a"); err == nil {
		t.Error("backup a should have been deleted")
	}
	if _, err := st.GetBackup("c"); err != nil {
		t.Error("backup c was not requested and must remain")
	}

	// An empty id list is a bad request, not a silent no-op.
	rec2 := httptest.NewRecorder()
	s.handleBulkDeleteBackup(rec2, httptest.NewRequest("POST", "/api/backups/delete", bytes.NewReader([]byte(`{"ids":[]}`))))
	if rec2.Code != http.StatusBadRequest {
		t.Errorf("empty ids status = %d, want 400", rec2.Code)
	}
}
