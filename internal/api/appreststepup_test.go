package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dockback/internal/appbackup"
	"dockback/internal/backup"
	"dockback/internal/config"
	"dockback/internal/crypto"
	"dockback/internal/store"
)

// F199 — downloading the control-plane archive already demanded the password.
// RESTORING it did not, and restoring is the destructive half: it replaces every
// node credential, the whole catalog and the admin account, then restarts the
// app. A hijacked session could do all of that silently.

func appRestoreServer(t *testing.T) *Server {
	t.Helper()
	st := testStore(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 5)
	}
	dir := t.TempDir()
	s := &Server{
		store: st, guard: newLoginGuard(st),
		cfg:    &config.Config{EncryptionKey: key, DataDir: dir, TmpDir: t.TempDir(), BackupsDir: dir},
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

func withSession(r *http.Request) *http.Request {
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	return r
}

func assertStepUpRefusal(t *testing.T, rec *httptest.ResponseRecorder, what string) {
	t.Helper()
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("%s: code = %d, want 401: %s", what, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["step_up_required"] != true {
		t.Errorf("%s: the client must be told to prompt, not signed out: %s", what, rec.Body.String())
	}
}

// AC1 — restoring a stored backup without credentials is refused, and nothing is
// staged.
func TestAppBackupRestoreLocalNeedsStepUp(t *testing.T) {
	s := appRestoreServer(t)
	body := `{"file":"dockback-config-20260101-000000.dback"}`
	rec := httptest.NewRecorder()
	s.handleAppBackupRestoreLocal(rec, withSession(httptest.NewRequest("POST", "/api/app-backup/restore-local", strings.NewReader(body))))
	assertStepUpRefusal(t, rec, "restore-local")

	// With the password it gets PAST the gate. The archive does not exist, so it
	// then fails for that reason — which is the proof it went further.
	rec = httptest.NewRecorder()
	withPw := `{"file":"dockback-config-20260101-000000.dback","password":"` + rsPassword + `"}`
	s.handleAppBackupRestoreLocal(rec, withSession(httptest.NewRequest("POST", "/api/app-backup/restore-local", strings.NewReader(withPw))))
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("the correct password must satisfy the gate: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "step_up_required") {
		t.Errorf("no prompt should remain: %s", rec.Body.String())
	}
}

// A malformed request must be refused before the prompt: asking someone to
// re-authenticate for a request that was never going to work wastes their time.
func TestAppBackupRestoreLocalRejectsBadNameBeforePrompting(t *testing.T) {
	s := appRestoreServer(t)
	rec := httptest.NewRecorder()
	s.handleAppBackupRestoreLocal(rec, withSession(httptest.NewRequest("POST", "/api/app-backup/restore-local", strings.NewReader(`{"file":"../../etc/passwd"}`))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "step_up_required") {
		t.Error("an invalid name must not produce a password prompt")
	}
}

// AC2 — the same for a restore pulled from an external destination.
func TestAppExternalRestoreNeedsStepUp(t *testing.T) {
	s := appRestoreServer(t)
	r := withSession(httptest.NewRequest("POST", "/api/app-backup/destinations/d1/restore",
		strings.NewReader(`{"file":"dockback-config-20260101-000000.dback"}`)))
	r.SetPathValue("id", "d1")
	rec := httptest.NewRecorder()
	s.handleAppExternalRestore(rec, r)
	assertStepUpRefusal(t, rec, "external restore")

	// With the password it proceeds to look the destination up (and fails there).
	r2 := withSession(httptest.NewRequest("POST", "/api/app-backup/destinations/d1/restore",
		strings.NewReader(`{"file":"dockback-config-20260101-000000.dback","password":"`+rsPassword+`"}`)))
	r2.SetPathValue("id", "d1")
	rec2 := httptest.NewRecorder()
	s.handleAppExternalRestore(rec2, r2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("past the gate it should fail on the missing destination, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

// AC3 — the uploaded-archive path, which is the one an attacker would reach for:
// it accepts an archive of their choosing. The gate must fire before the archive
// is opened, decrypted or staged.
func TestAppBackupRestoreUploadNeedsStepUp(t *testing.T) {
	s := appRestoreServer(t)

	// A REAL archive, so a failure to gate would actually stage a restore.
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := s.store.SnapshotTo(snap); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	man := appbackup.Manifest{Format: appbackup.Format, Version: appbackup.Version, CreatedAt: time.Now().Unix(), KeyFingerprint: s.engine.MasterKeyFP()}
	if err := appbackup.Create(&archive, snap, s.cfg.EncryptionKey, man); err != nil {
		t.Fatal(err)
	}

	upload := func(creds map[string]string) *httptest.ResponseRecorder {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		for k, v := range creds {
			_ = mw.WriteField(k, v)
		}
		part, err := mw.CreateFormFile("file", "dockback-config-20260101-000000.dback")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(archive.Bytes()); err != nil {
			t.Fatal(err)
		}
		mw.Close()
		r := withSession(httptest.NewRequest("POST", "/api/app-backup/restore", &body))
		r.Header.Set("Content-Type", mw.FormDataContentType())
		rec := httptest.NewRecorder()
		s.handleAppBackupRestore(rec, r)
		return rec
	}

	assertStepUpRefusal(t, upload(nil), "upload restore")
	// Nothing may have been staged by the refused attempt.
	if entries, _ := os.ReadDir(s.cfg.DataDir); len(entries) > 0 {
		for _, e := range entries {
			if strings.Contains(e.Name(), "pending") || strings.HasSuffix(e.Name(), ".staged") {
				t.Errorf("a refused restore staged %q", e.Name())
			}
		}
	}
	// A wrong password is refused too, and counted as a failed attempt.
	assertStepUpRefusal(t, upload(map[string]string{"password": "not-the-password"}), "upload restore, wrong password")

	// With the right password it goes through.
	if rec := upload(map[string]string{"password": rsPassword}); rec.Code != http.StatusOK {
		t.Fatalf("the correct password must let the restore proceed, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := s.store.GetSetting("nonexistent", ""); err != nil {
		t.Logf("store still usable after staging: %v", err)
	}
	_ = store.Backup{}
}
