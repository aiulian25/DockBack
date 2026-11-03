package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dockback/internal/dockercli"
)

// F10: the "restore as a copy" name must be a valid Docker container name — so a
// clone can't collide with a daemon/shell edge or inject.
func TestValidContainerName(t *testing.T) {
	good := []string{"paperless-restored", "app_copy", "db.test", "a", "X1", "immich-server-restored"}
	for _, n := range good {
		if !validContainerName(n) {
			t.Errorf("%q should be a valid container name", n)
		}
	}
	bad := []string{"", "-startsdash", ".startsdot", "has space", "bad/slash", "quote'", "semi;colon", "$(x)"}
	for _, n := range bad {
		if validContainerName(n) {
			t.Errorf("%q should be rejected", n)
		}
	}
}

// fakeDockerHolding answers a container list holding one container of that name,
// and fails the test on anything that is not a read.
func fakeDockerHolding(t *testing.T, s *Server, nodeID, name string) {
	t.Helper()
	docker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("API-Version", "1.44")
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			t.Errorf("a refused copy touched the daemon: %s %s", r.Method, r.URL.Path)
		}
		if strings.HasSuffix(r.URL.Path, "/containers/json") {
			_, _ = w.Write([]byte(`[{"Id":"unrelated-id","Names":["/` + name + `"]}]`))
		}
	}))
	t.Cleanup(docker.Close)
	s.reg.Set(dockercli.NodeConn{ID: nodeID, Transport: dockercli.TransportTCPProxy, Address: "tcp://" + docker.Listener.Addr().String()})
}

// A copy creates a container. A name something already holds is a refusal with
// the reason, before the lock and before anything is recorded as started.
func TestRestoreAsACopyRefusesANameInUse(t *testing.T) {
	s := stepUpRestoreServer(t)
	mkRestorable(t, s, "b1", "n1", "app")
	fakeDockerHolding(t, s, "n1", "taken")

	rec := postRestore(s, "b1", map[string]any{
		"node_id": "n1", "target_id": "cid", "volumes": true, "confirm": true,
		"as_name": "taken", "isolated": true, "recreate": true,
	})

	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "taken") {
		t.Fatalf("code = %d, want 409 naming it: %s", rec.Code, rec.Body.String())
	}
	if auditHas(t, s, "restore.clone") {
		t.Error("a refused copy must not be audited as started")
	}
	if !s.locks.acquireRestore(stackKey("n1", "", "taken")) {
		t.Error("a refused copy must not hold the lock")
	}
}

func TestRestoreAsACopyMustBeIsolated(t *testing.T) {
	s := stepUpRestoreServer(t)
	mkRestorable(t, s, "b1", "n1", "app")

	rec := postRestore(s, "b1", map[string]any{
		"node_id": "n1", "target_id": "cid", "volumes": true, "confirm": true, "as_name": "free-name",
	})

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "isolated") {
		t.Fatalf("code = %d, want 400 saying why: %s", rec.Code, rec.Body.String())
	}
}
