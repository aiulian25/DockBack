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

// A model server whose models download again was flagged as never backed up on
// every page and alert, and Full Server Backup would have copied every model.
// Ignored, it is left out of both — and so is every member of an ignored stack —
// while a schedule that names it still backs it up.
func TestWholeServerRunsLeaveIgnoredOut(t *testing.T) {
	ollama := &dockercli.Container{ID: "c1", Name: "ollama", State: "running"}
	web := &dockercli.Container{ID: "c2", Name: "shop-web-1", Stack: "shop", State: "running"}
	other := &dockercli.Container{ID: "c3", Name: "other", State: "running"}
	ignored := ignoreSet{ignoreKey(ignoreKindContainer, "ollama"): true, ignoreKey(ignoreKindStack, "shop"): true}

	got := nodeBackupTargets([]*dockercli.Container{ollama, web, other}, true, ignored)
	if len(got) != 1 || got[0].Name != "other" {
		t.Fatalf("Full Server Backup must skip the ignored container and stack, got %v", targetNames(got))
	}

	wholeServer := Schedule{Targets: []ScheduleTarget{{NodeID: "n1"}}}
	if covers := scheduleCovers(wholeServer, "n1", ollama, ignored); covers != "" {
		t.Errorf("a whole-server schedule does not back up an ignored container, but it says %q", covers)
	}
	named := Schedule{Targets: []ScheduleTarget{{NodeID: "n1", ContainerName: "ollama"}}}
	if covers := scheduleCovers(named, "n1", ollama, ignored); covers != coveredByName {
		t.Errorf("a schedule that names an ignored container still backs it up, got %q", covers)
	}
}

func TestIgnoringTakesAContainerOffTheWarnings(t *testing.T) {
	s := &Server{store: testStore(t), stats: map[string]*nodeStat{}}
	if err := s.store.UpsertNode(&store.Node{ID: "n1", Name: "node-a"}); err != nil {
		t.Fatal(err)
	}
	s.stats["n1"] = &nodeStat{Reachable: true, Containers: []*dockercli.Container{
		{ID: "c1", Name: "ollama", Image: "ollama/ollama:latest", State: "running"},
		{ID: "c2", Name: "web", State: "running"},
	}}

	listed := callIgnore(t, s, http.MethodPut, ignoreKindContainer, "ollama")
	if len(listed) != 1 || !listed[0].Present || listed[0].ContainerID != "c1" || listed[0].Detail != "ollama/ollama:latest" {
		t.Fatalf("the ignore list should hold ollama as it is now, got %+v", listed)
	}
	cov, err := s.computeCoverage(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(cov.Nodes) != 1 || len(cov.Nodes[0].Unprotected) != 1 || cov.Nodes[0].Unprotected[0].Name != "web" || cov.RunningTotal != 1 {
		t.Fatalf("only web is unprotected once ollama is ignored, got %+v", cov.Nodes)
	}

	if listed := callIgnore(t, s, http.MethodDelete, ignoreKindContainer, "ollama"); len(listed) != 0 {
		t.Fatalf("stop ignoring empties the list, got %+v", listed)
	}
	cov, _ = s.computeCoverage(time.Now())
	if len(cov.Nodes[0].Unprotected) != 2 {
		t.Fatalf("ollama is back on the warnings, got %+v", cov.Nodes[0].Unprotected)
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/", nil)
	r.SetPathValue("id", "n1")
	r.SetPathValue("kind", "volume")
	r.SetPathValue("name", "data")
	s.handleIgnore(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("only containers and stacks can be ignored, got %d", rec.Code)
	}
}

// An ignored stack is described by how many services it has, and one that is
// gone stays listed so the choice can be undone. Pure.
func TestDescribeIgnored(t *testing.T) {
	containers := []*dockercli.Container{{ID: "c1", Name: "shop-web-1", Stack: "shop"}, {ID: "c2", Name: "shop-db-1", Stack: "shop"}}
	if item := describeIgnored(ignoreKindStack, "shop", containers); !item.Present || item.Detail != "2 services" {
		t.Errorf("a present stack: %+v", item)
	}
	if item := describeIgnored(ignoreKindContainer, "gone", containers); item.Present || item.ContainerID != "" {
		t.Errorf("a removed container: %+v", item)
	}
}

// callIgnore ignores (PUT) or stops ignoring (DELETE) one item on node n1.
func callIgnore(t *testing.T, s *Server, method, kind, name string) []ignoredItem {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(method, "/", nil)
	r.SetPathValue("id", "n1")
	r.SetPathValue("kind", kind)
	r.SetPathValue("name", name)
	s.handleIgnore(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s %s %s = %d: %s", method, kind, name, rec.Code, rec.Body.String())
	}
	var out []ignoredItem
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
