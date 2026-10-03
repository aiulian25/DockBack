package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dockback/internal/store"
)

// readJSON caps the body and refuses unknown fields. Three handlers wired their
// own decoder or discarded its error, so those protections simply did not apply
// to them.

func TestReadJSONCapsAndRejectsUnknownFields(t *testing.T) {
	var body struct {
		Enabled bool `json:"enabled"`
	}

	// A field nobody declared is a caller error, not something to ignore.
	r := httptest.NewRequest("POST", "/x", strings.NewReader(`{"enabled":true,"rpo_seconds":5}`))
	if err := readJSON(r, &body); err == nil {
		t.Error("an unknown field must be refused — silently dropping it hides a typo in a caller")
	}

	// An oversized body is cut off rather than read into memory, which shows up
	// as a decode failure.
	huge := `{"enabled":true,"pad":"` + strings.Repeat("x", 2<<20) + `"}`
	r = httptest.NewRequest("POST", "/x", strings.NewReader(huge))
	if err := readJSON(r, &body); err == nil {
		t.Error("a body past the cap must not decode")
	}

	// The ordinary case still works.
	r = httptest.NewRequest("POST", "/x", strings.NewReader(`{"enabled":true}`))
	if err := readJSON(r, &body); err != nil || !body.Enabled {
		t.Errorf("a valid body must decode: %v", err)
	}
}

// The mirror endpoint treats an absent body as "every enabled destination".
// A MALFORMED body is not the same thing: discarding the error meant a request
// that named two destinations and got the JSON wrong mirrored to all of them.
func TestMirrorRejectsAMalformedBodyButAllowsNone(t *testing.T) {
	s := &Server{store: testStore(t)}
	// A real, completed backup, so the handler reaches the body at all.
	b := &store.Backup{ID: "b1", NodeID: "n1", TargetName: "app", Status: "success", CreatedAt: 1000}
	if err := s.store.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	b.StorageKey = "k/b1"
	if err := s.store.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}

	post := func(body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body == "" {
			r = httptest.NewRequest("POST", "/api/backups/b1/mirror", nil)
		} else {
			r = httptest.NewRequest("POST", "/api/backups/b1/mirror", strings.NewReader(body))
		}
		r.SetPathValue("id", "b1")
		rec := httptest.NewRecorder()
		s.handleMirrorBackup(rec, r)
		return rec
	}

	// Malformed: refused, and nothing is started.
	if rec := post(`{"destinations": [`); rec.Code != http.StatusBadRequest {
		t.Errorf("a malformed body = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	// An unknown field is malformed too.
	if rec := post(`{"destination":"typo"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown field = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	// Absent body: still the documented "every enabled destination" path, which
	// gets past the decode and fails later on the missing backup.
	if rec := post(""); rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "invalid request") {
		t.Errorf("an absent body must still be accepted: %s", rec.Body.String())
	}
}
