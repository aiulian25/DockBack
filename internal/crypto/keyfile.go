package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"golang.org/x/crypto/argon2"
)

// keyfile.go — passphrase-wrapped master-key escrow (PLAN §9.2). The 32-byte
// master key can be stored as an argon2id-KEK-wrapped, AES-256-GCM-sealed
// keyfile instead of as plaintext hex. The operator supplies the passphrase at
// boot (env/secret); a wrong passphrase or a tampered keyfile fails GCM auth.
// This protects the key at rest without changing how backups are encrypted.

const keyfileFormat = "dockback-keyfile-v1"

// argon2id KEK parameters for the keyfile. Heavier memory than password hashing
// is fine for a once-per-boot derivation, and raises the cost of offline guesses.
const (
	kekTime    uint32 = 4
	kekMemory  uint32 = 128 * 1024 // 128 MiB
	kekThreads uint8  = 4
)

type wrappedKeyfile struct {
	Format  string `json:"format"`
	KDF     string `json:"kdf"`
	Time    uint32 `json:"t"`
	Memory  uint32 `json:"m"`
	Threads uint8  `json:"p"`
	Salt    string `json:"salt"`   // base64 argon2 salt
	Sealed  string `json:"sealed"` // base64(nonce||ciphertext) of hex(master) under the KEK
}

// WrapKeyfile produces a passphrase-protected keyfile (pretty JSON) for a
// 32-byte master key. Store it (mounted read-only / a Docker secret) and set
// DOCKBACK_ENCRYPTION_KEYFILE + DOCKBACK_ENCRYPTION_PASSPHRASE to use it.
func WrapKeyfile(master []byte, passphrase string) ([]byte, error) {
	if len(master) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes, got %d", len(master))
	}
	if passphrase == "" {
		return nil, errors.New("passphrase required")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	kek := argon2.IDKey([]byte(passphrase), salt, kekTime, kekMemory, kekThreads, 32)
	sealed, err := SealString(hex.EncodeToString(master), kek)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(wrappedKeyfile{
		Format: keyfileFormat, KDF: "argon2id",
		Time: kekTime, Memory: kekMemory, Threads: kekThreads,
		Salt:   base64.StdEncoding.EncodeToString(salt),
		Sealed: base64.StdEncoding.EncodeToString(sealed),
	}, "", "  ")
}

// UnwrapKeyfile recovers the 32-byte master key from a passphrase-wrapped
// keyfile. A wrong passphrase or a tampered file fails (GCM authentication).
func UnwrapKeyfile(data []byte, passphrase string) ([]byte, error) {
	if passphrase == "" {
		return nil, errors.New("passphrase required")
	}
	var kf wrappedKeyfile
	if err := json.Unmarshal(data, &kf); err != nil {
		return nil, fmt.Errorf("invalid keyfile: %w", err)
	}
	if kf.Format != keyfileFormat || kf.KDF != "argon2id" {
		return nil, fmt.Errorf("unsupported keyfile format %q", kf.Format)
	}
	salt, err := base64.StdEncoding.DecodeString(kf.Salt)
	if err != nil {
		return nil, fmt.Errorf("invalid keyfile salt: %w", err)
	}
	sealed, err := base64.StdEncoding.DecodeString(kf.Sealed)
	if err != nil {
		return nil, fmt.Errorf("invalid keyfile body: %w", err)
	}
	t, m, p := kf.Time, kf.Memory, kf.Threads
	if t == 0 {
		t = kekTime
	}
	if m == 0 {
		m = kekMemory
	}
	if p == 0 {
		p = kekThreads
	}
	kek := argon2.IDKey([]byte(passphrase), salt, t, m, p, 32)
	hexKey, err := OpenString(sealed, kek)
	if err != nil {
		return nil, errors.New("wrong passphrase or corrupt keyfile")
	}
	key, err := hex.DecodeString(hexKey)
	if err != nil || len(key) != 32 {
		return nil, errors.New("keyfile did not contain a valid 32-byte key")
	}
	return key, nil
}
