// Package crypto implements DockBack's at-rest encryption and password
// hashing. Backups are encrypted with AES-256-GCM in an authenticated,
// chunked stream so arbitrarily large archives are handled without exceeding
// GCM's per-nonce limits, while truncation/reordering are still detected
// (PLAN §2.7, §3.4, §9.3). The on-disk format is documented below so a backup
// can be decrypted by other tooling if DockBack is gone (PLAN §9.3).
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Stream format (after the literal magic):
//
//	magic[7]        = "DBACKv1"
//	noncePrefix[4]  = random, shared by all frames
//	frame*:
//	  flag[1]       = 0 (more) | 1 (final)   -- authenticated as AAD
//	  len[4]        = big-endian ciphertext length
//	  ct[len]       = AES-256-GCM(plaintext_chunk), nonce = noncePrefix||counter
//
// The per-frame counter (8-byte big-endian) makes nonces unique and detects
// reordering; the final flag is authenticated so truncation is caught.
const (
	magic     = "DBACKv1"
	chunkSize = 1 << 20 // 1 MiB plaintext per frame
)

// maxFrameCipherBytes is the largest ciphertext a legitimate DBACKv1 frame can
// carry: one full plaintext chunk plus the GCM tag. The length is read FROM THE
// STREAM, so it must be bounded before it is used to allocate — a corrupt or
// hostile length field must be a refusal, not a 4 GiB allocation.
const maxFrameCipherBytes = chunkSize + 16

// Encrypt reads plaintext from src, writes the encrypted stream to dst, and
// returns the hex SHA-256 of the *ciphertext* (for manifest integrity checks,
// PLAN §3.4). key must be 32 bytes.
func Encrypt(dst io.Writer, src io.Reader, key []byte) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}

	h := sha256.New()
	w := io.MultiWriter(dst, h)

	prefix := make([]byte, 4)
	if _, err := rand.Read(prefix); err != nil {
		return "", err
	}
	if _, err := io.WriteString(w, magic); err != nil {
		return "", err
	}
	if _, err := w.Write(prefix); err != nil {
		return "", err
	}

	buf := make([]byte, chunkSize)
	var counter uint64
	for {
		n, readErr := io.ReadFull(src, buf)
		final := false
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			final = true
		} else if readErr != nil {
			return "", readErr
		}

		flag := byte(0)
		if final {
			flag = 1
		}
		nonce := makeNonce(prefix, counter)
		ct := gcm.Seal(nil, nonce, buf[:n], []byte{flag})

		if err := writeFrame(w, flag, ct); err != nil {
			return "", err
		}
		counter++
		if final {
			break
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// Decrypt reads the encrypted stream from src and writes plaintext to dst.
// If hcheck is non-nil it must be a fresh sha256 hasher fed the ciphertext for
// an integrity pre-check by the caller; Decrypt itself relies on GCM auth tags.
func Decrypt(dst io.Writer, src io.Reader, key []byte) error {
	gcm, err := newGCM(key)
	if err != nil {
		return err
	}

	hdr := make([]byte, len(magic)+4)
	if _, err := io.ReadFull(src, hdr); err != nil {
		return fmt.Errorf("reading header: %w", err)
	}
	if string(hdr[:len(magic)]) != magic {
		return errors.New("bad magic: not a DockBack archive")
	}
	prefix := hdr[len(magic):]

	var counter uint64
	for {
		flag, ct, err := readFrame(src)
		if err != nil {
			return err
		}
		nonce := makeNonce(prefix, counter)
		pt, err := gcm.Open(nil, nonce, ct, []byte{flag})
		if err != nil {
			return fmt.Errorf("frame %d auth failed (corrupt or tampered): %w", counter, err)
		}
		if _, err := dst.Write(pt); err != nil {
			return err
		}
		counter++
		if flag == 1 {
			return nil
		}
	}
}

// DecryptFirstFrame opens ONLY the first frame of an encrypted stream (F204).
//
// It exists for the recovery-key drill: proving that a saved offline private key
// actually decrypts a write-only backup requires decrypting something, but not
// all of it. The very first GCM tag is sufficient proof — the key that opens
// frame 0 is the key that opens every frame, since one DEK encrypts the whole
// stream — and stopping there turns "does my key work" from a full download and
// restore into a bounded, seconds-long check the operator will actually run.
//
// It reads at most the header plus one frame and never writes the plaintext
// anywhere. The frame length is bounded before allocating, because the length is
// read from the stream: a corrupt or hostile archive claiming a 4 GiB frame must
// be a refusal, not a 4 GiB allocation. No legitimate DBACKv1 frame can exceed
// one chunk plus the GCM tag.
//
// It deliberately does NOT return the plaintext. The caller wants a yes or no,
// and handing back a megabyte of a backup's contents from a verification path
// would be a way to read a write-only backup a slice at a time.
func DecryptFirstFrame(src io.Reader, key []byte) error {
	gcm, err := newGCM(key)
	if err != nil {
		return err
	}
	hdr := make([]byte, len(magic)+4)
	if _, err := io.ReadFull(src, hdr); err != nil {
		return fmt.Errorf("reading header: %w", err)
	}
	if string(hdr[:len(magic)]) != magic {
		return errors.New("bad magic: not a DockBack archive")
	}
	prefix := hdr[len(magic):]

	var fh [5]byte
	if _, err := io.ReadFull(src, fh[:]); err != nil {
		return fmt.Errorf("reading frame header: %w", err)
	}
	n := binary.BigEndian.Uint32(fh[1:])
	if int64(n) > int64(maxFrameCipherBytes) {
		return fmt.Errorf("frame length %d exceeds the format maximum (corrupt or not a DockBack archive)", n)
	}
	ct := make([]byte, n)
	if _, err := io.ReadFull(src, ct); err != nil {
		return fmt.Errorf("reading frame body: %w", err)
	}
	pt, err := gcm.Open(nil, makeNonce(prefix, 0), ct, []byte{fh[0]})
	if err != nil {
		return fmt.Errorf("frame 0 auth failed (wrong key, or corrupt/tampered archive): %w", err)
	}
	zero(pt)
	return nil
}

// MaxFirstFrameBytes is how much of an archive DecryptFirstFrame can consume:
// the magic, the nonce prefix, one frame header, and one full frame. Callers
// bound their reader with it so a verification never streams a whole backup.
const MaxFirstFrameBytes = len(magic) + 4 + 5 + chunkSize + 16

// CipherSHA256 streams src through a sha256 hasher without decrypting, used to
// verify the stored checksum before a restore (PLAN §3.4/§9.4).
func CipherSHA256(src io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, src); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func makeNonce(prefix []byte, counter uint64) []byte {
	nonce := make([]byte, 12)
	copy(nonce[:4], prefix)
	binary.BigEndian.PutUint64(nonce[4:], counter)
	return nonce
}

func writeFrame(w io.Writer, flag byte, ct []byte) error {
	var hdr [5]byte
	hdr[0] = flag
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(ct)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(ct)
	return err
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, fmt.Errorf("reading frame header: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if int64(n) > int64(maxFrameCipherBytes) {
		return 0, nil, fmt.Errorf("frame length %d exceeds the format maximum (corrupt or not a DockBack archive)", n)
	}
	ct := make([]byte, n)
	if _, err := io.ReadFull(r, ct); err != nil {
		return 0, nil, fmt.Errorf("reading frame body: %w", err)
	}
	return hdr[0], ct, nil
}

// --- Small-secret sealing (storage credentials at rest, PLAN §3.8) ---

// SealString encrypts a short secret (e.g. a destination's credentials JSON)
// with AES-256-GCM, returning nonce||ciphertext.
func SealString(plain string, key []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(plain), nil), nil
}

// OpenString reverses SealString.
func OpenString(blob, key []byte) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	ns := gcm.NonceSize()
	if len(blob) < ns {
		return "", errors.New("sealed blob too short")
	}
	pt, err := gcm.Open(nil, blob[:ns], blob[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// --- Envelope encryption: per-backup data key wrapped by the master key (PLAN §3.3) ---

// NewDataKey returns a fresh random 32-byte data-encryption key (DEK). Each
// backup gets its own DEK; the archive is encrypted with the DEK and the DEK is
// wrapped (encrypted) with the master key, so rotating the master key only
// re-wraps DEKs instead of re-encrypting every archive.
func NewDataKey() ([]byte, error) {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	return k, nil
}

// WrapKey encrypts a data key (DEK) with the master key (KEK) and returns it
// base64-encoded for storage in the manifest (PLAN §3.3).
func WrapKey(dek, master []byte) (string, error) {
	if len(dek) != 32 {
		return "", fmt.Errorf("data key must be 32 bytes, got %d", len(dek))
	}
	blob, err := SealString(string(dek), master)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(blob), nil
}

// UnwrapKey reverses WrapKey: it base64-decodes and decrypts the wrapped DEK
// with the master key. A wrong master key fails the GCM auth tag (no plaintext
// is ever produced), which is how rotation/key-mismatch is detected.
func UnwrapKey(wrapped string, master []byte) ([]byte, error) {
	blob, err := base64.StdEncoding.DecodeString(wrapped)
	if err != nil {
		return nil, fmt.Errorf("wrapped key not valid base64: %w", err)
	}
	s, err := OpenString(blob, master)
	if err != nil {
		return nil, err
	}
	if len(s) != 32 {
		return nil, fmt.Errorf("unwrapped key has wrong length %d", len(s))
	}
	return []byte(s), nil
}

// RewrapDEK re-wraps a base64 wrapped DEK from oldKey to newKey for master-key
// rotation (F16). It unwraps with oldKey — whose GCM auth tag proves the DEK
// actually belongs to oldKey — and re-wraps with newKey, so the underlying data
// key (and therefore the archive it encrypts) is never changed and never leaves
// memory in the clear. A wrapped key that doesn't belong to oldKey returns an
// error, letting the caller SKIP it (leaving it byte-for-byte intact) rather than
// corrupt a backup that was wrapped by an unrelated key.
func RewrapDEK(wrapped string, oldKey, newKey []byte) (string, error) {
	dek, err := UnwrapKey(wrapped, oldKey)
	if err != nil {
		return "", err
	}
	defer func() {
		for i := range dek {
			dek[i] = 0
		}
	}()
	return WrapKey(dek, newKey)
}

// --- Manifest signing: tamper-evident metadata (PLAN §3.5) ---

// manifestHMACKey derives a domain-separated subkey for manifest signing so the
// master key is never used directly for HMAC (key separation).
func manifestHMACKey(master []byte) []byte {
	m := hmac.New(sha256.New, master)
	m.Write([]byte("dockback-manifest-hmac-v1"))
	return m.Sum(nil)
}

// SignManifest returns the hex HMAC-SHA256 of data under a master-derived subkey.
// The signature is stored beside the (readable) manifest so tampering with a
// stored manifest — swapping the wrapped key, cipher hash, image digest, etc. —
// is detectable, while the manifest stays self-describing for hand-restore
// (PLAN §3.5/§9.3).
func SignManifest(data, master []byte) string {
	m := hmac.New(sha256.New, manifestHMACKey(master))
	m.Write(data)
	return hex.EncodeToString(m.Sum(nil))
}

// VerifyManifest reports, in constant time, whether sig is a valid signature of
// data under the master key.
func VerifyManifest(data, master []byte, sig string) bool {
	want := SignManifest(data, master)
	return subtle.ConstantTimeCompare([]byte(want), []byte(sig)) == 1
}

// --- Password hashing (argon2id), single-admin auth (PLAN §3.1) ---

const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// HashPassword returns a self-describing argon2id encoded hash.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether pw matches the encoded argon2id hash, using a
// constant-time comparison.
func VerifyPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version, mem, time, threads int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &time, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, uint32(time), uint32(mem), uint8(threads), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}
