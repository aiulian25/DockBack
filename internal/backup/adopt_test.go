package backup

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"dockback/internal/storage"
	"dockback/internal/store"
)

// writeOrphan puts a `.dback` archive + its (signed, readable) manifest sidecar on
// be under archiveKey, signed with signKey — WITHOUT creating a catalog row.
func writeOrphan(t *testing.T, e *Engine, be storage.Backend, archiveKey string, man Manifest, signKey []byte) {
	t.Helper()
	ctx := context.Background()
	if _, err := be.Put(ctx, archiveKey, strings.NewReader("ciphertext-bytes-here")); err != nil {
		t.Fatal(err)
	}
	// A real manifest always records WHICH master key wrapped its DEK (packEncryptStore
	// stamps it). The fixture must too, or the F102 skip report has nothing to read.
	if man.KeyFingerprint == "" {
		man.KeyFingerprint = KeyFingerprint(signKey)
	}
	mb, _ := json.Marshal(man)
	// Sign under signKey (may differ from the engine key, to exercise the third-key case).
	signer := &Engine{Store: e.Store, Storage: be, Key: signKey, Log: e.Log}
	if err := signer.writeManifestSidecar(ctx, be, archiveKey, mb); err != nil {
		t.Fatal(err)
	}
}

func TestAdoptFromBackend(t *testing.T) {
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

	// One adoptable orphan, signed under the CURRENT key.
	writeOrphan(t, e, be, "node1/web/2026-01-01_00-00-00_abc.dback", Manifest{
		BackupID: "bk-adopt", NodeID: "node1", Stack: "web", TargetName: "nginx",
		CreatedAt: "2026-01-01T00:00:00Z", CipherSHA256: "deadbeef", CipherSize: 1234,
	}, key)

	// One archive whose manifest is signed with a DIFFERENT key (F16 third-key) — must be skipped.
	other := make([]byte, 32)
	_, _ = rand.Read(other)
	writeOrphan(t, e, be, "node1/db/2026-01-02_00-00-00_xyz.dback", Manifest{
		BackupID: "bk-otherkey", NodeID: "node1", TargetName: "postgres",
		CreatedAt: "2026-01-02T00:00:00Z",
	}, other)

	// First run: exactly one adopted (ours), one skipped (third-key).
	adopted, skipped, err := e.AdoptFromBackend(ctx, be, "S3 offsite", "dest-1", "s3")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if adopted != 1 || len(skipped) != 1 {
		t.Fatalf("first run adopted=%d skipped=%v, want 1/1", adopted, skipped)
	}
	// F102: "1 skipped" is unactionable. The report must name the object, say it
	// belongs to a different master key, and identify WHICH key — that is what
	// tells an operator to re-import an old key rather than shrug.
	if got := skipped[0]; got.Reason != SkipForeignKey {
		t.Fatalf("skip reason = %q, want %q", got.Reason, SkipForeignKey)
	}
	if !strings.Contains(skipped[0].Key, "2026-01-02_00-00-00_xyz.dback") {
		t.Fatalf("the skip must name the object: %q", skipped[0].Key)
	}
	if skipped[0].KeyFingerprint == "" {
		t.Fatal("a readable foreign manifest must report its key fingerprint")
	}
	if skipped[0].KeyFingerprint == KeyFingerprint(key) {
		t.Fatal("the reported fingerprint must be the FOREIGN key's, not ours")
	}

	// The row exists with the manifest's target/size/location.
	b, err := st.GetBackup("bk-adopt")
	if err != nil {
		t.Fatalf("adopted row not found: %v", err)
	}
	if b.TargetName != "nginx" || b.Status != "success" || b.Verified != "unverified" {
		t.Errorf("row = %+v, want nginx/success/unverified", b)
	}
	if b.StorageKey != "node1/web/2026-01-01_00-00-00_abc.dback" {
		t.Errorf("storage key = %q", b.StorageKey)
	}
	if b.SizeBytes != int64(len("ciphertext-bytes-here")) {
		t.Errorf("size = %d, want the stat'd archive size", b.SizeBytes)
	}
	var locs []Location
	_ = json.Unmarshal([]byte(b.LocationsJSON), &locs)
	if len(locs) != 1 || locs[0].DestID != "dest-1" || locs[0].Kind != "dest" || locs[0].Name != "S3 offsite" {
		t.Errorf("locations = %v, want one dest:dest-1", locs)
	}
	// The third-key archive was NOT registered (never a corrupt/partial row).
	if _, err := st.GetBackup("bk-otherkey"); err == nil {
		t.Error("a manifest signed with a different key must NOT be adopted")
	}

	// Second run is idempotent: nothing new, the ours-one now counts as skipped.
	adopted2, skipped2, err := e.AdoptFromBackend(ctx, be, "S3 offsite", "dest-1", "s3")
	if err != nil {
		t.Fatal(err)
	}
	if adopted2 != 0 || len(skipped2) != 2 {
		t.Fatalf("second run adopted=%d skipped=%v, want 0/2 (already-catalogued + third-key)", adopted2, skipped2)
	}
	reasons := map[string]string{}
	for _, sk := range skipped2 {
		reasons[sk.Reason] = sk.Key
	}
	if _, ok := reasons[SkipCatalogued]; !ok {
		t.Fatalf("the re-scan must report the already-catalogued archive distinctly: %+v", skipped2)
	}
	if _, ok := reasons[SkipForeignKey]; !ok {
		t.Fatalf("the foreign-key archive must still be reported: %+v", skipped2)
	}
}

// A foreign manifest may belong to ANOTHER operator's instance. Its container
// names, stacks and volume paths are not ours to surface — only the key
// fingerprint, which is what makes the skip actionable.
func TestAdoptSkipReportRevealsOnlyTheFingerprint(t *testing.T) {
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

	other := make([]byte, 32)
	_, _ = rand.Read(other)
	writeOrphan(t, e, be, "n/secret/2026-05-05_00-00-00_zzz.dback", Manifest{
		BackupID: "bk-theirs", NodeID: "their-node", Stack: "their-secret-stack",
		TargetName: "their-confidential-app", CreatedAt: "2026-05-05T00:00:00Z",
	}, other)

	_, skipped, err := e.AdoptFromBackend(context.Background(), be, "shared bucket", "d1", "s3")
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 {
		t.Fatalf("want one skip, got %+v", skipped)
	}
	js, _ := json.Marshal(skipped[0])
	for _, leak := range []string{"their-confidential-app", "their-secret-stack", "their-node", "bk-theirs"} {
		if strings.Contains(string(js), leak) {
			t.Fatalf("a foreign manifest's %q must not be surfaced: %s", leak, js)
		}
	}
	if skipped[0].KeyFingerprint == "" {
		t.Fatalf("the fingerprint IS the actionable part and must be present: %s", js)
	}
}

// A sealed sidecar written under another key is opaque ciphertext — nothing can
// be read from it. Reporting no fingerprint is the honest answer, not a gap.
func TestAdoptSkipSealedForeignManifestHasNoFingerprint(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	be, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetSetting("manifest.encrypt", "true")
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	e := &Engine{Store: st, Storage: be, Key: key, Log: func(string, string, string) {}}

	other := make([]byte, 32)
	_, _ = rand.Read(other)
	writeOrphan(t, e, be, "n/app/2026-06-06_00-00-00_sealed.dback", Manifest{
		BackupID: "bk-sealed-theirs", NodeID: "n", TargetName: "app", CreatedAt: "2026-06-06T00:00:00Z",
	}, other)

	_, skipped, err := e.AdoptFromBackend(context.Background(), be, "B2", "d1", "s3")
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 {
		t.Fatalf("want one skip, got %+v", skipped)
	}
	if skipped[0].KeyFingerprint != "" {
		t.Fatalf("a sealed foreign manifest reveals nothing — fingerprint must be empty, got %q", skipped[0].KeyFingerprint)
	}
	if skipped[0].Reason != SkipUnreadable {
		t.Fatalf("opaque ciphertext is unreadable, not identifiably foreign: %q", skipped[0].Reason)
	}
}

// The classifier is what separates "another instance's backup" (actionable) from
// "not a manifest at all". Pure, so every shape is testable directly.
func TestAdoptSkipClassification(t *testing.T) {
	manifest := []byte(`{"version":1,"backup_id":"abc","key_fingerprint":"aabbccdd","target_name":"app"}`)
	if got := adoptSkipReason(manifest); got != SkipForeignKey {
		t.Fatalf("a parseable manifest is a foreign-key skip, got %q", got)
	}
	if got := foreignKeyFingerprint(manifest); got != "aabbccdd" {
		t.Fatalf("fingerprint = %q", got)
	}
	for _, raw := range [][]byte{
		nil, {}, []byte("not json"), []byte(`{"hello":"world"}`), {0x00, 0x01, 0x02},
	} {
		if got := adoptSkipReason(raw); got != SkipUnreadable {
			t.Fatalf("adoptSkipReason(%q) = %q, want unreadable", raw, got)
		}
		if got := foreignKeyFingerprint(raw); got != "" {
			t.Fatalf("foreignKeyFingerprint(%q) = %q, want empty", raw, got)
		}
	}
	// A fingerprint field carrying something that is not a fingerprint must not be
	// quoted back into the UI.
	for _, bad := range []string{
		`{"backup_id":"a","key_fingerprint":"` + strings.Repeat("x", 100) + `"}`,
		`{"backup_id":"a","key_fingerprint":"aa
bb"}`,
	} {
		if got := foreignKeyFingerprint([]byte(bad)); got != "" {
			t.Fatalf("a malformed fingerprint must be dropped, got %q", got)
		}
	}
}

// TestAdoptFromBackendEncrypted covers the sealed-manifest (.enc) path.
func TestAdoptFromBackendEncrypted(t *testing.T) {
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
	_ = st.SetSetting("manifest.encrypt", "true") // engine writes sealed sidecars
	e := &Engine{Store: st, Storage: be, Key: key, Log: func(string, string, string) {}}
	ctx := context.Background()

	writeOrphan(t, e, be, "n/app/2026-03-03_00-00-00_enc.dback", Manifest{
		BackupID: "bk-enc", NodeID: "n", TargetName: "app", CreatedAt: "2026-03-03T00:00:00Z",
	}, key)

	adopted, skipped, err := e.AdoptFromBackend(ctx, be, "B2", "dest-b2", "s3")
	if err != nil {
		t.Fatal(err)
	}
	if adopted != 1 || len(skipped) != 0 {
		t.Fatalf("sealed adopt adopted=%d skipped=%v, want 1/0", adopted, skipped)
	}
	if b, err := st.GetBackup("bk-enc"); err != nil || b.TargetName != "app" {
		t.Fatalf("sealed-manifest backup not adopted: %v / %+v", err, b)
	}
}
