// Package storage abstracts backup destinations behind one interface so new
// backends (S3, B2, SMB, Nextcloud, Synology) are trivial to add (PLAN §4.9).
// v1 ships the local mounted-volume backend; the manifest is always written
// beside the archive so a destination is self-describing (PLAN §9.3).
package storage

import (
	"context"
	"io"
	"strconv"
	"strings"
	"time"
)

// Backend is a backup destination. Every method takes a context so a slow or
// hung destination can be bounded by a timeout and a backup can be canceled.
type Backend interface {
	// Put streams an object to key. Returns bytes written.
	Put(ctx context.Context, key string, r io.Reader) (int64, error)
	// Get opens an object for reading; caller closes.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes an object.
	Delete(ctx context.Context, key string) error
	// Stat reports size in bytes and existence.
	//
	// The three results are a CONTRACT, because callers delete things based on
	// them: (n, true, nil) the object is there; (0, false, nil) the destination
	// answered and the object is definitively NOT there; (0, false, err) the
	// question could not be answered — a network blip, an expired credential, a
	// permission change. An implementation must never report the third case as
	// the second. "I could not reach the destination" and "the copy is gone" lead
	// to opposite actions, and conflating them is how a transient error becomes a
	// deleted backup.
	Stat(ctx context.Context, key string) (int64, bool, error)
	// FreeBytes reports free space at the destination (0 if unknown), used by
	// pre-flight disk checks (PLAN §4.4).
	FreeBytes(ctx context.Context) (uint64, error)
	// Name identifies the backend for logs/UI.
	Name() string
}

// Immutabler is implemented by backends that write WORM/object-locked objects
// (PLAN §9.1). Immutable reports whether locking is active; LockDays is the
// retention period applied to each object, so callers can record when it expires.
type Immutabler interface {
	Immutable() bool
	LockDays() int
}

// RetentionLocker is implemented by filesystem-like backends that can make an
// object read-only AFTER it is written (F97): local disk, SMB and SFTP.
//
// This is deliberately NOT the same guarantee as S3 Object Lock, and the
// difference matters enough to state at the interface. Object Lock is enforced
// by the SERVER — it refuses a delete even when asked with valid credentials.
// A filesystem lock is applied by DockBack, after the write, and on POSIX the
// permission to REMOVE a file comes from its directory, not the file itself. So
// this stops an in-place overwrite and turns a casual delete into an explicit
// forced one; it does not stop a determined caller or root.
//
// It is still worth having: DockBack's own prune honours the recorded lock (the
// most common way a copy actually disappears), and the read-only bit blocks the
// straightforward encrypt-in-place that ransomware attempts on a mounted share.
// The UI and docs say all of this plainly rather than badging it as WORM.
//
// A backend that cannot lock must NOT implement this — a destination that
// claims a protection it does not apply is worse than one that claims nothing.
type RetentionLocker interface {
	LockUntil(ctx context.Context, key string, until time.Time) error
}

// retentionLock is the config-driven half of a filesystem retention lock, shared
// by every backend that supports one so "how many days" is parsed and clamped in
// exactly one place.
type retentionLock struct{ lockDays int }

// Immutable reports whether this destination applies a retention lock. It
// satisfies Immutabler, so the existing mirror/prune/UI paths that already
// understand WORM copies pick filesystem locks up with no special-casing.
func (r retentionLock) Immutable() bool { return r.lockDays > 0 }

// LockDays is the configured lock period in days (0 = off).
func (r retentionLock) LockDays() int { return r.lockDays }

// maxRetentionLockDays bounds the configured period at ~10 years. A lock is not
// revocable through the UI, so an unbounded value is a way to accidentally make
// a destination permanently unprunable.
const maxRetentionLockDays = 3650

// parseRetentionLockDays reads and clamps retention_lock_days from a destination
// config. Anything absent, unparseable or <= 0 means the lock is off — the safe
// reading, since a malformed value must never be taken as "lock forever".
func parseRetentionLockDays(cfg map[string]string) int {
	n, err := strconv.Atoi(strings.TrimSpace(cfg["retention_lock_days"]))
	if err != nil || n <= 0 {
		return 0
	}
	if n > maxRetentionLockDays {
		return maxRetentionLockDays
	}
	return n
}

// ObjectLockStatus is the result of a non-destructive WORM preflight (PLAN §9.1):
// does the destination's bucket actually enforce Object Lock? This closes the gap
// where an operator marks a destination immutable but the bucket was never
// created with Object Lock — so backups silently would NOT be ransomware-proof.
type ObjectLockStatus struct {
	Enforced bool   `json:"enforced"` // bucket has Object Lock enabled/enforcing
	Checked  bool   `json:"checked"`  // we could determine it (false = e.g. append-only key lacks the read permission)
	Detail   string `json:"detail"`   // human-readable explanation
}

// ObjectLockVerifier is implemented by backends that can confirm, without writing
// anything, whether their target bucket actually enforces Object Lock (PLAN §9.1).
type ObjectLockVerifier interface {
	VerifyObjectLock(ctx context.Context) ObjectLockStatus
}

// runCtx runs fn but returns as soon as ctx is canceled or times out, even if
// fn itself is blocked in I/O that can't be interrupted (e.g. a hung WebDAV/SMB
// server). The orphaned goroutine unwinds on its own — bounded by the backend's
// own request/connection timeouts — so a stuck destination can never wedge a
// backup or defeat the Cancel button.
func runCtx[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	go func() { v, err := fn(); ch <- result{v, err} }()
	select {
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	case r := <-ch:
		return r.v, r.err
	}
}
