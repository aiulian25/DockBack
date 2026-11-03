package appbackup

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func createStored(t *testing.T, dir, snapshot string, key []byte, e Entry) (Entry, error) {
	t.Helper()
	return CreateFile(dir, snapshot, key, e, t.TempDir())
}

func testKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// A manual backup, the schedule and a key rotation can all start in the same second.
// Each must end up as its own intact backup, not truncate or replace another.
func TestBackupsInTheSameSecondAreKeptApart(t *testing.T) {
	const creators = 4
	const sameSecond = 1700000000
	dir := t.TempDir()
	snapshot := makeSnapshot(t)
	key := testKey(t)

	var wg sync.WaitGroup
	results := make([]Entry, creators)
	failures := make([]error, creators)
	for i := range creators {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], failures[i] = createStored(t, dir, snapshot, key, Entry{CreatedAt: sameSecond})
		}()
	}
	wg.Wait()

	seen := map[string]bool{}
	for i := range creators {
		if failures[i] != nil {
			t.Fatalf("creator %d failed: %v", i, failures[i])
		}
		seen[results[i].File] = true
	}
	if len(seen) != creators {
		t.Fatalf("%d creators produced %d distinct backups: %v", creators, len(seen), results)
	}
	listed, err := List(dir)
	if err != nil || len(listed) != creators {
		t.Fatalf("listed %d backups, want %d: %v", len(listed), creators, err)
	}
	for _, entry := range listed {
		if err := VerifyFile(dir, entry.File, key, t.TempDir()); err != nil {
			t.Fatalf("%s does not restore: %v", entry.File, err)
		}
	}
}

// A creator that fails must clean up only what it made. It used to truncate and then
// delete the final path, which in the same second was another creator's backup.
func TestAFailedBackupLeavesItsNeighbourAlone(t *testing.T) {
	const sameSecond = 1700000000
	dir := t.TempDir()
	key := testKey(t)
	good, err := createStored(t, dir, makeSnapshot(t), key, Entry{CreatedAt: sameSecond})
	if err != nil {
		t.Fatal(err)
	}

	missingSnapshot := filepath.Join(t.TempDir(), "gone.db")
	if _, err := createStored(t, dir, missingSnapshot, key, Entry{CreatedAt: sameSecond}); err == nil {
		t.Fatal("a backup of a missing snapshot must fail")
	}

	if err := VerifyFile(dir, good.File, key, t.TempDir()); err != nil {
		t.Fatalf("the earlier backup was damaged: %v", err)
	}
	if names := dirNames(t, dir); len(names) != 2 {
		t.Fatalf("want only the good backup and its sidecar, got %v", names)
	}
}

// What is listed is what a restore can use: an archive that does not open as a
// DockBack database is never published, and nothing of it is left behind.
func TestABackupThatDoesNotVerifyIsNeverPublished(t *testing.T) {
	dir := t.TempDir()
	notADatabase := filepath.Join(t.TempDir(), "snap.db")
	if err := os.WriteFile(notADatabase, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := createStored(t, dir, notADatabase, testKey(t), Entry{CreatedAt: 1700000000}); err == nil {
		t.Fatal("an archive that does not verify must not be reported as created")
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("nothing may be published or left behind, got %v", names)
	}
}

// Backups made by earlier versions carry no suffix in their names; they must still
// be listed, verified and deleted.
func TestOlderNamesStayReadable(t *testing.T) {
	dir := t.TempDir()
	key := testKey(t)
	created, err := createStored(t, dir, makeSnapshot(t), key, Entry{CreatedAt: 1700000000})
	if err != nil {
		t.Fatal(err)
	}
	const legacyName = "dockback-config-20231114-221320.dback"
	if err := os.Rename(filepath.Join(dir, created.File), filepath.Join(dir, legacyName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, created.File+metaSuffix)); err != nil {
		t.Fatal(err)
	}

	listed, err := List(dir)
	if err != nil || len(listed) != 1 || listed[0].File != legacyName {
		t.Fatalf("the legacy backup is not listed: %v %v", listed, err)
	}
	if err := VerifyFile(dir, legacyName, key, t.TempDir()); err != nil {
		t.Fatalf("the legacy backup does not verify: %v", err)
	}
	if err := DeleteFile(dir, legacyName); err != nil {
		t.Fatalf("the legacy backup cannot be deleted: %v", err)
	}

	now := time.Now()
	first, second := NewName(now), NewName(now)
	if first == second || !ValidName(first) || !ValidName(second) {
		t.Fatalf("new names must be valid and distinct within a second: %q %q", first, second)
	}
}

// A creator interrupted by a crash leaves a partial file. It is never listed, and
// clearing partials removes it without touching a published backup.
func TestInterruptedPartialsAreHiddenAndCleared(t *testing.T) {
	dir := t.TempDir()
	published, err := createStored(t, dir, makeSnapshot(t), testKey(t), Entry{CreatedAt: 1700000000})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, partialPrefix+"interrupted"), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}

	if listed, _ := List(dir); len(listed) != 1 || listed[0].File != published.File {
		t.Fatalf("a partial was listed: %v", listed)
	}
	if removed := RemovePartials(dir); removed != 1 {
		t.Fatalf("removed %d partials, want 1", removed)
	}
	if names := dirNames(t, dir); len(names) != 2 {
		t.Fatalf("want the published backup and its sidecar, got %v", names)
	}
}
