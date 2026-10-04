package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A second DockBack against the same data directory used to boot a full server,
// clearing the running instance's work directories and every session before it
// failed on the busy port. The lock is what stops it first.
func TestASecondInstanceCannotTakeTheDataDirectory(t *testing.T) {
	dir := t.TempDir()

	release, err := AcquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("the first instance must get the lock: %v", err)
	}

	if _, err := AcquireInstanceLock(dir); !errors.Is(err, ErrAnotherInstance) {
		t.Fatalf("a second instance must be refused, got %v", err)
	}

	// Releasing (or exiting) frees it for the next start.
	release()
	again, err := AcquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("after release the lock must be available again: %v", err)
	}
	again()
}

func TestTheLockFileIsPrivate(t *testing.T) {
	dir := t.TempDir()
	release, err := AcquireInstanceLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	info, err := os.Stat(filepath.Join(dir, InstanceLockFile))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("lock file mode = %o, want 600", mode)
	}
}
