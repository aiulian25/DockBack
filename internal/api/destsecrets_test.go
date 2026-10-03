package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dockback/internal/backup"
	"dockback/internal/config"
	"dockback/internal/store"
)

// The destination-config endpoint is documented as returning no secrets, and the
// edit form works by leaving a credential blank to keep the stored one. The S3
// access key was sent to the browser anyway — into history, into whatever an
// extension can read, and onto the screen of whoever is standing behind the
// operator — for nothing, because the form never needed it back.

func destSecretServer(t *testing.T) *Server {
	t.Helper()
	st := testStore(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 11)
	}
	return &Server{
		store:  st,
		cfg:    &config.Config{EncryptionKey: key},
		engine: &backup.Engine{Store: st, Key: key, KeyFP: backup.KeyFingerprint(key), Log: func(string, string, string) {}},
	}
}

func TestDestinationConfigReturnsNoCredentials(t *testing.T) {
	s := destSecretServer(t)
	const accessKey, secretKey = "AKIAEXAMPLEACCESSKEY", "s3-secret-key-value"
	cfg := map[string]string{
		"endpoint": "s3.example.com", "bucket": "backups", "path": "dockback",
		"access_key": accessKey, "secret_key": secretKey,
	}
	enc, err := s.encryptDestConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := &store.Destination{ID: "d1", Name: "offsite", Type: "s3", Enabled: true, ConfigEnc: enc}
	if err := s.store.CreateDestination(d); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/api/destinations/d1/config", nil)
	r.SetPathValue("id", "d1")
	rec := httptest.NewRecorder()
	s.handleGetDestinationConfig(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for what, secret := range map[string]string{"access key": accessKey, "secret key": secretKey} {
		if strings.Contains(body, secret) {
			t.Errorf("the %s reached the browser: %s", what, body)
		}
	}

	var out struct {
		Config       map[string]string `json:"config"`
		AccessKeySet bool              `json:"access_key_set"`
		SecretKeySet bool              `json:"secret_key_set"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// Non-credential fields are still returned, or the form cannot prefill.
	for k, want := range map[string]string{"endpoint": "s3.example.com", "bucket": "backups", "path": "dockback"} {
		if out.Config[k] != want {
			t.Errorf("config[%q] = %q, want %q — the form needs these", k, out.Config[k], want)
		}
	}
	if !out.AccessKeySet || !out.SecretKeySet {
		t.Error("the form must be told a credential IS stored, so it can say 'leave blank to keep current'")
	}

	// A destination with no access key says so.
	empty, _ := s.encryptDestConfig(map[string]string{"endpoint": "e", "bucket": "b"})
	if err := s.store.CreateDestination(&store.Destination{ID: "d2", Name: "n", Type: "s3", ConfigEnc: empty}); err != nil {
		t.Fatal(err)
	}
	r2 := httptest.NewRequest("GET", "/api/destinations/d2/config", nil)
	r2.SetPathValue("id", "d2")
	rec2 := httptest.NewRecorder()
	s.handleGetDestinationConfig(rec2, r2)
	_ = json.Unmarshal(rec2.Body.Bytes(), &out)
	if out.AccessKeySet {
		t.Error("a destination with no access key must not claim one is stored")
	}
}

// The other half of the contract: a blank credential on edit keeps the stored
// one, or hiding it from the form would silently erase it on every save.
func TestBlankCredentialOnEditKeepsTheStoredOne(t *testing.T) {
	stored := map[string]string{
		"endpoint": "s3.example.com", "bucket": "backups",
		"access_key": "AKIAEXAMPLEACCESSKEY", "secret_key": "s3-secret-key-value",
	}
	// What the browser sends back after editing only the bucket: the credentials
	// come back blank, because the endpoint never gave them to it.
	submitted := map[string]string{
		"endpoint": "s3.example.com", "bucket": "renamed",
		"access_key": "", "secret_key": "",
	}
	merged := mergeDestConfig(stored, submitted)

	if merged["access_key"] != "AKIAEXAMPLEACCESSKEY" {
		t.Errorf("a blank access key must keep the stored one, got %q — every save would break the destination", merged["access_key"])
	}
	if merged["secret_key"] != "s3-secret-key-value" {
		t.Errorf("a blank secret key must keep the stored one, got %q", merged["secret_key"])
	}
	if merged["bucket"] != "renamed" {
		t.Errorf("an edited field must be applied, got %q", merged["bucket"])
	}
	// And a deliberately changed credential still replaces it.
	rotated := mergeDestConfig(stored, map[string]string{"access_key": "AKIANEWKEY"})
	if rotated["access_key"] != "AKIANEWKEY" {
		t.Errorf("rotating a credential must work, got %q", rotated["access_key"])
	}
}
