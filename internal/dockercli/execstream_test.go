package dockercli

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// A database import writes a multi-GB dump into an exec's stdin while the client
// writes back to stdout. The output pipe is small, so reading it only AFTER the
// whole dump has been written deadlocks the moment the client says anything
// substantial — and because a hijacked connection ignores the context, nothing
// ever times it out. The restore lock and the run registration stayed held until
// the process was restarted.

// dockerFrame wraps payload the way Docker multiplexes exec output, so StdCopy
// parses it exactly as it would from a real attach.
func dockerFrame(stream byte, payload string) []byte {
	hdr := make([]byte, 8)
	hdr[0] = stream // 1 = stdout, 2 = stderr
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(payload)))
	return append(hdr, payload...)
}

// blockingConn is an exec's stdin: it accepts a limited number of bytes and then
// blocks, standing in for a full pipe. Only closing it, or the reader draining,
// lets the write through.
type blockingConn struct {
	accepted int
	capacity int
	release  chan struct{}
}

func (b *blockingConn) Write(p []byte) (int, error) {
	if b.accepted+len(p) > b.capacity {
		<-b.release // never unblocks unless something else acts
		return 0, errors.New("connection closed")
	}
	b.accepted += len(p)
	return len(p), nil
}

func TestStreamExecIODrainsWhileWriting(t *testing.T) {
	// The client talks while the dump is still streaming in. With a sequential
	// read this output would sit unread in the pipe.
	var chatter bytes.Buffer
	for i := 0; i < 200; i++ {
		chatter.Write(dockerFrame(1, "CREATE TABLE\n"))
	}
	chatter.Write(dockerFrame(2, "ERROR:  relation \"users\" already exists\n"))

	var out tailWriter
	out.limit = maxExecOutputTail
	closed := false
	s := execStream{
		Write:      io.Discard,
		Output:     &chatter,
		CloseWrite: func() error { return nil },
		Close:      func() { closed = true },
	}
	if err := streamExecIO(context.Background(), s, strings.NewReader(strings.Repeat("INSERT;", 5000)), &out); err != nil {
		t.Fatalf("a normal import must succeed: %v", err)
	}
	if closed {
		t.Error("a clean run must not have to tear the connection down")
	}
	got := out.String()
	if !strings.Contains(got, "already exists") {
		t.Errorf("stderr must be captured alongside stdout: %q", got)
	}
	if strings.Count(got, "CREATE TABLE") != 200 {
		t.Errorf("every line the client wrote must be captured, got %d", strings.Count(got, "CREATE TABLE"))
	}
}

// The deadlock itself: stdin blocks until the output is drained. A sequential
// implementation can never finish this; the concurrent one does.
func TestStreamExecIODoesNotDeadlockOnAFullPipe(t *testing.T) {
	drained := make(chan struct{})
	// The output is only readable once something has started draining it, which
	// is what releases the blocked stdin write.
	output := readerFunc(func(p []byte) (int, error) {
		select {
		case <-drained:
		default:
			close(drained)
		}
		return 0, io.EOF
	})
	conn := &blockingConn{capacity: 10, release: drained}

	var out tailWriter
	out.limit = maxExecOutputTail
	s := execStream{
		Write: conn, Output: output,
		CloseWrite: func() error { return nil },
		Close:      func() {},
	}

	done := make(chan error, 1)
	go func() {
		done <- streamExecIO(context.Background(), s, strings.NewReader(strings.Repeat("x", 4096)), &out)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stdin was never unblocked — the output is not being drained concurrently")
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// A hijacked connection does not watch the context, so cancellation has to close
// it. Without that, a cancelled restore leaves this goroutine parked forever.
func TestStreamExecIOHonoursCancellation(t *testing.T) {
	hang := make(chan struct{})
	closed := make(chan struct{})
	s := execStream{
		Write:      io.Discard,
		Output:     readerFunc(func(p []byte) (int, error) { <-hang; return 0, io.EOF }),
		CloseWrite: func() error { return nil },
		Close:      func() { close(hang); close(closed) },
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()

	var out tailWriter
	out.limit = maxExecOutputTail
	done := make(chan error, 1)
	go func() { done <- streamExecIO(ctx, s, strings.NewReader("dump"), &out) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled import must report cancellation, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancellation must close the hijacked connection, not wait on it")
	}
	select {
	case <-closed:
	default:
		t.Error("the connection must be closed so the drain goroutine ends")
	}
}

// A failed stdin write must still stop the drain before the caller reads the
// buffer, or the two race.
func TestStreamExecIOStopsTheDrainOnAWriteError(t *testing.T) {
	var chatter bytes.Buffer
	chatter.Write(dockerFrame(2, "FATAL:  database \"app\" does not exist\n"))
	stopped := make(chan struct{})
	s := execStream{
		Write:      writerFunc(func(p []byte) (int, error) { return 0, errors.New("broken pipe") }),
		Output:     &chatter,
		CloseWrite: func() error { return nil },
		Close:      func() { close(stopped) },
	}

	var out tailWriter
	out.limit = maxExecOutputTail
	err := streamExecIO(context.Background(), s, strings.NewReader("dump"), &out)
	if err == nil || !strings.Contains(err.Error(), "writing stdin") {
		t.Fatalf("a broken stdin must be reported, got %v", err)
	}
	select {
	case <-stopped:
	default:
		t.Error("the connection must be closed so the drain cannot race the caller's read")
	}
	if !strings.Contains(out.String(), "does not exist") {
		t.Errorf("what the client said before the pipe broke explains why: %q", out.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// The capture is bounded, keeps the END (where a client reports its failures),
// and never presents a fragment as if it were everything.
func TestTailWriterKeepsTheEndAndSaysWhenItDropped(t *testing.T) {
	w := &tailWriter{limit: 16}

	n, err := w.Write([]byte("hello"))
	if n != 5 || err != nil {
		t.Fatalf("Write = (%d, %v), want (5, nil)", n, err)
	}
	if w.String() != "hello" {
		t.Errorf("short output must be kept whole: %q", w.String())
	}
	if w.dropped {
		t.Error("nothing was dropped yet")
	}

	// Exactly at the limit: still whole.
	w = &tailWriter{limit: 5}
	_, _ = w.Write([]byte("hello"))
	if w.String() != "hello" || w.dropped {
		t.Errorf("output exactly at the limit must be kept whole: %q", w.String())
	}

	// Across several writes, the tail survives and truncation is announced.
	w = &tailWriter{limit: 8}
	for _, s := range []string{"aaaa", "bbbb", "cccc"} {
		if n, _ := w.Write([]byte(s)); n != len(s) {
			t.Fatalf("Write must report the full length it accepted, got %d", n)
		}
	}
	if !strings.HasSuffix(w.String(), "bbbbcccc") {
		t.Errorf("the last 8 bytes must survive: %q", w.String())
	}
	if !strings.Contains(w.String(), "earlier output dropped") {
		t.Errorf("a truncated capture must say so: %q", w.String())
	}

	// One write larger than the whole window keeps that write's tail.
	w = &tailWriter{limit: 4}
	_, _ = w.Write([]byte("0123456789"))
	if !strings.HasSuffix(w.String(), "6789") {
		t.Errorf("a single oversized write must keep its own tail: %q", w.String())
	}

	// Memory really is bounded: 8 MiB in, never more than the window retained.
	w = &tailWriter{limit: 1024}
	chunk := bytes.Repeat([]byte("x"), 4096)
	for i := 0; i < 2048; i++ {
		_, _ = w.Write(chunk)
	}
	if len(w.buf) > w.limit {
		t.Errorf("retained %d bytes, want at most %d", len(w.buf), w.limit)
	}
}
