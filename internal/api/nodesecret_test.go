package api

import (
	"bytes"
	"testing"
)

// TestNodeSecretSealRoundTrip covers the at-rest sealing of node secrets (F2):
// a seal→open round-trip returns the original, the sealed blob carries the NSE1
// magic and no plaintext, legacy plaintext passes through both ways, and a wrong
// master key returns the blob unchanged (never a silent empty credential).
func TestNodeSecretSealRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, 32)
	plain := []byte(`{"key":"-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----","passphrase":"hunter2"}`)

	sealed := SealNodeSecret(plain, key)
	if !isSealedNodeSecret(sealed) {
		t.Fatal("sealed blob must carry the NSE1 magic")
	}
	if !bytes.HasPrefix(sealed, nseMagic) {
		t.Fatalf("sealed blob must begin with %q", nseMagic)
	}
	if bytes.Contains(sealed, []byte("BEGIN OPENSSH")) || bytes.Contains(sealed, []byte("hunter2")) {
		t.Fatal("sealed blob must not contain the plaintext key or passphrase")
	}

	if got := OpenNodeSecret(sealed, key); !bytes.Equal(got, plain) {
		t.Fatalf("round-trip mismatch:\n got  %q\n want %q", got, plain)
	}

	// Legacy plaintext (no magic) passes through unchanged, both directions.
	legacy := []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nlegacy\n-----END OPENSSH PRIVATE KEY-----")
	if isSealedNodeSecret(legacy) {
		t.Fatal("legacy plaintext must not be detected as sealed")
	}
	if got := OpenNodeSecret(legacy, key); !bytes.Equal(got, legacy) {
		t.Fatal("legacy plaintext must pass through OpenNodeSecret unchanged")
	}

	// Empty secret (proxy transports) is left empty, not sealed.
	if got := SealNodeSecret(nil, key); len(got) != 0 {
		t.Fatalf("empty secret must stay empty, got %d bytes", len(got))
	}
	if got := SealNodeSecret([]byte{}, key); len(got) != 0 {
		t.Fatalf("empty secret must stay empty, got %d bytes", len(got))
	}

	// A wrong master key can't open the blob: it's returned as-is so the transport
	// surfaces a clear error rather than this layer masking it as empty.
	wrong := bytes.Repeat([]byte{0x22}, 32)
	if got := OpenNodeSecret(sealed, wrong); !bytes.Equal(got, sealed) {
		t.Fatal("open with the wrong key must return the sealed blob unchanged (never empty/partial)")
	}
}
