package crypto

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestKeyfileRoundTrip(t *testing.T) {
	master := make([]byte, 32)
	_, _ = rand.Read(master)

	kf, err := WrapKeyfile(master, "correct horse battery staple")
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	got, err := UnwrapKeyfile(kf, "correct horse battery staple")
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	if !bytes.Equal(got, master) {
		t.Fatal("unwrapped key != original")
	}
}

func TestKeyfileWrongPassphrase(t *testing.T) {
	master := make([]byte, 32)
	_, _ = rand.Read(master)
	kf, _ := WrapKeyfile(master, "right")
	if _, err := UnwrapKeyfile(kf, "wrong"); err == nil {
		t.Fatal("wrong passphrase must fail")
	}
}

func TestKeyfileTamperFails(t *testing.T) {
	master := make([]byte, 32)
	_, _ = rand.Read(master)
	kf, _ := WrapKeyfile(master, "pass")
	// Flip a byte in the middle of the JSON (hits the base64 sealed body/salt).
	kf[len(kf)/2] ^= 0xFF
	if _, err := UnwrapKeyfile(kf, "pass"); err == nil {
		t.Fatal("tampered keyfile must fail")
	}
}

func TestKeyfileRejectsBadInputs(t *testing.T) {
	if _, err := WrapKeyfile(make([]byte, 16), "pass"); err == nil {
		t.Error("non-32-byte key must be rejected")
	}
	if _, err := WrapKeyfile(make([]byte, 32), ""); err == nil {
		t.Error("empty passphrase must be rejected")
	}
	if _, err := UnwrapKeyfile([]byte("not json"), "pass"); err == nil {
		t.Error("invalid keyfile must be rejected")
	}
}
