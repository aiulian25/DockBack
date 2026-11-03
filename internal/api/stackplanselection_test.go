package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// The capacity verdict is the one number an operator acts on before committing
// a stack restore. Sized over the whole stack while half of it is deselected,
// it can refuse a restore that would comfortably fit — a refusal with no
// remedy, since deselecting more does not change it.
func planEntries(names ...string) []backup.StackPlanEntry {
	out := make([]backup.StackPlanEntry, 0, len(names))
	for _, n := range names {
		out = append(out, backup.StackPlanEntry{Service: n})
	}
	return out
}

func serviceNames(entries []backup.StackPlanEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Service)
	}
	return out
}

func TestSelectedEntriesNarrowsToTheKeptServices(t *testing.T) {
	all := planEntries("db", "api", "web")

	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{"a subset", "db,web", []string{"db", "web"}},
		{"one service", "api", []string{"api"}},
		{"spaces are tolerated", " db , api ", []string{"db", "api"}},
		// A client that sends nothing means the whole stack, so the endpoint
		// behaves exactly as it did before the parameter existed.
		{"absent", "", []string{"db", "api", "web"}},
		{"empty list", ",,", []string{"db", "api", "web"}},
		// This panel refreshes mid-selection; an unknown name is stale input, not
		// an error, and must not silently size the restore at zero.
		{"only unknown names", "ghost", []string{"db", "api", "web"}},
		{"unknown names are ignored", "db,ghost", []string{"db"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := serviceNames(selectedEntries(url.Values{"services": {tc.query}}, all))
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v (order must follow the plan)", got, tc.want)
				}
			}
		})
	}

	// The caller still gets the full list for the picker.
	if len(all) != 3 {
		t.Errorf("the plan itself must not be mutated: %v", serviceNames(all))
	}
}

func TestSelectedPlanEntriesFollowsTheRestoreRequest(t *testing.T) {
	all := []backup.StackPlanEntry{
		{Service: "db", TargetName: "blog-db"},
		{Service: "web", TargetName: "blog-web"},
	}
	cases := []struct {
		name  string
		names []string
		want  []string
	}{
		{"no names mean the whole stack", nil, []string{"db", "web"}},
		{"a service", []string{"db"}, []string{"db"}},
		{"a container name", []string{"blog-web"}, []string{"web"}},
		// Unlike the plan panel: the engine refuses this, so nothing is left to judge.
		{"an unknown name matches nothing", []string{"ghost"}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := serviceNames(selectedPlanEntries(all, tc.names)); !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func postStackRestoreQuery(s *Server, query string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/nodes/n1/stacks/blog/restore?"+query, strings.NewReader(""))
	r.SetPathValue("id", "n1")
	r.SetPathValue("project", "blog")
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	rec := httptest.NewRecorder()
	s.handleRestoreStack(rec, r)
	return rec
}

// The page sends one service= per member and a source=. Reading any other name
// would look identical from the outside: the request succeeds and restores the
// whole stack from the fastest copy.
func TestStackRestoreReadsTheServicesAndCopyTheClientSends(t *testing.T) {
	s := stepUpRestoreServer(t)
	mkStackMember(t, s, "b-db", "n1", "blog", "db", "blog-db")
	mkStackMember(t, s, "b-app", "n1", "blog", "app", "blog-app")
	markCritical(t, s, "n1", "blog-db")

	if rec := postStackRestoreQuery(s, "service=db"); rec.Code != http.StatusUnauthorized {
		t.Errorf("the protected member was selected, so it must be gated: %d %s", rec.Code, rec.Body.String())
	}
	if rec := postStackRestoreQuery(s, "service=app&service=db"); rec.Code != http.StatusUnauthorized {
		t.Errorf("every repeated service= counts: %d %s", rec.Code, rec.Body.String())
	}

	rec := postStackRestoreQuery(s, "service=app&source=local")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("the protected member was left out, so nothing should be asked: %d %s", rec.Code, rec.Body.String())
	}
	started := false
	for _, e := range mustAudit(t, s) {
		if !strings.HasPrefix(e.Action, "stack.") {
			continue
		}
		started = true
		for _, want := range []string{"(services: app)", "(source: local)"} {
			if !strings.Contains(e.Detail, want) {
				t.Errorf("the audit row should record %q: %q", want, e.Detail)
			}
		}
		if strings.Contains(e.Detail, "protected") {
			t.Errorf("nothing protected is being overwritten: %q", e.Detail)
		}
	}
	if !started {
		t.Fatal("the restore should have been audited")
	}
}

// Omitting the parameter means the whole stack. Sending it empty does not: that
// is a script with an empty variable, and a destructive request must not widen.
func TestStackRestoreRefusesASelectionThatNamesNothing(t *testing.T) {
	s := stepUpRestoreServer(t)
	mkStackMember(t, s, "b-db", "n1", "blog", "db", "blog-db")

	for _, query := range []string{"service=", "service=&service=%20"} {
		rec := postStackRestoreQuery(s, query)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400: %s", query, rec.Code, rec.Body.String())
		}
	}
	for _, e := range mustAudit(t, s) {
		if strings.HasPrefix(e.Action, "stack.") {
			t.Errorf("a refused restore must not be audited as started: %+v", e)
		}
	}
	if !s.locks.acquireRestore(stackKey("n1", "blog", "")) {
		t.Error("a refused restore must not hold the lock")
	}
	s.locks.releaseRestore(stackKey("n1", "blog", ""))
}

// fakeDockerWithNoNetworks answers a Docker client's version check and network
// list, so a cross-host gate can reach a verdict with no daemon.
func fakeDockerWithNoNetworks(t *testing.T) *httptest.Server {
	t.Helper()
	docker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("API-Version", "1.44")
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/networks") {
			_, _ = w.Write([]byte("[]"))
		}
	}))
	t.Cleanup(docker.Close)
	return docker
}

func joinSharedNetwork(t *testing.T, s *Server, backupID, networkName string) {
	t.Helper()
	b, err := s.store.GetBackup(backupID)
	if err != nil {
		t.Fatal(err)
	}
	man := manifestOf(b)
	man.Networks = []backup.NetworkRef{{Name: networkName}}
	raw, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	b.ManifestJSON = string(raw)
	if err := s.store.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
}

// Sending the selection made this reachable: unticking the only service on a
// shared network must not end in a prompt about that service, on a page that says
// it is not part of this restore. The devices gate walks the same list.
func TestStackRestoreNetworkGateFollowsTheSelection(t *testing.T) {
	s := stepUpRestoreServer(t)
	mkStackMember(t, s, "b-db", "n1", "blog", "db", "blog-db")
	mkStackMember(t, s, "b-app", "n1", "blog", "app", "blog-app")
	joinSharedNetwork(t, s, "b-app", "proxy")
	if err := s.store.UpsertNode(&store.Node{ID: "n2", Name: "n2", Transport: dockercli.TransportTCPProxy}); err != nil {
		t.Fatal(err)
	}
	s.reg.Set(dockercli.NodeConn{ID: "n2", Transport: dockercli.TransportTCPProxy, Address: "tcp://" + fakeDockerWithNoNetworks(t).Listener.Addr().String()})
	// Held so that a request past every gate stops at the lock instead of starting
	// a real restore, which leaves it answering with the lock's own conflict.
	targetLock := stackKey("n2", "blog", "")
	if !s.locks.acquireRestore(targetLock) {
		t.Fatal("the target lock should be free")
	}
	defer s.locks.releaseRestore(targetLock)

	refusedForTheNetwork := func(query string) bool {
		rec := postStackRestoreQuery(s, "target_node=n2&"+query)
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s: code = %d, want 409: %s", query, rec.Code, rec.Body.String())
		}
		return strings.Contains(rec.Body.String(), "missing_networks")
	}
	if !refusedForTheNetwork("service=app") {
		t.Error("the selected service joins a network the target lacks, so it must be asked about")
	}
	if !refusedForTheNetwork("confirm_missing=true") {
		t.Error("no selection means the whole stack, which includes that service")
	}
	if refusedForTheNetwork("service=db") {
		t.Error("the service on that network was left out, so it must not stop this restore")
	}
}
