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
	"dockback/internal/store"
)

// stepUpSession creates a logged-in admin and pre-grants a fresh step-up for its
// session, so the endpoint tests exercise the handler rather than re-testing the
// step-up gate (which auth_test.go already covers). The 401-without-a-session
// case is asserted separately below.
func stepUpSession(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	hash, err := crypto.HashPassword("correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := s.store.CreateUser("admin", hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.CreateSession("tok-wo", uid, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetSetting(stepUpKey(store.SessionKey("tok-wo")), strconv.FormatInt(time.Now().Unix(), 10)); err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: sessionCookie, Value: "tok-wo"}
}

// Arming write-only mode changes what a break-in is worth, so it must never be
// reachable from a merely-hijacked session without re-authentication.
func TestWriteOnlyEnableRequiresStepUp(t *testing.T) {
	s := writeOnlyServer(t)
	r := httptest.NewRequest("POST", "/api/security/write-only/enable", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	s.handleWriteOnlyEnable(w, r)
	if w.Code == http.StatusOK {
		t.Fatal("enabling write-only mode MUST require authentication")
	}
	if v, _ := s.store.GetSetting("backup.write_only_pubkey", ""); v != "" {
		t.Fatal("a refused request must not have armed the mode")
	}
}

// The endpoint contract that matters: the private key is returned EXACTLY ONCE,
// is never persisted, and never reaches the audit trail.

func writeOnlyServer(t *testing.T) *Server {
	t.Helper()
	st := testStore(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return &Server{
		store:  st,
		cfg:    &config.Config{EncryptionKey: key},
		engine: &backup.Engine{Store: st, Key: key},
	}
}

func TestWriteOnlyStatusReturnsPublicMaterialOnly(t *testing.T) {
	s := writeOnlyServer(t)

	r := httptest.NewRequest("GET", "/api/security/write-only", nil)
	w := httptest.NewRecorder()
	s.handleWriteOnlyStatus(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var got map[string]any
	json.Unmarshal(w.Body.Bytes(), &got)
	if got["enabled"] != false {
		t.Fatal("write-only must be off by default")
	}
	// There must be no field that could ever carry a private key.
	if _, bad := got["private_key"]; bad {
		t.Fatal("the status endpoint must NEVER return a private key")
	}
}

// The private key must be issued once and then be unobtainable — including to an
// attacker who has full read access to the database and the audit trail.
func TestWriteOnlyEnableIssuesKeyOnceAndNeverStoresIt(t *testing.T) {
	s := writeOnlyServer(t)

	ck := stepUpSession(t, s)
	r := httptest.NewRequest("POST", "/api/security/write-only/enable", strings.NewReader("{}"))
	r.AddCookie(ck)
	w := httptest.NewRecorder()
	s.handleWriteOnlyEnable(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		PublicKey   string `json:"public_key"`
		PrivateKey  string `json:"private_key"`
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.PrivateKey == "" || got.PublicKey == "" {
		t.Fatal("enable must return both halves once")
	}
	if got.Fingerprint != crypto.BackupPubFP(got.PublicKey) {
		t.Fatal("the fingerprint must match the issued public key")
	}
	// The keypair must actually work.
	dek, _ := crypto.NewDataKey()
	wrapped, err := crypto.WrapKeyPub(dek, got.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crypto.UnwrapKeyPriv(wrapped, got.PrivateKey); err != nil {
		t.Fatalf("the issued keypair must round-trip: %v", err)
	}

	// ONLY the public key is persisted.
	stored, _ := s.store.GetSetting("backup.write_only_pubkey", "")
	if stored != got.PublicKey {
		t.Fatalf("the public key must be stored, got %q", stored)
	}
	// Nothing anywhere in settings may hold the private key.
	for _, k := range []string{"backup.write_only_pubkey", "backup.write_only_privkey", "backup.write_only_key"} {
		if v, _ := s.store.GetSetting(k, ""); v != "" && v == got.PrivateKey {
			t.Fatalf("setting %q leaked the PRIVATE key", k)
		}
	}
	// And the audit trail must not carry it either.
	rows, err := s.store.ListAudit(200)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range rows {
		if strings.Contains(a.Target, got.PrivateKey) || strings.Contains(a.Detail, got.PrivateKey) {
			t.Fatal("the audit trail leaked the PRIVATE key")
		}
		if a.Action == "writeonly.enable" {
			found = true
		}
	}
	if !found {
		t.Fatal("arming write-only mode must be audited")
	}

	// Re-enabling must be refused rather than silently superseding the keypair.
	r2 := httptest.NewRequest("POST", "/api/security/write-only/enable", strings.NewReader("{}"))
	r2.AddCookie(ck)
	w2 := httptest.NewRecorder()
	s.handleWriteOnlyEnable(w2, r2)
	if w2.Code != http.StatusConflict {
		t.Fatalf("a second enable must 409, got %d", w2.Code)
	}
}

// Disabling changes only what NEW backups get; it must say how many existing
// backups still depend on the offline key.
func TestWriteOnlyDisableReportsRemainingBackups(t *testing.T) {
	s := writeOnlyServer(t)
	pub, _, _ := crypto.NewBackupKeypair()
	if err := s.store.SetSetting("backup.write_only_pubkey", pub); err != nil {
		t.Fatal(err)
	}
	dek, _ := crypto.NewDataKey()
	wrapped, _ := crypto.WrapKeyPub(dek, pub)
	man, _ := json.Marshal(&backup.Manifest{WrappedKeyPub: wrapped, BackupPubFP: crypto.BackupPubFP(pub)})
	b := &store.Backup{ID: "b1", NodeID: "n1", TargetName: "app", Status: "success", ManifestJSON: string(man)}
	if err := s.store.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("POST", "/api/security/write-only/disable", strings.NewReader("{}"))
	r.AddCookie(stepUpSession(t, s))
	w := httptest.NewRecorder()
	s.handleWriteOnlyDisable(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		StillWriteOnly int `json:"still_write_only"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.StillWriteOnly != 1 {
		t.Fatalf("still_write_only = %d, want 1", got.StillWriteOnly)
	}
	if v, _ := s.store.GetSetting("backup.write_only_pubkey", ""); v != "" {
		t.Fatal("disabling must clear the public key")
	}
	// The stored backup is NOT converted — it stays sealed to the offline key.
	after, _ := s.store.GetBackup("b1")
	if !backup.IsWriteOnly(manifestOf(after)) {
		t.Fatal("an existing write-only backup must stay write-only after disabling")
	}
}
