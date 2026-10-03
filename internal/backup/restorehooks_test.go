package backup

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"dockback/internal/store"
)

func hooksEngine(t *testing.T) (*Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Engine{Store: st, Log: func(string, string, string) {}}, st
}

// TestNextcloudRestoreHooks (F140) covers the gap that made Nextcloud restores
// impossible: the backup captures config.php with maintenance mode ON, so a
// faithful restore serves 503, the healthcheck fails and the gate rolls back.
func TestNextcloudRestoreHooks(t *testing.T) {
	e, _ := hooksEngine(t)
	hooks := e.gatherRestoreHooks("nextcloud:34.0.2", "n1", "Nextcloud")
	if len(hooks) != 2 {
		t.Fatalf("expected maintenance-off then data-fingerprint, got %+v", hooks)
	}
	first := strings.Join(hooks[0].Cmd, " ")
	if !strings.Contains(first, "maintenance:mode --off") {
		t.Fatalf("the FIRST step must take the instance out of maintenance mode, got %q", first)
	}
	if !strings.Contains(strings.Join(hooks[1].Cmd, " "), "data-fingerprint") {
		t.Errorf("the second step should tell clients the state came from a backup, got %v", hooks[1].Cmd)
	}
	// occ must run as the web user from the webroot, or it cannot find its config.
	for _, h := range hooks {
		if h.User != "www-data" || h.WorkDir != "/var/www/html" {
			t.Errorf("occ must run as www-data in the webroot, got user=%q dir=%q", h.User, h.WorkDir)
		}
	}
	// The cron container runs the SAME image and has no healthcheck; failing a
	// restore because occ could not run there would break restores that are fine.
	for _, h := range hooks {
		if !h.Ignore {
			t.Errorf("a post-restore step must not itself fail the restore: %v", h.Cmd)
		}
	}
	// Every other image gets nothing at all — the restore path is unchanged.
	for _, img := range []string{"nginx:alpine", "postgres:16", "", "ghcr.io/gotify/server"} {
		if got := e.gatherRestoreHooks(img, "n1", "x"); len(got) != 0 {
			t.Errorf("%q must produce no restore hooks, got %+v", img, got)
		}
	}
}

// TestCustomPostRestoreHooks: an operator's own step runs AFTER the built-ins,
// so it acts on an app that is already out of maintenance mode.
func TestCustomPostRestoreHooks(t *testing.T) {
	e, st := hooksEngine(t)
	js, _ := json.Marshal(SavedHooks{PostRestore: []string{"php occ files:scan --all", "   ", "echo done"}, User: "www-data"})
	if err := st.SetSetting(HooksSettingKey("n1", "Nextcloud"), string(js)); err != nil {
		t.Fatal(err)
	}
	hooks := e.gatherRestoreHooks("nextcloud:34.0.2", "n1", "Nextcloud")
	if len(hooks) != 4 {
		t.Fatalf("expected 2 built-in + 2 custom (blank dropped), got %d: %+v", len(hooks), hooks)
	}
	if !strings.Contains(strings.Join(hooks[0].Cmd, " "), "maintenance:mode --off") {
		t.Error("built-ins must come first so custom steps run against a live app")
	}
	if !strings.Contains(strings.Join(hooks[2].Cmd, " "), "files:scan") {
		t.Errorf("custom lines must follow, got %v", hooks[2].Cmd)
	}
	// A blank line is not a command.
	for _, h := range hooks {
		if strings.TrimSpace(strings.Join(h.Cmd, " ")) == "/bin/sh -c" {
			t.Error("a blank line must not become a hook")
		}
	}
	// Custom hooks work for an image with no built-in preset too.
	if got := e.gatherRestoreHooks("nginx:alpine", "n1", "Nextcloud"); len(got) != 2 {
		t.Errorf("custom post-restore lines must apply to any image, got %+v", got)
	}
}

// TestSavedHooksBackwardCompatible: hooks saved before this existed must still
// unmarshal, with no post-restore steps invented for them.
func TestSavedHooksBackwardCompatible(t *testing.T) {
	var sh SavedHooks
	if err := json.Unmarshal([]byte(`{"pre":["a"],"post":["b"],"user":"root"}`), &sh); err != nil {
		t.Fatal(err)
	}
	if len(sh.Pre) != 1 || len(sh.Post) != 1 || sh.User != "root" {
		t.Fatalf("existing fields must survive: %+v", sh)
	}
	if len(sh.PostRestore) != 0 {
		t.Errorf("no post-restore steps may be invented, got %v", sh.PostRestore)
	}
	// And the field is omitted when empty, so saving does not rewrite old rows
	// with a null.
	out, _ := json.Marshal(SavedHooks{Pre: []string{"a"}})
	if strings.Contains(string(out), "post_restore") {
		t.Errorf("an empty post_restore must be omitted, got %s", out)
	}
}

// TestNextcloudRestoreHookAdvertised: the container page must say the restore
// half exists, since it is what makes a Nextcloud restore possible at all.
func TestNextcloudRestoreHookAdvertised(t *testing.T) {
	labels := strings.Join(AutoHookLabels("nextcloud:34.0.2", false), " ")
	if !strings.Contains(labels, "post-restore") {
		t.Errorf("the restore-side preset must be advertised, got %q", labels)
	}
	if !strings.Contains(labels, "503") {
		t.Errorf("the label should name the symptom it prevents, got %q", labels)
	}
}
