package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Cancelable restores: the registry hands out a context that a cancel actually
// cancels, reports operator-cancel distinctly from a deadline, refuses a
// duplicate run under the same id, and cleans up on finish.

func TestRestoreCancelRegistry(t *testing.T) {
	st := testStore(t)
	s := &Server{store: st}

	ctx, finish, ok := s.beginRestoreRun(context.Background(), "b1", "nextcloud", "n1", time.Hour)
	if !ok || ctx == nil {
		t.Fatal("first run must register")
	}
	if ctx.Err() != nil {
		t.Fatal("a fresh run's context must be live")
	}
	if got := s.runningRestores(); len(got) != 1 || got[0].ID != "b1" {
		t.Fatalf("runningRestores = %v, want [b1]", runIDs(got))
	}

	// A second run under the same id is refused (the locks normally prevent
	// this; the registry is the last-resort guard) and must NOT kill the first.
	if _, _, dup := s.beginRestoreRun(context.Background(), "b1", "nextcloud", "n1", time.Hour); dup {
		t.Fatal("a duplicate run id must be refused")
	}
	if ctx.Err() != nil {
		t.Fatal("refusing a duplicate must not cancel the live run")
	}

	// Cancel: the context ends, and the run is flagged as OPERATOR-canceled.
	if s.restoreWasCanceled("b1") {
		t.Fatal("a running restore is not canceled yet")
	}
	label, ok := s.cancelRestoreRun("b1")
	if !ok || label != "nextcloud" {
		t.Fatalf("cancel: ok=%v label=%q", ok, label)
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("cancel must cancel the run's context")
	}
	if !s.restoreWasCanceled("b1") {
		t.Fatal("cancel must mark the run as operator-canceled")
	}

	// finish() deregisters, so a later restore of the same backup can start and
	// a stale cancel is a clean 404.
	finish()
	if got := s.runningRestores(); len(got) != 0 {
		t.Fatalf("finished run must deregister, got %v", runIDs(got))
	}
	if _, ok := s.cancelRestoreRun("b1"); ok {
		t.Fatal("cancelling a finished run must report not-found")
	}
	if _, _, ok := s.beginRestoreRun(context.Background(), "b1", "nextcloud", "n1", time.Hour); !ok {
		t.Fatal("the id must be reusable after finish()")
	}
}

// A run that hits its own deadline is NOT reported as an operator cancel — the
// console must not claim the user stopped something they didn't.
func TestRestoreDeadlineIsNotOperatorCancel(t *testing.T) {
	s := &Server{store: testStore(t)}
	ctx, finish, ok := s.beginRestoreRun(context.Background(), "b2", "app", "n1", 20*time.Millisecond)
	if !ok {
		t.Fatal("register")
	}
	defer finish()
	<-ctx.Done()
	if s.restoreWasCanceled("b2") {
		t.Fatal("a deadline must not be reported as an operator cancel")
	}
}

func TestCancelRestoreEndpoint(t *testing.T) {
	s := &Server{store: testStore(t)}

	// Unknown id → 404, never a silent 202.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/restores/nope/cancel", nil)
	r.SetPathValue("id", "nope")
	s.handleCancelRestore(rec, r)
	if rec.Code != 404 {
		t.Fatalf("unknown run: code=%d, want 404", rec.Code)
	}

	ctx, finish, _ := s.beginRestoreRun(context.Background(), "stack:blog", "stack blog", "n1", time.Hour)
	defer finish()
	rec = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/api/restores/stack:blog/cancel", nil)
	r.SetPathValue("id", "stack:blog")
	s.handleCancelRestore(rec, r)
	if rec.Code != 202 {
		t.Fatalf("running run: code=%d, want 202", rec.Code)
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the endpoint must cancel the run's context")
	}
	// The action is audited, so a destructive-flow interruption is accountable.
	rows, _ := s.store.ListAudit(20)
	found := false
	for _, a := range rows {
		if a.Action == "restore.cancel" && a.Target == "stack:blog" {
			found = true
		}
	}
	if !found {
		t.Fatal("cancel must be audited")
	}
}

// Re-attaching to a restore in progress (F100).
//
// A restore outlives the page that started it. Before this, reloading during a
// 40-minute restore left a UI that looked idle while a destructive operation
// continued — with no way to reach Cancel short of restarting DockBack. This
// endpoint is how a reloaded page finds the run again, so it has to carry enough
// to re-attach AND label the banner, and nothing more.

func TestRunningRestoresReportsIDLabelAndStart(t *testing.T) {
	s := &Server{store: testStore(t)}
	_, finish, ok := s.beginRestoreRun(context.Background(), "bk-1", "nextcloud", "n1", time.Minute)
	if !ok {
		t.Fatal("the run must register")
	}
	defer finish()

	got := s.runningRestores()
	if len(got) != 1 {
		t.Fatalf("want 1 running restore, got %v", runIDs(got))
	}
	// The ID is what BOTH the log stream and the cancel endpoint key on — a label
	// alone would leave a reloaded page unable to re-attach or stop anything.
	if got[0].ID != "bk-1" || got[0].Label != "nextcloud" {
		t.Fatalf("id/label = %q/%q", got[0].ID, got[0].Label)
	}
	if got[0].StartedAt == 0 {
		t.Fatal("started_at must be set so the banner can say how long it has run")
	}
}

// The list is polled every 15 seconds while the Backups page is open, so an
// unsorted map walk would reshuffle the banner on every refresh.
func TestRunningRestoresStableOrder(t *testing.T) {
	s := &Server{store: testStore(t)}
	for _, id := range []string{"zzz", "aaa", "mmm", "stack:blog"} {
		_, finish, ok := s.beginRestoreRun(context.Background(), id, id, "n1", time.Minute)
		if !ok {
			t.Fatalf("%s must register", id)
		}
		defer finish()
	}
	first := runIDs(s.runningRestores())
	for i := 0; i < 20; i++ {
		next := runIDs(s.runningRestores())
		for j := range first {
			if next[j] != first[j] {
				t.Fatalf("order changed between polls: %v then %v", first, next)
			}
		}
	}
}

func TestHandleListRestoresShape(t *testing.T) {
	s := &Server{store: testStore(t)}

	// An empty list must serialize as [], not null — the client maps over it.
	if body := listRestoresBody(t, s); !strings.Contains(body, `"running":[]`) {
		t.Fatalf("an empty list must serialize as []: %s", body)
	}

	_, finish, _ := s.beginRestoreRun(context.Background(), "stack:blog", "stack blog", "n1", time.Minute)
	defer finish()

	body := listRestoresBody(t, s)
	var resp struct {
		Running []RunningRestore `json:"running"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Running) != 1 || resp.Running[0].ID != "stack:blog" || resp.Running[0].Label != "stack blog" {
		t.Fatalf("want the run id and label, got %+v", resp.Running)
	}
	if resp.Running[0].StartedAt == 0 {
		t.Fatal("started_at must reach the client")
	}
	// The response describes the RUN — never the restore's target, node, source
	// copy or any credential. This endpoint exists to find and stop a restore.
	for _, leak := range []string{"target", "node_id", "password", "private_key", "config", "address"} {
		if strings.Contains(body, leak) {
			t.Fatalf("the response must not carry %q: %s", leak, body)
		}
	}
}

func listRestoresBody(t *testing.T, s *Server) string {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleListRestores(w, httptest.NewRequest("GET", "/api/restores", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	return w.Body.String()
}

func runIDs(rs []RunningRestore) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}
