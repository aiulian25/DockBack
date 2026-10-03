package api

import (
	"encoding/json"
	"testing"

	"dockback/internal/backup"
	"dockback/internal/store"
)

// locJSON builds a LocationsJSON string from the given locations.
func locJSON(locs ...backup.Location) string {
	b, _ := json.Marshal(locs)
	return string(b)
}

func boolp(v bool) *bool { return &v }

// TestScrubBatchRotation proves scrubs rotate across a backup's copies (F44): when
// the local copy was verified recently but a destination copy is stale, the next
// pick is the destination.
func TestScrubBatchRotation(t *testing.T) {
	const cutoff = 1000
	b := &store.Backup{
		ID: "b1", Status: "success", LastVerifiedAt: 2000,
		LocationsJSON: locJSON(
			backup.Location{Kind: "local", Name: "local", Type: "local", VerifiedAt: 2000, VerifyOK: boolp(true)},
			backup.Location{Kind: "dest", DestID: "d1", Name: "NAS", Type: "smb", VerifiedAt: 0},
		),
	}
	batch := selectScrubBatch([]*store.Backup{b}, cutoff, 3)
	if len(batch) != 1 {
		t.Fatalf("local is fresh (2000 >= cutoff), only the dest is due — want 1 pick, got %d: %+v", len(batch), batch)
	}
	if batch[0].Source != "d1" {
		t.Fatalf("next pick must be the stale destination copy, got source %q", batch[0].Source)
	}
}

// TestScrubBatchAdoptedDestOnly: an adopted backup with ONLY a destination copy
// (no local) is scrubbed against that destination, never spuriously against local.
func TestScrubBatchAdoptedDestOnly(t *testing.T) {
	b := &store.Backup{
		ID: "b2", Status: "success", LastVerifiedAt: 0,
		LocationsJSON: locJSON(backup.Location{Kind: "dest", DestID: "d9", Name: "B2", Type: "s3", VerifiedAt: 0}),
	}
	batch := selectScrubBatch([]*store.Backup{b}, 1000, 3)
	if len(batch) != 1 || batch[0].Source != "d9" {
		t.Fatalf("adopted backup must scrub its dest copy, got %+v", batch)
	}
}

// TestScrubBatchLegacyAndSkips: legacy rows (no LocationsJSON) fall back to a local
// copy keyed on the global clock; failed/deferred copies are never selected.
func TestScrubBatchLegacyAndSkips(t *testing.T) {
	legacy := &store.Backup{ID: "leg", Status: "success", LastVerifiedAt: 10} // no locations
	failedDest := &store.Backup{
		ID: "fd", Status: "success", LastVerifiedAt: 0,
		LocationsJSON: locJSON(
			backup.Location{Kind: "local", Name: "local", Type: "local", VerifiedAt: 5},
			backup.Location{Kind: "dest", DestID: "dx", Name: "X", Type: "s3", Status: "failed"},   // skip
			backup.Location{Kind: "dest", DestID: "dy", Name: "Y", Type: "s3", Status: "deferred"}, // skip
		),
	}
	batch := selectScrubBatch([]*store.Backup{legacy, failedDest}, 1000, 10)
	// legacy(local@10) + fd(local@5) = 2 pairs; the failed/deferred dests are skipped.
	if len(batch) != 2 {
		t.Fatalf("expected 2 pairs (both local; failed/deferred skipped), got %d: %+v", len(batch), batch)
	}
	for _, p := range batch {
		if p.Source != "local" {
			t.Errorf("only local copies should be due here, got source %q", p.Source)
		}
	}
	// Least-recently-verified first: fd(5) before legacy(10).
	if batch[0].Backup.ID != "fd" {
		t.Errorf("least-recently-verified must sort first, got %q", batch[0].Backup.ID)
	}

	// Per-cycle cap is honored.
	if capped := selectScrubBatch([]*store.Backup{legacy, failedDest}, 1000, 1); len(capped) != 1 {
		t.Errorf("per-cycle cap not applied: got %d", len(capped))
	}
}
