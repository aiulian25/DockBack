package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"dockback/internal/store"
)

// A hand-arranged node list. The whole feature is applyNodeOrder — the endpoint
// only parks a JSON array in a settings row — so that is where the tests are.

func nodesNamed(ids ...string) []*store.Node {
	out := make([]*store.Node, len(ids))
	for i, id := range ids {
		out[i] = &store.Node{ID: id, Name: id}
	}
	return out
}

func orderOf(nodes []*store.Node) string {
	ids := make([]string, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID
	}
	return strings.Join(ids, ",")
}

func TestApplyNodeOrder(t *testing.T) {
	// The store hands them over name-ascending; the operator wants otherwise.
	nodes := nodesNamed("alpha", "bravo", "charlie")
	if got := orderOf(applyNodeOrder(nodes, []string{"charlie", "alpha", "bravo"})); got != "charlie,alpha,bravo" {
		t.Errorf("the saved arrangement must be honoured, got %s", got)
	}

	// A node the order has never seen goes last, keeping its place among the
	// others — adding a node appends it, it does not reshuffle the arrangement.
	nodes = nodesNamed("alpha", "bravo", "charlie", "delta")
	if got := orderOf(applyNodeOrder(nodes, []string{"delta", "bravo"})); got != "delta,bravo,alpha,charlie" {
		t.Errorf("unordered nodes go last in their existing order, got %s", got)
	}

	// A forgotten node leaves no hole.
	nodes = nodesNamed("alpha", "bravo")
	if got := orderOf(applyNodeOrder(nodes, []string{"ghost", "bravo", "alpha"})); got != "bravo,alpha" {
		t.Errorf("an id matching no node must be skipped, got %s", got)
	}

	// No preference saved yet: the store's own order stands.
	nodes = nodesNamed("alpha", "bravo")
	if got := orderOf(applyNodeOrder(nodes, nil)); got != "alpha,bravo" {
		t.Errorf("no order means unchanged, got %s", got)
	}

	// The caller's slice is the store's slice; rearranging the view must not
	// rearrange it.
	nodes = nodesNamed("alpha", "bravo", "charlie")
	applyNodeOrder(nodes, []string{"charlie", "bravo"})
	if got := orderOf(nodes); got != "alpha,bravo,charlie" {
		t.Errorf("the input slice must not be mutated, got %s", got)
	}
}

func TestSanitizeNodeOrder(t *testing.T) {
	got := sanitizeNodeOrder([]string{" n1 ", "", "  ", "n2", "n1", strings.Repeat("z", 900)})
	if strings.Join(got, ",") != "n1,n2,"+strings.Repeat("z", maxNodeIDTextSize) {
		t.Errorf("want trimmed, deduped, blank-free and clamped, got %+v", got)
	}
	// Never nil — the client indexes the result unconditionally.
	if sanitizeNodeOrder(nil) == nil {
		t.Error("no order is an empty list, not nil")
	}
	// The ceiling holds.
	many := make([]string, maxNodeOrderIDs+50)
	for i := range many {
		many[i] = "n" + strconv.Itoa(i)
	}
	if n := len(sanitizeNodeOrder(many)); n != maxNodeOrderIDs {
		t.Errorf("order must be capped at %d, got %d", maxNodeOrderIDs, n)
	}
}

// What the browser saves is what the next node list is arranged by — the row and
// the list handler have to agree on the key, so the round trip is tested through
// both halves.
func TestNodeOrderRoundTrip(t *testing.T) {
	s := nodeBackupServer(t)
	if err := s.store.UpsertNode(&store.Node{ID: "n2", Name: "node-two", Transport: "socket"}); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("PUT", "/api/ui/node-order", strings.NewReader(`{"order":["n2"," n1 ","n2"]}`))
	rec := httptest.NewRecorder()
	s.handleSetNodeOrder(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Order []string `json:"order"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Join(out.Order, ",") != "n2,n1" {
		t.Errorf("the sanitized order comes back, got %+v", out.Order)
	}

	stored, err := s.store.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	if got := orderOf(applyNodeOrder(stored, s.storedNodeOrder())); got != "n2,n1" {
		t.Errorf("the node list must read back in the saved order, got %s", got)
	}

	// An unparseable row is not a 500 on the node list.
	if err := s.store.SetSetting(nodeOrderSettingKey, "{not json"); err != nil {
		t.Fatal(err)
	}
	if got := s.storedNodeOrder(); len(got) != 0 {
		t.Errorf("a broken row means no order, got %+v", got)
	}

	// A malformed body is refused rather than silently flattening the order.
	rec = httptest.NewRecorder()
	s.handleSetNodeOrder(rec, httptest.NewRequest("PUT", "/api/ui/node-order", strings.NewReader("{not json")))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed = %d, want 400", rec.Code)
	}
}
