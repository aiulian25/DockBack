package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A database name in a backup request ends up as an argument to a client run as
// root inside the database container. Refusing an impossible one HERE is what
// lets the caller be told what is wrong, instead of a dump that fails minutes
// later inside the container with an opaque client error.
func TestCreateBackupRejectsUnusableDatabaseNames(t *testing.T) {
	s := &Server{store: testStore(t)}

	post := func(dbs []string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{
			"node_id": "n1", "container_id": "cid", "databases": dbs,
		})
		r := httptest.NewRequest("POST", "/api/backups", strings.NewReader(string(body)))
		rec := httptest.NewRecorder()
		s.handleCreateBackup(rec, r)
		return rec
	}

	for what, dbs := range map[string][]string{
		"a quote breakout": {"appdb'; touch /tmp/x; echo '"},
		"a glob":           {"*"},
		"an option":        {"--all-databases"},
		"a path":           {"../../etc/passwd"},
		"one bad of many":  {"appdb", "a;b"},
	} {
		rec := post(dbs)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400: %s", what, rec.Code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), "not usable database names") {
			t.Errorf("%s: the refusal must say what is wrong: %s", what, rec.Body.String())
		}
	}

	// The refusal names every offender, so a caller fixes them in one go.
	body := post([]string{"appdb", "*", "a;b"}).Body.String()
	for _, want := range []string{"*", "a;b"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal must name %q: %s", want, body)
		}
	}

	// Ordinary names get past this check — the node lookup is what fails next,
	// which is the proof it went further.
	if rec := post([]string{"appdb", "my-app", "book.v2"}); rec.Code == http.StatusBadRequest &&
		strings.Contains(rec.Body.String(), "not usable database names") {
		t.Errorf("ordinary names must be accepted: %s", rec.Body.String())
	}
	// And no selection at all is the common case.
	if rec := post(nil); rec.Code == http.StatusBadRequest &&
		strings.Contains(rec.Body.String(), "not usable database names") {
		t.Errorf("a whole-cluster backup must not be refused: %s", rec.Body.String())
	}
}
