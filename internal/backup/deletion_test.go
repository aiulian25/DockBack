package backup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dockback/internal/storage"
	"dockback/internal/store"
)

// DBK-029: a delete may only forget a backup once nothing it names is stored.

const (
	deletionTestNode   = "node1"
	deletionTestTarget = "nginx"
	deletionTestDest   = "d-offsite"
)

// deletionTestBackup stores an archive locally and records it under these
// locations, as a finished backup.
func deletionTestBackup(t *testing.T, e *Engine, id string, locations []Location) *store.Backup {
	t.Helper()
	if _, err := e.Storage.Put(context.Background(), lockArchiveKey, strings.NewReader("ciphertext")); err != nil {
		t.Fatal(err)
	}
	b := &store.Backup{ID: id, NodeID: deletionTestNode, TargetName: deletionTestTarget, Status: "success",
		StorageKey: lockArchiveKey, LocationsJSON: mustJSON(locations), CreatedAt: 1}
	if err := e.Store.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func localCopy() Location { return Location{Kind: "local", Name: "local", Type: "local"} }

func offsiteCopy() Location {
	return Location{Kind: "dest", DestID: deletionTestDest, Name: deletionTestDest, Type: "local"}
}

func storedLocally(t *testing.T, e *Engine) bool {
	t.Helper()
	_, exists, _ := e.Storage.Stat(context.Background(), lockArchiveKey)
	return exists
}

func TestDeleteBackupKeepsTheRowWhileADestinationDoesNotAnswer(t *testing.T) {
	e, st, key, _ := lockTestEngine(t)
	ctx := context.Background()
	// The offsite copy's destination is not there to ask.
	b := deletionTestBackup(t, e, "bk-unreachable", []Location{localCopy(), offsiteCopy()})

	deletion, err := e.DeleteBackup(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if deletion.Removed {
		t.Fatal("a backup with a copy that could not be deleted must stay in the catalog")
	}
	if kept := deletion.Kept(); len(kept) != 1 || kept[0].Location != deletionTestDest || kept[0].Outcome != CopyFailed {
		t.Fatalf("kept = %+v, want only the unreachable offsite copy", kept)
	}
	if storedLocally(t, e) {
		t.Fatal("the local copy could be deleted, so it must be gone")
	}
	row, err := st.GetBackup(b.ID)
	if err != nil {
		t.Fatalf("the row must still be there: %v", err)
	}
	if row.Status != store.BackupStatusDeleting {
		t.Fatalf("status = %q, want %q", row.Status, store.BackupStatusDeleting)
	}
	left := e.locations(row)
	if len(left) != 1 || left[0].DestID != deletionTestDest || left[0].DeleteOutcome != CopyFailed || left[0].DeleteDetail == "" {
		t.Fatalf("the row must list only the copy that is left, with why: %+v", left)
	}
	if left[0].Attempts != 1 || left[0].NextAttemptAt <= time.Now().Unix() {
		t.Fatalf("a failed delete must wait before its next try: %+v", left[0])
	}

	// The destination comes back, holding the copy.
	destDir := t.TempDir()
	lockDest(t, st, key, deletionTestDest, destDir, 0)
	dest, err := storage.NewLocal(destDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dest.Put(ctx, lockArchiveKey, strings.NewReader("ciphertext")); err != nil {
		t.Fatal(err)
	}

	if finished := e.RetryPendingDeletions(ctx, time.Now()); finished != 0 {
		t.Fatal("a delete must not be retried before its wait is over")
	}
	afterWait := time.Unix(left[0].NextAttemptAt, 0).Add(time.Second)
	if finished := e.RetryPendingDeletions(ctx, afterWait); finished != 1 {
		t.Fatalf("finished = %d, want the one pending delete", finished)
	}
	if _, err := st.GetBackup(b.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("once its last copy is gone the row must go too, got %v", err)
	}
	if _, exists, _ := dest.Stat(ctx, lockArchiveKey); exists {
		t.Fatal("the retried delete must have removed the offsite copy")
	}
}

func TestDeleteBackupKeepsTheRowUnderALiveLock(t *testing.T) {
	e, st, key, _ := lockTestEngine(t)
	ctx := context.Background()
	destDir := t.TempDir()
	lockDest(t, st, key, deletionTestDest, destDir, 0)
	dest, err := storage.NewLocal(destDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{lockArchiveKey, lockArchiveKey + ".manifest.json"} {
		if _, err := dest.Put(ctx, name, strings.NewReader("stored")); err != nil {
			t.Fatal(err)
		}
	}
	lockEnds := time.Now().Add(30 * 24 * time.Hour).Unix()
	locked := offsiteCopy()
	locked.Immutable, locked.LockUntil = true, lockEnds
	b := deletionTestBackup(t, e, "bk-locked", []Location{localCopy(), locked})

	deletion, err := e.DeleteBackup(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if deletion.Removed {
		t.Fatal("a backup with a locked copy must stay in the catalog")
	}
	row, _ := st.GetBackup(b.ID)
	left := e.locations(row)
	if len(left) != 1 || left[0].DeleteOutcome != CopyLocked || left[0].NextAttemptAt != lockEnds {
		t.Fatalf("the locked copy must stay listed and wait for its lock to end: %+v", left)
	}
	for _, name := range []string{lockArchiveKey, lockArchiveKey + ".manifest.json"} {
		if _, exists, _ := dest.Stat(ctx, name); !exists {
			t.Fatalf("%s must survive: a copy that stays keeps the files that describe it", name)
		}
	}
	if finished := e.RetryPendingDeletions(ctx, time.Now()); finished != 0 {
		t.Fatal("a locked copy must not be retried while its lock runs")
	}

	// The lock runs out.
	left[0].LockUntil = time.Now().Add(-time.Hour).Unix()
	left[0].NextAttemptAt = left[0].LockUntil
	row.LocationsJSON = mustJSON(left)
	if err := st.UpdateBackup(row); err != nil {
		t.Fatal(err)
	}
	if finished := e.RetryPendingDeletions(ctx, time.Now()); finished != 1 {
		t.Fatalf("finished = %d, want the delete completed once the lock ended", finished)
	}
	if _, err := st.GetBackup(b.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the row must be gone, got %v", err)
	}
	if _, exists, _ := dest.Stat(ctx, lockArchiveKey); exists {
		t.Fatal("the copy must be gone once its lock has ended")
	}
}

func TestDeleteBackupIsRefusedWhileTheTargetIsOnHold(t *testing.T) {
	e, st, _, _ := lockTestEngine(t)
	ctx := context.Background()
	b := deletionTestBackup(t, e, "bk-held", []Location{localCopy()})
	if err := st.SetSetting(RetentionHoldKey(deletionTestNode, deletionTestTarget), "mass-change|1"); err != nil {
		t.Fatal(err)
	}

	if _, err := e.DeleteBackup(ctx, b); !errors.Is(err, ErrTargetHeld) {
		t.Fatalf("err = %v, want ErrTargetHeld", err)
	}
	if row, err := st.GetBackup(b.ID); err != nil || row.Status != "success" {
		t.Fatalf("a refused delete must leave the row as it was: %+v %v", row, err)
	}
	if !storedLocally(t, e) {
		t.Fatal("a refused delete must not touch the archive")
	}
	if held := HeldTargets(st, deletionTestNode); len(held) != 1 || held[0] != deletionTestTarget {
		t.Fatalf("held targets = %v", held)
	}

	// Cleared, the same delete goes through.
	if err := st.DeleteSetting(RetentionHoldKey(deletionTestNode, deletionTestTarget)); err != nil {
		t.Fatal(err)
	}
	deletion, err := e.DeleteBackup(ctx, b)
	if err != nil || !deletion.Removed {
		t.Fatalf("deletion = %+v, err = %v, want the backup removed", deletion, err)
	}
}

// A backup that never stored anything (it failed early) has no copy to remove, and
// an empty storage key must never be handed to a backend as something to delete.
func TestDeleteBackupThatStoredNothingLeavesTheCatalog(t *testing.T) {
	e, st, _, _ := lockTestEngine(t)
	b := &store.Backup{ID: "bk-empty", NodeID: deletionTestNode, TargetName: deletionTestTarget, Status: "failed", CreatedAt: 1}
	if err := st.CreateBackup(b); err != nil {
		t.Fatal(err)
	}
	deletion, err := e.DeleteBackup(context.Background(), b)
	if err != nil || !deletion.Removed {
		t.Fatalf("deletion = %+v, err = %v, want the row removed", deletion, err)
	}
	if _, err := st.GetBackup(b.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the row must be gone, got %v", err)
	}
}

// A backup that is being deleted is not a recovery point: nothing that counts or
// protects successful backups may see it, or it would take a kept backup's place.
func TestADeletingBackupIsNotCountedAsABackup(t *testing.T) {
	e, st, _, _ := lockTestEngine(t)
	b := deletionTestBackup(t, e, "bk-pending", []Location{localCopy(), offsiteCopy()})
	if _, err := e.DeleteBackup(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	parents, err := st.BackupParentMap(deletionTestNode, deletionTestTarget)
	if err != nil || len(parents) != 0 {
		t.Fatalf("parent map = %v, err = %v", parents, err)
	}
	awaitingOffsite, err := st.ListBackupsAwaitingOffsite()
	if err != nil || len(awaitingOffsite) != 0 {
		t.Fatalf("a deleting backup must not be queued for upload: %v %v", awaitingOffsite, err)
	}
	pending, err := st.ListBackupsDeleting()
	if err != nil || len(pending) != 1 || pending[0].ID != b.ID {
		t.Fatalf("pending deletions = %v, err = %v", pending, err)
	}
}
