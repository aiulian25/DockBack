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
	"dockback/internal/config"
	"dockback/internal/crypto"
)

// The keyfile passphrase wraps the MASTER KEY, which opens every backup ever
// taken, and the file it protects is by design kept beside the deployment. It
// was held to eight characters while the login password was held to twelve.
func keyfileServer(t *testing.T) *Server {
	t.Helper()
	st := testStore(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 2)
	}
	s := &Server{
		store: st, guard: newLoginGuard(st),
		cfg:    &config.Config{EncryptionKey: key, MinPasswordLen: 12},
		engine: &backup.Engine{Store: st, Key: key, KeyFP: backup.KeyFingerprint(key), Log: func(string, string, string) {}},
	}
	hash, err := crypto.HashPassword(rsPassword)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := st.CreateUser("admin", hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession("tok", uid, time.Hour); err != nil {
		t.Fatal(err)
	}
	return s
}

func postKeyfile(s *Server, passphrase string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"passphrase": passphrase, "password": rsPassword})
	r := httptest.NewRequest("POST", "/api/security/key-keyfile", strings.NewReader(string(body)))
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	rec := httptest.NewRecorder()
	s.handleKeyKeyfile(rec, r)
	return rec
}

func TestKeyfilePassphraseIsHeldToTheAccountMinimum(t *testing.T) {
	s := keyfileServer(t)
	want := s.minPasswordLength()
	if want < 12 {
		t.Fatalf("the account minimum is %d; this test assumes the shipped floor of 12", want)
	}

	// The old limit: eight characters, which this must now refuse.
	rec := postKeyfile(s, "8charsxx")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an 8-character passphrase = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), strconv.Itoa(want)) {
		t.Errorf("the refusal must state the real minimum (%d): %s", want, rec.Body.String())
	}

	// One character short is still short.
	if rec := postKeyfile(s, strings.Repeat("a", want-1)); rec.Code != http.StatusBadRequest {
		t.Errorf("%d characters = %d, want 400", want-1, rec.Code)
	}
	// At the minimum it is accepted and a keyfile comes back.
	rec = postKeyfile(s, strings.Repeat("a", want))
	if rec.Code != http.StatusOK {
		t.Fatalf("a passphrase at the minimum = %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["keyfile"] == "" {
		t.Error("a valid request must return the keyfile")
	}
}

// The form is told the number rather than hard-coding one that can disagree.
func TestKeyStatusReportsThePassphraseMinimum(t *testing.T) {
	s := keyfileServer(t)
	r := httptest.NewRequest("GET", "/api/security/key-status", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	rec := httptest.NewRecorder()
	s.handleKeyStatus(rec, r)

	var out struct {
		MinPassphraseLen int `json:"min_passphrase_len"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.MinPassphraseLen != s.minPasswordLength() {
		t.Errorf("status reports %d, the handler enforces %d — the form would state the wrong number",
			out.MinPassphraseLen, s.minPasswordLength())
	}

	// A raised policy is reflected, so the two can never drift.
	if err := s.store.SetSetting("security.min_password_len", "20"); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	s.handleKeyStatus(rec, httptest.NewRequest("GET", "/api/security/key-status", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.MinPassphraseLen != 20 {
		t.Errorf("a raised minimum must be reported, got %d", out.MinPassphraseLen)
	}
	if rec := postKeyfile(s, strings.Repeat("a", 19)); rec.Code != http.StatusBadRequest {
		t.Errorf("and enforced: 19 characters under a 20 minimum = %d", rec.Code)
	}
}
