package backup

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"dockback/internal/crypto"
	"dockback/internal/storage"
	"dockback/internal/store"
)

// TestRestoreRejectsCorruptCiphertext locks in PLAN §3.4: the stored ciphertext
// SHA-256 is re-checked before a restore reads a copy, so storage bit-rot or a
// truncated transfer is caught BEFORE any destructive restore begins. bestLocation
// is the gate used by every restore/download path (via openVerified).
func TestRestoreRejectsCorruptCiphertext(t *testing.T) {
	dir := t.TempDir()
	be, err := storage.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Storage: be, Log: func(string, string, string) {}}

	ctx := context.Background()
	const key = "node1/app/backup1.dback"

	// Opaque "ciphertext" bytes — the integrity check only hashes what's stored.
	payload := bytes.Repeat([]byte("ciphertext-archive-bytes-"), 1000)
	if _, err := be.Put(ctx, key, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	sum, err := crypto.CipherSHA256(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}

	b := &store.Backup{
		ID:            "backup1",
		StorageKey:    key,
		CipherSHA256:  sum,
		LocationsJSON: `[{"kind":"local","name":"local","type":"local"}]`,
	}

	// 1) Intact copy passes the pre-restore integrity check.
	if _, _, err := e.bestLocation(ctx, b, ""); err != nil {
		t.Fatalf("intact archive should pass integrity check: %v", err)
	}

	// 2) Bit-rot: flip one byte → SHA no longer matches → must be refused.
	corrupt := append([]byte(nil), payload...)
	corrupt[42] ^= 0xFF
	if _, err := be.Put(ctx, key, bytes.NewReader(corrupt)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.bestLocation(ctx, b, ""); err == nil {
		t.Fatal("bit-rotted archive must be rejected before restore")
	} else if !strings.Contains(err.Error(), "integrity mismatch") {
		t.Fatalf("expected integrity-mismatch error, got: %v", err)
	}

	// 3) Truncation (transfer cut short) is likewise caught.
	if _, err := be.Put(ctx, key, bytes.NewReader(payload[:len(payload)-100])); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.bestLocation(ctx, b, ""); err == nil {
		t.Fatal("truncated archive must be rejected before restore")
	}

	// 4) Restoring the good bytes works again (proves it wasn't a blanket deny).
	if _, err := be.Put(ctx, key, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.bestLocation(ctx, b, ""); err != nil {
		t.Fatalf("restored-good archive should pass again: %v", err)
	}
}
