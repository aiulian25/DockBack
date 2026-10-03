package storage

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Filesystem retention lock (F97). "Immutable" was an S3-only badge, so a NAS or
// SSH box — what most homelabs actually back up to — had no protection at all.
// These tests pin what the lock DOES do, and just as importantly that a
// destination never claims a protection it has not applied.

func TestLocalLockUntilMakesFileUnwritable(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLocalFromConfig(map[string]string{"path": dir, "retention_lock_days": "30"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := "node/app/backup.dback"
	if _, err := l.Put(ctx, key, strings.NewReader("archive bytes")); err != nil {
		t.Fatal(err)
	}

	// Before the lock the object is writable, as every other destination's is.
	p := filepath.Join(dir, key)
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("precondition: a fresh object must be writable: %v", err)
	}
	f.Close()

	if err := l.LockUntil(ctx, key, time.Now().Add(30*24*time.Hour)); err != nil {
		t.Fatalf("LockUntil: %v", err)
	}
	// THE acceptance property: an in-place overwrite — the shape ransomware uses
	// on a mounted share — is refused.
	if f, err := os.OpenFile(p, os.O_WRONLY, 0); err == nil {
		f.Close()
		t.Fatal("a locked object must not be writable")
	}
	// Reading must keep working, or a locked copy could never be restored from.
	rc, err := l.Get(ctx, key)
	if err != nil {
		t.Fatalf("a locked copy must stay readable: %v", err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if string(body) != "archive bytes" {
		t.Fatalf("locked copy reads back as %q", body)
	}
}

// A destination must claim immutability only when it is configured to lock —
// otherwise prune would retain copies nothing is protecting.
func TestLocalImmutableOnlyWhenConfigured(t *testing.T) {
	locked, err := NewLocalFromConfig(map[string]string{"path": t.TempDir(), "retention_lock_days": "14"})
	if err != nil {
		t.Fatal(err)
	}
	if !locked.Immutable() || locked.LockDays() != 14 {
		t.Fatalf("immutable=%v days=%d, want true/14", locked.Immutable(), locked.LockDays())
	}

	// Unconfigured, zero, and malformed all mean OFF. A value that cannot be
	// parsed must never be read as "lock forever".
	for _, cfg := range []map[string]string{
		{"path": t.TempDir()},
		{"path": t.TempDir(), "retention_lock_days": "0"},
		{"path": t.TempDir(), "retention_lock_days": "-5"},
		{"path": t.TempDir(), "retention_lock_days": "forever"},
		{"path": t.TempDir(), "retention_lock_days": ""},
	} {
		l, err := NewLocalFromConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if l.Immutable() || l.LockDays() != 0 {
			t.Fatalf("cfg %v: immutable=%v days=%d, want false/0", cfg, l.Immutable(), l.LockDays())
		}
	}

	// DockBack's own primary storage is never lock-configured.
	plain, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if plain.Immutable() {
		t.Fatal("the app's own local storage must never report itself immutable")
	}
}

// An absurd period is clamped rather than honoured: a lock cannot be lifted from
// the UI, so an unbounded value is a way to make a destination permanently
// unprunable by accident.
func TestRetentionLockDaysClamped(t *testing.T) {
	if got := parseRetentionLockDays(map[string]string{"retention_lock_days": "999999"}); got != maxRetentionLockDays {
		t.Fatalf("clamp = %d, want %d", got, maxRetentionLockDays)
	}
	if got := parseRetentionLockDays(map[string]string{"retention_lock_days": " 30 "}); got != 30 {
		t.Fatalf("surrounding space must be tolerated, got %d", got)
	}
}

// A lock must not stop DockBack itself once the recorded period has expired —
// prune calls Delete only then, and a copy that can never be removed is a disk
// that eventually fills.
func TestLocalLockedObjectIsStillDeletable(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLocalFromConfig(map[string]string{"path": dir, "retention_lock_days": "1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := "node/app/old.dback"
	if _, err := l.Put(ctx, key, bytes.NewReader([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	if err := l.LockUntil(ctx, key, time.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := l.Delete(ctx, key); err != nil {
		t.Fatalf("an expired lock must not block DockBack's own prune: %v", err)
	}
	if _, ok, _ := l.Stat(ctx, key); ok {
		t.Fatal("the object should be gone")
	}
}

// Traversal is rejected on the lock path too — it must not become a way to chmod
// a file outside the destination root.
func TestLocalLockUntilRejectsTraversal(t *testing.T) {
	l, err := NewLocalFromConfig(map[string]string{"path": t.TempDir(), "retention_lock_days": "5"})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.LockUntil(context.Background(), "../../etc/passwd", time.Now()); err == nil {
		t.Fatal("a traversal key must be refused")
	}
}

// The interface is the contract: a backend that cannot lock must not implement
// it, because a destination claiming a protection it does not apply is worse
// than one claiming nothing. WebDAV has no permission model to use.
func TestOnlyLockableBackendsImplementRetentionLocker(t *testing.T) {
	local, err := NewLocalFromConfig(map[string]string{"path": t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := any(local).(RetentionLocker); !ok {
		t.Fatal("local must implement RetentionLocker")
	}
	smb, err := NewSMB(map[string]string{"host": "nas.example", "share": "backups"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := any(smb).(RetentionLocker); !ok {
		t.Fatal("smb must implement RetentionLocker")
	}
	sftpBe, err := NewSFTP(map[string]string{"host": "box.example", "user": "u", "password": "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sftpBe.(RetentionLocker); !ok {
		t.Fatal("sftp must implement RetentionLocker")
	}

	dav, err := NewWebDAV(map[string]string{"url": "https://dav.example/remote.php/webdav", "user": "u", "password": "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := any(dav).(RetentionLocker); ok {
		t.Fatal("WebDAV cannot apply a retention lock and must NOT claim it can")
	}
	if im, ok := any(dav).(Immutabler); ok && im.Immutable() {
		t.Fatal("WebDAV must never report itself immutable")
	}
}

// SMB and SFTP read the same clamped config key, so one destination type cannot
// silently behave differently from another.
func TestRemoteBackendsHonourRetentionLockConfig(t *testing.T) {
	smb, err := NewSMB(map[string]string{"host": "nas.example", "share": "b", "retention_lock_days": "7"})
	if err != nil {
		t.Fatal(err)
	}
	if !smb.Immutable() || smb.LockDays() != 7 {
		t.Fatalf("smb: immutable=%v days=%d", smb.Immutable(), smb.LockDays())
	}
	sftpBe, err := NewSFTP(map[string]string{"host": "box.example", "user": "u", "password": "p", "retention_lock_days": "9999"})
	if err != nil {
		t.Fatal(err)
	}
	im, ok := sftpBe.(Immutabler)
	if !ok || !im.Immutable() || im.LockDays() != maxRetentionLockDays {
		t.Fatalf("sftp: ok=%v days=%d, want a clamped %d", ok, im.LockDays(), maxRetentionLockDays)
	}
}

// The remote chattr command takes the path as a quoted argument, so no key
// spelling can be read as shell syntax by the remote sh.
func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/backups/a.dback": `'/backups/a.dback'`,
		"a b.dback":        `'a b.dback'`,
		"it's.dback":       `'it'\''s.dback'`,
		"a;rm -rf /":       `'a;rm -rf /'`,
		"$(whoami).dback":  `'$(whoami).dback'`,
		"`id`.dback":       "'`id`.dback'",
	} {
		if got := shellQuote(in); got != want {
			t.Fatalf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
