package crypto

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
)

// TestGenerateWriteOnlyVector is a helper, not an assertion: with DBACK_VECTOR
// set it emits a Go-produced write-only envelope so the pure-Python recovery
// script can be checked against it. Cross-language agreement on this envelope is
// the difference between "write-only" and "write-only and unrecoverable".
func TestGenerateWriteOnlyVector(t *testing.T) {
	path := os.Getenv("DBACK_VECTOR")
	if path == "" {
		t.Skip("set DBACK_VECTOR=<file> to emit a cross-language test vector")
	}
	pub, priv, err := NewBackupKeypair()
	if err != nil {
		t.Fatal(err)
	}
	dek, _ := NewDataKey()
	wrapped, err := WrapKeyPub(dek, pub)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{
		"priv": priv, "pub": pub, "wrapped": wrapped,
		"dek": base64.StdEncoding.EncodeToString(dek), "fp": BackupPubFP(pub),
	})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
