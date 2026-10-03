package backup

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"dockback/internal/storage"
	"dockback/internal/store"
)

// The 27 log lines that never reached anybody.
//
// This package emitted both "ERR" and "ERROR"; every consumer — the run
// consoles' colouring, and three pages' end-of-run detection — keys on "ERR"
// alone. An "ERROR" line therefore rendered as ordinary grey text and ended
// nothing, so a refused stack restore left the spinner turning forever.
func TestNormalizeLogLevel(t *testing.T) {
	if got := normalizeLogLevel("ERROR"); got != "ERR" {
		t.Errorf("normalizeLogLevel(ERROR) = %q, want ERR — otherwise the UI never learns the run failed", got)
	}
	for _, keep := range []string{"ERR", "WARN", "INFO", ""} {
		if got := normalizeLogLevel(keep); got != keep {
			t.Errorf("normalizeLogLevel(%q) = %q, want it unchanged", keep, got)
		}
	}
}

// putRefuser is a storage backend whose Put fails without reading a byte — a
// full disk, EIO, a refused upload. Only Put is ever called by packEncryptStore.
type putRefuser struct{ storage.Backend }

var errDiskFull = errors.New("write /backups/app.dback: no space left on device")

func (putRefuser) Put(context.Context, string, io.Reader) (int64, error) { return 0, errDiskFull }

// TestPackEncryptStoreReturnsWhenPutFails: a failed Put used to park the
// encryptor on a pipe write nobody drained, so the backup goroutine never
// returned and its queue slot and container lock leaked until a restart. The
// manifest alone gives the encryptor something to write, so no volumes.tar is
// needed to reproduce it.
func TestPackEncryptStoreReturnsWhenPutFails(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	e := &Engine{Store: st, Storage: putRefuser{}, Key: key, KeyFP: KeyFingerprint(key), Log: func(string, string, string) {}}
	man := &Manifest{
		Version: ManifestVersion, BackupID: "b1", TargetName: "app",
		KeyFingerprint: e.KeyFP, Format: Format{Algorithm: "zstd", Archive: "tar"},
	}

	done := make(chan error, 1)
	go func() {
		_, _, perr := e.packEncryptStore(context.Background(), t.TempDir(), man, "node/app/x.dback", "zstd", zstd.SpeedDefault, 0)
		done <- perr
	}()
	select {
	case perr := <-done:
		if !errors.Is(perr, errDiskFull) {
			t.Fatalf("got %v, want the storage error that caused the failure", perr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("packEncryptStore never returned after Put failed — the queue slot and container lock would leak")
	}
}
