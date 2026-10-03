package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dockback/internal/backup"
)

// Export presets (F101). A preset holds a shell line DockBack runs inside a
// container, and the library can be IMPORTED from another deployment — so what
// these tests pin is that nothing gets in without passing validation, and that
// an import can never happen by accident or quietly clobber local work.

func presetServer(t *testing.T) *Server {
	t.Helper()
	return &Server{store: testStore(t)}
}

func postPresets(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleSaveExportPresets(w, httptest.NewRequest("POST", "/api/export-presets", strings.NewReader(body)))
	return w
}

func listPresets(t *testing.T, s *Server) []backup.ExportPreset {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleListExportPresets(w, httptest.NewRequest("GET", "/api/export-presets", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list: want 200, got %d", w.Code)
	}
	var resp struct {
		Presets []backup.ExportPreset `json:"presets"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Presets
}

func TestExportPresetSaveListDelete(t *testing.T) {
	s := presetServer(t)

	// A fresh install lists only the built-ins, so the library is never empty and
	// an operator can see the shipped recipes to copy from.
	base := listPresets(t, s)
	if len(base) != len(backup.BuiltinExportPresets()) {
		t.Fatalf("a fresh library must show the built-ins, got %d", len(base))
	}

	w := postPresets(t, s, `{"preset":{"name":"Vaultwarden","match":"vaultwarden","dir":"/data/export","export_cmd":"vw-backup","import_cmd":"vw-restore"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("save: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	var saved struct {
		Preset backup.ExportPreset `json:"preset"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &saved)
	if saved.Preset.ID == "" {
		t.Fatal("a new preset must be assigned an id")
	}

	all := listPresets(t, s)
	if len(all) != len(base)+1 || all[0].Name != "Vaultwarden" {
		t.Fatalf("the saved preset must lead the library: %+v", all)
	}

	// Saving with the same id EDITS rather than duplicating.
	w = postPresets(t, s, `{"preset":{"id":"`+saved.Preset.ID+`","name":"Vaultwarden v2","match":"vaultwarden","dir":"/data/export","export_cmd":"vw-backup -v2","import_cmd":"vw-restore"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("edit: %d (%s)", w.Code, w.Body.String())
	}
	if all = listPresets(t, s); len(all) != len(base)+1 || all[0].Name != "Vaultwarden v2" {
		t.Fatalf("an edit must replace, not duplicate: %+v", all)
	}

	// Delete removes it and is idempotent-safe (a second delete is a clean 404).
	dw := httptest.NewRecorder()
	dr := httptest.NewRequest("DELETE", "/api/export-presets/x", nil)
	dr.SetPathValue("id", saved.Preset.ID)
	s.handleDeleteExportPreset(dw, dr)
	if dw.Code != http.StatusOK {
		t.Fatalf("delete: %d (%s)", dw.Code, dw.Body.String())
	}
	if got := listPresets(t, s); len(got) != len(base) {
		t.Fatalf("the preset must be gone: %+v", got)
	}
	dw = httptest.NewRecorder()
	dr = httptest.NewRequest("DELETE", "/api/export-presets/x", nil)
	dr.SetPathValue("id", saved.Preset.ID)
	s.handleDeleteExportPreset(dw, dr)
	if dw.Code != http.StatusNotFound {
		t.Fatalf("deleting a gone preset must be a clean 404, got %d", dw.Code)
	}

	// Every mutation is audited — these commands run inside containers, so
	// "when did this appear?" has to be answerable.
	rows, _ := s.store.ListAudit(20)
	seen := map[string]bool{}
	for _, a := range rows {
		seen[a.Action] = true
	}
	if !seen["exportpreset.save"] || !seen["exportpreset.delete"] {
		t.Fatalf("save and delete must both be audited: %v", seen)
	}
}

// The built-ins are display entries the server owns. Deleting one, or claiming
// built-in status on the way in, must both be refused.
func TestExportPresetBuiltinsAreReadOnly(t *testing.T) {
	s := presetServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/export-presets/x", nil)
	r.SetPathValue("id", "builtin:paperless")
	s.handleDeleteExportPreset(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a built-in must not be deletable, got %d", w.Code)
	}

	if w := postPresets(t, s, `{"preset":{"name":"Sneaky","match":"x","dir":"/e","export_cmd":"e","import_cmd":"i","builtin":true}}`); w.Code != http.StatusOK {
		t.Fatalf("save: %d", w.Code)
	}
	for _, p := range listPresets(t, s) {
		if p.Name == "Sneaky" && p.Builtin {
			t.Fatal("a client must not be able to mark its own preset read-only")
		}
	}
}

func TestExportPresetValidation(t *testing.T) {
	s := presetServer(t)
	for _, c := range []struct{ name, body string }{
		{"no name", `{"preset":{"dir":"/e","export_cmd":"e","import_cmd":"i"}}`},
		{"blank name", `{"preset":{"name":"   ","dir":"/e","export_cmd":"e","import_cmd":"i"}}`},
		{"empty", `{"preset":{"name":"Empty"}}`},
		{"relative dir", `{"preset":{"name":"N","dir":"exports","export_cmd":"e","import_cmd":"i"}}`},
		{"null byte", "{\"preset\":{\"name\":\"N\",\"dir\":\"/e\",\"export_cmd\":\"e\\u0000vil\",\"import_cmd\":\"i\"}}"},
		{"oversize", `{"preset":{"name":"N","dir":"/e","export_cmd":"` + strings.Repeat("x", 5000) + `","import_cmd":"i"}}`},
		{"both forms", `{"preset":{"name":"N","dir":"/e","export_cmd":"e","import_cmd":"i"},"presets":[]}`},
		{"nothing", `{}`},
		{"malformed", `not json`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if w := postPresets(t, s, c.body); w.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d (%s)", w.Code, w.Body.String())
			}
		})
	}
	// Nothing above may have landed in the library.
	if got := listPresets(t, s); len(got) != len(backup.BuiltinExportPresets()) {
		t.Fatalf("a rejected preset must not be stored: %+v", got)
	}
}

// Importing a library replaces every command DockBack would run. It must not be
// possible without saying so explicitly.
func TestExportPresetImportRequiresExplicitReplace(t *testing.T) {
	s := presetServer(t)
	lib := `{"presets":[{"name":"Immich","match":"immich","dir":"/e","export_cmd":"e","import_cmd":"i"}]}`
	if w := postPresets(t, s, lib); w.Code != http.StatusBadRequest {
		t.Fatalf("an import without replace=true must be refused, got %d", w.Code)
	}
	if !strings.Contains(postPresets(t, s, lib).Body.String(), "replace=true") {
		t.Fatal("the refusal must say how to confirm")
	}
	if got := listPresets(t, s); len(got) != len(backup.BuiltinExportPresets()) {
		t.Fatal("a refused import must change nothing")
	}

	w := postPresets(t, s, `{"replace":true,"presets":[{"name":"Immich","match":"immich","dir":"/e","export_cmd":"e","import_cmd":"i"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("a confirmed import must succeed: %d (%s)", w.Code, w.Body.String())
	}
	got := listPresets(t, s)
	if len(got) != len(backup.BuiltinExportPresets())+1 || got[0].Name != "Immich" {
		t.Fatalf("the imported library must be in place: %+v", got)
	}
	if got[0].ID == "" {
		t.Fatal("an imported preset with no id must be assigned one")
	}
}

// One bad entry rejects the WHOLE import: a partially-applied library of shell
// commands is worse than none, because it is not what the operator reviewed.
func TestExportPresetImportIsAllOrNothing(t *testing.T) {
	s := presetServer(t)
	if w := postPresets(t, s, `{"replace":true,"presets":[{"name":"Good","match":"a","dir":"/e","export_cmd":"e","import_cmd":"i"},{"name":"","dir":"/e","export_cmd":"e","import_cmd":"i"}]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a library with a bad entry, got %d", w.Code)
	}
	if got := listPresets(t, s); len(got) != len(backup.BuiltinExportPresets()) {
		t.Fatalf("a rejected import must store nothing: %+v", got)
	}
}

// Duplicate ids in an imported file are re-minted, so an import can never
// overwrite an unrelated local preset by reusing its id.
func TestExportPresetImportRemintsDuplicateIDs(t *testing.T) {
	s := presetServer(t)
	w := postPresets(t, s, `{"replace":true,"presets":[
		{"id":"same","name":"A","match":"a","dir":"/e","export_cmd":"e","import_cmd":"i"},
		{"id":"same","name":"B","match":"b","dir":"/e","export_cmd":"e","import_cmd":"i"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("import: %d (%s)", w.Code, w.Body.String())
	}
	got := listPresets(t, s)
	if got[0].ID == got[1].ID {
		t.Fatalf("duplicate ids must be re-minted: %+v", got[:2])
	}
}

// The library is read on every export-profile lookup, so it has to stay bounded.
func TestExportPresetLibraryIsBounded(t *testing.T) {
	s := presetServer(t)
	big := make([]string, 0, maxExportPresets+1)
	for i := 0; i <= maxExportPresets; i++ {
		big = append(big, `{"name":"p","match":"x","dir":"/e","export_cmd":"e","import_cmd":"i"}`)
	}
	body := `{"replace":true,"presets":[` + strings.Join(big, ",") + `]}`
	if w := postPresets(t, s, body); w.Code != http.StatusBadRequest {
		t.Fatalf("an oversized library must be refused, got %d", w.Code)
	}
}
