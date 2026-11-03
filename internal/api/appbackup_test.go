package api

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"dockback/internal/appbackup"
	"dockback/internal/backup"
	"dockback/internal/config"
)

// TestPruneAppBackups verifies the automatic-schedule retention (F4): keep the
// newest N stored application backups and delete the rest, and that keep<=0
// keeps everything.
func TestPruneAppBackups(t *testing.T) {
	s := &Server{store: testStore(t), cfg: &config.Config{BackupsDir: t.TempDir()}}

	// A creator proves each archive restores before publishing it, so the snapshot
	// must be a real one.
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := s.store.SnapshotTo(snap); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)

	// Create 5 stored backups a second apart.
	base := time.Now().Unix() - 100
	created := make([]appbackup.Entry, 5)
	for i := range created {
		entry, err := appbackup.CreateFile(s.appBackupDir(), snap, key, appbackup.Entry{CreatedAt: base + int64(i)}, t.TempDir())
		if err != nil {
			t.Fatalf("CreateFile #%d: %v", i, err)
		}
		created[i] = entry
	}

	// keep<=0 must be a no-op.
	if n := s.pruneAppBackups(0); n != 0 {
		t.Fatalf("prune(0) deleted %d, want 0", n)
	}
	if list, _ := appbackup.List(s.appBackupDir()); len(list) != 5 {
		t.Fatalf("after prune(0): %d backups, want 5", len(list))
	}

	// Keep the newest 2 → 3 deleted.
	if n := s.pruneAppBackups(2); n != 3 {
		t.Fatalf("prune(2) deleted %d, want 3", n)
	}
	list, err := appbackup.List(s.appBackupDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("after prune(2): %d backups, want 2", len(list))
	}
	// List is newest-first; the two survivors must be the newest CreatedAt.
	if list[0].CreatedAt != base+4 || list[1].CreatedAt != base+3 {
		t.Fatalf("kept the wrong backups: %d, %d (want %d, %d)", list[0].CreatedAt, list[1].CreatedAt, base+4, base+3)
	}
	// The sidecar .meta.json of a pruned backup is gone too.
	pruned := filepath.Join(s.appBackupDir(), created[0].File)
	if _, err := os.Stat(pruned + ".meta.json"); !os.IsNotExist(err) {
		t.Fatalf("pruned backup's sidecar still present: %v", err)
	}

	// Keeping more than exist is a no-op.
	if n := s.pruneAppBackups(10); n != 0 {
		t.Fatalf("prune(10) deleted %d, want 0", n)
	}
}

// The manual button, the schedule and a key rotation can ask for a backup at the
// same moment. Each gets its own published backup.
func TestCreatorsAtTheSameMomentEachGetTheirOwnBackup(t *testing.T) {
	const creators = 3
	s := &Server{store: testStore(t), cfg: &config.Config{BackupsDir: t.TempDir(), TmpDir: t.TempDir(), EncryptionKey: make([]byte, 32)}, engine: &backup.Engine{}}

	var wg sync.WaitGroup
	failures := make(chan error, creators)
	for range creators {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.createAppBackup("tester"); err != nil {
				failures <- err
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatalf("a creator failed: %v", err)
	}
	list, err := appbackup.List(s.appBackupDir())
	if err != nil || len(list) != creators {
		t.Fatalf("listed %d backups, want %d: %v", len(list), creators, err)
	}
}
