package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dockback/internal/store"
)

// F223 — the recovery wizard is state and guidance. These tests hold the two
// things that decide whether it appears at the right moment: what "a fresh
// install" means, and whether the step survives the restart an app-restore
// causes — which is the one moment the database it would have been written to
// is replaced.

func wizardServer(t *testing.T) *Server {
	t.Helper()
	s := nodeBackupServer(t)
	s.cfg.DataDir = t.TempDir()
	return s
}

func wizardState(t *testing.T, s *Server) recoveryStateResp {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleRecoveryState(rec, httptest.NewRequest("GET", "/api/recovery/state", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", rec.Code, rec.Body.String())
	}
	var out recoveryStateResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func putWizardStep(s *Server, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.handleSetRecoveryState(rec, httptest.NewRequest("PUT", "/api/recovery/state", strings.NewReader(body)))
	return rec
}

// AC1 — a fresh install shows the wizard; connecting a node ends it.
//
// The fixture already has the auto-registered `local` node, which is exactly the
// case a literal "zero nodes" test gets wrong: main.go registers it at boot on
// every normal install, so the wizard would never appear at all.
func TestWizardFreshInstallIgnoresTheAutoLocalNode(t *testing.T) {
	s := wizardServer(t)
	if err := s.store.UpsertNode(&store.Node{ID: "local", Name: "local", Transport: "socket", Address: "unix:///x"}); err != nil {
		t.Fatal(err)
	}
	// The fixture's own node is "n1" — an operator-added one. Remove it so this
	// install is genuinely empty.
	if err := s.store.DeleteNode("n1"); err != nil {
		t.Fatal(err)
	}

	got := wizardState(t, s)
	if !got.FreshInstall {
		t.Fatalf("an install holding only the auto-registered local node is fresh: %+v", got)
	}
	if got.Nodes != 0 || got.Backups != 0 {
		t.Errorf("the local node must not count as something the operator added: %+v", got)
	}

	// Connect a node: that is somebody using the product, not recovering.
	if err := s.store.UpsertNode(&store.Node{ID: "nas", Name: "nas", Transport: "ssh", Address: "10.0.0.5"}); err != nil {
		t.Fatal(err)
	}
	if got := wizardState(t, s); got.FreshInstall || got.Nodes != 1 {
		t.Errorf("adding a node must end the wizard: %+v", got)
	}
}

// A catalog with backups in it is not a fresh install either, even with no node
// connected — that is precisely the state a restored app-backup leaves behind.
func TestWizardIsNotFreshOnceBackupsExist(t *testing.T) {
	s := wizardServer(t)
	if err := s.store.DeleteNode("n1"); err != nil {
		t.Fatal(err)
	}
	if got := wizardState(t, s); !got.FreshInstall {
		t.Fatalf("empty means empty: %+v", got)
	}
	if err := s.store.CreateBackup(&store.Backup{ID: "b1", NodeID: "n1", TargetName: "app", Status: "success", CreatedAt: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	got := wizardState(t, s)
	if got.FreshInstall || got.Backups != 1 {
		t.Errorf("a catalog with history is not a fresh install: %+v", got)
	}
}

// The step round-trips, and only known steps are accepted — this value drives
// what the dashboard tells somebody to do next during a disaster.
func TestWizardStepTransitions(t *testing.T) {
	s := wizardServer(t)

	if got := wizardState(t, s); got.Step != "" {
		t.Fatalf("no state until something happens, got %q", got.Step)
	}
	for _, step := range []string{wizardStepAppRestored, wizardStepDestsChecked, wizardStepDone, wizardStepDismissed} {
		if rec := putWizardStep(s, `{"step":"`+step+`"}`); rec.Code != http.StatusOK {
			t.Fatalf("PUT %q = %d: %s", step, rec.Code, rec.Body.String())
		}
		if got := wizardState(t, s); got.Step != step {
			t.Errorf("step = %q, want %q", got.Step, step)
		}
	}
	// Clearing is allowed (the wizard can be reset); nonsense is not.
	if rec := putWizardStep(s, `{"step":""}`); rec.Code != http.StatusOK {
		t.Errorf("clearing must be allowed: %s", rec.Body.String())
	}
	for _, bad := range []string{`{"step":"whatever"}`, `{"step":"DONE"}`, `not json`} {
		if rec := putWizardStep(s, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("body %q = %d, want 400", bad, rec.Code)
		}
	}
	// A hand-edited settings row is not trusted into the UI either.
	if err := s.store.SetSetting(wizardStateKey, "something-else"); err != nil {
		t.Fatal(err)
	}
	if got := wizardState(t, s); got.Step != "" {
		t.Errorf("an unknown stored step must read as no state, got %q", got.Step)
	}
}

// AC2 — the step survives the restart an app-restore causes.
//
// This is the whole reason the marker is a FILE. At the moment finishRestore
// runs, the database any setting would be written to is about to be replaced by
// the restored one, so the setting would vanish seconds later.
func TestWizardMarkerSurvivesTheDatabaseSwap(t *testing.T) {
	s := wizardServer(t)
	s.markWizardAppRestored()

	marker := filepath.Join(s.cfg.DataDir, wizardMarkerFile)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the marker must be written beside the staged database: %v", err)
	}

	// The restart: a different Server, a different (restored) database, the same
	// data directory — which is exactly what carries the note across.
	restored := wizardServer(t)
	restored.cfg.DataDir = s.cfg.DataDir
	if err := restored.store.DeleteNode("n1"); err != nil {
		t.Fatal(err)
	}

	got := wizardState(t, restored)
	if got.Step != wizardStepAppRestored {
		t.Fatalf("the resume banner must appear after the restart, got step %q", got.Step)
	}
	// Consumed exactly once: the banner is driven by the setting from here on,
	// so a marker left on disk would resurrect it after every dismissal.
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("the marker must be removed once adopted: %v", err)
	}
	if got := wizardState(t, restored); got.Step != wizardStepAppRestored {
		t.Errorf("and the step persists across reloads, got %q", got.Step)
	}
	// Dismissing sticks, and a stale marker from an older restore cannot reopen it.
	if rec := putWizardStep(restored, `{"step":"`+wizardStepDismissed+`"}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	restored.markWizardAppRestored()
	if got := wizardState(t, restored); got.Step != wizardStepDismissed {
		t.Errorf("a stale marker must never move the wizard backwards, got %q", got.Step)
	}
}

// A marker containing junk is ignored rather than shown. Guidance that cannot be
// trusted should disappear, not assert something wrong in the middle of a
// recovery.
func TestWizardIgnoresAnUnreadableMarker(t *testing.T) {
	s := wizardServer(t)
	marker := filepath.Join(s.cfg.DataDir, wizardMarkerFile)
	if err := os.WriteFile(marker, []byte("rm -rf /"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := wizardState(t, s); got.Step != "" {
		t.Errorf("junk in the marker must not become a step: %q", got.Step)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("and it is cleaned up rather than re-read every poll")
	}
	// No data dir configured at all: no panic, no marker, no state.
	bare := nodeBackupServer(t)
	bare.cfg.DataDir = ""
	bare.markWizardAppRestored()
	if got := wizardState(t, bare); got.Step != "" {
		t.Errorf("no data dir means no marker machinery, got %q", got.Step)
	}
}
