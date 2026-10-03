package api

import (
	"bytes"

	"dockback/internal/crypto"
)

// Node connection secrets — SSH private keys + passphrases, and mTLS bundles —
// are sealed at rest with the master key (F2 / PLAN §3.8), exactly like
// destination credentials. The dockercli transport layer stays crypto-free: the
// API seals before every store write and unseals right before building a
// NodeConn, so the registry only ever sees plaintext at use time.

// nseMagic prefixes a sealed node secret. A stored blob WITHOUT it is a legacy
// plaintext secret (pre-F2), detected and migrated seamlessly on next startup.
var nseMagic = []byte("NSE1")

// SealNodeSecret AES-256-GCM seals a node secret with the master key and prefixes
// the version magic. An empty secret (proxy transports carry none) is returned
// unchanged — there is nothing to protect. On the practically-impossible seal
// error (the master key is validated to 32 bytes at config load) the plaintext is
// returned unchanged so a node is never lost; the next migration pass re-seals it.
func SealNodeSecret(plain, key []byte) []byte {
	if len(plain) == 0 {
		return plain
	}
	blob, err := crypto.SealString(string(plain), key)
	if err != nil {
		return plain
	}
	out := make([]byte, 0, len(nseMagic)+len(blob))
	out = append(out, nseMagic...)
	return append(out, blob...)
}

// OpenNodeSecret reverses SealNodeSecret. A blob without the NSE1 magic is a
// legacy plaintext secret (or empty) and is returned as-is — seamless migration.
// A sealed blob that fails to open (e.g. a different master key) is returned as-is
// too, so the transport surfaces a clear parse/auth error instead of this layer
// masking it as an empty credential.
func OpenNodeSecret(blob, key []byte) []byte {
	if !isSealedNodeSecret(blob) {
		return blob
	}
	pt, err := crypto.OpenString(blob[len(nseMagic):], key)
	if err != nil {
		return blob
	}
	return []byte(pt)
}

// isSealedNodeSecret reports whether a stored blob is already sealed (carries the
// magic), so the startup migration only re-seals legacy plaintext.
func isSealedNodeSecret(blob []byte) bool {
	return len(blob) >= len(nseMagic) && bytes.Equal(blob[:len(nseMagic)], nseMagic)
}
