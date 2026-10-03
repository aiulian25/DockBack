package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"dockback/internal/backup"
)

// TestExportProfileRoundTrip covers F32's core: a custom profile exported from one
// container (as a portable SavedExportProfile) and imported onto a second yields the
// same effective Dir/ExportCmd/ImportCmd — the exact ExportProfileFor + Saved()
// logic the GET/POST handlers use.
func TestExportProfileRoundTrip(t *testing.T) {
	st := testStore(t)
	e := &backup.Engine{Store: st}

	// A hand-authored custom profile on container c1 (no built-in preset for "myapp").
	custom := backup.SavedExportProfile{
		Dir:       "/data/export",
		ExportCmd: "myapp export --to /data/export",
		ImportCmd: "myapp import --from /data/export",
		User:      "appuser",
	}
	jsb, _ := json.Marshal(custom)
	js := string(jsb)
	if err := st.SetSetting(backup.ExportProfileKey("n1", "c1"), js); err != nil {
		t.Fatal(err)
	}

	// "Export": the effective profile as a portable SavedExportProfile.
	got := e.ExportProfileFor("myapp:1", "n1", "c1").Saved()
	if got.Dir != custom.Dir || got.ExportCmd != custom.ExportCmd || got.ImportCmd != custom.ImportCmd || got.User != custom.User {
		t.Fatalf("exported profile = %+v, want %+v", got, custom)
	}

	// "Import" the exported profile onto c2, then confirm ExportProfileFor matches.
	js2b, _ := json.Marshal(got)
	js2 := string(js2b)
	if err := st.SetSetting(backup.ExportProfileKey("n1", "c2"), js2); err != nil {
		t.Fatal(err)
	}
	prof2 := e.ExportProfileFor("myapp:1", "n1", "c2")
	if prof2.Dir != custom.Dir {
		t.Errorf("c2 Dir = %q, want %q", prof2.Dir, custom.Dir)
	}
	if len(prof2.ExportCmd) != 3 || prof2.ExportCmd[2] != custom.ExportCmd {
		t.Errorf("c2 ExportCmd = %v, want sh -c %q", prof2.ExportCmd, custom.ExportCmd)
	}
	if len(prof2.ImportCmd) != 3 || prof2.ImportCmd[2] != custom.ImportCmd {
		t.Errorf("c2 ImportCmd = %v, want sh -c %q", prof2.ImportCmd, custom.ImportCmd)
	}
	if !prof2.Available {
		t.Error("c2 profile should be available (dir + both commands set)")
	}

	// A preset (paperless) with no custom override still exports a usable profile.
	pp := e.ExportProfileFor("ghcr.io/paperless-ngx/paperless-ngx:2", "n1", "cx").Saved()
	if pp.Dir == "" || pp.ExportCmd == "" || pp.ImportCmd == "" {
		t.Errorf("paperless preset should export a non-empty profile: %+v", pp)
	}
}

// TestExportProfileImportRejectsMalformed: the import handler answers 400 on
// malformed JSON (before any Docker round-trip), and 400 on an empty profile.
func TestExportProfileImportRejectsMalformed(t *testing.T) {
	s := &Server{}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/nodes/n1/containers/c1/export-profile", strings.NewReader("{not valid json"))
	s.handleExportProfileImport(rr, req)
	if rr.Code != 400 {
		t.Fatalf("malformed JSON: status = %d, want 400", rr.Code)
	}

	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/api/nodes/n1/containers/c1/export-profile", strings.NewReader(`{"dir":"","export_cmd":"  ","import_cmd":""}`))
	s.handleExportProfileImport(rr2, req2)
	if rr2.Code != 400 {
		t.Fatalf("empty profile: status = %d, want 400", rr2.Code)
	}
}
