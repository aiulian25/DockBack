package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
	"dockback/internal/storage"
	"dockback/internal/store"
)

// DBK-029: the delete endpoints refuse what must not be deleted, and say what a
// delete had to leave behind.

const (
	deletionNode    = "n1"
	deletionTarget  = "app"
	missingDestName = "offsite-nas"
)

// mkStoredRow persists a finished backup with a local copy and, when asked, a copy
// on a destination that is not there to answer a delete.
func mkStoredRow(t *testing.T, st *store.Store, id string, withUnreachableCopy bool) {
	t.Helper()
	locations := []backup.Location{{Kind: "local", Name: "local", Type: "local"}}
	if withUnreachableCopy {
		locations = append(locations, backup.Location{Kind: "dest", DestID: "gone", Name: missingDestName, Type: "smb"})
	}
	encoded, _ := json.Marshal(locations)
	manifest, _ := json.Marshal(backup.Manifest{BackupID: id, TargetName: deletionTarget})
	b := &store.Backup{ID: id, NodeID: deletionNode, TargetName: deletionTarget, Status: "success", CreatedAt: 1,
		StorageKey: "k/" + id, LocationsJSON: string(encoded), ManifestJSON: string(manifest)}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
}

type deleteReply struct {
	Status string                `json:"status"`
	Error  string                `json:"error"`
	Kept   []backup.CopyDeletion `json:"kept"`
}

func requestDelete(t *testing.T, s *Server, id, body string) (int, deleteReply) {
	t.Helper()
	r := httptest.NewRequest("DELETE", "/api/backups/"+id, bytes.NewReader([]byte(body)))
	r.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	s.handleDeleteBackup(rec, r)
	var reply deleteReply
	_ = json.Unmarshal(rec.Body.Bytes(), &reply)
	return rec.Code, reply
}

func backupStatus(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	b, err := st.GetBackup(id)
	if errors.Is(err, store.ErrNotFound) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Status
}

func TestDeleteIsRefusedWhileTheBackupIsBeingRead(t *testing.T) {
	s := chainTestServer(t)
	mkStoredRow(t, s.store, "drilled", false)
	mkStoredRow(t, s.store, "restoring", false)

	s.drillsInFlight.Store("drilled", struct{}{})
	if code, reply := requestDelete(t, s, "drilled", ""); code != http.StatusConflict || reply.Error == "" {
		t.Fatalf("delete under a drill: %d %+v, want 409 with a reason", code, reply)
	}
	s.drillsInFlight.Delete("drilled")

	lock := stackKey(deletionNode, "", deletionTarget)
	if !s.locks.acquireRestore(lock) {
		t.Fatal("could not take the restore lock")
	}
	if code, _ := requestDelete(t, s, "restoring", ""); code != http.StatusConflict {
		t.Fatalf("delete under a restore: %d, want 409", code)
	}
	s.locks.releaseRestore(lock)

	for _, id := range []string{"drilled", "restoring"} {
		if backupStatus(t, s.store, id) != "success" {
			t.Fatalf("%s: a refused delete must leave the backup as it was", id)
		}
		if code, reply := requestDelete(t, s, id, ""); code != http.StatusOK || reply.Status != deleteStatusDeleted {
			t.Fatalf("%s: once nothing reads it the delete must go through: %d %+v", id, code, reply)
		}
	}
}

func TestDeleteIsRefusedWhileTheContainerIsOnHold(t *testing.T) {
	s := chainTestServer(t)
	mkStoredRow(t, s.store, "held", false)
	_ = s.store.SetSetting(backup.RetentionHoldKey(deletionNode, deletionTarget), "mass-change|1")

	if code, reply := requestDelete(t, s, "held", ""); code != http.StatusConflict || reply.Error == "" {
		t.Fatalf("delete under a hold: %d %+v, want 409 with a reason", code, reply)
	}
	if backupStatus(t, s.store, "held") != "success" {
		t.Fatal("a held backup must stay exactly as it was")
	}
}

func TestIncompleteDeleteStaysListedUntilForgotten(t *testing.T) {
	s := chainTestServer(t)
	mkStoredRow(t, s.store, "partial", true)
	mkStoredRow(t, s.store, "whole", false)

	code, reply := requestDelete(t, s, "partial", "")
	if code != http.StatusOK || reply.Status != deleteStatusIncomplete {
		t.Fatalf("delete with an unreachable copy: %d %+v, want 200 incomplete", code, reply)
	}
	if len(reply.Kept) != 1 || reply.Kept[0].Location != missingDestName || reply.Kept[0].Detail == "" {
		t.Fatalf("the reply must name the copy that is left and why: %+v", reply.Kept)
	}
	if backupStatus(t, s.store, "partial") != store.BackupStatusDeleting {
		t.Fatal("the backup must stay listed as being deleted")
	}

	// Asking again is a retry, with the same answer while nothing has changed.
	if code, reply := requestDelete(t, s, "partial", ""); code != http.StatusOK || reply.Status != deleteStatusIncomplete {
		t.Fatalf("retry: %d %+v", code, reply)
	}

	// Only a delete that could not finish can be forgotten.
	if code, _ := requestDelete(t, s, "whole", `{"forget":true}`); code != http.StatusBadRequest {
		t.Fatalf("forgetting an ordinary backup: %d, want 400", code)
	}
	if backupStatus(t, s.store, "whole") != "success" {
		t.Fatal("a refused forget must leave the backup alone")
	}

	if code, reply := requestDelete(t, s, "partial", `{"forget":true}`); code != http.StatusOK || reply.Status != deleteStatusForgotten {
		t.Fatalf("forget: %d %+v", code, reply)
	}
	if backupStatus(t, s.store, "partial") != "" {
		t.Fatal("a forgotten backup must leave the catalog")
	}
	entries, err := s.store.ListAudit(10)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]string{}
	for _, entry := range entries {
		actions[entry.Action] = entry.Detail
	}
	for _, action := range []string{auditBackupDeleteIncomplete, auditBackupDeleteForget} {
		if !bytes.Contains([]byte(actions[action]), []byte(missingDestName)) {
			t.Fatalf("audit %q must name the copy left behind, got %q", action, actions[action])
		}
	}
}

func TestBulkDeleteReportsWhatCouldNotBeFinished(t *testing.T) {
	s := chainTestServer(t)
	mkStoredRow(t, s.store, "bulk-whole", false)
	mkStoredRow(t, s.store, "bulk-partial", true)

	body, _ := json.Marshal(map[string]any{"ids": []string{"bulk-whole", "bulk-partial"}})
	rec := httptest.NewRecorder()
	s.handleBulkDeleteBackup(rec, httptest.NewRequest("POST", "/api/backups/delete", bytes.NewReader(body)))
	var result struct {
		Deleted    int      `json:"deleted"`
		Incomplete []string `json:"incomplete"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 1 || len(result.Incomplete) != 1 || result.Incomplete[0] != "bulk-partial" {
		t.Fatalf("result = %+v, want one deleted and bulk-partial incomplete", result)
	}
}

func purgeTestServer(t *testing.T) *Server {
	t.Helper()
	st := testStore(t)
	blobs, err := storage.NewLocal(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		store:    st,
		locks:    newOpLocks(),
		reg:      dockercli.NewRegistry(),
		engine:   &backup.Engine{Store: st, Storage: blobs, Log: func(string, string, string) {}},
		machines: newMachineCache(),
	}
	if err := st.UpsertNode(&store.Node{ID: deletionNode, Name: "gone", Transport: "ssh", Address: "x"}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPurgedNodeKeepsBackupsItCouldNotDeleteListed(t *testing.T) {
	s := purgeTestServer(t)
	mkStoredRow(t, s.store, "purge-whole", false)
	mkStoredRow(t, s.store, "purge-partial", true)

	s.purgeNode(deletionNode)

	if _, err := s.store.GetNode(deletionNode); err == nil {
		t.Fatal("the node itself must be purged")
	}
	if backupStatus(t, s.store, "purge-whole") != "" {
		t.Fatal("a backup with every copy removed must leave the catalog")
	}
	if backupStatus(t, s.store, "purge-partial") != store.BackupStatusDeleting {
		t.Fatal("a backup with a copy still stored must stay listed after its node is gone")
	}
}

func TestNodeWithAHeldContainerIsNeitherRemovedNorPurged(t *testing.T) {
	s := purgeTestServer(t)
	mkStoredRow(t, s.store, "held-on-node", false)
	_ = s.store.SetSetting(backup.RetentionHoldKey(deletionNode, deletionTarget), "mass-change|1")

	r := httptest.NewRequest("DELETE", "/api/nodes/"+deletionNode, nil)
	r.SetPathValue("id", deletionNode)
	rec := httptest.NewRecorder()
	s.handleDeleteNode(rec, r)
	if rec.Code != http.StatusConflict {
		t.Fatalf("removing a server with a held container: %d, want 409", rec.Code)
	}

	s.purgeNode(deletionNode)
	if _, err := s.store.GetNode(deletionNode); err != nil {
		t.Fatal("a node with a held container must not be purged")
	}
	if backupStatus(t, s.store, "held-on-node") != "success" {
		t.Fatal("a held backup must survive a purge attempt untouched")
	}
}
