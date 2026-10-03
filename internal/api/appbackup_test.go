package api

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"dockback/internal/appbackup"
	"dockback/internal/config"
)

// TestPruneAppBackups verifies the automatic-schedule retention (F4): keep the
// newest N stored application backups and delete the rest, and that keep<=0
// keeps everything.
func TestPruneAppBackups(t *testing.T) {
	s := &Server{store: testStore(t), cfg: &config.Config{BackupsDir: t.TempDir()}}

	// A dummy snapshot file to archive (content is irrelevant to retention).
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := os.WriteFile(snap, []byte("dockback-test-snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32) // any 32-byte key; we never decrypt here

	// Create 5 stored backups a second apart (the filename is derived from
	// CreatedAt to the second, so distinct seconds = distinct files).
	base := time.Now().Unix() - 100
	for i := 0; i < 5; i++ {
		if _, err := appbackup.CreateFile(s.appBackupDir(), snap, key, appbackup.Entry{CreatedAt: base + int64(i)}); err != nil {
			t.Fatalf("CreateFile #%d: %v", i, err)
		}
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
	pruned := filepath.Join(s.appBackupDir(), "dockback-config-"+time.Unix(base, 0).UTC().Format("20060102-150405")+".dback")
	if _, err := os.Stat(pruned + ".meta.json"); !os.IsNotExist(err) {
		t.Fatalf("pruned backup's sidecar still present: %v", err)
	}

	// Keeping more than exist is a no-op.
	if n := s.pruneAppBackups(10); n != 0 {
		t.Fatalf("prune(10) deleted %d, want 0", n)
	}
}
