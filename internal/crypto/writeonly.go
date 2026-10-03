package crypto

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// Write-only backups (F86): an ASYMMETRIC envelope for the per-backup data key.
//
// The problem it solves is structural, not a bug. Normally a backup's DEK is
// wrapped with the SYMMETRIC master key that the running process holds
// (WrapKey), so anyone who obtains a shell in the DockBack container obtains the
// master key, the destination credentials sealed under it, and therefore
// plaintext access to every backup DockBack has ever written — everywhere it is
// stored. S3 Object Lock stops deletion; nothing stops reading.
//
// In write-only mode DockBack holds only a PUBLIC key. It can still create,
// mirror, prune and structurally verify backups, but it cannot read their
// payloads: decryption requires a private key that exists only offline, supplied
// by hand for a single restore and never persisted.
//
// CONSTRUCTION — an ephemeral-sender sealed box, built from primitives this
// codebase and the offline recovery script already have:
//
//	ephPriv, ephPub := X25519 keypair (fresh per wrap)
//	shared          := X25519(ephPriv, recipientPub)
//	kek             := HKDF-SHA256(shared, salt = ephPub || recipientPub, info = wrapInfo)
//	wrapped         := base64( ephPub(32) || AES-256-GCM(dek) under kek )
//
// Deliberately NOT NaCl's box.SealAnonymous, despite the same shape. Two
// reasons, both about the recovery path: this reuses the exact AES-256-GCM
// sealing already vetted here and already implemented in
// `scripts/dockback-recover.py`, and it needs nothing beyond the Go standard
// library (crypto/ecdh, crypto/hkdf). NaCl would have forced HSalsa20 +
// XSalsa20-Poly1305 into a recovery script that is deliberately pure-stdlib
// Python — a large, hand-rolled crypto surface in exactly the tool you reach for
// when everything else is gone. A backup you cannot recover without DockBack is
// not a backup.
//
// The ephemeral sender key means the wrap is one-way even for whoever performed
// it: DockBack cannot unwrap what it just wrapped, because ephPriv is discarded
// and never leaves the wrapping function.

// wrapInfo domain-separates this KDF from every other use of HKDF in the app.
// Changing it would make every existing write-only backup unrecoverable, so it
// is versioned and must never be edited in place.
const wrapInfo = "dockback-write-only-v1"

// ErrNotWriteOnlyWrapped signals a wrapped blob that isn't in this format.
var ErrNotWriteOnlyWrapped = errors.New("not a write-only wrapped key")

// NewBackupKeypair generates an X25519 keypair for write-only mode. Both halves
// are returned base64; the caller stores ONLY pub and shows priv exactly once.
func NewBackupKeypair() (pub, priv string, err error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(k.PublicKey().Bytes()),
		base64.StdEncoding.EncodeToString(k.Bytes()), nil
}

// BackupPubFP is a short, stable fingerprint of a public key (first 16 hex chars
// of its SHA-256) so the UI can say which keypair a backup belongs to — and so a
// restore can tell "wrong private key" from "corrupt archive" before spending
// time on a decrypt. It is derived from PUBLIC material only.
func BackupPubFP(pubB64 string) string {
	raw, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16]
}

// deriveKEK computes the shared key-encryption key for one wrap. Both sides run
// this identically; only the source of the X25519 shared secret differs.
func deriveKEK(shared, ephPub, recipientPub []byte) ([]byte, error) {
	salt := make([]byte, 0, len(ephPub)+len(recipientPub))
	salt = append(salt, ephPub...)
	salt = append(salt, recipientPub...)
	return hkdf.Key(sha256.New, shared, salt, wrapInfo, 32)
}

// WrapKeyPub seals a 32-byte DEK to a base64 X25519 public key. The result is
// base64(ephPub || nonce || ciphertext+tag).
//
// The ephemeral private key is never returned and never stored, so this
// operation is irreversible without the recipient's private key — including for
// the process that performed it. That is the entire point.
func WrapKeyPub(dek []byte, pubB64 string) (string, error) {
	if len(dek) != 32 {
		return "", fmt.Errorf("data key must be 32 bytes, got %d", len(dek))
	}
	rawPub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil {
		return "", fmt.Errorf("write-only public key is not valid base64: %w", err)
	}
	recipient, err := ecdh.X25519().NewPublicKey(rawPub)
	if err != nil {
		return "", fmt.Errorf("write-only public key is not a valid X25519 key: %w", err)
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	shared, err := eph.ECDH(recipient)
	if err != nil {
		return "", err
	}
	defer zero(shared)

	ephPub := eph.PublicKey().Bytes()
	kek, err := deriveKEK(shared, ephPub, rawPub)
	if err != nil {
		return "", err
	}
	defer zero(kek)

	sealed, err := SealString(string(dek), kek)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(append(ephPub, sealed...)), nil
}

// UnwrapKeyPriv recovers a DEK sealed by WrapKeyPub using the offline private
// key. A wrong private key derives a different KEK and therefore fails the GCM
// auth tag — no plaintext is ever produced, so a bad key can't yield a partial
// or subtly-wrong result.
func UnwrapKeyPriv(wrapped, privB64 string) ([]byte, error) {
	blob, err := base64.StdEncoding.DecodeString(wrapped)
	if err != nil {
		return nil, fmt.Errorf("wrapped key not valid base64: %w", err)
	}
	if len(blob) < 32 {
		return nil, ErrNotWriteOnlyWrapped
	}
	rawPriv, err := base64.StdEncoding.DecodeString(privB64)
	if err != nil {
		return nil, errors.New("the private key is not valid base64 — paste the whole line exactly as it was issued")
	}
	priv, err := ecdh.X25519().NewPrivateKey(rawPriv)
	if err != nil {
		return nil, errors.New("that is not a valid write-only private key")
	}
	ephPub, sealed := blob[:32], blob[32:]
	ephKey, err := ecdh.X25519().NewPublicKey(ephPub)
	if err != nil {
		return nil, ErrNotWriteOnlyWrapped
	}
	shared, err := priv.ECDH(ephKey)
	if err != nil {
		return nil, err
	}
	defer zero(shared)

	kek, err := deriveKEK(shared, ephPub, priv.PublicKey().Bytes())
	if err != nil {
		return nil, err
	}
	defer zero(kek)

	s, err := OpenString(sealed, kek)
	if err != nil {
		return nil, errors.New("the private key does not match this backup — check you pasted the right one")
	}
	if len(s) != 32 {
		return nil, fmt.Errorf("unwrapped key has wrong length %d", len(s))
	}
	return []byte(s), nil
}

// PublicKeyFromPrivate derives the public half of a base64 X25519 private key,
// so a restore can check the pasted key against the manifest's fingerprint and
// say "wrong keypair" instead of "decryption failed".
func PublicKeyFromPrivate(privB64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(privB64)
	if err != nil {
		return "", errors.New("the private key is not valid base64")
	}
	priv, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return "", errors.New("that is not a valid write-only private key")
	}
	return base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes()), nil
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// Zero wipes a key the caller is done with (F204). Exported so a caller outside
// this package that has legitimately unwrapped a DEK — the recovery-key drill —
// can clear it on the way out rather than leaving it in a live heap object for
// the garbage collector to get to eventually.
func Zero(b []byte) { zero(b) }
