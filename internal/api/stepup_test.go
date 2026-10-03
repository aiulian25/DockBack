package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"dockback/internal/backup"
	"dockback/internal/crypto"
	"dockback/internal/store"
)

// F64: step-up re-authentication ("sudo mode"). Key-material operations demand
// the account password inside the request; a success is cached per session for
// stepUpWindow; failures share the login lockout.

const stepUpTestPass = "correct-horse-battery"

// stepUpServer builds a server with one user ("admin"), one live session
// ("sess1"), and a minimal engine so key-reveal can respond.
func stepUpServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	hash, err := crypto.HashPassword(stepUpTestPass)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := s.store.CreateUser("admin", hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.CreateSession("sess1", uid, time.Hour); err != nil {
		t.Fatal(err)
	}
	s.engine = &backup.Engine{Key: make([]byte, 32), KeyFP: "test-fp"}
	return s
}

func stepUpPost(s *Server, handler http.HandlerFunc, body string) (*httptest.ResponseRecorder, map[string]any) {
	r := httptest.NewRequest("POST", "/api/security/key-reveal", strings.NewReader(body))
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "sess1"})
	rec := httptest.NewRecorder()
	handler(rec, r)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestStepUpRevealFlow(t *testing.T) {
	s := stepUpServer(t)

	// 1) No credentials: 401 with the step_up_required marker (NOT a redirect-to-
	//    login "unauthenticated" — the session itself is valid).
	rec, out := stepUpPost(s, s.handleKeyReveal, `{}`)
	if rec.Code != 401 || out["step_up_required"] != true {
		t.Fatalf("no creds: code=%d body=%v, want 401 + step_up_required", rec.Code, out)
	}
	if _, leaked := out["key_hex"]; leaked {
		t.Fatal("key material must never leak on a refused step-up")
	}

	// 2) Wrong password: still 401, the failure counts toward the login lockout,
	//    and an audit row is written.
	rec, out = stepUpPost(s, s.handleKeyReveal, `{"password":"wrong"}`)
	if rec.Code != 401 || out["step_up_required"] != true {
		t.Fatalf("wrong password: code=%d body=%v", rec.Code, out)
	}
	if fails := s.guard.load(lockKey("user", "admin")).Fails; fails != 1 {
		t.Fatalf("failed step-up must hit the login lockout counter, fails=%d want 1", fails)
	}
	found := false
	if rows, _ := s.store.ListAudit(50); rows != nil {
		for _, a := range rows {
			if a.Action == "security.stepup.failed" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("expected a security.stepup.failed audit row")
	}

	// 3) Correct password: the key comes back and a grant is written.
	rec, out = stepUpPost(s, s.handleKeyReveal, `{"password":"`+stepUpTestPass+`"}`)
	if rec.Code != 200 || out["key_hex"] == "" {
		t.Fatalf("correct password: code=%d body=%v", rec.Code, out)
	}

	// 4) Within the grant window a second call needs no password (multi-step flow
	//    prompts once).
	rec, _ = stepUpPost(s, s.handleKeyReveal, `{}`)
	if rec.Code != 200 {
		t.Fatalf("fresh grant must skip the prompt: code=%d", rec.Code)
	}

	// 5) An EXPIRED grant prompts again.
	old := time.Now().Add(-stepUpWindow - time.Minute).Unix()
	_ = s.store.SetSetting(stepUpKey(store.SessionKey("sess1")), strconv.FormatInt(old, 10))
	rec, out = stepUpPost(s, s.handleKeyReveal, `{}`)
	if rec.Code != 401 || out["step_up_required"] != true {
		t.Fatalf("expired grant: code=%d body=%v, want 401", rec.Code, out)
	}
}

func TestStepUpLockout(t *testing.T) {
	s := stepUpServer(t)
	// Hammer wrong passwords until the account lock trips: the step-up endpoint
	// must shed with 429 exactly like login.
	for i := 0; i < userLockPolicy.max+1; i++ {
		rec, _ := stepUpPost(s, s.handleKeyReveal, `{"password":"wrong"}`)
		if rec.Code == 429 {
			return // locked — success
		}
	}
	rec, _ := stepUpPost(s, s.handleKeyReveal, `{"password":"`+stepUpTestPass+`"}`)
	if rec.Code != 429 {
		t.Fatalf("after repeated failures even the CORRECT password must be locked out: code=%d", rec.Code)
	}
}

// TestStepUpGuardsAllDoors: keyfile and token-mint enforce the same gate, and a
// single grant covers all of them (one prompt per flow, not per handler).
func TestStepUpGuardsAllDoors(t *testing.T) {
	s := stepUpServer(t)

	rec, out := stepUpPost(s, s.handleKeyKeyfile, `{"passphrase":"long-enough-passphrase"}`)
	if rec.Code != 401 || out["step_up_required"] != true {
		t.Fatalf("keyfile without step-up: code=%d body=%v", rec.Code, out)
	}
	rec, out = stepUpPost(s, s.handleCreateToken, `{"name":"ci","scopes":["read"]}`)
	if rec.Code != 401 || out["step_up_required"] != true {
		t.Fatalf("token mint without step-up: code=%d body=%v", rec.Code, out)
	}

	// One successful step-up (via keyfile) grants the whole flow.
	rec, _ = stepUpPost(s, s.handleKeyKeyfile, `{"passphrase":"long-enough-passphrase","password":"`+stepUpTestPass+`"}`)
	if rec.Code != 200 {
		t.Fatalf("keyfile with password: code=%d", rec.Code)
	}
	rec, out = stepUpPost(s, s.handleCreateToken, `{"name":"ci","scopes":["read"]}`)
	if rec.Code != 200 || out["token"] == "" {
		t.Fatalf("token mint under a fresh grant: code=%d body=%v", rec.Code, out)
	}
}

// TestStepUpGrantClearedOnLogout: logging out deletes the grant with the session.
func TestStepUpGrantCleared(t *testing.T) {
	s := stepUpServer(t)
	rec, _ := stepUpPost(s, s.handleKeyReveal, `{"password":"`+stepUpTestPass+`"}`)
	if rec.Code != 200 {
		t.Fatalf("step-up: code=%d", rec.Code)
	}
	if v, _ := s.store.GetSetting(stepUpKey(store.SessionKey("sess1")), ""); v == "" {
		t.Fatal("grant should exist after a successful step-up")
	}
	r := httptest.NewRequest("POST", "/api/logout", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "sess1"})
	s.handleLogout(httptest.NewRecorder(), r)
	if v, _ := s.store.GetSetting(stepUpKey(store.SessionKey("sess1")), ""); v != "" {
		t.Fatal("logout must delete the step-up grant")
	}
}
