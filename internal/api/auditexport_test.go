package api

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"dockback/internal/backup"
	"dockback/internal/config"
	"dockback/internal/store"
)

// The export is now streamed a page at a time instead of assembled in memory.
// The output it produces has to be exactly as valid as before, or a compliance
// hand-off silently becomes an unparseable file.
func auditExportServer(t *testing.T, rows int) *Server {
	t.Helper()
	st := testStore(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 4)
	}
	s := &Server{
		store: st, cfg: &config.Config{EncryptionKey: key},
		engine: &backup.Engine{Store: st, Key: key, KeyFP: backup.KeyFingerprint(key), Log: func(string, string, string) {}},
	}
	for i := 0; i < rows; i++ {
		if err := st.Audit("admin", "backup.start", fmt.Sprintf("app%d", i), "detail, with a comma"); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestAuditExportStreamsValidOutput(t *testing.T) {
	const rows = 2500 // more than one page

	t.Run("csv", func(t *testing.T) {
		s := auditExportServer(t, rows)
		rec := httptest.NewRecorder()
		s.handleAuditExport(rec, httptest.NewRequest("GET", "/api/audit/export?format=csv", nil))
		recs, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
		if err != nil {
			t.Fatalf("the export is not parseable CSV: %v", err)
		}
		if len(recs) != rows+1 { // + header
			t.Fatalf("got %d rows (incl. header), want %d", len(recs), rows+1)
		}
		if recs[0][0] != "timestamp_utc" {
			t.Errorf("header changed: %v", recs[0])
		}
		// A comma in a field must still be quoted correctly across a page boundary.
		if !strings.Contains(recs[1][5], "with a comma") {
			t.Errorf("field content lost: %v", recs[1])
		}
	})

	t.Run("json", func(t *testing.T) {
		s := auditExportServer(t, rows)
		rec := httptest.NewRecorder()
		s.handleAuditExport(rec, httptest.NewRequest("GET", "/api/audit/export?format=json", nil))
		var out []store.AuditEntry
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("the export is not parseable JSON: %v\nfirst 200 bytes: %.200s", err, rec.Body.String())
		}
		if len(out) != rows {
			t.Fatalf("got %d entries, want %d", len(out), rows)
		}
		if out[0].Actor != "admin" || out[0].Action != "backup.start" {
			t.Errorf("entry content lost: %+v", out[0])
		}
	})

	t.Run("empty trail is still valid", func(t *testing.T) {
		empty := auditExportServer(t, 0)
		rec := httptest.NewRecorder()
		empty.handleAuditExport(rec, httptest.NewRequest("GET", "/api/audit/export?format=json", nil))
		var out []store.AuditEntry
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("an empty export must still be valid JSON: %v (%q)", err, rec.Body.String())
		}
		if len(out) != 0 {
			t.Errorf("an empty trail exported %d entries", len(out))
		}
	})

	// The export records itself, including how many rows left the machine.
	s := auditExportServer(t, 3)
	rec := httptest.NewRecorder()
	s.handleAuditExport(rec, httptest.NewRequest("GET", "/api/audit/export?format=csv", nil))
	found := false
	entries, err := s.store.ListAudit(50)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Action == "audit.export" {
			found = true
			if !strings.Contains(e.Detail, "rows=") {
				t.Errorf("the export record must say how much left: %q", e.Detail)
			}
		}
	}
	if !found {
		t.Error("an export must itself be audited")
	}
}
