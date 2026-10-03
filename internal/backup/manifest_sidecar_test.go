package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"dockback/internal/crypto"
	"dockback/internal/storage"
	"dockback/internal/store"
)

func readObj(t *testing.T, be storage.Backend, key string) []byte {
	t.Helper()
	rc, err := be.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return b
}

func objExists(be storage.Backend, key string) bool {
	rc, err := be.Get(context.Background(), key)
	if err != nil {
		return false
	}
	rc.Close()
	return true
}

// TestManifestSidecarModes verifies PLAN §3.5's two sidecar modes: the default
// readable+signed manifest, and the optional encrypted manifest that hides recon.
func TestManifestSidecarModes(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	be, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	e := &Engine{Store: st, Storage: be, Key: key, Log: func(string, string, string) {}}
	ctx := context.Background()
	manBytes := []byte(`{"backup_id":"b1","wrapped_key":"secretwrap","cipher_sha256":"deadbeef"}`)

	// Default: readable manifest + valid HMAC signature, no encrypted copy.
	if err := e.writeManifestSidecar(ctx, be, "k1", manBytes); err != nil {
		t.Fatal(err)
	}
	if got := readObj(t, be, "k1.manifest.json"); !bytes.Equal(got, manBytes) {
		t.Fatal("readable manifest should equal the original bytes")
	}
	sig := strings.TrimSpace(string(readObj(t, be, "k1.manifest.json.sig")))
	if !crypto.VerifyManifest(manBytes, key, sig) {
		t.Fatal("signature should verify the manifest")
	}
	if objExists(be, "k1.manifest.json.enc") {
		t.Fatal("encrypted sidecar must not exist in signed mode")
	}

	// Encrypted mode: only the sealed copy, and it must not leak plaintext recon.
	if err := st.SetSetting("manifest.encrypt", "true"); err != nil {
		t.Fatal(err)
	}
	if err := e.writeManifestSidecar(ctx, be, "k2", manBytes); err != nil {
		t.Fatal(err)
	}
	if objExists(be, "k2.manifest.json") || objExists(be, "k2.manifest.json.sig") {
		t.Fatal("readable/signed sidecars must not be written in encrypted mode")
	}
	enc := readObj(t, be, "k2.manifest.json.enc")
	if bytes.Contains(enc, []byte("wrapped_key")) || bytes.Contains(enc, []byte("cipher_sha256")) || bytes.Contains(enc, []byte("secretwrap")) {
		t.Fatal("encrypted sidecar leaks plaintext metadata")
	}
	dec, derr := crypto.OpenString(enc, key)
	if derr != nil {
		t.Fatalf("encrypted sidecar should decrypt: %v", derr)
	}
	if dec != string(manBytes) {
		t.Fatal("decrypted manifest mismatch")
	}
	// A wrong master key must fail to open (tamper/rotation detection).
	other := make([]byte, 32)
	_, _ = rand.Read(other)
	if _, e2 := crypto.OpenString(enc, other); e2 == nil {
		t.Fatal("encrypted sidecar must not open under a different key")
	}
}
