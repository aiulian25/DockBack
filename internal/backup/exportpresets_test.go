package backup

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"dockback/internal/store"
)

// Reusable export presets (F101). The generic per-container mechanism already
// worked; what these pin is the PRECEDENCE that makes a library useful without
// taking anything away: container profile > operator preset > built-in.

func presetEngine(t *testing.T) *Engine {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{Store: st, Log: func(string, string, string) {}}
}

func savePresets(t *testing.T, e *Engine, ps []ExportPreset) {
	t.Helper()
	js, _ := json.Marshal(ps)
	if err := e.Store.SetSetting(ExportPresetsKey, string(js)); err != nil {
		t.Fatal(err)
	}
}

// THE acceptance case: a saved preset whose match hits the image wins over the
// built-in table, and with no preset the built-in still applies.
func TestExportProfileForPresetBeatsBuiltin(t *testing.T) {
	e := presetEngine(t)

	// No library yet: the shipped paperless preset is what a paperless image gets.
	got := e.ExportProfileFor("ghcr.io/paperless-ngx/paperless-ngx:2.11", "n1", "docs")
	if got.Tool != "paperless" || !got.Available {
		t.Fatalf("without a library the built-in must apply: %+v", got)
	}

	savePresets(t, e, []ExportPreset{{
		ID: "p1", Name: "Paperless (our tuning)", Match: "paperless",
		Dir:       "/opt/exports",
		ExportCmd: "document_exporter /opt/exports --no-thumbnail",
		ImportCmd: "document_importer /opt/exports",
		User:      "paperless",
	}})

	got = e.ExportProfileFor("ghcr.io/paperless-ngx/paperless-ngx:2.11", "n1", "docs")
	if got.Tool != "Paperless (our tuning)" {
		t.Fatalf("an operator preset must beat the built-in: tool=%q", got.Tool)
	}
	if got.Dir != "/opt/exports" || got.User != "paperless" {
		t.Fatalf("the preset's own values must be used: %+v", got)
	}
	// Commands are wrapped as a shell line, matching the per-container path.
	if len(got.ExportCmd) != 3 || got.ExportCmd[0] != "/bin/sh" || !strings.Contains(got.ExportCmd[2], "--no-thumbnail") {
		t.Fatalf("export cmd = %v", got.ExportCmd)
	}
	if !got.Available {
		t.Fatal("a complete preset must produce an available profile")
	}
}

// A preset covers images the built-ins never did — the whole point of the
// feature — and an image nothing matches stays unconfigured rather than
// picking up someone else's recipe.
func TestExportProfileForNewImagesAndNoMatch(t *testing.T) {
	e := presetEngine(t)
	savePresets(t, e, []ExportPreset{{
		ID: "p1", Name: "Vaultwarden", Match: "vaultwarden",
		Dir: "/data/export", ExportCmd: "vaultwarden-backup", ImportCmd: "vaultwarden-restore",
	}})

	if got := e.ExportProfileFor("vaultwarden/server:1.32", "n1", "vw"); got.Tool != "Vaultwarden" || !got.Available {
		t.Fatalf("a preset must cover an image the built-ins don't: %+v", got)
	}
	if got := e.ExportProfileFor("nginx:1.27", "n1", "web"); got.Available || got.Tool != "" {
		t.Fatalf("an unmatched image must stay unconfigured: %+v", got)
	}
}

// A container's own saved profile is the most specific statement about that
// container and must still win — otherwise adding a preset would silently
// rewrite deliberate per-container work.
func TestExportProfileForContainerProfileBeatsPreset(t *testing.T) {
	e := presetEngine(t)
	savePresets(t, e, []ExportPreset{{
		ID: "p1", Name: "Paperless (library)", Match: "paperless",
		Dir: "/opt/exports", ExportCmd: "library-export", ImportCmd: "library-import",
	}})
	own, _ := json.Marshal(SavedExportProfile{
		Dir: "/srv/mine", ExportCmd: "my-export", ImportCmd: "my-import",
	})
	if err := e.Store.SetSetting(ExportProfileKey("n1", "docs"), string(own)); err != nil {
		t.Fatal(err)
	}

	got := e.ExportProfileFor("paperless-ngx:2.11", "n1", "docs")
	if got.Dir != "/srv/mine" || !strings.Contains(got.ExportCmd[2], "my-export") {
		t.Fatalf("the container's own profile must win: %+v", got)
	}
	// It keeps the preset's NAME as the tool: the container overrode the commands,
	// not the identity of the recipe it started from.
	if got.Tool != "Paperless (library)" {
		t.Fatalf("tool = %q", got.Tool)
	}
}

// A half-written library entry must never drive a backup — it would produce an
// export that silently captured nothing.
func TestMatchExportPresetSkipsIncomplete(t *testing.T) {
	presets := []ExportPreset{
		{ID: "a", Name: "half", Match: "immich", Dir: "/exports"}, // no commands
		{ID: "b", Name: "full", Match: "immich", Dir: "/exports", ExportCmd: "e", ImportCmd: "i"},
	}
	got, ok := MatchExportPreset(presets, "ghcr.io/immich-app/immich-server:v1.118")
	if !ok || got.ID != "b" {
		t.Fatalf("the first COMPLETE match must win: %+v ok=%v", got, ok)
	}
	if _, ok := MatchExportPreset(presets[:1], "immich"); ok {
		t.Fatal("an incomplete preset must not match at all")
	}
}

// An empty match is library-only: applicable by hand, never claiming an image on
// its own. Without this, a blank field would match EVERY container.
func TestMatchExportPresetEmptyMatchNeverClaims(t *testing.T) {
	presets := []ExportPreset{{ID: "a", Name: "manual", Dir: "/e", ExportCmd: "e", ImportCmd: "i"}}
	if _, ok := MatchExportPreset(presets, "anything:latest"); ok {
		t.Fatal("a preset with no match must never claim an image automatically")
	}
}

func TestMatchExportPresetIsCaseInsensitive(t *testing.T) {
	presets := []ExportPreset{{ID: "a", Name: "N", Match: "PaperLess", Dir: "/e", ExportCmd: "e", ImportCmd: "i"}}
	if _, ok := MatchExportPreset(presets, "GHCR.IO/Paperless-NGX:2"); !ok {
		t.Fatal("matching must be case-insensitive, like the built-in table")
	}
}

// A corrupt setting must degrade to "no presets" — which falls back to the
// built-ins — rather than break every backup that looks up a profile.
func TestParseExportPresetsToleratesGarbage(t *testing.T) {
	for _, js := range []string{"", "   ", "not json", "{}", `{"presets":[]}`} {
		if got := ParseExportPresets(js); len(got) != 0 {
			t.Fatalf("ParseExportPresets(%q) = %+v, want empty", js, got)
		}
	}
	e := presetEngine(t)
	if err := e.Store.SetSetting(ExportPresetsKey, "{{{ corrupt"); err != nil {
		t.Fatal(err)
	}
	if got := e.ExportProfileFor("paperless-ngx:2", "n1", "docs"); got.Tool != "paperless" {
		t.Fatalf("a corrupt library must fall back to the built-in, got %q", got.Tool)
	}
}

// The built-ins are surfaced in the library so they can be seen and copied —
// flagged read-only, and complete enough to actually be used as a starting point.
func TestBuiltinExportPresetsAreListable(t *testing.T) {
	got := BuiltinExportPresets()
	if len(got) != len(exportPresets) {
		t.Fatalf("want one entry per built-in, got %d", len(got))
	}
	names := map[string]bool{}
	for _, p := range got {
		if !p.Builtin {
			t.Fatalf("%s must be flagged built-in", p.Name)
		}
		if !strings.HasPrefix(p.ID, "builtin:") {
			t.Fatalf("%s must carry a builtin id, got %q", p.Name, p.ID)
		}
		if !p.Complete() {
			t.Fatalf("%s must be usable as a starting point: %+v", p.Name, p)
		}
		names[p.Name] = true
	}
	for _, want := range []string{"paperless", "forgejo", "gitea"} {
		if !names[want] {
			t.Fatalf("the %s built-in must be listed", want)
		}
	}
}
