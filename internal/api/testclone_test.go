package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
)

// F219 — the server picks the clone's name and its lifetime, so both are tested
// where they are decided.

// AC1 — the name is <container>-test-<MMDD>. Derived here rather than in the
// browser so two operators, two time zones and a stale tab cannot disagree about
// what today is.
func TestTestCloneNameIsDatedAndValid(t *testing.T) {
	s := stepUpRestoreServer(t)
	// No node is registered, so the collision probe cannot run — the unsuffixed
	// name is the right answer, not an error: the restore is about to fail on the
	// unreachable node and needs one reason, not two.
	got, err := s.testCloneName(context.Background(), "no-such-node", "paperless")
	if err != nil {
		t.Fatalf("an unreachable node must not block naming: %v", err)
	}
	want := fmt.Sprintf("paperless-test-%s", time.Now().Format("0102"))
	if got != want {
		t.Errorf("name = %q, want %q", got, want)
	}
	// And it must be a name Docker will actually accept, since handleRestore
	// validates it a few lines later and a refusal there would be DockBack's own
	// fault rather than the operator's.
	if !validContainerName(got) {
		t.Errorf("the derived name must pass the same validation the request does: %q", got)
	}
}

// A container whose name cannot produce a legal clone name is refused with a
// reason, not with an invalid create the daemon rejects later.
func TestTestCloneNameRefusesAnUnusableBase(t *testing.T) {
	s := stepUpRestoreServer(t)
	for _, bad := range []string{"", "  ", "has spaces", "weird/name", "volume:photos"} {
		if _, err := s.testCloneName(context.Background(), "n1", bad); err == nil {
			t.Errorf("target %q cannot make a valid clone name and must be refused", bad)
		}
	}
}

// The request shape: test_clone names itself, so sending a name with it is a
// contradiction the server does not silently pick a winner for.
func TestTestCloneRejectsAnExplicitName(t *testing.T) {
	s := stepUpRestoreServer(t)
	mkRestorable(t, s, "b1", "n1", "prod-db")

	rec := postRestore(s, "b1", map[string]any{
		"node_id": "n1", "target_id": "cid", "volumes": true, "confirm": true,
		"test_clone": true, "as_name": "my-own-name",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "as_name") {
		t.Errorf("the refusal must name the field that conflicts: %s", rec.Body.String())
	}
}

// A test clone is never an in-place restore, so it can never demand the F206
// step-up — the same exemption restore-as-a-copy already has, reached through
// the new flag. If this ever regressed, the safe path would become the slow one.
func TestTestCloneIsIsolatedAndNeedsNoStepUp(t *testing.T) {
	s := stepUpRestoreServer(t)
	mkRestorable(t, s, "b1", "n1", "prod-db")
	crit := s.loadCriticalDBs()
	crit[critKey("n1", "prod-db")] = CriticalDB{NodeID: "n1", Name: "prod-db", RPOSeconds: 300}
	_ = s.saveCriticalDBs(crit)

	rec := postRestore(s, "b1", map[string]any{
		"node_id": "n1", "target_id": "cid", "volumes": true, "confirm": true,
		"test_clone": true,
	})
	if rec.Code == http.StatusUnauthorized || strings.Contains(rec.Body.String(), "step_up_required") {
		t.Fatalf("a test clone touches nothing that exists and must not be gated: %d %s", rec.Code, rec.Body.String())
	}
	// It is recorded as its own action, so the trail distinguishes "proved a
	// backup" from "overwrote a container".
	rows, err := s.store.ListAudit(50)
	if err != nil {
		t.Fatal(err)
	}
	found := ""
	for _, a := range rows {
		if a.Action == "restore.test_clone" {
			found = a.Detail
		}
	}
	if found == "" {
		t.Error("a test clone must be audited under its own action")
	}
}

// The TTL is bounded where every other tunable is. A clone that outlives its
// purpose by a quarter is the problem this feature exists to remove, so the
// ceiling is part of the feature, not decoration.
func TestTestCloneTTLIsClampedAndDefaulted(t *testing.T) {
	s := stepUpRestoreServer(t)
	if got := s.testCloneTTLHours(); got != defaultTestCloneTTLHours {
		t.Errorf("unset TTL = %d, want the %d-hour default", got, defaultTestCloneTTLHours)
	}
	for _, tc := range []struct{ in, want string }{
		{"0", "1"}, {"1", "1"}, {"48", "48"}, {"168", "168"}, {"9999", "168"}, {"-5", "1"}, {"nonsense", "24"},
	} {
		got, ok := coerceSetting("restore.test_clone_ttl_hours", tc.in)
		if !ok || got != tc.want {
			t.Errorf("coerce(%q) = %q,%v — want %q", tc.in, got, ok, tc.want)
		}
	}
	// And the clamped value is what the confirm dialog and the stamp both read.
	if err := s.store.SetSetting("restore.test_clone_ttl_hours", "72"); err != nil {
		t.Fatal(err)
	}
	if got := s.testCloneTTLHours(); got != 72 {
		t.Errorf("configured TTL = %d, want 72", got)
	}
}

// The remove-now route is not a general container delete. It is authenticated,
// but so is everything; what stops it being a wrecking ball is that the engine
// refuses anything without the marker.
func TestDeleteTestCloneValidatesItsInput(t *testing.T) {
	s := stepUpRestoreServer(t)
	for _, body := range []map[string]any{
		{},
		{"node_id": "n1"},
		{"container_id": "abc"},
	} {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", "/api/test-clones/remove", strings.NewReader(string(raw)))
		rec := httptest.NewRecorder()
		s.handleDeleteTestClone(rec, r)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %v = %d, want 400: %s", body, rec.Code, rec.Body.String())
		}
	}
	// An unknown node is a 404, not an attempt against a nil client.
	raw, _ := json.Marshal(map[string]any{"node_id": "ghost", "container_id": "abc"})
	rec := httptest.NewRecorder()
	s.handleDeleteTestClone(rec, httptest.NewRequest("POST", "/api/test-clones/remove", strings.NewReader(string(raw))))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown node = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// The rest of the app has to ignore a test clone, or this feature quietly
// creates work for the operator: a whole-node schedule would back the clone up,
// and the coverage panel would list it as an unprotected container to protect —
// both true statements about something that will not exist tomorrow.
func TestTestClonesAreInvisibleToScheduleAndCoverage(t *testing.T) {
	clone := &dockercli.Container{ID: "c1", Name: "app-test-0104", State: "running",
		Labels: map[string]string{backup.TestCloneLabel: strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)}}
	real := &dockercli.Container{ID: "c2", Name: "app", State: "running",
		Labels: map[string]string{"com.docker.compose.project": "blog"}}

	if !isTestClone(clone) {
		t.Error("a marked clone must be recognised")
	}
	if isTestClone(real) {
		t.Error("an ordinary container must never be mistaken for one")
	}
	if isTestClone(nil) || isTestClone(&dockercli.Container{ID: "c3", Name: "bare"}) {
		t.Error("no labels at all is not a test clone")
	}
	// An unreadable expiry means "not a clone I can reason about" here too — the
	// same fail-safe the reaper uses, so the two can never disagree about which
	// containers are DockBack's.
	if isTestClone(&dockercli.Container{ID: "c4", Labels: map[string]string{backup.TestCloneLabel: "tomorrow"}}) {
		t.Error("an unreadable marker must not make a container ours")
	}
}
