package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"dockback/internal/backup"
	"dockback/internal/store"
)

// F218 — a backup whose LAST VERIFICATION FAILED does not restore by accident.
//
// The drawer disabled its Restore button for anything not verified and the
// server enforced nothing, so the rule lived in one client and the API let the
// same request through. That asymmetry is now resolved in the direction that
// costs nothing: only a RECORDED FAILURE is refused — an unverified backup still
// restores exactly as it always has, because that is what an adopted backup is
// and refusing it would break a real recovery for a rule about paperwork.

// mkVerified is mkRestorable with the verification verdict set, which is the
// only field these tests turn on.
func mkVerified(t *testing.T, s *Server, id, node, target, verified string) {
	t.Helper()
	mkRestorable(t, s, id, node, target)
	b, err := s.store.GetBackup(id)
	if err != nil {
		t.Fatal(err)
	}
	b.Verified = verified
	if err := s.store.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
}

// AC3 — restoring a verification-failed backup is a 409 that says why, and
// nothing has been started.
func TestRestoreOfVerificationFailedBackupIsRefused(t *testing.T) {
	s := stepUpRestoreServer(t)
	mkVerified(t, s, "b1", "n1", "prod-db", "failed")

	rec := postRestore(s, "b1", map[string]any{
		"node_id": "n1", "target_id": "cid", "volumes": true, "database": true, "confirm": true,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["verify_failed"] != true {
		t.Errorf("the client needs the typed flag to offer the override: %s", rec.Body.String())
	}
	// A 409 that reads as "already in progress" would send the operator away to
	// wait for a restore that is not running. It has to name the real reason.
	if msg, _ := out["error"].(string); !strings.Contains(strings.ToLower(msg), "verification") {
		t.Errorf("the refusal must say what is wrong: %q", msg)
	}
	// Refused BEFORE the exclusive lock — a gate that left the stack locked would
	// turn a declined restore into a stuck container.
	if !s.locks.acquireRestore(stackKey("n1", "", "prod-db")) {
		t.Error("the restore lock must not be held after a refusal")
	}
	s.locks.releaseRestore(stackKey("n1", "", "prod-db"))
}

// AC3 — the same request with the acknowledgement proceeds. It fails later at an
// unreachable Docker node, so the only thing asserted is that it was not refused
// for this reason.
func TestRestoreOfVerificationFailedBackupProceedsWhenConfirmed(t *testing.T) {
	s := stepUpRestoreServer(t)
	mkVerified(t, s, "b1", "n1", "prod-db", "failed")

	rec := postRestore(s, "b1", map[string]any{
		"node_id": "n1", "target_id": "cid", "volumes": true, "database": true, "confirm": true,
		"confirm_unverified": true,
	})
	if strings.Contains(rec.Body.String(), "verify_failed") {
		t.Fatalf("the acknowledgement must be honored: %d %s", rec.Code, rec.Body.String())
	}
	// And the override is on the record — this is the line that answers "why is
	// this container full of corrupt data".
	rows, err := s.store.ListAudit(50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range rows {
		if strings.HasPrefix(a.Action, "restore.") && strings.Contains(a.Detail, "override confirmed") {
			found = true
		}
	}
	if !found {
		t.Error("overriding a known-bad restore must be audited")
	}
}

// The regression that matters most: an UNVERIFIED backup is untouched by this
// gate. An adopted backup is unverified by construction, and a disaster recovery
// that refuses the only copy on the shelf is worse than one that warns.
func TestUnverifiedBackupsRestoreUnchanged(t *testing.T) {
	for _, v := range []string{"", "unverified", "verified"} {
		s := stepUpRestoreServer(t)
		mkVerified(t, s, "b1", "n1", "plain-app", v)

		rec := postRestore(s, "b1", map[string]any{
			"node_id": "n1", "target_id": "cid", "volumes": true, "confirm": true,
		})
		if strings.Contains(rec.Body.String(), "verify_failed") {
			t.Errorf("verified=%q must not be gated: %d %s", v, rec.Code, rec.Body.String())
		}
	}
}

// A standalone VOLUME backup rots exactly like a container's, and its archive is
// pure data — nothing is recreated from an image. The gate is checked before the
// volume dispatch so this path cannot walk around it.
func TestVolumeRestoreOfVerificationFailedBackupIsRefused(t *testing.T) {
	s := stepUpRestoreServer(t)
	man, _ := json.Marshal(backup.Manifest{BackupID: "v1", TargetName: "volume:photos"})
	b := &store.Backup{ID: "v1", NodeID: "n1", TargetName: "volume:photos", Status: "success",
		Verified: "failed", CreatedAt: time.Now().Unix(), ManifestJSON: string(man), StorageKey: "k/v1"}
	if err := s.store.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpsertNode(&store.Node{ID: "n1", Name: "n1", Transport: "socket", Address: "unix:///var/run/docker.sock"}); err != nil {
		t.Fatal(err)
	}

	rec := postRestore(s, "v1", map[string]any{"node_id": "n1", "volumes": true, "confirm": true})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "verify_failed") {
		t.Fatalf("a volume backup that failed verification must be refused too: %d %s", rec.Code, rec.Body.String())
	}
	// Refused before the volume's own lock, for the same reason as above.
	if !s.locks.acquireRestore(stackKey("n1", "", "volume:photos")) {
		t.Error("the volume restore lock must not be held after a refusal")
	}
	s.locks.releaseRestore(stackKey("n1", "", "volume:photos"))
}

// The gate is not a substitute for confirm. An unconfirmed request is still
// refused first, so a client cannot reach the override by omitting the basics.
func TestVerifyFailedGateDoesNotBypassConfirm(t *testing.T) {
	s := stepUpRestoreServer(t)
	mkVerified(t, s, "b1", "n1", "prod-db", "failed")

	rec := postRestore(s, "b1", map[string]any{
		"node_id": "n1", "target_id": "cid", "volumes": true, "confirm_unverified": true,
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("confirm=true is still required: %d %s", rec.Code, rec.Body.String())
	}
}
