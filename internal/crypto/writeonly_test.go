package crypto

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

// F86's security claim is precise: with only the PUBLIC key, the wrapping party
// cannot recover what it wrapped. These tests pin that, plus the failure modes a
// user will actually hit (wrong key, corrupted blob, legacy blob).

func TestKeypairRoundTrip(t *testing.T) {
	pub, priv, err := NewBackupKeypair()
	if err != nil {
		t.Fatal(err)
	}
	dek, err := NewDataKey()
	if err != nil {
		t.Fatal(err)
	}

	wrapped, err := WrapKeyPub(dek, pub)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	got, err := UnwrapKeyPriv(wrapped, priv)
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Fatal("the unwrapped DEK must equal the original")
	}
}

// A DIFFERENT keypair must fail — this is what makes the offline key the only
// thing that can restore.
func TestKeypairWrongPrivateKeyFails(t *testing.T) {
	pub, _, _ := NewBackupKeypair()
	_, otherPriv, _ := NewBackupKeypair()
	dek, _ := NewDataKey()

	wrapped, err := WrapKeyPub(dek, pub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnwrapKeyPriv(wrapped, otherPriv); err == nil {
		t.Fatal("a wrong private key MUST fail")
	}
}

// Every wrap uses a fresh ephemeral sender key, so wrapping the same DEK twice
// must produce different ciphertext. Identical output would leak that two
// backups share a key.
func TestKeypairWrapIsNonDeterministic(t *testing.T) {
	pub, priv, _ := NewBackupKeypair()
	dek, _ := NewDataKey()

	a, _ := WrapKeyPub(dek, pub)
	b, _ := WrapKeyPub(dek, pub)
	if a == b {
		t.Fatal("two wraps of the same DEK must differ (fresh ephemeral key each time)")
	}
	// Both must still open to the same DEK.
	ka, err := UnwrapKeyPriv(a, priv)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := UnwrapKeyPriv(b, priv)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ka, kb) || !bytes.Equal(ka, dek) {
		t.Fatal("both wraps must recover the original DEK")
	}
}

// The master key must be USELESS against a write-only wrap. If UnwrapKey ever
// succeeded here, write-only mode would be decorative.
func TestKeypairMasterKeyCannotUnwrap(t *testing.T) {
	pub, _, _ := NewBackupKeypair()
	dek, _ := NewDataKey()
	master := bytes.Repeat([]byte{0x42}, 32)

	wrapped, _ := WrapKeyPub(dek, pub)
	if _, err := UnwrapKey(wrapped, master); err == nil {
		t.Fatal("the symmetric master key MUST NOT unwrap an asymmetric envelope")
	}
}

// A symmetric (legacy) wrapped key handed to the asymmetric path must be
// rejected cleanly rather than mis-parsed.
func TestKeypairRejectsSymmetricBlob(t *testing.T) {
	master := bytes.Repeat([]byte{0x7}, 32)
	dek, _ := NewDataKey()
	sym, err := WrapKey(dek, master)
	if err != nil {
		t.Fatal(err)
	}
	_, priv, _ := NewBackupKeypair()
	if _, err := UnwrapKeyPriv(sym, priv); err == nil {
		t.Fatal("a symmetric wrap must not open with a private key")
	}
}

func TestKeypairMalformedInputs(t *testing.T) {
	pub, priv, _ := NewBackupKeypair()
	dek, _ := NewDataKey()
	wrapped, _ := WrapKeyPub(dek, pub)

	if _, err := UnwrapKeyPriv("!!!not base64!!!", priv); err == nil {
		t.Error("non-base64 blob must fail")
	}
	if _, err := UnwrapKeyPriv(base64.StdEncoding.EncodeToString([]byte("short")), priv); err == nil {
		t.Error("a too-short blob must fail")
	}
	if _, err := UnwrapKeyPriv(wrapped, "not base64"); err == nil {
		t.Error("a non-base64 private key must fail")
	}
	if _, err := UnwrapKeyPriv(wrapped, base64.StdEncoding.EncodeToString([]byte("wrong length"))); err == nil {
		t.Error("a wrong-length private key must fail")
	}
	// A DEK of the wrong size must be refused rather than silently padded.
	if _, err := WrapKeyPub([]byte("too short"), pub); err == nil {
		t.Error("a non-32-byte DEK must be refused")
	}
	if _, err := WrapKeyPub(dek, "not base64"); err == nil {
		t.Error("a non-base64 public key must be refused")
	}
}

// The error a user sees when they paste the wrong key has to say so, because
// "cipher: message authentication failed" reads like data corruption and would
// send them looking for a problem that isn't there.
func TestKeypairWrongKeyErrorIsActionable(t *testing.T) {
	pub, _, _ := NewBackupKeypair()
	_, otherPriv, _ := NewBackupKeypair()
	dek, _ := NewDataKey()
	wrapped, _ := WrapKeyPub(dek, pub)

	_, err := UnwrapKeyPriv(wrapped, otherPriv)
	if err == nil || !strings.Contains(err.Error(), "does not match this backup") {
		t.Fatalf("the error must name the cause, got: %v", err)
	}
}

func TestBackupPubFPStableAndPublicOnly(t *testing.T) {
	pub, priv, _ := NewBackupKeypair()
	fp := BackupPubFP(pub)
	if len(fp) != 16 {
		t.Fatalf("fingerprint = %q, want 16 hex chars", fp)
	}
	if fp != BackupPubFP(pub) {
		t.Fatal("the fingerprint must be stable")
	}
	other, _, _ := NewBackupKeypair()
	if fp == BackupPubFP(other) {
		t.Fatal("different keypairs must fingerprint differently")
	}
	// The private key must derive the SAME public half, so a restore can check a
	// pasted key against the manifest before attempting a decrypt.
	derived, err := PublicKeyFromPrivate(priv)
	if err != nil {
		t.Fatal(err)
	}
	if derived != pub {
		t.Fatal("PublicKeyFromPrivate must reproduce the public key")
	}
	if BackupPubFP(derived) != fp {
		t.Fatal("the derived public key must fingerprint identically")
	}
}
