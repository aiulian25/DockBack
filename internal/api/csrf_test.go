package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"dockback/internal/crypto"
	"dockback/internal/store"
)

// TestCSRFRequiredOnMutations locks in PLAN §3.2: every state-changing endpoint
// must reject a request that lacks the double-submit CSRF token, even with a
// valid session. The check runs against the real routed handler, so a future
// mutating route that forgets the s.csrf wrapper makes this test fail. (The
// csrf middleware runs before the handler, so no backend deps are exercised.)
func TestCSRFRequiredOnMutations(t *testing.T) {
	s := newTestServer(t)
	hash, _ := crypto.HashPassword("currentpass-123")
	uid, err := s.store.CreateUser("admin", hash)
	if err != nil {
		t.Fatal(err)
	}
	// A valid session so auth passes and we reach the csrf check.
	if err := s.store.CreateSession("tok", uid, time.Hour); err != nil {
		t.Fatal(err)
	}
	_ = s.store.SetSetting("csrf:"+store.SessionKey("tok"), "the-real-token")

	h := s.Handler(fstest.MapFS{})

	// Representative state-changing endpoints (must be CSRF-protected).
	mutations := []struct{ method, path string }{
		{"POST", "/api/session/extend"},
		{"POST", "/api/session/activity"},
		{"POST", "/api/session/revoke-others"},
		{"POST", "/api/account/password"},
		{"POST", "/api/account/2fa/begin"},
		{"POST", "/api/account/2fa/enable"},
		{"POST", "/api/account/2fa/disable"},
		{"POST", "/api/app-backup/create"},
		{"POST", "/api/app-backup/restore"},
		{"POST", "/api/app-backup/restore-local"},
		{"DELETE", "/api/app-backup/some-file"},
		{"POST", "/api/app-backup/destinations"},
		{"PUT", "/api/app-backup/destinations/d1"},
		{"DELETE", "/api/app-backup/destinations/d1"},
		{"POST", "/api/app-backup/external"},
		{"POST", "/api/app-backup/destinations/d1/restore"},
		{"POST", "/api/nodes"},
		{"POST", "/api/nodes/test"},
		{"PUT", "/api/nodes/n1"},
		{"DELETE", "/api/nodes/n1"},
		{"POST", "/api/nodes/n1/stacks/proj/backup"},
		{"POST", "/api/nodes/n1/stacks/proj/restore"},
		{"PUT", "/api/nodes/n1/containers/c1/hooks"},
		{"PUT", "/api/nodes/n1/containers/c1/pause-mode"},
		{"PUT", "/api/nodes/n1/containers/c1/policy"},
		{"PUT", "/api/nodes/n1/containers/c1/export-profile"},
		{"POST", "/api/backups"},
		{"DELETE", "/api/backups/b1"},
		{"POST", "/api/backups/delete"},
		{"POST", "/api/backups/b1/cancel"},
		{"POST", "/api/backups/b1/restore"},
		{"POST", "/api/backups/b1/mirror"},
		{"POST", "/api/backups/b1/restore-file"},
		{"POST", "/api/export-presets"},
		{"DELETE", "/api/export-presets/p1"},
		{"PUT", "/api/policy"},
		{"POST", "/api/policy/run"},
		{"POST", "/api/destinations"},
		{"POST", "/api/destinations/test"},
		{"PUT", "/api/destinations/d1"},
		{"POST", "/api/destinations/d1/test"},
		{"DELETE", "/api/destinations/d1"},
		{"POST", "/api/settings"},
		{"PUT", "/api/notifications"},
		{"POST", "/api/notifications/test"},
	}

	for _, m := range mutations {
		t.Run(m.method+" "+m.path, func(t *testing.T) {
			// Authenticated, but NO X-CSRF-Token header.
			r := httptest.NewRequest(m.method, m.path, nil)
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("%s %s without CSRF token: want 403, got %d (%s) — is the route s.csrf-wrapped?",
					m.method, m.path, w.Code, w.Body.String())
			}
		})
	}

	// Sanity: with the correct token the csrf gate passes (so it isn't just
	// blanket-denying). We don't assert a specific downstream code — only that it
	// is NOT the csrf 403.
	t.Run("passes with correct token", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/api/session/activity", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
		r.Header.Set(csrfHeader, "the-real-token")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusForbidden {
			t.Fatalf("valid CSRF token should pass the gate, got 403: %s", w.Body.String())
		}
	})
}
