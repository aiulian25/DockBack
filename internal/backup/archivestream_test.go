package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"dockback/internal/crypto"
)

const (
	// Well past one cipher frame, so a reader that stops early leaves the producer
	// with more to write. Random, so compression cannot shrink it below that.
	earlyStopPayloadBytes = 4 << 20
	producerExitDeadline  = 2 * time.Second
	decryptFrame          = "internal/crypto.Decrypt("
)

var allCompressionAlgorithms = []string{algoZstd, algoGzip, algoXz}

func incompressible(t *testing.T) []byte {
	t.Helper()
	payload := make([]byte, earlyStopPayloadBytes)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func testArchiveKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func sealed(t *testing.T, plain, key []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if _, err := crypto.Encrypt(&out, bytes.NewReader(plain), key); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func sealedCompressed(t *testing.T, plain, key []byte, algo string) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer, err := newCompressWriter(&compressed, algo, zstd.SpeedFastest, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return sealed(t, compressed.Bytes(), key)
}

type trackedSource struct {
	io.Reader
	closed atomic.Bool
}

func (s *trackedSource) Close() error {
	s.closed.Store(true)
	return nil
}

func assertReleased(t *testing.T, stream *archiveStream, source *trackedSource) {
	t.Helper()
	select {
	case <-stream.producerDone:
	default:
		t.Error("the decrypt goroutine is still running")
	}
	if !source.closed.Load() {
		t.Error("the source was left open")
	}
}

func TestStoppingEarlyReleasesTheProducer(t *testing.T) {
	key := testArchiveKey(t)
	payload := incompressible(t)
	for _, algo := range allCompressionAlgorithms {
		t.Run(algo, func(t *testing.T) {
			source := &trackedSource{Reader: bytes.NewReader(sealedCompressed(t, payload, key, algo))}
			stream, err := openArchiveStream(context.Background(), source, key, algo)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(stream, make([]byte, 16)); err != nil {
				t.Fatal(err)
			}
			stream.Close()
			assertReleased(t, stream, source)
		})
	}
}

func TestAFullReadIsUnchanged(t *testing.T) {
	key := testArchiveKey(t)
	payload := incompressible(t)
	for _, algo := range allCompressionAlgorithms {
		t.Run(algo, func(t *testing.T) {
			source := &trackedSource{Reader: bytes.NewReader(sealedCompressed(t, payload, key, algo))}
			stream, err := openArchiveStream(context.Background(), source, key, algo)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(stream)
			if err != nil {
				t.Fatal(err)
			}
			stream.Close()
			if !bytes.Equal(got, payload) {
				t.Fatal("the bytes changed")
			}
			assertReleased(t, stream, source)
		})
	}
}

// gzip and xz read their header while being constructed, so a malformed one fails
// before there is anything to close but the stream's own parts.
func TestAMalformedHeaderReleasesEverything(t *testing.T) {
	key := testArchiveKey(t)
	garbage := sealed(t, incompressible(t), key)
	for _, algo := range []string{algoGzip, algoXz} {
		t.Run(algo, func(t *testing.T) {
			source := &trackedSource{Reader: bytes.NewReader(garbage)}
			if _, err := openArchiveStream(context.Background(), source, key, algo); err == nil {
				t.Fatal("garbage must not open")
			}
			if !source.closed.Load() {
				t.Error("the source was left open")
			}
			awaitNoDecryptProducer(t)
		})
	}
}

func TestAnUnknownAlgorithmReleasesEverything(t *testing.T) {
	key := testArchiveKey(t)
	source := &trackedSource{Reader: bytes.NewReader(sealed(t, incompressible(t), key))}
	if _, err := openArchiveStream(context.Background(), source, key, "lz4"); err == nil {
		t.Fatal("an unknown algorithm must not open")
	}
	if !source.closed.Load() {
		t.Error("the source was left open")
	}
	awaitNoDecryptProducer(t)
}

func TestCancellingEndsAProducerMidStream(t *testing.T) {
	key := testArchiveKey(t)
	source := &trackedSource{Reader: bytes.NewReader(sealedCompressed(t, incompressible(t), key, algoGzip))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := openArchiveStream(ctx, source, key, algoGzip)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := io.Copy(io.Discard, stream); !errors.Is(err, context.Canceled) {
		t.Errorf("the read should say it was canceled, got %v", err)
	}
	stream.Close()
	assertReleased(t, stream, source)
}

// blockingSource is a network read that only closing can end.
type blockingSource struct {
	unblock chan struct{}
	closed  atomic.Bool
}

func (s *blockingSource) Read([]byte) (int, error) {
	<-s.unblock
	return 0, io.ErrClosedPipe
}

func (s *blockingSource) Close() error {
	if s.closed.CompareAndSwap(false, true) {
		close(s.unblock)
	}
	return nil
}

func TestClosingEndsAProducerBlockedOnItsSource(t *testing.T) {
	source := &blockingSource{unblock: make(chan struct{})}
	key := testArchiveKey(t)
	pipeReader, pipeWriter := io.Pipe()
	stream := &archiveStream{pipe: pipeReader, source: source, producerDone: make(chan struct{})}
	go func() {
		defer close(stream.producerDone)
		pipeWriter.CloseWithError(crypto.Decrypt(pipeWriter, source, key))
	}()
	finished := make(chan struct{})
	go func() {
		stream.Close()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(producerExitDeadline):
		t.Fatal("closing waited on a read that only closing can end")
	}
	select {
	case <-stream.producerDone:
	default:
		t.Fatal("the producer should have exited")
	}
}

// stuckSource is a read that closing does not break.
type stuckSource struct{ release chan struct{} }

func (s *stuckSource) Read([]byte) (int, error) {
	<-s.release
	return 0, io.EOF
}

func (s *stuckSource) Close() error { return nil }

func TestClosingDoesNotHangOnASourceThatWillNotClose(t *testing.T) {
	original := archiveProducerExitWait
	archiveProducerExitWait = 50 * time.Millisecond
	t.Cleanup(func() { archiveProducerExitWait = original })

	source := &stuckSource{release: make(chan struct{})}
	defer close(source.release)
	pipeReader, _ := io.Pipe()
	stream := &archiveStream{pipe: pipeReader, source: source, producerDone: make(chan struct{})}
	finished := make(chan struct{})
	go func() {
		stream.Close()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(producerExitDeadline):
		t.Fatal("closing must be bounded")
	}
}

// decryptProducers counts goroutines inside crypto.Decrypt: narrower than a global
// count, so one leak cannot hide in or be blamed on anything else's goroutines.
func decryptProducers() int {
	stacks := make([]byte, 1<<22)
	stacks = stacks[:runtime.Stack(stacks, true)]
	return bytes.Count(stacks, []byte(decryptFrame))
}

func awaitNoDecryptProducer(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(producerExitDeadline)
	for decryptProducers() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if count := decryptProducers(); count > 0 {
		t.Fatalf("%d decrypt goroutine(s) still running", count)
	}
}

// The finding: this is what browsing and single-file extraction do.
func TestReadingOneMemberRepeatedlyAccumulatesNothing(t *testing.T) {
	e, b := archiveWithPayload(t, incompressible(t))
	const reads = 5
	for range reads {
		if _, err := e.extractEntry(context.Background(), b, "", "config/inspect.json"); err != nil {
			t.Fatal(err)
		}
	}
	awaitNoDecryptProducer(t)
}

func TestEveryWayOutOfTheWalkReleasesTheProducer(t *testing.T) {
	stop := errors.New("the callback gave up")
	cases := map[string]func(*tar.Reader, *tar.Header) (bool, error){
		"callback stops": func(*tar.Reader, *tar.Header) (bool, error) { return false, nil },
		"callback fails": func(*tar.Reader, *tar.Header) (bool, error) { return false, stop },
	}
	e, b := archiveWithPayload(t, incompressible(t))
	for name, callback := range cases {
		t.Run(name, func(t *testing.T) {
			_ = e.streamArchive(context.Background(), b, "", callback)
			awaitNoDecryptProducer(t)
		})
	}
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		_ = e.streamArchive(ctx, b, "", func(*tar.Reader, *tar.Header) (bool, error) {
			cancel()
			return true, nil
		})
		cancel()
		awaitNoDecryptProducer(t)
	})
}

type abortingClient struct{}

func (abortingClient) Write([]byte) (int, error) { return 0, errors.New("the client went away") }

// A download the browser abandons.
func TestAnAbortedDownloadReleasesTheProducer(t *testing.T) {
	e, b := archiveWithPayload(t, incompressible(t))
	if err := e.DecryptTo(context.Background(), b.ID, abortingClient{}); err == nil {
		t.Fatal("the write failure should come back")
	}
	awaitNoDecryptProducer(t)
}

func TestVerifyStillReadsTheWholeArchiveAndReleases(t *testing.T) {
	e, b := archiveWithPayload(t, incompressible(t))
	man := &Manifest{}
	if err := unmarshal(b.ManifestJSON, man); err != nil {
		t.Fatal(err)
	}
	report := e.Verify(context.Background(), b, man, "")
	for _, check := range report.Checks {
		if check.Name == "decompress" && !check.OK {
			t.Fatalf("%+v", check)
		}
	}
	awaitNoDecryptProducer(t)
}
