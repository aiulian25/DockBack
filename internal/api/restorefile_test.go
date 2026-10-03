package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// Single-file restore (F96) writes into a LIVE container, so the gate that
// matters is the one that keeps it from racing anything else touching the same
// stack — and the audit row that records what was attempted.

func newRestoreFileServer(t *testing.T) *Server {
	t.Helper()
	st := testStore(t)
	// A real engine over an EMPTY node registry: every guard in the handler runs
	// for real, and the write itself stops at "node not registered" instead of
	// needing a live daemon.
	s := &Server{
		store: st, locks: newOpLocks(),
		engine: &backup.Engine{Store: st, Reg: dockercli.NewRegistry(), Log: func(string, string, string) {}},
		bcast:  newBroadcaster(16),
	}
	if err := st.UpsertNode(&store.Node{ID: "n1", Name: "razer", Transport: "local", Address: "local"}); err != nil {
		t.Fatal(err)
	}
	b := &store.Backup{ID: "b1", NodeID: "n1", Stack: "paperless", TargetName: "paperless-web", Status: "success"}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	return s
}

func restoreFileReqFor(body string) *http.Request {
	return httptest.NewRequest("POST", "/api/backups/b1/restore-file", strings.NewReader(body))
}

// A write-back must never land mid-backup (it would be captured half-written) or
// mid-restore (it would be overwritten). It takes the SAME stack lock a full
// restore does, so a busy stack is refused with the established message.
func TestRestoreFileRefusedWhileStackIsBusy(t *testing.T) {
	s := newRestoreFileServer(t)
	// Someone is already backing up a service of this stack.
	if !s.locks.acquireBackup(stackKey("n1", "paperless", ""), containerKey("n1", "paperless-db")) {
		t.Fatal("precondition: the backup lock should be free")
	}

	r := restoreFileReqFor(`{"path":"data/config/app.conf","target_id":"c1","node_id":"n1"}`)
	r.SetPathValue("id", "b1")
	w := httptest.NewRecorder()
	s.handleRestoreFile(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("want 409 while the stack is busy, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "already in progress") {
		t.Fatalf("the 409 must reuse the established message: %s", w.Body.String())
	}
	// Refused early: nothing was attempted, so nothing may be audited.
	rows, _ := s.store.ListAudit(10)
	for _, row := range rows {
		if row.Action == "restore.file" {
			t.Fatal("a refused request must not write an audit row")
		}
	}
	// And the lock is left exactly as it was — a refusal must not steal it.
	s.locks.releaseBackup(stackKey("n1", "paperless", ""), containerKey("n1", "paperless-db"))
	if !s.locks.acquireRestore(stackKey("n1", "paperless", "")) {
		t.Fatal("the refused request must not have held the restore lock")
	}
	s.locks.releaseRestore(stackKey("n1", "paperless", ""))
}

// The audit row names the PATH. A write that fails half way still has to leave a
// trace of what was attempted, so it is written before the write, not after it.
func TestRestoreFileAuditsThePath(t *testing.T) {
	s := newRestoreFileServer(t)

	r := restoreFileReqFor(`{"path":"data/config/app.conf","target_id":"c1","node_id":"n1","keep_backup":true}`)
	r.SetPathValue("id", "b1")
	s.handleRestoreFile(httptest.NewRecorder(), r) // fails at the daemon; the audit still stands

	rows, err := s.store.ListAudit(10)
	if err != nil {
		t.Fatal(err)
	}
	var found *store.AuditEntry
	for _, row := range rows {
		if row.Action == "restore.file" {
			found = row
		}
	}
	if found == nil {
		t.Fatal("a restore.file audit row must be written")
	}
	if !strings.Contains(found.Detail, "data/config/app.conf") {
		t.Fatalf("the audit row must name the path: %q", found.Detail)
	}
	if found.Target != "b1" {
		t.Fatalf("the audit row must point at the backup: %q", found.Target)
	}
}

// Bad input is rejected before any lock is taken or any row is written.
func TestRestoreFileValidatesInput(t *testing.T) {
	s := newRestoreFileServer(t)
	for _, c := range []struct {
		name, body string
		want       int
	}{
		{"no path", `{"target_id":"c1","node_id":"n1"}`, http.StatusBadRequest},
		{"blank path", `{"path":"   ","target_id":"c1","node_id":"n1"}`, http.StatusBadRequest},
		{"no target", `{"path":"a/b","node_id":"n1"}`, http.StatusBadRequest},
		{"unknown node", `{"path":"a/b","target_id":"c1","node_id":"nope"}`, http.StatusNotFound},
		{"malformed", `not json`, http.StatusBadRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := restoreFileReqFor(c.body)
			r.SetPathValue("id", "b1")
			w := httptest.NewRecorder()
			s.handleRestoreFile(w, r)
			if w.Code != c.want {
				t.Fatalf("want %d, got %d (%s)", c.want, w.Code, w.Body.String())
			}
		})
	}
	// The stack lock must be free afterwards — a rejection cannot leak it.
	if !s.locks.acquireRestore(stackKey("n1", "paperless", "")) {
		t.Fatal("a rejected request leaked the restore lock")
	}
	s.locks.releaseRestore(stackKey("n1", "paperless", ""))
}

// A failed or still-running backup may hold a half-captured file. Overwriting
// live data with that is worse than the deletion being recovered from.
func TestRestoreFileRefusesIncompleteBackup(t *testing.T) {
	s := newRestoreFileServer(t)
	for _, status := range []string{"running", "failed"} {
		if err := s.store.CreateBackup(&store.Backup{ID: "b-" + status, NodeID: "n1", TargetName: "app", Status: status}); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("POST", "/api/backups/x/restore-file", strings.NewReader(`{"path":"a/b","target_id":"c1","node_id":"n1"}`))
		r.SetPathValue("id", "b-"+status)
		w := httptest.NewRecorder()
		s.handleRestoreFile(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s backup: want 400, got %d (%s)", status, w.Code, w.Body.String())
		}
	}
}

// A standalone volume backup (F23) stores volume-CONTENTS-relative paths, so
// "/" + name would name a plausible-looking but wrong container path.
func TestRestoreFileRefusesVolumeOnlyBackup(t *testing.T) {
	s := newRestoreFileServer(t)
	if err := s.store.CreateBackup(&store.Backup{ID: "bv", NodeID: "n1", TargetName: "volume:photos", Status: "success"}); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/backups/bv/restore-file", strings.NewReader(`{"path":"a/b","target_id":"c1","node_id":"n1"}`))
	r.SetPathValue("id", "bv")
	w := httptest.NewRecorder()
	s.handleRestoreFile(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a volume-only backup, got %d (%s)", w.Code, w.Body.String())
	}
}
