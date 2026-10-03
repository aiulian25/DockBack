package backup

import (
	"encoding/json"
	"testing"
	"time"

	"dockback/internal/store"
)

func mkBackup(locs []Location) *store.Backup {
	b, _ := json.Marshal(locs)
	return &store.Backup{ID: "b1", LocationsJSON: string(b)}
}

func TestHasLiveImmutable(t *testing.T) {
	e := &Engine{} // hasLiveImmutable/locations don't log or touch other deps
	future := time.Now().Add(48 * time.Hour).Unix()
	past := time.Now().Add(-48 * time.Hour).Unix()

	// A live (unexpired) immutable offsite copy → retained.
	if !e.hasLiveImmutable(mkBackup([]Location{
		{Kind: "local", Name: "local"},
		{Kind: "dest", Name: "s3", Immutable: true, LockUntil: future},
	})) {
		t.Error("expected live immutable copy to be detected")
	}

	// Expired lock → no longer protected.
	if e.hasLiveImmutable(mkBackup([]Location{{Kind: "dest", Name: "s3", Immutable: true, LockUntil: past}})) {
		t.Error("expired lock must not count as live")
	}

	// A failed (never-uploaded) immutable copy doesn't count.
	if e.hasLiveImmutable(mkBackup([]Location{{Kind: "dest", Name: "s3", Immutable: true, LockUntil: future, Status: "failed"}})) {
		t.Error("failed copy must not count")
	}

	// No immutable copies at all.
	if e.hasLiveImmutable(mkBackup([]Location{{Kind: "local", Name: "local"}})) {
		t.Error("local-only backup is not immutable")
	}
}
