package config

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"dockback/internal/crypto"
)

// loadKey must unwrap a passphrase-protected keyfile at boot (PLAN §9.2).
func TestLoadKeyFromKeyfile(t *testing.T) {
	master := make([]byte, 32)
	_, _ = rand.Read(master)
	kf, err := crypto.WrapKeyfile(master, "boot-passphrase-123")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "key.json")
	if err := os.WriteFile(path, kf, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DOCKBACK_ENCRYPTION_KEYFILE", path)
	t.Setenv("DOCKBACK_ENCRYPTION_PASSPHRASE", "boot-passphrase-123")

	got, ephemeral, err := loadKey()
	if err != nil {
		t.Fatalf("loadKey: %v", err)
	}
	if ephemeral {
		t.Error("a keyfile-supplied key is not ephemeral")
	}
	if !bytes.Equal(got, master) {
		t.Fatal("loaded key != original")
	}
}

func TestLoadKeyKeyfileWrongPassphrase(t *testing.T) {
	master := make([]byte, 32)
	_, _ = rand.Read(master)
	kf, _ := crypto.WrapKeyfile(master, "right-pass")
	path := filepath.Join(t.TempDir(), "key.json")
	_ = os.WriteFile(path, kf, 0o600)
	t.Setenv("DOCKBACK_ENCRYPTION_KEYFILE", path)
	t.Setenv("DOCKBACK_ENCRYPTION_PASSPHRASE", "wrong-pass")
	if _, _, err := loadKey(); err == nil {
		t.Fatal("wrong passphrase must fail boot")
	}
}

func TestLoadKeyKeyfileMissingPassphrase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key.json")
	_ = os.WriteFile(path, []byte("{}"), 0o600)
	t.Setenv("DOCKBACK_ENCRYPTION_KEYFILE", path)
	t.Setenv("DOCKBACK_ENCRYPTION_PASSPHRASE", "")
	if _, _, err := loadKey(); err == nil {
		t.Fatal("keyfile without passphrase must error")
	}
}
