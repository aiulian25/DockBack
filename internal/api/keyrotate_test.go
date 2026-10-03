package api

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"dockback/internal/backup"
	"dockback/internal/config"
	"dockback/internal/crypto"
)

// F16 master-key rotation. Two things went wrong at once: the swap was an
// unsynchronised write to fields other goroutines were reading, and a backup
// that straddled it sealed its archive with one key while recording the other
// key's fingerprint. That backup is permanently "Key mismatch" — it survives
// every verification and fails only when someone finally needs it.

func rotateTestServer(t *testing.T) *Server {
	t.Helper()
	st := testStore(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	e := &backup.Engine{Store: st, Key: key, KeyFP: backup.KeyFingerprint(key), Log: func(string, string, string) {}}
	s := &Server{
		store: st, engine: e, guard: newLoginGuard(st),
		cfg:      &config.Config{EncryptionKey: key, MinPasswordLen: 12, BackupsDir: t.TempDir()},
		jobs:     map[string]*backupJob{},
		queueSig: make(chan struct{}, 1),
	}
	hash, err := crypto.HashPassword(rsPassword)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := st.CreateUser("admin", hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession("tok", uid, time.Hour); err != nil {
		t.Fatal(err)
	}
	return s
}

func postRotate(s *Server, current, next []byte) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{
		"current_hex": hex.EncodeToString(current),
		"new_hex":     hex.EncodeToString(next),
		"password":    rsPassword,
	})
	r := httptest.NewRequest("POST", "/api/security/key/rotate", strings.NewReader(string(body)))
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	rec := httptest.NewRecorder()
	s.handleKeyRotate(rec, r)
	return rec
}

func otherKey(b byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = b
	}
	return k
}

// AC1 — a rotation started while a backup is queued or running is refused, not
// raced. The re-wrap walks every stored data key; a run writing a new one behind
// it would be missed and left sealed with the outgoing key.
func TestRotationWhileRunRefused(t *testing.T) {
	s := rotateTestServer(t)
	s.jobs["job-1"] = &backupJob{nodeID: "n1", containerID: "cid"}

	rec := postRotate(s, s.cfg.EncryptionKey, otherKey(0xAB))
	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "wait for them to finish") {
		t.Errorf("the refusal must say what to do: %s", rec.Body.String())
	}
	// Refusing must not leave the key half-changed, nor the dispatcher held.
	if _, fp := s.engine.CurrentKey(); fp != backup.KeyFingerprint(s.cfg.EncryptionKey) {
		t.Error("a refused rotation must not touch the running key")
	}
	if s.rotating.Load() {
		t.Error("a refused rotation must release the dispatcher hold")
	}

	// Once the queue drains it succeeds.
	delete(s.jobs, "job-1")
	next := otherKey(0xAB)
	if rec := postRotate(s, s.cfg.EncryptionKey, next); rec.Code != http.StatusOK {
		t.Fatalf("rotation on an idle queue = %d: %s", rec.Code, rec.Body.String())
	}
	key, fp := s.engine.CurrentKey()
	if hex.EncodeToString(key) != hex.EncodeToString(next) {
		t.Error("the engine must be running on the new key")
	}
	if fp != backup.KeyFingerprint(next) {
		t.Errorf("the fingerprint must match the key it was installed with: %q", fp)
	}
	if s.rotating.Load() {
		t.Error("the dispatcher hold must be released when rotation finishes")
	}
}

// AC2 — while a rotation is in progress the dispatcher starts nothing new.
// Checking for in-flight jobs alone would leave a window: a scheduled job firing
// mid-rotation would be sealed with the outgoing key after the re-wrap had
// already passed it by.
func TestRotationHoldsTheDispatcher(t *testing.T) {
	s := rotateTestServer(t)
	s.locks = newOpLocks()
	s.queue = []*queuedJob{{id: "q1", nodeID: "n1", stackKey: stackKey("n1", "", "app"), ctrKey: containerKey("n1", "app")}}

	release := s.holdBackupsForRotation()
	if j, _, _ := s.takeEligible(); j != nil {
		t.Fatal("no job may start while the master key is being re-wrapped")
	}
	if len(s.queue) != 1 {
		t.Error("the job must stay QUEUED, not be dropped")
	}

	release()
	if !s.rotating.Load() == false {
		t.Error("the hold must be lifted")
	}
	if j, _, _ := s.takeEligible(); j == nil {
		t.Fatal("the job must run once the rotation is done")
	}
}

// AC3 — the key and its fingerprint are always read as one consistent pair, and
// concurrent readers never race the writer. Run with -race: this is the data
// race the accessors exist to remove.
func TestKeyAccessorsAreRaceFree(t *testing.T) {
	e := &backup.Engine{Key: otherKey(1), KeyFP: backup.KeyFingerprint(otherKey(1))}
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				key, fp := e.CurrentKey()
				if fp != backup.KeyFingerprint(key) {
					t.Errorf("read a key and a fingerprint that do not belong together: %q", fp)
					return
				}
				_ = e.MasterKey()
				_ = e.MasterKeyFP()
			}
		}()
	}
	for i := byte(2); i < 12; i++ {
		k := otherKey(i)
		e.SetKey(k, backup.KeyFingerprint(k))
		time.Sleep(time.Millisecond)
	}
	close(stop)
	wg.Wait()
}

// A rotated key must actually open what was re-sealed under it, or the rotation
// has quietly destroyed the thing it was protecting.
func TestRotationReSealsWithTheInstalledKey(t *testing.T) {
	s := rotateTestServer(t)
	if err := s.engine.SetRedisAuth("n1", "redis", "the-password"); err != nil {
		t.Fatal(err)
	}
	next := otherKey(0x7C)
	if rec := postRotate(s, s.cfg.EncryptionKey, next); rec.Code != http.StatusOK {
		t.Fatalf("rotate: %s", rec.Body.String())
	}
	// The stored secret must open with the NEW key and no longer with the old.
	if got := openRecorded(t, s, "n1", "redis", next); got != "the-password" {
		t.Errorf("the new key must open what the rotation re-sealed, got %q", got)
	}
}
