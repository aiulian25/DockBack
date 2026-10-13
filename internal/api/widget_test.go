package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// A dashboard widget shows the catalog's counts, the coverage the dashboard
// shows, and the failures still waiting for a fix — never one of a container
// that is gone or ignored, or on a node DockBack no longer has.
func TestWidgetSummary(t *testing.T) {
	s := newTestServer(t)
	now := time.Now()
	for _, n := range []*store.Node{{ID: "n1", Name: "Acer", Transport: "local"}, {ID: "n2", Name: "Pi", Transport: "tcp"}} {
		if err := s.store.UpsertNode(n); err != nil {
			t.Fatal(err)
		}
	}
	s.setStat("n1", &nodeStat{Reachable: true, Containers: []*dockercli.Container{
		{Name: "app", State: "running"}, {Name: "web", State: "running"}, {Name: "cache", State: "running"},
	}})
	s.saveKeySet(ignoredSettingKey("n1"), []string{ignoreKey(ignoreKindContainer, "cache")})
	hourAgo, twoDaysAgo := now.Add(-time.Hour).Unix(), now.Add(-48*time.Hour).Unix()
	for _, b := range []*store.Backup{
		{ID: "app-ok", NodeID: "n1", TargetName: "app", Status: "success", CreatedAt: twoDaysAgo},
		{ID: "app-fail", NodeID: "n1", TargetName: "app", Status: "failed", CreatedAt: hourAgo},
		{ID: "gone-fail", NodeID: "n1", TargetName: "gone", Status: "failed", CreatedAt: hourAgo},
		{ID: "cache-fail", NodeID: "n1", TargetName: "cache", Status: "failed", CreatedAt: hourAgo},
		{ID: "old-fail", NodeID: "n2", TargetName: "old", Status: "failed", CreatedAt: twoDaysAgo},
		{ID: "orphan-fail", NodeID: "n3", TargetName: "x", Status: "failed", CreatedAt: hourAgo},
		{ID: "web-run", NodeID: "n1", TargetName: "web", Status: "running", CreatedAt: hourAgo},
	} {
		if err := s.store.CreateBackup(b); err != nil {
			t.Fatal(err)
		}
	}

	summary, err := s.widgetSummary(now)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Nodes != 2 || summary.NodesOffline != 1 {
		t.Errorf("two nodes, the one with no inventory offline: %+v", summary)
	}
	if summary.Backups != 7 || summary.Failed24h != 4 || summary.Running != 1 {
		t.Errorf("every catalog entry, the last day's failures, the running one: %+v", summary)
	}
	if summary.Unprotected != 1 {
		t.Errorf("web was never backed up; app was two days ago; cache is ignored: unprotected %d", summary.Unprotected)
	}
	want := []widgetTarget{{Name: "app", Node: "Acer", NodeID: "n1"}, {Name: "old", Node: "Pi", NodeID: "n2"}}
	if summary.Failing != 2 || len(summary.FailingTargets) != 2 || summary.FailingTargets[0] != want[0] || summary.FailingTargets[1] != want[1] {
		t.Errorf("failing: the present container and the node with no inventory yet, newest first: %d %+v", summary.Failing, summary.FailingTargets)
	}
	if summary.LastBackup != time.Unix(twoDaysAgo, 0).UTC().Format(time.RFC3339) {
		t.Errorf("last backup is the newest success, in RFC 3339: %q", summary.LastBackup)
	}
}

// An empty install still answers with every field a widget maps, so a tile
// shows 0 instead of an error, and the failing list is an empty array.
func TestWidgetSummaryOfAnEmptyInstall(t *testing.T) {
	s := newTestServer(t)
	w := httptest.NewRecorder()
	s.handleWidget(w, httptest.NewRequest(http.MethodGet, widgetPath, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"nodes", "nodes_offline", "backups", "backups_bytes", "failed_24h", "failing", "unprotected", "running"} {
		if body[field] != float64(0) {
			t.Errorf("%s = %v, want 0", field, body[field])
		}
	}
	if list, ok := body["failing_targets"].([]any); !ok || len(list) != 0 {
		t.Errorf("failing_targets must be an empty array, got %v", body["failing_targets"])
	}
	if _, present := body["last_backup"]; present {
		t.Error("last_backup is left out when there has never been a backup")
	}
}
