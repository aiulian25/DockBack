package backup

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"dockback/internal/crypto"
	"dockback/internal/storage"
	"dockback/internal/store"
)

// F78: per-destination sealed manifest sidecars. A seal_manifests:true
// destination stores ONLY the .enc sidecar; false stores readable+signed;
// unset falls back to the global manifest.encrypt setting. Adopt reconstructs
// rows from the sealed form.

// sealDest creates an enabled destination row whose config is sealed under key,
// pointing a "local" backend at dir. sealFlag "" omits the setting entirely.
func sealDest(t *testing.T, st *store.Store, key []byte, id, dir, sealFlag string) {
	t.Helper()
	cfg := map[string]string{"path": dir}
	if sealFlag != "" {
		cfg["seal_manifests"] = sealFlag
	}
	js, _ := json.Marshal(cfg)
	enc, err := crypto.SealString(string(js), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDestination(&store.Destination{ID: id, Name: id, Type: "local", Enabled: true, ConfigEnc: enc}); err != nil {
		t.Fatal(err)
	}
}

// listKeys returns every object key on a local backend rooted at dir.
func listKeys(t *testing.T, dir string) map[string]bool {
	t.Helper()
	be, err := storage.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := be.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, k := range keys {
		out[k] = true
	}
	return out
}

func TestSealManifestPerDestination(t *testing.T) {
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

	// The local archive the mirror reads from.
	const archiveKey = "node1/web/2026-01-01_00-00-00_abc.dback"
	if _, err := be.Put(ctx, archiveKey, strings.NewReader("ciphertext-bytes")); err != nil {
		t.Fatal(err)
	}
	man := Manifest{BackupID: "bk-seal", NodeID: "node1", TargetName: "nginx",
		CreatedAt: "2026-01-01T00:00:00Z", CipherSHA256: "deadbeef", CipherSize: 16}
	manBytes, _ := json.Marshal(man)

	sealedDir, plainDir, fallbackDir := t.TempDir(), t.TempDir(), t.TempDir()
	sealDest(t, st, key, "d-sealed", sealedDir, "true")
	sealDest(t, st, key, "d-plain", plainDir, "false")
	sealDest(t, st, key, "d-fallback", fallbackDir, "") // unset → global (false by default)

	locs := e.mirror(ctx, "run1", archiveKey, manBytes, nil, false, true)
	if len(locs) != 3 {
		t.Fatalf("expected 3 mirrored locations, got %d: %+v", len(locs), locs)
	}
	for _, l := range locs {
		if l.Status == "failed" {
			t.Fatalf("mirror to %s failed: %s", l.Name, l.Detail)
		}
	}

	// Sealed destination: ONLY the .enc sidecar, no readable manifest or sig.
	keys := listKeys(t, sealedDir)
	if !keys[archiveKey+".manifest.json.enc"] {
		t.Fatal("sealed destination must hold the .manifest.json.enc sidecar")
	}
	if keys[archiveKey+".manifest.json"] || keys[archiveKey+".manifest.json.sig"] {
		t.Fatal("sealed destination must NOT hold a readable manifest or signature")
	}

	// Plain destination: readable + signed, no .enc.
	keys = listKeys(t, plainDir)
	if !keys[archiveKey+".manifest.json"] || !keys[archiveKey+".manifest.json.sig"] {
		t.Fatal("plain destination must hold the readable manifest + signature")
	}
	if keys[archiveKey+".manifest.json.enc"] {
		t.Fatal("plain destination must not hold a sealed sidecar")
	}

	// Unset destination follows the global setting (false here → readable).
	keys = listKeys(t, fallbackDir)
	if !keys[archiveKey+".manifest.json"] || keys[archiveKey+".manifest.json.enc"] {
		t.Fatal("unset destination must fall back to the global (readable) form")
	}

	// …and flipping the global flips the fallback destination only.
	_ = st.SetSetting("manifest.encrypt", "true")
	if !e.sealForDest(map[string]string{}) {
		t.Fatal("unset destination must follow global manifest.encrypt=true")
	}
	if e.sealForDest(map[string]string{"seal_manifests": "false"}) {
		t.Fatal("an explicit false must beat the global true")
	}

	// Adopt from the SEALED destination reconstructs the catalog row: the .enc
	// sidecar decrypts with the master key (metadata privacy, not data loss).
	sealedBE, err := storage.NewLocal(sealedDir)
	if err != nil {
		t.Fatal(err)
	}
	adopted, skipped, err := e.AdoptFromBackend(ctx, sealedBE, "sealed offsite", "d-sealed", "local")
	if err != nil {
		t.Fatal(err)
	}
	if adopted != 1 || len(skipped) != 0 {
		t.Fatalf("adopt from sealed destination: adopted=%d skipped=%v, want 1/0", adopted, skipped)
	}
	rows, _ := st.ListBackups("", 100)
	found := false
	for _, b := range rows {
		if b.TargetName == "nginx" && b.Status == "success" {
			found = true
		}
	}
	if !found {
		t.Fatal("adopted row from the sealed manifest not found in the catalog")
	}
}
