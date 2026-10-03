package api

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"dockback/internal/backup"
	"dockback/internal/config"
	"dockback/internal/store"
)

const shareKeyFP = "SECRETKEYFINGERPRINT0011"

func shareTestServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	// A node + one successful backup so the runbook has a service with a stack.
	if err := st.UpsertNode(&store.Node{ID: "n1", Name: "Edge Node", Transport: "ssh", Address: "ssh://root@10.9.8.7:22"}); err != nil {
		t.Fatal(err)
	}
	b := &store.Backup{ID: "b1", NodeID: "n1", TargetName: "myblog-web", Stack: "myblog", Status: "success", CreatedAt: 1000}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	man, _ := json.Marshal(backup.Manifest{Stack: "myblog", Service: "web", TargetName: "myblog-web", Image: "ghcr.io/x/blog:1", ContainerID: "c1"})
	b.ManifestJSON = string(man)
	b.LocationsJSON = `[{"kind":"local","name":"local","type":"local"}]`
	b.CompletedAt = 1000
	if err := st.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}

	eng := &backup.Engine{Store: st, Key: make([]byte, 32), KeyFP: shareKeyFP, Log: func(string, string, string) {}}
	return &Server{
		store:  st,
		engine: eng,
		cfg:    &config.Config{EncryptionKey: make([]byte, 32), BackupsDir: t.TempDir(), DataDir: t.TempDir()},
	}
}

func serveShare(s *Server, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/share/runbook/"+token, nil)
	req.SetPathValue("token", token)
	req.RemoteAddr = "203.0.113.7:44444"
	rec := httptest.NewRecorder()
	s.handleSharedRunbook(rec, req)
	return rec
}

func TestShareTokenStates(t *testing.T) {
	s := shareTestServer(t)
	key := s.cfg.EncryptionKey

	// --- valid ---
	// The token must be an ISSUED link: after Step 4 a signed token whose id is
	// not in the issued list fails closed (see the "never issued" case below), so
	// the valid case has to register the link the way handleShareRunbook does.
	id := "abc123def456"
	exp := time.Now().Add(time.Hour).Unix()
	sig := signShare(id, exp, key)
	s.saveShareLinks([]shareLink{{ID: id, Created: time.Now().Unix(), Exp: exp}})
	valid := id + "." + itoa64(exp) + "." + sig
	rec := serveShare(s, valid)
	if rec.Code != 200 {
		t.Fatalf("valid token: code=%d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "myblog") || !strings.Contains(body, "myblog-web") {
		t.Errorf("valid share must contain the stack/service names")
	}
	if strings.Contains(body, shareKeyFP) {
		t.Errorf("REDACTION FAILED: the master-key fingerprint leaked into the shared page")
	}
	if strings.Contains(body, "10.9.8.7") {
		t.Errorf("REDACTION FAILED: a node address leaked into the shared page")
	}

	// --- expired ---
	pexp := time.Now().Add(-time.Hour).Unix()
	expired := id + "." + itoa64(pexp) + "." + signShare(id, pexp, key)
	if rec := serveShare(s, expired); rec.Code != 404 {
		t.Errorf("expired token: code=%d, want 404", rec.Code)
	}

	// --- tampered signature ---
	// Flip (never merely set) the last character: when the genuine signature
	// already ends in '0', replacing it with '0' would leave the token VALID and
	// this check failed spuriously (~1 in 16 signatures).
	repl := byte('0')
	if sig[len(sig)-1] == '0' {
		repl = '1'
	}
	tampered := id + "." + itoa64(exp) + "." + sig[:len(sig)-1] + string(repl)
	if rec := serveShare(s, tampered); rec.Code == 200 {
		t.Errorf("tampered signature must NOT render (got 200)")
	}

	// --- validly signed but NEVER ISSUED (Step 4 fail-closed) ---
	// A correct signature is no longer sufficient: a token whose id is not in the
	// issued list is treated as revoked, so a link evicted from a replaced
	// database — or minted by something holding the signing key but not this
	// instance's list — cannot render.
	uid := "neverissued00"
	unissued := uid + "." + itoa64(exp) + "." + signShare(uid, exp, key)
	if rec := serveShare(s, unissued); rec.Code != 404 {
		t.Errorf("unissued but validly signed token: code=%d, want 404 (fail-closed)", rec.Code)
	}

	// --- revoked ---
	rid := "revoked00id0"
	rexp := time.Now().Add(time.Hour).Unix()
	rtoken := rid + "." + itoa64(rexp) + "." + signShare(rid, rexp, key)
	s.saveShareLinks([]shareLink{{ID: rid, Created: time.Now().Unix(), Exp: rexp, Revoked: true}})
	if rec := serveShare(s, rtoken); rec.Code != 404 {
		t.Errorf("revoked token: code=%d, want 404", rec.Code)
	}
	// The same token before revocation would have rendered — prove the sig itself is valid.
	if !shareSigValid(rid, rexp, signShare(rid, rexp, key), key) {
		t.Errorf("sanity: signature should validate; revocation is what blocks it")
	}
}

func TestParseShareToken(t *testing.T) {
	if _, _, _, ok := parseShareToken("only.two"); ok {
		t.Error("2-part token must be rejected")
	}
	if _, _, _, ok := parseShareToken("id.notanumber.sig"); ok {
		t.Error("non-numeric expiry must be rejected")
	}
	if id, exp, sig, ok := parseShareToken("myid.1700000000.deadbeef"); !ok || id != "myid" || exp != 1700000000 || sig != "deadbeef" {
		t.Errorf("valid token misparsed: %q %d %q %v", id, exp, sig, ok)
	}
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }
