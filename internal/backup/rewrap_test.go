package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"path/filepath"
	"testing"

	"dockback/internal/crypto"
	"dockback/internal/storage"
	"dockback/internal/store"
)

// F16: RewrapAll re-wraps a backup's DEK from the old master key to the new one —
// leaving the DEK (and archive) unchanged — updates the manifest-of-record's
// wrapped_key + fingerprint, and SKIPS (never corrupts) a backup wrapped by an
// unrelated key or a legacy direct-key backup with no wrapped DEK.
func TestRewrapAll(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	be, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	oldKey := make([]byte, 32)
	newKey := make([]byte, 32)
	thirdKey := make([]byte, 32)
	_, _ = rand.Read(oldKey)
	_, _ = rand.Read(newKey)
	_, _ = rand.Read(thirdKey)

	e := &Engine{Store: st, Storage: be, Key: oldKey, KeyFP: KeyFingerprint(oldKey), Log: func(string, string, string) {}}

	// Helper: create a backup whose manifest wraps `dek` under `wrapKey`.
	mk := func(id string, dek, wrapKey []byte) {
		wrapped, err := crypto.WrapKey(dek, wrapKey)
		if err != nil {
			t.Fatal(err)
		}
		man := Manifest{BackupID: id, WrappedKey: wrapped, KeyFingerprint: KeyFingerprint(wrapKey)}
		mj, _ := json.Marshal(&man)
		b := &store.Backup{ID: id, NodeID: "n1", TargetName: id, Status: "success", Verified: "verified", StorageKey: id + ".dback", ManifestJSON: string(mj), CreatedAt: 100}
		if err := st.CreateBackup(b); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateBackup(b); err != nil { // persist ManifestJSON + StorageKey
			t.Fatal(err)
		}
	}

	ours, _ := crypto.NewDataKey()   // wrapped by oldKey → should be re-wrapped
	theirs, _ := crypto.NewDataKey() // wrapped by a third key → should be skipped
	mk("ours", ours, oldKey)
	mk("theirs", theirs, thirdKey)

	// A legacy direct-key backup (no wrapped DEK) → skipped, untouched.
	legacy := &store.Backup{ID: "legacy", NodeID: "n1", TargetName: "legacy", Status: "success", StorageKey: "legacy.dback", ManifestJSON: `{"backup_id":"legacy","wrapped_key":"","key_fingerprint":"` + KeyFingerprint(oldKey) + `"}`, CreatedAt: 100}
	if err := st.CreateBackup(legacy); err != nil {
		t.Fatal(err)
	}
	_ = st.UpdateBackup(legacy)

	res := e.RewrapAll(context.Background(), oldKey, newKey)
	if res.Rewrapped != 1 {
		t.Errorf("rewrapped = %d, want 1", res.Rewrapped)
	}
	if res.Skipped != 2 {
		t.Errorf("skipped = %d, want 2 (third-key + legacy)", res.Skipped)
	}
	if res.Failed != 0 {
		t.Errorf("failed = %d, want 0", res.Failed)
	}

	// "ours": manifest now wrapped under newKey, fingerprint updated, DEK intact.
	got, _ := st.GetBackup("ours")
	var m Manifest
	if err := json.Unmarshal([]byte(got.ManifestJSON), &m); err != nil {
		t.Fatal(err)
	}
	if m.KeyFingerprint != KeyFingerprint(newKey) {
		t.Errorf("fingerprint not updated to the new key: %s", m.KeyFingerprint)
	}
	dek, err := crypto.UnwrapKey(m.WrappedKey, newKey)
	if err != nil {
		t.Fatalf("re-wrapped DEK must unwrap with the new key: %v", err)
	}
	if !bytes.Equal(dek, ours) {
		t.Fatal("DEK changed during rotation — the archive would be unrecoverable")
	}
	if _, err := crypto.UnwrapKey(m.WrappedKey, oldKey); err == nil {
		t.Fatal("re-wrapped DEK must no longer unwrap with the old key")
	}

	// "theirs": untouched — still wrapped by the third key, fingerprint unchanged.
	th, _ := st.GetBackup("theirs")
	var mt Manifest
	_ = json.Unmarshal([]byte(th.ManifestJSON), &mt)
	if mt.KeyFingerprint != KeyFingerprint(thirdKey) {
		t.Error("a third-key backup must be left unchanged")
	}
	if _, err := crypto.UnwrapKey(mt.WrappedKey, thirdKey); err != nil {
		t.Fatal("a skipped backup must remain recoverable with its own key")
	}
}
