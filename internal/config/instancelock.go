package config

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"
)

// InstanceLockFile lives in the data directory. Holding an exclusive lock on it
// is what makes a process THE DockBack for that directory.
const InstanceLockFile = "dockback.lock"

// ErrAnotherInstance means a running DockBack already holds the data directory.
var ErrAnotherInstance = errors.New("another DockBack is already running with this data directory")

// AcquireInstanceLock takes an exclusive, non-blocking lock on the data
// directory and returns the function that releases it.
//
// A second process pointed at the same directory — a mistaken
// `docker exec … /dockback`, or two containers sharing one volume — gets
// ErrAnotherInstance here and exits before it can clear the work directories or
// sessions the running instance depends on. The kernel drops the lock when the
// process exits, however it exits, so a crash never leaves it stuck.
//
// A filesystem that cannot lock at all (some network shares) is NOT a reason to
// refuse to start: that would turn a safety net into an outage. The guard is
// skipped there, loudly.
func AcquireInstanceLock(dataDir string) (release func(), err error) {
	path := filepath.Join(dataDir, InstanceLockFile)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the instance lock %s: %w", path, err)
	}
	lockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if lockErr == nil {
		return func() { file.Close() }, nil
	}
	file.Close()
	if errors.Is(lockErr, syscall.EWOULDBLOCK) {
		return nil, fmt.Errorf("%w (%s); refusing to start a second one", ErrAnotherInstance, dataDir)
	}
	log.Printf("startup: WARNING: cannot lock %s (%v) — this filesystem does not support locks, so nothing stops a second DockBack from starting against the same data", path, lockErr)
	return func() {}, nil
}
