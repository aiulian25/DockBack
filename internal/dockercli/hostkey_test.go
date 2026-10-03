package dockercli

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// newTestHostKey generates a throwaway ed25519 ssh.PublicKey for the callback tests.
func newTestHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return pk
}

// TestPinningHostKeyCallback covers trust-on-first-use pinning (F1): first use
// saves + accepts, a matching key accepts, and a changed key is refused.
func TestPinningHostKeyCallback(t *testing.T) {
	keyA := newTestHostKey(t)
	keyB := newTestHostKey(t)

	// --- First use: nothing stored → save + accept. ---
	var saved []byte
	cb := PinningHostKeyCallback(
		func() ([]byte, bool) { return saved, saved != nil },
		func(pub ssh.PublicKey) { saved = ssh.MarshalAuthorizedKey(pub) },
	)
	if err := cb("host:22", nil, keyA); err != nil {
		t.Fatalf("first use should accept and pin: %v", err)
	}
	if saved == nil {
		t.Fatal("first use should have saved the host key")
	}

	// --- Same key on a later connect → accept, no error. ---
	if err := cb("host:22", nil, keyA); err != nil {
		t.Fatalf("matching key should accept: %v", err)
	}

	// --- Changed key → refuse with the actionable mismatch error. ---
	err := cb("host:22", nil, keyB)
	if err == nil {
		t.Fatal("a changed host key must be refused")
	}
	msg := err.Error()
	for _, want := range []string{"ssh host key changed for host:22", "refusing to connect", "reset its pinned key"} {
		if !strings.Contains(msg, want) {
			t.Errorf("mismatch error missing %q: %s", want, msg)
		}
	}

	// --- nil load/save (un-wired registry) accepts first-use without panicking. ---
	if err := PinningHostKeyCallback(nil, nil)("h", nil, keyA); err != nil {
		t.Fatalf("nil load/save should accept first-use: %v", err)
	}
}

// TestParseHostKeyRoundTrip verifies ParseHostKey + AuthorizedKeyBytes reconstruct
// a comparable authorized-key line (F1 persistence <-> comparison).
func TestParseHostKeyRoundTrip(t *testing.T) {
	key := newTestHostKey(t)
	marshaled := ssh.MarshalAuthorizedKey(key)
	kt, kb, fp, ok := ParseHostKey(marshaled)
	if !ok {
		t.Fatal("ParseHostKey failed on a valid marshaled key")
	}
	if !strings.HasPrefix(fp, "SHA256:") {
		t.Fatalf("fingerprint should be SHA256:… got %q", fp)
	}
	// Reconstructed authorized-key bytes must equal the original (trimmed).
	got := strings.TrimSpace(string(AuthorizedKeyBytes(kt, kb)))
	if got != strings.TrimSpace(string(marshaled)) {
		t.Fatalf("round-trip mismatch:\n got  %q\n want %q", got, strings.TrimSpace(string(marshaled)))
	}
}
