package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Local is a backup destination on a mounted volume/path (PLAN §4.9, the
// "app is the backup target" model in PLAN §8.1).
type Local struct {
	root string
	// F97: an optional post-write retention lock. Zero for the app's own primary
	// local storage, so nothing about the default path changes.
	retentionLock
}

// NewLocalFromConfig builds a Local destination from a config map, including its
// optional retention lock (F97). NewLocal stays the constructor for DockBack's
// own primary storage, which is never lock-configured.
func NewLocalFromConfig(cfg map[string]string) (*Local, error) {
	l, err := NewLocal(cfg["path"])
	if err != nil {
		return nil, err
	}
	l.lockDays = parseRetentionLockDays(cfg)
	return l, nil
}

// NewLocal creates a Local backend rooted at dir, creating it if needed.
func NewLocal(dir string) (*Local, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return &Local{root: abs}, nil
}

// safePath joins key onto root, rejecting traversal (PLAN §2.10).
func (l *Local) safePath(key string) (string, error) {
	clean := filepath.Clean("/" + key) // force-absolute then strip
	p := filepath.Join(l.root, clean)
	if p != l.root && !strings.HasPrefix(p, l.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid storage key %q (path traversal)", key)
	}
	return p, nil
}

// Put writes the object atomically (temp file + rename).
func (l *Local) Put(_ context.Context, key string, r io.Reader) (int64, error) {
	p, err := l.safePath(key)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename

	n, err := io.Copy(tmp, r)
	if err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmpName, p); err != nil {
		return 0, err
	}
	return n, nil
}

// Get opens the object for reading.
func (l *Local) Get(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := l.safePath(key)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

// Delete removes the object (idempotent).
func (l *Local) Delete(_ context.Context, key string) error {
	p, err := l.safePath(key)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Stat reports size and existence.
func (l *Local) Stat(_ context.Context, key string) (int64, bool, error) {
	p, err := l.safePath(key)
	if err != nil {
		return 0, false, err
	}
	fi, err := os.Stat(p)
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return fi.Size(), true, nil
}

// FreeBytes reports free space on the filesystem holding root (PLAN §4.4).
func (l *Local) FreeBytes(_ context.Context) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(l.root, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// TotalBytes reports the total capacity of the filesystem holding root, so the
// local/primary backups volume satisfies the Capacity interface and is covered by
// the same "almost full" / "filling up" alerts and forecast as an offsite
// destination (F42). Uses Blocks (the whole filesystem) — not Bavail, which
// FreeBytes correctly uses for the space available to a non-root process.
func (l *Local) TotalBytes(_ context.Context) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(l.root, &st); err != nil {
		return 0, err
	}
	return st.Blocks * uint64(st.Bsize), nil
}

// Ping is a cheap reachability check — stat the root dir, catching an unmounted
// or removed backup volume. Writes nothing.
func (l *Local) Ping(_ context.Context) error {
	fi, err := os.Stat(l.root)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("local: %q is not a directory", l.root)
	}
	return nil
}

// List returns object keys directly under prefix (non-recursive, files only).
func (l *Local) List(_ context.Context, prefix string) ([]string, error) {
	dir, err := l.safePath(prefix)
	if err != nil {
		return nil, err
	}
	// Walk RECURSIVELY so nested archive layouts (<node>/<stack>/<container>/…) are
	// enumerated — matching the S3 backend's recursive listing, so ListKeys is a
	// uniform "every object under prefix" contract (F20 adopt relies on this). Keys
	// are returned relative to the backend root with forward slashes.
	out := []string{}
	werr := filepath.WalkDir(dir, func(p string, de os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // a missing prefix dir is simply empty
			}
			return err
		}
		if de.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(l.root, p)
		if rerr != nil {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if werr != nil {
		return nil, werr
	}
	return out, nil
}

// LockUntil makes an already-written object read-only (F97).
//
// The file loses its write bits; the containing DIRECTORY is deliberately left
// alone, because removing directory write would break every sibling write —
// including the next backup into the same folder.
//
// The honest consequence, stated here because it shapes what this is worth: on
// POSIX, permission to REMOVE a file comes from its directory, so this stops an
// in-place overwrite (the ransomware pattern on a mounted share) and makes a
// delete require an explicit force, but it cannot stop a determined caller.
// DockBack's own prune is what honours the recorded expiry.
//
// `until` is not stored in the filesystem — there is nowhere portable to put it.
// It lives in the backup's location entry, which is what prune reads.
func (l *Local) LockUntil(_ context.Context, key string, _ time.Time) error {
	p, err := l.safePath(key)
	if err != nil {
		return err
	}
	if err := os.Chmod(p, 0o440); err != nil {
		return fmt.Errorf("retention lock %s: %w", key, err)
	}
	return nil
}

// Name identifies the backend.
func (l *Local) Name() string { return "local:" + l.root }
