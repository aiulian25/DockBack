package api

import (
	"encoding/json"
	"net/http"
	"strings"
)

// F222 — favorite backup targets, stored where the operator is rather than where
// the browser is.
//
// They lived in localStorage, so a second machine started empty and a cleared
// browser lost the list. And a container favorite stored the container ID, which
// dies on every `docker compose up -d` — after which the favorite silently
// failed with a generic error. Both halves of that are fixed by the same move:
// key by NAME, keep the list on the server.
//
// The list is a UI preference, not a secret and not a policy: node ids, names,
// and the kind. Nothing here grants access to anything — resolving a favorite
// goes through the ordinary backup endpoint with the ordinary checks — so it is
// stored plainly in one settings row and needs no step-up.

// One row, not one per user: this build creates exactly one admin account (the
// bootstrap in auth.go only creates a user when there are none), so "the
// instance's favorites" and "this operator's favorites" are the same set. If
// multiple accounts are ever added, this key needs a user suffix — noted here
// because the failure would be quiet: two people silently sharing a list.
const favoritesSettingKey = "ui.backup_favorites"

// Bounds. A preference row is still operator-supplied JSON that this server
// stores and hands back, so it is parsed into a known shape and clamped rather
// than kept verbatim: a settings row must not become somewhere to park an
// arbitrary payload, and the UI must be able to trust what it renders.
const (
	maxFavorites    = 200 // far more than anyone curates; a ceiling, not a target
	maxFavoriteText = 256 // container/stack/node names are far shorter than this
)

// uiFavorite is one favorite backup target.
type uiFavorite struct {
	Kind   string `json:"kind"`   // "stack" | "container"
	NodeID string `json:"nodeId"` // camelCase: this is the browser's own shape, stored verbatim
	// NodeName is a display cache so the menu paints without a fetch. Advisory —
	// the node id is what anything acts on.
	NodeName string `json:"nodeName"`
	// Ref is the stack PROJECT name, or (F222) the container NAME. It used to be
	// the container id for containers; the client migrates those.
	Ref  string `json:"ref"`
	Name string `json:"name"` // display name
}

// sanitizeFavorites drops what cannot be acted on and clamps what is kept, so a
// malformed or oversized list is corrected rather than refused — a preference is
// not worth failing a save over, but it is worth not storing garbage.
//
// Returns a non-nil slice, so an empty list serializes as [] rather than null.
func sanitizeFavorites(in []uiFavorite) []uiFavorite {
	out := make([]uiFavorite, 0, len(in))
	seen := map[string]bool{}
	for _, f := range in {
		f.Kind = strings.TrimSpace(f.Kind)
		if f.Kind != "stack" && f.Kind != "container" {
			continue
		}
		f.NodeID = clampText(f.NodeID)
		f.Ref = clampText(f.Ref)
		if f.NodeID == "" || f.Ref == "" {
			continue // nothing to run
		}
		f.NodeName = clampText(f.NodeName)
		f.Name = clampText(f.Name)
		if f.Name == "" {
			f.Name = f.Ref
		}
		key := f.Kind + ":" + f.NodeID + ":" + f.Ref
		if seen[key] {
			continue // the same target twice is one favorite
		}
		seen[key] = true
		out = append(out, f)
		if len(out) >= maxFavorites {
			break
		}
	}
	return out
}

func clampText(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxFavoriteText {
		return s[:maxFavoriteText]
	}
	return s
}

// handleGetFavorites returns the stored favorite list.
func (s *Server) handleGetFavorites(w http.ResponseWriter, r *http.Request) {
	raw, _ := s.store.GetSetting(favoritesSettingKey, "")
	list := []uiFavorite{}
	if strings.TrimSpace(raw) != "" {
		// A row this server wrote should always parse; if it somehow does not,
		// an empty list is better than a 500 on a sidebar button.
		_ = json.Unmarshal([]byte(raw), &list)
	}
	writeJSON(w, http.StatusOK, map[string]any{"favorites": sanitizeFavorites(list)})
}

// handleSetFavorites replaces the stored favorite list.
func (s *Server) handleSetFavorites(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Favorites []uiFavorite `json:"favorites"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	list := sanitizeFavorites(body.Favorites)
	js, err := json.Marshal(list)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid favorites")
		return
	}
	if err := s.store.SetSetting(favoritesSettingKey, string(js)); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Deliberately NOT audited. The audit trail records administrative acts on
	// data and access; which buttons somebody likes on their own sidebar is
	// neither, and writing a row every time a checkbox moves would bury the
	// entries that matter.
	writeJSON(w, http.StatusOK, map[string]any{"favorites": list})
}
