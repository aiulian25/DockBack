package dockercli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// A database dump is bounded by SILENCE now, not by the clock. The old fixed
// 30-minute cap failed a large pg_dumpall after every byte had already been
// written, and reported it as a generic exec-inspect error — so the biggest
// databases were the ones that could never be backed up or restored.

func TestIdleContextEndsOnSilence(t *testing.T) {
	ctx, _, stop := idleContext(context.Background(), 100*time.Millisecond)
	defer stop()

	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Errorf("err = %v, want cancellation", ctx.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a command that says nothing at all must still be ended")
	}
}

func TestIdleContextSurvivesFarBeyondTheWindowWhileMoving(t *testing.T) {
	const idle = 100 * time.Millisecond
	ctx, keepAlive, stop := idleContext(context.Background(), idle)
	defer stop()

	// Report progress for five times the window — the case the fixed deadline got
	// wrong.
	deadline := time.Now().Add(5 * idle)
	for time.Now().Before(deadline) {
		time.Sleep(idle / 4)
		keepAlive()
		if ctx.Err() != nil {
			t.Fatalf("a stream still moving was cut off after %v", time.Since(deadline.Add(-5*idle)))
		}
	}
	// …and once it really stops, it ends.
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a stream that stopped must eventually be ended")
	}
}

func TestIdleContextStopReleasesEverything(t *testing.T) {
	ctx, keepAlive, stop := idleContext(context.Background(), time.Hour)
	stop()
	if ctx.Err() == nil {
		t.Error("stop must cancel the derived context so nothing leaks")
	}
	keepAlive() // must not panic or resurrect the context
	if ctx.Err() == nil {
		t.Error("a keep-alive after stop must not revive the context")
	}
}

// A cancelled parent must still end the exec: the idle clock only ever ADDS a
// bound, it never removes the caller's.
func TestIdleContextInheritsTheParent(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	ctx, _, stop := idleContext(parent, time.Hour)
	defer stop()

	cancelParent()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the caller's own deadline must still bound the exec")
	}
}

// The reader is what ties the two together: bytes moving is what counts as
// progress, and the data must pass through untouched.
func TestKeepAliveReaderReportsOnlyRealProgress(t *testing.T) {
	beats := 0
	src := strings.NewReader("pg_dump output")
	k := keepAliveReader{r: src, keepAlive: func() { beats++ }}

	got, err := io.ReadAll(k)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "pg_dump output" {
		t.Errorf("the stream must pass through unchanged: %q", got)
	}
	if beats == 0 {
		t.Error("bytes moving must restart the clock")
	}

	// An empty read is not progress — a reader returning (0, nil) in a loop must
	// not hold a hung command open forever.
	beats = 0
	empty := keepAliveReader{r: readerFunc(func(p []byte) (int, error) { return 0, nil }), keepAlive: func() { beats++ }}
	for i := 0; i < 10; i++ {
		if _, err := empty.Read(make([]byte, 8)); err != nil {
			t.Fatal(err)
		}
	}
	if beats != 0 {
		t.Errorf("empty reads must not count as progress, got %d", beats)
	}
}

// End to end over the two pieces: a trickle far longer than the window keeps the
// exec alive, and the silence after it ends the exec.
func TestSlowStreamOutlivesTheIdleWindow(t *testing.T) {
	const idle = 150 * time.Millisecond
	ctx, keepAlive, stop := idleContext(context.Background(), idle)
	defer stop()

	// One byte every 30ms for a second: seven times the window, always moving.
	sent := 0
	trickle := readerFunc(func(p []byte) (int, error) {
		if sent >= 33 {
			return 0, io.EOF
		}
		time.Sleep(30 * time.Millisecond)
		sent++
		p[0] = 'x'
		return 1, nil
	})

	start := time.Now()
	n, err := io.Copy(io.Discard, keepAliveReader{r: trickle, keepAlive: keepAlive})
	if err != nil {
		t.Fatalf("a trickling dump must complete: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 3*idle {
		t.Fatalf("the transfer took %v, too short to prove it outlived the %v window", elapsed, idle)
	}
	if n != 33 {
		t.Errorf("every byte must arrive, got %d", n)
	}
	if ctx.Err() != nil {
		t.Fatal("a stream that never paused was cut off — this is the bug being fixed")
	}
}
