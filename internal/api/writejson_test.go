package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"dockback/internal/backup"
)

// F179 — an empty list must serialize as [], never null.
//
// Reported as a whole page failing to open: "Unexpected Application Error! can't
// access property filter, c is null", on four stacks and no others. The four had
// one thing in common — each contained a service with nothing to mount
// (paperless ships gotenberg and tika, and a socket proxy or a metrics container
// has no volumes of its own). The mounts endpoint answered that honest "none" as
// a nil slice, Go marshalled it as null, and the browser called .filter on it.
//
// Fixed at the boundary, because the call site is wherever the next empty list
// happens to be.
func TestWriteJSONEmptyListIsNotNull(t *testing.T) {
	// The exact shape that caused it.
	var mounts []backup.MountInfo
	rec := httptest.NewRecorder()
	writeJSON(rec, 200, mounts)
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("a nil slice must serialize as [], got %q", got)
	}

	// A nil map is the same hazard wearing different brackets.
	var byName map[string]int
	rec = httptest.NewRecorder()
	writeJSON(rec, 200, byName)
	if got := strings.TrimSpace(rec.Body.String()); got != "{}" {
		t.Errorf("a nil map must serialize as {}, got %q", got)
	}
}

// Everything else has to pass through untouched. This runs on every response the
// API sends, so it must not quietly reshape any of them.
func TestWriteJSONLeavesEverythingElseAlone(t *testing.T) {
	cases := map[string]any{
		`{"a":1}`:       map[string]int{"a": 1},
		`[1,2,3]`:       []int{1, 2, 3},
		`[]`:            []int{},
		`{}`:            map[string]int{},
		`"hi"`:          "hi",
		`7`:             7,
		`true`:          true,
		`null`:          nil,
		`{"error":"x"}`: map[string]string{"error": "x"},
	}
	for want, in := range cases {
		rec := httptest.NewRecorder()
		writeJSON(rec, 200, in)
		if got := strings.TrimSpace(rec.Body.String()); got != want {
			t.Errorf("writeJSON(%#v) = %s, want %s", in, got, want)
		}
	}

	// A struct carrying a nil slice FIELD is deliberately left as it is: the
	// client already reads those defensively, and rewriting arbitrary nested
	// values on the way out is a much larger promise than this makes.
	type resp struct {
		Items []string `json:"items"`
		Name  string   `json:"name"`
	}
	rec := httptest.NewRecorder()
	writeJSON(rec, 200, resp{Name: "x"})
	var back map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if back["items"] != nil {
		t.Errorf("a nested nil slice is out of scope here, got %v", back["items"])
	}
	if back["name"] != "x" {
		t.Errorf("the rest of the struct must be untouched, got %v", back)
	}

	// The status code still reaches the client unchanged.
	rec = httptest.NewRecorder()
	writeJSON(rec, 404, map[string]string{"error": "not found"})
	if rec.Code != 404 {
		t.Errorf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q", ct)
	}
}
