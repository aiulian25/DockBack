package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// slowReader emits one byte per call forever — a stand-in for a sidecar tar of a
// huge volume: always has more data, never ends on its own.
type slowReader struct{ n int }

func (s *slowReader) Read(p []byte) (int, error) {
	s.n++
	if len(p) > 0 {
		p[0] = 'x'
	}
	return 1, nil
}

// A copy from an endless stream must stop as soon as the run is canceled —
// before this fix, a canceled backup kept reading until the whole volume had
// been tarred, which on a large container looked like "cancel never finishes".
func TestCtxReaderStopsEndlessCopy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	src := &slowReader{}
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()

	start := time.Now()
	_, err := io.Copy(io.Discard, ctxReader(ctx, src))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("copy must end with the cancellation error, got %v", err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("cancel must stop the copy promptly, took %s", el)
	}
}

// Until it's canceled, ctxReader is a transparent passthrough — the same bytes,
// no truncation, no extra buffering semantics.
func TestCtxReaderPassesBytesThrough(t *testing.T) {
	want := strings.Repeat("volume-bytes-", 5000)
	var got bytes.Buffer
	n, err := io.Copy(&got, ctxReader(context.Background(), strings.NewReader(want)))
	if err != nil {
		t.Fatal(err)
	}
	if int(n) != len(want) || got.String() != want {
		t.Fatalf("passthrough altered the stream: %d of %d bytes", n, len(want))
	}

	// A cancelable context wraps; a context that can NEVER be canceled
	// (Background, or nil) passes the reader straight through — no wrapper, no
	// per-read overhead on paths that can't be canceled anyway.
	r := strings.NewReader("x")
	cctx, ccancel := context.WithCancel(context.Background())
	defer ccancel()
	if ctxReader(cctx, r) == io.Reader(r) {
		t.Fatal("a cancelable context must wrap")
	}
	if ctxReader(context.Background(), r) != io.Reader(r) {
		t.Fatal("a non-cancelable context must pass through unwrapped")
	}
	if ctxReader(nil, r) != io.Reader(r) { //nolint:staticcheck // explicit nil-ctx guard
		t.Fatal("a nil context must pass the reader through unwrapped")
	}
}
