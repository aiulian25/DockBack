package backup

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// "Scan & adopt" reads manifest sidecars straight off a destination the operator
// has pointed at. Reading them with no bound means a corrupt or hostile
// destination can answer a request for a kilobyte manifest with gigabytes, and
// the process is OOM-killed inside its 1 GiB limit while the operator is merely
// looking for orphaned backups.

// countingCloser reports whether the reader was closed, and how much of it was
// actually consumed.
type countingCloser struct {
	r      io.Reader
	read   int
	closed bool
}

func (c *countingCloser) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += n
	return n, err
}
func (c *countingCloser) Close() error { c.closed = true; return nil }

func TestReadSidecarAcceptsARealManifest(t *testing.T) {
	body := []byte(`{"version":1,"target_name":"paperless","volumes":[]}`)
	rc := &countingCloser{r: bytes.NewReader(body)}

	got, err := readSidecar(rc, "node/app/x.dback.manifest.json")
	if err != nil {
		t.Fatalf("a real manifest must read: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("bytes changed: %q", got)
	}
	if !rc.closed {
		t.Error("the reader must be closed, or a scan of many objects leaks handles")
	}
}

func TestReadSidecarAcceptsExactlyTheLimit(t *testing.T) {
	rc := &countingCloser{r: bytes.NewReader(bytes.Repeat([]byte("x"), maxSidecarBytes))}
	got, err := readSidecar(rc, "k")
	if err != nil {
		t.Fatalf("a sidecar exactly at the limit must still read: %v", err)
	}
	if len(got) != maxSidecarBytes {
		t.Errorf("got %d bytes, want %d", len(got), maxSidecarBytes)
	}
}

func TestAdoptRejectsOversizedSidecar(t *testing.T) {
	// A 9 MiB "manifest" — past the bound, and the read must stop rather than
	// pull the whole thing in.
	const oversized = 9 << 20
	rc := &countingCloser{r: bytes.NewReader(bytes.Repeat([]byte("x"), oversized))}

	got, err := readSidecar(rc, "node/app/x.dback.manifest.json")
	if err == nil {
		t.Fatal("an implausibly large sidecar must be refused, not read into memory")
	}
	if got != nil {
		t.Error("nothing may be returned from a refused read")
	}
	if !strings.Contains(err.Error(), "node/app/x.dback.manifest.json") {
		t.Errorf("the error must name the object to go and look at: %v", err)
	}
	// The bound is what makes this a fix: it read one byte past the limit to
	// notice, and not the other megabyte.
	if rc.read > maxSidecarBytes+1 {
		t.Errorf("read %d bytes, want at most %d — the limit is not being applied", rc.read, maxSidecarBytes+1)
	}
	if !rc.closed {
		t.Error("a refused read must still close the reader")
	}
}

// A destination that fails mid-read is an error, not a truncated manifest that
// happens to parse.
func TestReadSidecarPropagatesAReadFailure(t *testing.T) {
	boom := errors.New("connection reset by peer")
	rc := &countingCloser{r: io.MultiReader(bytes.NewReader([]byte(`{"ver`)), errReader{boom})}
	if _, err := readSidecar(rc, "k"); !errors.Is(err, boom) {
		t.Fatalf("the transport failure must survive: %v", err)
	}
	if !rc.closed {
		t.Error("the reader must be closed on the failure path too")
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
