package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// SEC-001 to SEC-003: a signed-in session alone must not be able to hand a
// container over. Each gate is tested for what it refuses and for what it leaves
// alone, since a prompt on every ordinary save would be a gate people learn to hate.

const (
	gateNode      = "n1"
	gateContainer = "app"
)

// signedIn builds a request carrying the test session, with body as JSON.
func signedIn(method, path string, body any) *http.Request {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	return r
}

func withPassword() stepUpBody { return stepUpBody{Password: rsPassword} }

func TestAddsOrChangesCommands(t *testing.T) {
	hook := func(pre ...string) string {
		encoded, _ := json.Marshal(backup.SavedHooks{Pre: pre, User: "root"})
		return string(encoded)
	}
	cases := []struct {
		name              string
		current, proposed string
		want              bool
	}{
		{"a first command", "", hook("touch /x"), true},
		{"a different command", hook("touch /x"), hook("touch /y"), true},
		{"the same commands saved again", hook("touch /x"), hook("touch /x"), false},
		{"an empty form over nothing stored", "", `{"pre":[],"post":[],"user":""}`, false},
		{"clearing commands only takes power away", hook("touch /x"), `{"pre":[],"post":[],"user":""}`, false},
		{"a different run-as user", hook("touch /x"), `{"pre":["touch /x"],"user":"0"}`, true},
	}
	for _, c := range cases {
		if got := addsOrChangesCommands(c.current, c.proposed); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSavingCommandsNeedsThePassword(t *testing.T) {
	s := stepUpRestoreServer(t)
	key := backup.HooksSettingKey(gateNode, gateContainer)
	proposed := `{"pre":["id > /data/proof"],"user":"0"}`

	refused := httptest.NewRecorder()
	if s.freshProofForCommands(refused, signedIn("PUT", "/", nil), stepUpBody{}, key, proposed) {
		t.Fatal("a new command must not be saved on the session alone")
	}
	if refused.Code != http.StatusUnauthorized || !strings.Contains(refused.Body.String(), "step_up_required") {
		t.Fatalf("the refusal must ask for the password: %d %s", refused.Code, refused.Body.String())
	}

	if !s.freshProofForCommands(httptest.NewRecorder(), signedIn("PUT", "/", nil), withPassword(), key, proposed) {
		t.Fatal("the password must let the save through")
	}
}

func TestSavingUnchangedCommandsAsksForNothing(t *testing.T) {
	s := stepUpRestoreServer(t)
	key := backup.HooksSettingKey(gateNode, gateContainer)
	stored := `{"pre":["occ maintenance:mode --on"],"post":["occ maintenance:mode --off"],"user":"www-data"}`
	if err := s.store.SetSetting(key, stored); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	// A request with no session at all: the gate must not even look for one.
	if !s.freshProofForCommands(rec, httptest.NewRequest("PUT", "/", nil), stepUpBody{}, key, stored) {
		t.Fatalf("saving the same hooks again must not prompt: %d %s", rec.Code, rec.Body.String())
	}
}

func postSettings(s *Server, patch map[string]string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.handleSetSettings(rec, signedIn("POST", "/api/settings", patch))
	return rec
}

func TestChangingTheHelperImageNeedsThePassword(t *testing.T) {
	s := stepUpRestoreServer(t)
	original := dockercli.SidecarImage()
	t.Cleanup(func() { dockercli.SetSidecarImage(original) })
	substituted := "registry.example.test/helper:1"

	if rec := postSettings(s, map[string]string{helperImageSettingKey: substituted}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a different helper image on the session alone: %d, want 401", rec.Code)
	}
	if dockercli.SidecarImage() != original {
		t.Fatal("a refused change must not be applied")
	}

	// The settings form sends the field with every save. Unchanged, it must not prompt.
	if rec := postSettings(s, map[string]string{helperImageSettingKey: original, "scrub.per_cycle": "5"}); rec.Code != http.StatusOK {
		t.Fatalf("an ordinary save carrying the current image: %d %s", rec.Code, rec.Body.String())
	}

	withProof := map[string]string{helperImageSettingKey: substituted, stepUpPasswordKey: rsPassword}
	if rec := postSettings(s, withProof); rec.Code != http.StatusOK {
		t.Fatalf("the change with the password: %d %s", rec.Code, rec.Body.String())
	}
	if dockercli.SidecarImage() != substituted {
		t.Fatal("the change with the password must be applied")
	}
}

func postRepin(s *Server, body any) *httptest.ResponseRecorder {
	r := signedIn("POST", "/api/nodes/"+gateNode+"/sidecar/repin", body)
	r.SetPathValue("id", gateNode)
	rec := httptest.NewRecorder()
	s.handleRepinSidecar(rec, r)
	return rec
}

func TestRepinningTheHelperImageNeedsThePassword(t *testing.T) {
	s := stepUpRestoreServer(t)
	if err := s.store.UpsertNode(&store.Node{ID: gateNode, Name: "home", Transport: "ssh", Address: "x"}); err != nil {
		t.Fatal(err)
	}
	const pinned = "alpine:3.20|sha256:aaaa"
	if err := s.store.SetSidecarPin(gateNode, pinned); err != nil {
		t.Fatal(err)
	}

	if rec := postRepin(s, map[string]string{}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a re-pin on the session alone: %d, want 401", rec.Code)
	}
	if pin, _ := s.store.GetSidecarPin(gateNode); pin != pinned {
		t.Fatalf("a refused re-pin must leave the pin in place, got %q", pin)
	}

	if rec := postRepin(s, withPassword()); rec.Code != http.StatusOK {
		t.Fatalf("a re-pin with the password: %d %s", rec.Code, rec.Body.String())
	}
	if pin, _ := s.store.GetSidecarPin(gateNode); pin != "" {
		t.Fatalf("the pin must be cleared, got %q", pin)
	}
}

func TestUnprotectingRestoresNeedsThePassword(t *testing.T) {
	s := stepUpRestoreServer(t)
	if err := s.setRestoreStepUp(gateNode, gateContainer, true, true); err != nil {
		t.Fatal(err)
	}
	allowed := func(credentials stepUpBody, protectedAfter bool) (bool, int) {
		rec := httptest.NewRecorder()
		ok := s.freshProofToUnprotectRestores(rec, signedIn("PUT", "/", nil), credentials, gateNode, gateContainer, protectedAfter)
		return ok, rec.Code
	}

	if ok, code := allowed(stepUpBody{}, false); ok || code != http.StatusUnauthorized {
		t.Fatalf("switching protection off on the session alone: allowed=%v code=%d", ok, code)
	}
	if ok, _ := allowed(withPassword(), false); !ok {
		t.Fatal("switching protection off with the password must go through")
	}
	if ok, _ := allowed(stepUpBody{}, true); !ok {
		t.Fatal("a change that keeps the container protected must not prompt")
	}

	// An unprotected container has nothing to take away, whatever the change.
	if err := s.setRestoreStepUp(gateNode, gateContainer, false, true); err != nil {
		t.Fatal(err)
	}
	if ok, _ := allowed(stepUpBody{}, false); !ok {
		t.Fatal("an unprotected container must not prompt")
	}
}

// Protection that follows from a mark can be taken off by removing the mark, so
// the outcome is worked out from the marks a change would leave.
func TestRestoreProtectionFollowsTheMarksAChangeLeaves(t *testing.T) {
	s := stepUpRestoreServer(t)
	if s.restoreProtectedWith(gateNode, gateContainer, false, false) {
		t.Error("no mark and no choice: unprotected")
	}
	if !s.restoreProtectedWith(gateNode, gateContainer, true, false) || !s.restoreProtectedWith(gateNode, gateContainer, false, true) {
		t.Error("either mark alone protects")
	}

	if err := s.setRestoreStepUp(gateNode, gateContainer, true, true); err != nil {
		t.Fatal(err)
	}
	if !s.restoreProtectedWith(gateNode, gateContainer, false, false) {
		t.Error("an explicit choice to protect outlives both marks")
	}
	if err := s.setRestoreStepUp(gateNode, gateContainer, false, true); err != nil {
		t.Fatal(err)
	}
	if s.restoreProtectedWith(gateNode, gateContainer, true, true) {
		t.Error("an explicit choice not to protect is honoured")
	}

	markCritical(t, s, gateNode, gateContainer)
	if !s.isCriticalDB(gateNode, gateContainer) || s.isCriticalDB(gateNode, "other") {
		t.Error("isCriticalDB must follow the critical list")
	}
}
