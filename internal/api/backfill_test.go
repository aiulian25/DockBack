package api

import (
	"encoding/json"
	"testing"

	"dockback/internal/backup"
	"dockback/internal/store"
)

// locs builds a LocationsJSON string.
func locsJSON(ls ...backup.Location) string { b, _ := json.Marshal(ls); return string(b) }

func healthyDest(id string) backup.Location {
	return backup.Location{Kind: "dest", DestID: id, Name: id, Type: "smb"}
}
func failedDest(id string) backup.Location {
	return backup.Location{Kind: "dest", DestID: id, Name: id, Type: "smb", Status: "failed"}
}
func localLoc() backup.Location { return backup.Location{Kind: "local", Name: "local", Type: "local"} }

func TestBackfillCandidates(t *testing.T) {
	all := []*store.Backup{
		// Node n1's policy includes d1.
		{ID: "b1", NodeID: "n1", Status: "success", CreatedAt: 300, LocationsJSON: locsJSON(localLoc())},                    // no copy → candidate
		{ID: "b2", NodeID: "n1", Status: "success", CreatedAt: 100, LocationsJSON: locsJSON(localLoc(), healthyDest("d1"))}, // healthy copy → excluded
		{ID: "b3", NodeID: "n1", Status: "success", CreatedAt: 200, LocationsJSON: locsJSON(localLoc(), failedDest("d1"))},  // failed copy → re-attempt
		{ID: "bf", NodeID: "n1", Status: "failed", CreatedAt: 50, LocationsJSON: locsJSON(localLoc())},                      // not success → skip
		// Node n2's policy EXCLUDES d1 → its backups are skipped even with no copy.
		{ID: "b4", NodeID: "n2", Status: "success", CreatedAt: 10, LocationsJSON: locsJSON(localLoc())},
	}
	eff := func(nodeID string) []string {
		if nodeID == "n1" {
			return []string{"d1", "d2"}
		}
		return []string{"d2"} // n2 excludes d1
	}

	got := backfillCandidates(all, "d1", eff)
	// Expect b3 (created 200) then b1 (created 300) — oldest-first; b2 excluded
	// (healthy), bf excluded (failed status), b4 excluded (policy).
	if len(got) != 2 {
		t.Fatalf("want 2 candidates, got %d: %+v", len(got), got)
	}
	if got[0].ID != "b3" || got[1].ID != "b1" {
		t.Fatalf("want oldest-first [b3,b1], got [%s,%s]", got[0].ID, got[1].ID)
	}
}

func TestBackfillCandidatesNoneWhenAllCovered(t *testing.T) {
	all := []*store.Backup{
		{ID: "b1", NodeID: "n1", Status: "success", CreatedAt: 1, LocationsJSON: locsJSON(healthyDest("d1"))},
	}
	eff := func(string) []string { return []string{"d1"} }
	if got := backfillCandidates(all, "d1", eff); len(got) != 0 {
		t.Fatalf("all covered → want 0, got %d", len(got))
	}
}
