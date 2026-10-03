package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dockback/internal/store"
)

// F208 — the operator's safety-snapshot choice actually reaches the engine on a
// standalone-volume restore.
//
// It did not. restoreVolumeBackup built its RestoreOptions by hand and listed
// four fields; Snapshot was not one of them. So the drawer's "Snapshot current
// state before restoring — a local backup of what's there now, so a bad restore
// is reversible" was, for a volume backup, a promise the server never kept: the
// volume was overwritten in place with no rollback point either way.

func TestVolumeRestoreForwardsTheSnapshotChoice(t *testing.T) {
	on := volumeRestoreOptions("b1", restoreReq{NodeID: "n1", Snapshot: true, Source: "dest-2"})
	if !on.Snapshot {
		t.Fatal("snapshot=true must reach the engine — this is the whole feature")
	}
	// Everything the path already carried still comes through.
	if on.BackupID != "b1" || on.NodeID != "n1" || on.Source != "dest-2" || !on.Volumes {
		t.Errorf("existing fields must be unchanged: %+v", on)
	}

	// And the opt-out is equally load-bearing: the confirm dialog says in so many
	// words that the overwrite is NOT reversible, so it must not quietly snapshot.
	off := volumeRestoreOptions("b1", restoreReq{NodeID: "n1", Snapshot: false})
	if off.Snapshot {
		t.Error("snapshot=false must stay false")
	}
}

// A volume restore never carries an alternate target name. The handler refuses
// the request outright rather than dropping the field, so the engine can treat
// the volume it is about to write as the one and only subject.
func TestVolumeRestoreOptionsCarryNoAlternateName(t *testing.T) {
	got := volumeRestoreOptions("b1", restoreReq{NodeID: "n1", AsName: "photos-copy"})
	if got.AsName != "" {
		t.Errorf("AsName must not be forwarded from this path, got %q", got.AsName)
	}
}

// "Restore as a copy" on a volume backup is refused at the door, and the refusal
// explains what would otherwise have happened.
//
// It used to be ACCEPTED and then ignored: the new name was dropped and the
// original volume was overwritten, while the dialog that sent the request had
// just promised the original would be left untouched. The refusal has to land
// before the lock and before anything else is touched, so the handler is driven
// with a bare Server — if it reaches the store or the registry, this panics and
// the test says so.
func TestVolumeRestoreAsCopyIsRefusedBeforeAnythingIsTouched(t *testing.T) {
	s := &Server{}
	b := &store.Backup{ID: "b1", TargetName: "volume:photos"}
	r := httptest.NewRequest("POST", "/api/backups/b1/restore", strings.NewReader("{}"))
	rec := httptest.NewRecorder()

	s.restoreVolumeBackup(rec, r, b, restoreReq{NodeID: "n1", AsName: "photos-copy", Confirm: true})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"standalone volume", "cannot be restored as a copy", "overwriting"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal should say %q, got: %s", want, body)
		}
	}
	// Whitespace is not a name — it must be refused the same way rather than
	// slipping through as "no alternate name given".
	rec2 := httptest.NewRecorder()
	s.restoreVolumeBackup(rec2, httptest.NewRequest("POST", "/x", strings.NewReader("{}")), b,
		restoreReq{NodeID: "n1", AsName: "   ", Confirm: true})
	if rec2.Code != http.StatusBadRequest {
		t.Errorf("a whitespace-only name = %d, want 400", rec2.Code)
	}
}
