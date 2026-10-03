package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dockback/internal/dockercli"
)

// F222 — favorites keyed by NAME, stored on the server.
//
// The two failures being fixed are one bug each: a container favorite stored the
// container id, which dies on every recreate, and the list lived in localStorage,
// so a second browser started empty.

func favServer(t *testing.T) *Server {
	t.Helper()
	s := nodeBackupServer(t)
	return s
}

func putFavorites(s *Server, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("PUT", "/api/ui/favorites", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleSetFavorites(rec, r)
	return rec
}

func getFavorites(t *testing.T, s *Server) []uiFavorite {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleGetFavorites(rec, httptest.NewRequest("GET", "/api/ui/favorites", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Favorites []uiFavorite `json:"favorites"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Favorites
}

// AC2 — what one browser saves, another reads back. The endpoint is the whole
// mechanism for that, so it round-trips exactly.
func TestFavoritesRoundTrip(t *testing.T) {
	s := favServer(t)

	if got := getFavorites(t, s); len(got) != 0 {
		t.Fatalf("a fresh install has no favorites, got %+v", got)
	}
	// Non-nil even when empty: the menu indexes it unconditionally, and null
	// would be a different answer from "none".
	rec := httptest.NewRecorder()
	s.handleGetFavorites(rec, httptest.NewRequest("GET", "/api/ui/favorites", nil))
	if !strings.Contains(rec.Body.String(), `"favorites":[]`) {
		t.Errorf("empty must serialize as [], got %s", rec.Body.String())
	}

	body := `{"favorites":[
		{"kind":"container","nodeId":"n1","nodeName":"node-one","ref":"paperless","name":"paperless"},
		{"kind":"stack","nodeId":"n1","nodeName":"node-one","ref":"blog","name":"blog"}
	]}`
	if rec := putFavorites(s, body); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	got := getFavorites(t, s)
	if len(got) != 2 {
		t.Fatalf("want both favorites back, got %+v", got)
	}
	if got[0].Kind != "container" || got[0].Ref != "paperless" || got[0].NodeName != "node-one" {
		t.Errorf("the container favorite came back wrong: %+v", got[0])
	}
	if got[1].Kind != "stack" || got[1].Ref != "blog" {
		t.Errorf("the stack favorite came back wrong: %+v", got[1])
	}
	// A save REPLACES — removing a favorite in one browser must remove it.
	if rec := putFavorites(s, `{"favorites":[]}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if got := getFavorites(t, s); len(got) != 0 {
		t.Errorf("an empty save must clear the list, got %+v", got)
	}
}

// A preference row is still operator-supplied JSON this server stores and hands
// back. It is clamped to a known shape rather than kept verbatim.
func TestFavoritesAreSanitized(t *testing.T) {
	long := strings.Repeat("x", 900)
	in := []uiFavorite{
		{Kind: "container", NodeID: "n1", Ref: "app", Name: ""},           // name defaults to ref
		{Kind: "container", NodeID: "n1", Ref: "app", Name: "duplicate"},  // same target twice
		{Kind: "wat", NodeID: "n1", Ref: "app", Name: "unknown kind"},     // not a thing we can run
		{Kind: "stack", NodeID: "", Ref: "blog", Name: "no node"},         // nothing to run it on
		{Kind: "stack", NodeID: "n1", Ref: "  ", Name: "blank ref"},       // ditto
		{Kind: "container", NodeID: "n1", Ref: long, Name: long},          // absurd lengths
		{Kind: " container ", NodeID: " n2 ", Ref: " db ", Name: " db  "}, // padded
	}
	got := sanitizeFavorites(in)

	if len(got) != 3 {
		t.Fatalf("want the 3 usable entries, got %d: %+v", len(got), got)
	}
	if got[0].Name != "app" {
		t.Errorf("a nameless favorite falls back to its ref: %+v", got[0])
	}
	if len(got[1].Ref) != maxFavoriteText || len(got[1].Name) != maxFavoriteText {
		t.Errorf("oversized text must be clamped: ref=%d name=%d", len(got[1].Ref), len(got[1].Name))
	}
	if got[2].Kind != "container" || got[2].NodeID != "n2" || got[2].Ref != "db" || got[2].Name != "db" {
		t.Errorf("padding must be trimmed, not stored: %+v", got[2])
	}
	// Never nil — the menu indexes the result unconditionally.
	if sanitizeFavorites(nil) == nil {
		t.Error("no favorites is an empty list, not nil")
	}
	// The ceiling holds.
	many := make([]uiFavorite, maxFavorites+50)
	for i := range many {
		many[i] = uiFavorite{Kind: "container", NodeID: "n1", Ref: string(rune('a'+i%26)) + strings.Repeat("y", i), Name: "x"}
	}
	if n := len(sanitizeFavorites(many)); n != maxFavorites {
		t.Errorf("list must be capped at %d, got %d", maxFavorites, n)
	}
}

// A malformed body is refused rather than silently clearing somebody's list —
// the destructive reading of "I could not understand this".
func TestFavoritesRejectMalformedBody(t *testing.T) {
	s := favServer(t)
	if rec := putFavorites(s, `{"favorites":[{"kind":"stack","nodeId":"n1","ref":"blog","name":"blog"}]}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if rec := putFavorites(s, "{not json"); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed = %d, want 400", rec.Code)
	}
	if got := getFavorites(t, s); len(got) != 1 {
		t.Errorf("a refused save must leave the list alone, got %+v", got)
	}
}

// AC1 — the favorite fires after the container is recreated: same name, new id.
// This is the whole feature, so it is tested as the sequence it actually is.
func TestCreateBackupByNameSurvivesARecreate(t *testing.T) {
	s := nodeBackupServer(t)
	seedInventory(t, s, "n1", []*dockercli.Container{
		{ID: "old-id-aaaa", Name: "paperless", State: "running"},
	})

	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/backups", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleCreateBackup(rec, r)
		return rec
	}

	rec := post(`{"node_id":"n1","container_name":"paperless","compression":"balanced"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("by name = %d: %s", rec.Code, rec.Body.String())
	}
	// `docker compose up -d`: same name, a different id.
	seedInventory(t, s, "n1", []*dockercli.Container{
		{ID: "new-id-bbbb", Name: "paperless", State: "running"},
	})
	rec = post(`{"node_id":"n1","container_name":"paperless","compression":"balanced"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("after a recreate = %d: %s — this is the bug the feature exists for", rec.Code, rec.Body.String())
	}

	// It resolved to the CURRENT id, not the one it was first told about.
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	if len(s.queue) != 2 {
		t.Fatalf("want two queued runs, got %d", len(s.queue))
	}
	if s.queue[0].opts.ContainerID != "old-id-aaaa" || s.queue[1].opts.ContainerID != "new-id-bbbb" {
		t.Errorf("each run must target the id of the day: %q then %q",
			s.queue[0].opts.ContainerID, s.queue[1].opts.ContainerID)
	}
}

// A name nobody has is a 404 that NAMES it — "not found" about a name the
// operator chose is a different problem from "not found" about an id they never
// saw, and the old favorite failed with a generic message.
func TestCreateBackupByNameReportsAMissingContainer(t *testing.T) {
	s := nodeBackupServer(t)
	seedInventory(t, s, "n1", []*dockercli.Container{{ID: "c1", Name: "app", State: "running"}})

	r := httptest.NewRequest("POST", "/api/backups", strings.NewReader(`{"node_id":"n1","container_name":"ghost"}`))
	rec := httptest.NewRecorder()
	s.handleCreateBackup(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ghost") {
		t.Errorf("the refusal must name what was not found: %s", rec.Body.String())
	}

	// Neither field: still the original bad-request, and it now mentions both
	// ways of asking.
	r = httptest.NewRequest("POST", "/api/backups", strings.NewReader(`{"node_id":"n1"}`))
	rec = httptest.NewRecorder()
	s.handleCreateBackup(rec, r)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "container_name") {
		t.Errorf("want a 400 naming both options, got %d: %s", rec.Code, rec.Body.String())
	}
}

// An explicit id still wins, so every existing caller — the container page, the
// stack dialog, the whole-node run, an API token script — is unchanged.
func TestCreateBackupIDStillTakesPrecedence(t *testing.T) {
	s := nodeBackupServer(t)
	seedInventory(t, s, "n1", []*dockercli.Container{
		{ID: "real-id", Name: "app", State: "running"},
	})

	r := httptest.NewRequest("POST", "/api/backups", strings.NewReader(
		`{"node_id":"n1","container_id":"real-id","container_name":"something-else"}`))
	rec := httptest.NewRecorder()
	s.handleCreateBackup(rec, r)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	if len(s.queue) != 1 || s.queue[0].opts.ContainerID != "real-id" {
		t.Errorf("an explicit id must be used verbatim: %+v", s.queue)
	}
}
