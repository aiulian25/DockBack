package api

import (
	"testing"

	"dockback/internal/storage"
)

// F42: the primary backups volume is capacity-sampled through the SAME path as any
// destination, keyed by the synthetic local:primary id — so a filling backups
// volume feeds the trend history + forecast instead of only failing a pre-flight.
func TestCheckOneDestinationSamplesPrimary(t *testing.T) {
	s := &Server{store: testStore(t)}
	be, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// No samples for the primary yet.
	if rows, _ := s.store.DestSamples(localPrimaryID, 0); len(rows) != 0 {
		t.Fatalf("expected no samples before the check, got %d", len(rows))
	}

	s.checkOneDestination(localPrimaryID, localPrimaryName, be)

	rows, err := s.store.DestSamples(localPrimaryID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly one sample for %q, got %d", localPrimaryID, len(rows))
	}
	// A real filesystem reports a non-zero total, so the local volume now has the
	// capacity figure it previously always lacked (total=0 → never alertable).
	if rows[0].Total == 0 {
		t.Errorf("primary volume sample should record a non-zero total, got %d", rows[0].Total)
	}
	if rows[0].Used > rows[0].Total {
		t.Errorf("used (%d) must not exceed total (%d)", rows[0].Used, rows[0].Total)
	}
}
