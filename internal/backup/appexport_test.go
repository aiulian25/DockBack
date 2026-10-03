package backup

import (
	"path/filepath"
	"testing"

	"dockback/internal/store"
)

// TestBuiltinExportPresets locks in the app-native export presets (F16): each
// recognized image yields an Available profile with the expected tool, symmetric
// export/import commands, and a non-empty export dir — while an unrecognized
// image yields nothing.
func TestBuiltinExportPresets(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	e := &Engine{Store: st, Log: func(string, string, string) {}}

	cases := []struct {
		image string
		tool  string
	}{
		{"ghcr.io/paperless-ngx/paperless-ngx:2.13", "paperless"},
		{"paperlessngx/paperless-ngx:latest", "paperless"},
		{"gitea/gitea:1.22", "gitea"},
		{"codeberg.org/forgejo/forgejo:8", "forgejo"},
	}
	for _, c := range cases {
		p := e.ExportProfileFor(c.image, "node1", "app")
		if !p.Available {
			t.Errorf("%s: expected an available export profile", c.image)
		}
		if p.Tool != c.tool {
			t.Errorf("%s: tool = %q, want %q", c.image, p.Tool, c.tool)
		}
		if p.Dir == "" || len(p.ExportCmd) == 0 || len(p.ImportCmd) == 0 {
			t.Errorf("%s: incomplete profile %+v", c.image, p)
		}
	}

	// An unrecognized image has no built-in preset (raw volume/DB capture is used).
	if p := e.ExportProfileFor("nginx:1.27", "node1", "web"); p.Available {
		t.Errorf("nginx should have no built-in export preset, got %+v", p)
	}
}

// TestExportPresetForgejoBeforeGitea confirms a Forgejo image resolves to the
// forgejo preset (not gitea) — order matters in the preset table.
func TestExportPresetForgejoBeforeGitea(t *testing.T) {
	if p := builtinExportProfile("codeberg.org/forgejo/forgejo:9"); p.Tool != "forgejo" {
		t.Fatalf("forgejo image resolved to tool %q, want forgejo", p.Tool)
	}
	// The Gitea preset runs as the git user (verifying the preset shape).
	if p := builtinExportProfile("gitea/gitea:1.22"); p.User != "git" {
		t.Fatalf("gitea preset user = %q, want git", p.User)
	}
}
