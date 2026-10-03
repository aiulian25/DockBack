package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dockback/internal/backup"
	"dockback/internal/config"
	"dockback/internal/crypto"
	"dockback/internal/storage"
	"dockback/internal/store"
)

// F204 — the recovery-key drill, end to end through the HTTP surface.
//
// Two things are being locked in here beyond "does it work": that the drill is
// step-up gated like every other operation involving key material, and that the
// pasted private key never reaches anywhere durable — not the audit trail, not
// the settings table, not a log line. An offline recovery key that leaks into
// the audit trail has undone write-only mode entirely, since that trail is
// readable by any admin and is exported inside app backups.

const rkPassword = "correct-horse-battery"

func rkServer(t *testing.T) (*Server, storage.Backend) {
	t.Helper()
	st := testStore(t)
	be, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		store:  st,
		cfg:    &config.Config{},
		guard:  newLoginGuard(st),
		engine: &backup.Engine{Store: st, Storage: be, Log: func(string, string, string) {}},
	}
	hash, err := crypto.HashPassword(rkPassword)
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
	return s, be
}

// rkWriteOnlyBackup stores a real write-only archive and its backup row,
// returning the private key that opens it.
func rkWriteOnlyBackup(t *testing.T, s *Server, be storage.Backend, id string) string {
	t.Helper()
	pub, priv, err := crypto.NewBackupKeypair()
	if err != nil {
		t.Fatal(err)
	}
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		t.Fatal(err)
	}
	wrapped, err := crypto.WrapKeyPub(dek, pub)
	if err != nil {
		t.Fatal(err)
	}
	key := "n1/app/" + id + ".dback"
	var ct bytes.Buffer
	if _, err := crypto.Encrypt(&ct, strings.NewReader("archive payload"), dek); err != nil {
		t.Fatal(err)
	}
	if _, err := be.Put(context.Background(), key, bytes.NewReader(ct.Bytes())); err != nil {
		t.Fatal(err)
	}
	man, _ := json.Marshal(backup.Manifest{BackupID: id, TargetName: "app",
		WrappedKeyPub: wrapped, BackupPubFP: crypto.BackupPubFP(pub)})
	b := &store.Backup{ID: id, NodeID: "n1", TargetName: "app", Status: "success",
		CreatedAt: time.Now().Unix(), ManifestJSON: string(man), StorageKey: key,
		LocationsJSON: `[{"kind":"local","name":"local","type":"local"}]`}
	if err := s.store.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
	return priv
}

func rkVerify(s *Server, id, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/backups/"+id+"/verify-key", strings.NewReader(body))
	r.SetPathValue("id", id)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	rec := httptest.NewRecorder()
	s.handleVerifyRecoveryKey(rec, r)
	return rec
}

func rkBody(priv string) string {
	b, _ := json.Marshal(map[string]string{"private_key": priv, "password": rkPassword})
	return string(b)
}

func rkDecode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON (%d): %s", rec.Code, rec.Body.String())
	}
	return out
}

// AC1 — the right key returns ok:true and the keypair fingerprint.
func TestVerifyKeyEndpointPasses(t *testing.T) {
	s, be := rkServer(t)
	priv := rkWriteOnlyBackup(t, s, be, "b1")

	rec := rkVerify(s, "b1", rkBody(priv))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	out := rkDecode(t, rec)
	if out["ok"] != true {
		t.Fatalf("the correct key must pass: %s", rec.Body.String())
	}
	fp, _ := out["fingerprint"].(string)
	if len(fp) != 16 {
		t.Errorf("fingerprint = %q, want 16 hex characters", fp)
	}
	if !auditHas(t, s, "recovery_key.verified") {
		t.Error("the drill must be audited")
	}
}

// AC2 — a wrong key is a VERDICT (200 + ok:false), not a 500, and carries a
// reason the operator can act on.
func TestVerifyKeyEndpointReportsAWrongKeyWithout500(t *testing.T) {
	s, be := rkServer(t)
	_ = rkWriteOnlyBackup(t, s, be, "b1")
	_, other, err := crypto.NewBackupKeypair()
	if err != nil {
		t.Fatal(err)
	}

	rec := rkVerify(s, "b1", rkBody(other))
	if rec.Code != http.StatusOK {
		t.Fatalf("a failed drill is a result, not a server error: got %d", rec.Code)
	}
	out := rkDecode(t, rec)
	if out["ok"] != false {
		t.Fatalf("a wrong key must not pass: %s", rec.Body.String())
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "keypair") {
		t.Errorf("the reason should be actionable, got %q", msg)
	}

	// Corrupt input is likewise a refusal, never a panic.
	for _, junk := range []string{"not-base64!!", "c2hvcnQ="} {
		if rec := rkVerify(s, "b1", rkBody(junk)); rec.Code >= 500 {
			t.Errorf("junk key %q produced %d", junk, rec.Code)
		}
	}
}

// AC3 — a non-write-only backup gets a distinct answer, not a false pass.
func TestVerifyKeyEndpointOnANonWriteOnlyBackup(t *testing.T) {
	s, _ := rkServer(t)
	man, _ := json.Marshal(backup.Manifest{BackupID: "b2", TargetName: "app", WrappedKey: "sym"})
	b := &store.Backup{ID: "b2", NodeID: "n1", TargetName: "app", Status: "success",
		CreatedAt: time.Now().Unix(), ManifestJSON: string(man), StorageKey: "n1/app/b2.dback"}
	if err := s.store.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	_ = s.store.UpdateBackup(b)
	_, priv, _ := crypto.NewBackupKeypair()

	rec := rkVerify(s, "b2", rkBody(priv))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "not write-only") {
		t.Errorf("the answer must say the backup has no keypair, got: %s", body)
	}
	if strings.Contains(body, `"ok":true`) {
		t.Error("this must never read as a pass")
	}
}

// The drill is gated behind fresh proof of the password, like every other
// operation in this app that handles key material.
func TestVerifyKeyEndpointRequiresStepUp(t *testing.T) {
	s, be := rkServer(t)
	priv := rkWriteOnlyBackup(t, s, be, "b1")

	body, _ := json.Marshal(map[string]string{"private_key": priv}) // no password
	rec := rkVerify(s, "b1", string(body))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
	if rkDecode(t, rec)["step_up_required"] != true {
		t.Errorf("the client must be told to prompt: %s", rec.Body.String())
	}

	// A backup that does not exist is refused BEFORE the password prompt —
	// re-authenticating for a nonexistent backup wastes the operator's time.
	if rec := rkVerify(s, "nope", rkBody(priv)); rec.Code != http.StatusNotFound {
		t.Errorf("unknown backup = %d, want 404", rec.Code)
	}
}

// The whole point: the pasted key must not survive the request anywhere.
func TestVerifyKeyNeverPersistsThePrivateKey(t *testing.T) {
	s, be := rkServer(t)
	priv := rkWriteOnlyBackup(t, s, be, "b1")

	// One pass and one failure, so both audit branches are exercised.
	if rec := rkVerify(s, "b1", rkBody(priv)); rec.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body.String())
	}
	_, other, _ := crypto.NewBackupKeypair()
	_ = rkVerify(s, "b1", rkBody(other))

	entries, err := s.store.ListAudit(1000)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, e := range entries {
		if e.Action != "recovery_key.verified" {
			continue
		}
		seen++
		blob := e.Action + " " + e.Target + " " + e.Detail + " " + e.Actor
		for _, secret := range []string{priv, other, rkPassword} {
			if strings.Contains(blob, secret) {
				t.Fatalf("a secret reached the audit trail: %q", blob)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("expected both drills audited, got %d", seen)
	}

	// Nor anywhere in the settings table, which is where this app persists
	// everything else it keeps.
	keys, err := s.store.SettingKeysWithPrefix("")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		v, _ := s.store.GetSetting(k, "")
		if strings.Contains(v, priv) || strings.Contains(k, priv) {
			t.Fatalf("the private key was persisted under setting %q", k)
		}
	}

	// And the response itself never echoes it back.
	rec := rkVerify(s, "b1", rkBody(priv))
	if strings.Contains(rec.Body.String(), priv) {
		t.Error("the response must not echo the private key")
	}
}

// Identifying a key in hand: which recovery sheet is this? Public material only.
func TestIdentifyKeyReportsTheFingerprint(t *testing.T) {
	s, _ := rkServer(t)
	pub, priv, err := crypto.NewBackupKeypair()
	if err != nil {
		t.Fatal(err)
	}

	call := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/security/write-only/identify-key", strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
		rec := httptest.NewRecorder()
		s.handleWriteOnlyKeyFingerprint(rec, r)
		return rec
	}

	rec := call(rkBody(priv))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	if got, _ := rkDecode(t, rec)["fingerprint"].(string); got != crypto.BackupPubFP(pub) {
		t.Errorf("fingerprint = %q, want %q", got, crypto.BackupPubFP(pub))
	}
	if strings.Contains(rec.Body.String(), priv) {
		t.Error("the response must not echo the private key")
	}

	// Still gated — checked on a session with no grant, since the call above
	// deliberately leaves a five-minute step-up window behind it.
	fresh, _ := rkServer(t)
	body, _ := json.Marshal(map[string]string{"private_key": priv})
	r := httptest.NewRequest("POST", "/api/security/write-only/identify-key", strings.NewReader(string(body)))
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	gated := httptest.NewRecorder()
	fresh.handleWriteOnlyKeyFingerprint(gated, r)
	if gated.Code != http.StatusUnauthorized {
		t.Errorf("without step-up = %d, want 401", gated.Code)
	}
}
