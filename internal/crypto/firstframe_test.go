package crypto

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"strings"
	"testing"
)

// F204 — decrypting exactly one frame, the primitive the recovery-key drill is
// built on. It has to be as strict as a full decrypt about what it accepts,
// because a check that passes on the wrong key is worse than no check at all.

func ffKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestDecryptFirstFrameAcceptsOnlyTheRightKey(t *testing.T) {
	key, wrong := ffKey(t), ffKey(t)

	for _, size := range []int{0, 100, chunkSize, chunkSize * 3} {
		plain := bytes.Repeat([]byte("x"), size)
		var ct bytes.Buffer
		if _, err := Encrypt(&ct, bytes.NewReader(plain), key); err != nil {
			t.Fatal(err)
		}
		if err := DecryptFirstFrame(bytes.NewReader(ct.Bytes()), key); err != nil {
			t.Errorf("size %d: the right key must open frame 0: %v", size, err)
		}
		if err := DecryptFirstFrame(bytes.NewReader(ct.Bytes()), wrong); err == nil {
			t.Errorf("size %d: the wrong key must NOT open frame 0", size)
		}
	}
}

// It stops after one frame — that is the entire performance claim.
func TestDecryptFirstFrameStopsAfterOneFrame(t *testing.T) {
	key := ffKey(t)
	var ct bytes.Buffer
	if _, err := Encrypt(&ct, bytes.NewReader(bytes.Repeat([]byte("y"), chunkSize*4)), key); err != nil {
		t.Fatal(err)
	}
	r := &countingReader{r: bytes.NewReader(ct.Bytes())}
	if err := DecryptFirstFrame(r, key); err != nil {
		t.Fatal(err)
	}
	if r.n >= int64(ct.Len()) {
		t.Fatalf("read %d of %d bytes — it must not stream the whole archive", r.n, ct.Len())
	}
	if r.n > int64(MaxFirstFrameBytes) {
		t.Errorf("read %d bytes, bound is %d", r.n, MaxFirstFrameBytes)
	}
}

// A declared frame length is attacker-controlled data. It must be refused
// before it is allocated, not after.
func TestDecryptFirstFrameRefusesAnAbsurdFrameLength(t *testing.T) {
	key := ffKey(t)
	buf := new(bytes.Buffer)
	buf.WriteString(magic)
	buf.Write([]byte{1, 2, 3, 4}) // nonce prefix
	var fh [5]byte
	binary.BigEndian.PutUint32(fh[1:], 0xFFFFFFFF) // "4 GiB coming"
	buf.Write(fh[:])

	err := DecryptFirstFrame(bytes.NewReader(buf.Bytes()), key)
	if err == nil {
		t.Fatal("an impossible frame length must be refused")
	}
	if !strings.Contains(err.Error(), "exceeds the format maximum") {
		t.Errorf("want a bounds refusal, got: %v", err)
	}
}

// Not an archive at all, and a truncated one.
func TestDecryptFirstFrameRejectsNonArchives(t *testing.T) {
	key := ffKey(t)
	if err := DecryptFirstFrame(strings.NewReader("this is not a backup at all, really"), key); err == nil {
		t.Error("arbitrary bytes must not decrypt")
	}
	var ct bytes.Buffer
	if _, err := Encrypt(&ct, strings.NewReader("hello"), key); err != nil {
		t.Fatal(err)
	}
	truncated := ct.Bytes()[:len(magic)+4+3]
	if err := DecryptFirstFrame(bytes.NewReader(truncated), key); err == nil {
		t.Error("a truncated archive must not report success")
	}
	// A short key is a caller error, not a pass.
	if err := DecryptFirstFrame(bytes.NewReader(ct.Bytes()), []byte("too-short")); err == nil {
		t.Error("a malformed key must be refused")
	}
}

type countingReader struct {
	r *bytes.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
