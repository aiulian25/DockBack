package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRecoveryToolMetadata verifies the served fingerprint is the REAL SHA-256 of
// the embedded script (F35 — no hardcoded value) and the metadata is well-formed.
func TestRecoveryToolMetadata(t *testing.T) {
	script := []byte("#!/usr/bin/env python3\nprint('recover')\n")
	s := &Server{}
	s.SetRecoveryTool("dockback-recover.py", script)

	sum := sha256.Sum256(script)
	wantSHA := hex.EncodeToString(sum[:])
	if s.recoveryToolSHA != wantSHA {
		t.Fatalf("precomputed sha = %s, want %s", s.recoveryToolSHA, wantSHA)
	}

	rr := httptest.NewRecorder()
	s.handleRecoveryTool(rr, httptest.NewRequest("GET", "/api/security/recovery-tool", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if m["sha256"] != wantSHA {
		t.Errorf("sha256 = %v, want %s", m["sha256"], wantSHA)
	}
	if m["available"] != true {
		t.Errorf("available = %v, want true", m["available"])
	}
	if url, _ := m["release_url"].(string); !strings.Contains(url, "aiulian25/dockback/releases/") ||
		!strings.HasSuffix(url, "dockback-recover.py") {
		t.Errorf("release_url looks wrong: %v", m["release_url"])
	}
	if inv, _ := m["invocation"].(string); !strings.Contains(inv, "--key") || !strings.Contains(inv, "--in") {
		t.Errorf("invocation missing flags: %v", m["invocation"])
	}
}

// TestRecoveryToolDownload verifies the embedded script is served verbatim, and a
// build with no embedded tool 404s rather than serving an empty file.
func TestRecoveryToolDownload(t *testing.T) {
	script := []byte("print('exact-bytes')\n")
	s := &Server{}
	s.SetRecoveryTool("dockback-recover.py", script)

	rr := httptest.NewRecorder()
	s.handleRecoveryToolDownload(rr, httptest.NewRequest("GET", "/api/security/recovery-tool/download", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Body.Bytes(); string(got) != string(script) {
		t.Errorf("served bytes differ from the embedded script")
	}
	if cd := rr.Header().Get("Content-Disposition"); !strings.Contains(cd, "dockback-recover.py") {
		t.Errorf("Content-Disposition = %q", cd)
	}

	// No tool embedded → 404.
	empty := &Server{}
	rr2 := httptest.NewRecorder()
	empty.handleRecoveryToolDownload(rr2, httptest.NewRequest("GET", "/x", nil))
	if rr2.Code != http.StatusNotFound {
		t.Errorf("empty build download status = %d, want 404", rr2.Code)
	}
}
