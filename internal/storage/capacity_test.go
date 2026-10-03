package storage

import (
	"context"
	"testing"
)

// F42: the local/primary backups volume must report total capacity (not just
// free), so it satisfies the Capacity interface and is covered by the same
// "almost full" / "filling up" alerts and forecast as an offsite destination.
var _ Capacity = (*Local)(nil)

func TestLocalCapacity(t *testing.T) {
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	total, err := l.TotalBytes(ctx)
	if err != nil {
		t.Fatalf("TotalBytes: %v", err)
	}
	free, err := l.FreeBytes(ctx)
	if err != nil {
		t.Fatalf("FreeBytes: %v", err)
	}
	if total == 0 {
		t.Fatal("TotalBytes should be non-zero on a real filesystem")
	}
	if total < free {
		t.Fatalf("TotalBytes (%d) must be >= FreeBytes (%d)", total, free)
	}

	// A backend built via the factory must also expose Capacity (the assertion the
	// alert monitor makes, alerts.go).
	if _, ok := interface{}(l).(Capacity); !ok {
		t.Fatal("Local must satisfy storage.Capacity")
	}
}
