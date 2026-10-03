package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"dockback/internal/store"
)

// A hand-arranged node list. The store returns nodes name-ascending, which is a
// fine default and a poor answer for an operator whose fleet has a shape — the
// two hosts they touch daily and the eight they do not.
//
// Ordering is presentation, so it lives here and not in the store's ORDER BY:
// the scheduler, retention and coverage all read the same ListNodes and none of
// them care what the sidebar looks like.
//
// There is deliberately no GET. GET /api/nodes already answers in the saved
// order, so the array the browser is holding IS the order.

// One row, not one per user — same reasoning as favoritesSettingKey: this build
// creates exactly one admin account, so the instance's order and the operator's
// order are the same list.
const nodeOrderSettingKey = "ui.node_order"

// Bounds, for the same reason favorites has them: a preference row is still
// operator-supplied JSON this server stores and hands back.
const (
	maxNodeOrderIDs   = 100 // a fleet of hosts, not a list of services; a ceiling, not a target
	maxNodeIDTextSize = 64  // node ids are generated here and are far shorter
)

// sanitizeNodeOrder drops what cannot name a node and clamps what is kept.
//
// Returns a non-nil slice, so an empty order serializes as [] rather than null.
func sanitizeNodeOrder(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, id := range in {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if len(id) > maxNodeIDTextSize {
			id = id[:maxNodeIDTextSize]
		}
		if seen[id] {
			continue // the same node twice is one position
		}
		seen[id] = true
		out = append(out, id)
		if len(out) >= maxNodeOrderIDs {
			break
		}
	}
	return out
}

// applyNodeOrder returns nodes rearranged to follow order: the ids named in
// order come first in that sequence, everything else keeps its existing
// relative order (name-ascending, from the store) behind them.
//
// An id naming no node is skipped rather than leaving a gap, and a node the
// saved order has never seen lands at the end — so adding a node appends it
// instead of shuffling an arrangement the operator already made.
//
// Pure: the input slice is neither reordered nor retained.
func applyNodeOrder(nodes []*store.Node, order []string) []*store.Node {
	if len(order) == 0 {
		return nodes
	}
	position := make(map[string]int, len(order))
	for i, id := range order {
		position[id] = i
	}
	arranged := make([]*store.Node, 0, len(nodes))
	rest := make([]*store.Node, 0, len(nodes))
	for _, n := range nodes {
		if _, ok := position[n.ID]; !ok {
			rest = append(rest, n)
			continue
		}
		arranged = append(arranged, n)
	}
	sort.SliceStable(arranged, func(a, b int) bool {
		return position[arranged[a].ID] < position[arranged[b].ID]
	})
	return append(arranged, rest...)
}

// storedNodeOrder reads the saved order, or nothing when there is none.
func (s *Server) storedNodeOrder() []string {
	raw, _ := s.store.GetSetting(nodeOrderSettingKey, "")
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	order := []string{}
	// A row this server wrote should always parse; if it somehow does not, the
	// default order is better than a 500 on the node list.
	_ = json.Unmarshal([]byte(raw), &order)
	return sanitizeNodeOrder(order)
}

// handleSetNodeOrder replaces the stored node order.
func (s *Server) handleSetNodeOrder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Order []string `json:"order"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	order := sanitizeNodeOrder(body.Order)
	js, err := json.Marshal(order)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid order")
		return
	}
	if err := s.store.SetSetting(nodeOrderSettingKey, string(js)); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Deliberately NOT audited, as with favorites: dragging a card is not an act
	// on data or access, and a row per drag would bury the entries that matter.
	writeJSON(w, http.StatusOK, map[string]any{"order": order})
}
