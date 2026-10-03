package crypto

import (
	"bytes"
	"crypto/rand"
	"io"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	// Span several chunk boundaries.
	sizes := []int{0, 1, 100, chunkSize - 1, chunkSize, chunkSize + 1, 3*chunkSize + 7}
	for _, n := range sizes {
		plain := make([]byte, n)
		rand.Read(plain)

		var ct bytes.Buffer
		sha, err := Encrypt(&ct, bytes.NewReader(plain), key)
		if err != nil {
			t.Fatalf("encrypt n=%d: %v", n, err)
		}
		if sha == "" {
			t.Fatalf("empty sha n=%d", n)
		}

		var out bytes.Buffer
		if err := Decrypt(&out, bytes.NewReader(ct.Bytes()), key); err != nil {
			t.Fatalf("decrypt n=%d: %v", n, err)
		}
		if !bytes.Equal(out.Bytes(), plain) {
			t.Fatalf("round-trip mismatch n=%d", n)
		}
	}
}

func TestTamperDetected(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	plain := []byte("sensitive backup bytes that must be authenticated")

	var ct bytes.Buffer
	if _, err := Encrypt(&ct, bytes.NewReader(plain), key); err != nil {
		t.Fatal(err)
	}
	b := ct.Bytes()
	b[len(b)-1] ^= 0xFF // flip a ciphertext bit

	var out bytes.Buffer
	if err := Decrypt(&out, bytes.NewReader(b), key); err == nil {
		t.Fatal("expected auth failure on tampered ciphertext")
	}
}

func TestWrongKeyFails(t *testing.T) {
	k1 := make([]byte, 32)
	k2 := make([]byte, 32)
	rand.Read(k1)
	rand.Read(k2)
	var ct bytes.Buffer
	Encrypt(&ct, bytes.NewReader([]byte("hello")), k1)
	if err := Decrypt(io.Discard, bytes.NewReader(ct.Bytes()), k2); err == nil {
		t.Fatal("expected failure decrypting with wrong key")
	}
}

func TestPasswordHashing(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword("correct horse battery staple", h) {
		t.Fatal("valid password rejected")
	}
	if VerifyPassword("wrong", h) {
		t.Fatal("wrong password accepted")
	}
}

func TestEnvelopeWrapUnwrap(t *testing.T) {
	master := make([]byte, 32)
	if _, err := rand.Read(master); err != nil {
		t.Fatal(err)
	}
	dek, err := NewDataKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(dek) != 32 {
		t.Fatalf("DEK length = %d, want 32", len(dek))
	}

	wrapped, err := WrapKey(dek, master)
	if err != nil {
		t.Fatalf("WrapKey: %v", err)
	}
	// Wrapped form must not leak the raw key.
	if bytes.Contains([]byte(wrapped), dek) {
		t.Fatal("wrapped key contains the raw DEK")
	}

	got, err := UnwrapKey(wrapped, master)
	if err != nil {
		t.Fatalf("UnwrapKey: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Fatalf("round-trip mismatch:\n got %x\nwant %x", got, dek)
	}

	// A different master key must fail to unwrap (this is how rotation/key
	// mismatch is detected) — never returns a bogus key.
	other := make([]byte, 32)
	if _, err := rand.Read(other); err != nil {
		t.Fatal(err)
	}
	if _, err := UnwrapKey(wrapped, other); err == nil {
		t.Fatal("unwrap with wrong master key should fail")
	}
}

func TestEnvelopeArchiveRoundTrip(t *testing.T) {
	master := make([]byte, 32)
	_, _ = rand.Read(master)
	dek, _ := NewDataKey()
	wrapped, _ := WrapKey(dek, master)

	plain := bytes.Repeat([]byte("envelope-encryption-payload!"), 5000)

	// Encrypt the archive with the DEK (as the engine does).
	var ct bytes.Buffer
	if _, err := Encrypt(&ct, bytes.NewReader(plain), dek); err != nil {
		t.Fatal(err)
	}

	// Restore path: unwrap the DEK with the master key, then decrypt.
	recoveredDEK, err := UnwrapKey(wrapped, master)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Decrypt(&out, bytes.NewReader(ct.Bytes()), recoveredDEK); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), plain) {
		t.Fatal("decrypted archive != original")
	}
}

func TestManifestSignVerify(t *testing.T) {
	master := make([]byte, 32)
	_, _ = rand.Read(master)
	manifest := []byte(`{"backup_id":"b1","cipher_sha256":"abc","wrapped_key":"def"}`)

	sig := SignManifest(manifest, master)
	if sig == "" {
		t.Fatal("empty signature")
	}
	if !VerifyManifest(manifest, master, sig) {
		t.Fatal("valid signature should verify")
	}

	// Tampered manifest (e.g. swapped wrapped key / cipher hash) must fail.
	tampered := []byte(`{"backup_id":"b1","cipher_sha256":"abc","wrapped_key":"EVIL"}`)
	if VerifyManifest(tampered, master, sig) {
		t.Fatal("tampered manifest must NOT verify")
	}

	// Wrong master key must fail (and the subkey is domain-separated).
	other := make([]byte, 32)
	_, _ = rand.Read(other)
	if VerifyManifest(manifest, other, sig) {
		t.Fatal("signature must not verify under a different master key")
	}

	// A garbage signature must fail (and not panic).
	if VerifyManifest(manifest, master, "not-a-real-sig") {
		t.Fatal("garbage signature must not verify")
	}
}

// F16: RewrapDEK must move a wrapped DEK from one master key to another without
// changing the underlying data key, and must refuse (error, no output) a wrapped
// key that doesn't belong to the old key — so rotation can safely SKIP it.
func TestRewrapDEK(t *testing.T) {
	oldKey := make([]byte, 32)
	newKey := make([]byte, 32)
	thirdKey := make([]byte, 32)
	_, _ = rand.Read(oldKey)
	_, _ = rand.Read(newKey)
	_, _ = rand.Read(thirdKey)

	dek, err := NewDataKey()
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := WrapKey(dek, oldKey)
	if err != nil {
		t.Fatal(err)
	}

	rewrapped, err := RewrapDEK(wrapped, oldKey, newKey)
	if err != nil {
		t.Fatalf("rewrap old->new: %v", err)
	}

	// The DEK recovered under the new key is identical to the original.
	got, err := UnwrapKey(rewrapped, newKey)
	if err != nil {
		t.Fatalf("unwrap with new key: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Fatal("re-wrapped DEK differs from the original — the archive would be unrecoverable")
	}

	// The re-wrapped key must NO LONGER open with the old key.
	if _, err := UnwrapKey(rewrapped, oldKey); err == nil {
		t.Fatal("re-wrapped DEK must not unwrap with the old key")
	}

	// A key wrapped by an unrelated (third) key is refused, not silently mangled.
	if _, err := RewrapDEK(wrapped, thirdKey, newKey); err == nil {
		t.Fatal("rewrap must fail when the wrapped key does not belong to the old key")
	}
}
