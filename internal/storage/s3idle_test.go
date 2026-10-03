package storage

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The S3 upload used to carry an ABSOLUTE two-hour deadline, so any archive
// bigger than roughly (link rate × 2h) could never reach the destination: the
// clock failed it, not the network, and the destination reported "Degraded" on
// every run for exactly the largest and most valuable backups. These tests hold
// the replacement: an upload is bounded by INACTIVITY, never by elapsed time.

// fakeS3 is the smallest server minio's unknown-size (multipart) upload path
// needs: initiate, one or more part uploads, complete.
type fakeS3 struct {
	*httptest.Server
	// partDelay is held before answering a part upload, standing in for a slow
	// link; hangPart makes the part upload never answer at all.
	partDelay time.Duration
	hangPart  bool
	parts     atomic.Int32
}

func newFakeS3(t *testing.T) *fakeS3 {
	t.Helper()
	f := &fakeS3{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodPost && q.Has("uploads"):
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><InitiateMultipartUploadResult><Bucket>backups</Bucket><Key>k</Key><UploadId>up-1</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPut && q.Has("partNumber"):
			_, _ = io.Copy(io.Discard, r.Body)
			f.parts.Add(1)
			if f.hangPart {
				<-r.Context().Done() // the destination stopped answering
				return
			}
			time.Sleep(f.partDelay)
			w.Header().Set("ETag", `"d41d8cd98f00b204e9800998ecf8427e"`)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && q.Has("uploadId"):
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><CompleteMultipartUploadResult><Bucket>backups</Bucket><Key>k</Key><ETag>"d41d8cd98f00b204e9800998ecf8427e"</ETag></CompleteMultipartUploadResult>`)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeS3) backend(t *testing.T, idle time.Duration) *S3 {
	t.Helper()
	be, err := NewS3(map[string]string{
		"endpoint": strings.TrimPrefix(f.URL, "http://"), "bucket": "backups",
		"access_key": "a", "secret_key": "s", "region": "us-east-1", "insecure": "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	be.idleTimeout = idle
	return be
}

// trickleReader delivers one byte at a time with a pause between each, the way a
// throttled upload feeds the encryptor's pipe.
type trickleReader struct {
	left  int
	pause time.Duration
}

func (t *trickleReader) Read(p []byte) (int, error) {
	if t.left == 0 {
		return 0, io.EOF
	}
	time.Sleep(t.pause)
	t.left--
	p[0] = 'x'
	return 1, nil
}

// TestS3PutSurvivesSlowReader: an upload whose TOTAL time is several times the
// inactivity window must still complete. Under the old absolute deadline this is
// exactly the case that failed.
func TestS3PutSurvivesSlowReader(t *testing.T) {
	f := newFakeS3(t)
	const idle = 300 * time.Millisecond
	be := f.backend(t, idle)

	slow := &trickleReader{left: 20, pause: 50 * time.Millisecond} // ~1s total
	start := time.Now()
	if _, err := be.Put(context.Background(), "node/app/x.dback", slow); err != nil {
		t.Fatalf("a steady but slow upload must not be failed by the clock: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 3*idle {
		t.Fatalf("the upload took %v, too short to prove it outlived the %v window", elapsed, idle)
	}
}

// TestS3PutFailsWhenTheDestinationStalls: the other half of the contract. A
// target that stops answering must still fail, and say so in words the run log
// can show — not as a bare "context canceled", which reads as an operator
// cancelling the backup.
func TestS3PutFailsWhenTheDestinationStalls(t *testing.T) {
	f := newFakeS3(t)
	f.hangPart = true
	be := f.backend(t, 200*time.Millisecond)

	done := make(chan error, 1)
	go func() {
		_, err := be.Put(context.Background(), "node/app/x.dback", strings.NewReader("some archive bytes"))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a destination that never answers must fail the upload")
		}
		if !strings.Contains(err.Error(), "no data moved") {
			t.Errorf("the failure must name the stall, got: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a stalled upload must be abandoned, not held open forever")
	}
}

// An upload the operator cancels must still report cancellation, not a stall.
func TestS3PutReportsOperatorCancel(t *testing.T) {
	f := newFakeS3(t)
	f.hangPart = true
	be := f.backend(t, time.Hour) // the watchdog must not be what fires

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	_, err := be.Put(ctx, "node/app/x.dback", strings.NewReader("some archive bytes"))
	if err == nil {
		t.Fatal("a canceled upload must fail")
	}
	if strings.Contains(err.Error(), "no data moved") {
		t.Errorf("an operator cancel must not be reported as a stall: %v", err)
	}
}

// The watchdog is driven by an activity clock touched from BOTH directions:
// bytes pulled from the source, and bytes handed to the network. minio reads a
// whole part into memory before sending any of it, so during that send the
// source is silent for minutes — a watchdog watching only the source would abort
// a healthy transfer on a slow link.
func TestUploadActivityWatchdog(t *testing.T) {
	t.Run("progress on either side keeps the upload alive", func(t *testing.T) {
		for _, side := range []string{"source", "wire"} {
			activity := newUploadActivity()
			var stalled atomic.Bool
			ctx, cancel := context.WithCancel(context.Background())
			stop := watchUploadIdle(activity, 200*time.Millisecond, cancel, &stalled)

			source := &activityReader{r: strings.NewReader(strings.Repeat("x", 100)), activity: activity}
			deadline := time.Now().Add(600 * time.Millisecond)
			for time.Now().Before(deadline) {
				time.Sleep(50 * time.Millisecond)
				if side == "source" {
					_, _ = source.Read(make([]byte, 1))
					continue
				}
				_, _ = activity.Read(make([]byte, 1)) // minio's progress hook
			}
			stop()
			cancel()
			if stalled.Load() {
				t.Errorf("%s progress must keep the upload alive past the window", side)
			}
			_ = ctx
		}
	})

	t.Run("silence on both sides aborts", func(t *testing.T) {
		activity := newUploadActivity()
		var stalled atomic.Bool
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stop := watchUploadIdle(activity, 100*time.Millisecond, cancel, &stalled)
		defer stop()

		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("a silent upload must be canceled")
		}
		if !stalled.Load() {
			t.Error("the watchdog must record that IT stopped the upload, so the error can say so")
		}
	})

	t.Run("the progress hook honours minio's reader contract", func(t *testing.T) {
		activity := newUploadActivity()
		buf := make([]byte, 7)
		n, err := activity.Read(buf)
		if n != len(buf) || err != nil {
			t.Fatalf("hook Read = (%d, %v), want (%d, nil)", n, err, len(buf))
		}
	})
}

// Control calls get a short fixed bound: each is one round trip, so a slow one
// is an endpoint that has stopped answering. Uploads must NOT use it.
func TestS3MetadataCallsAreBoundedShort(t *testing.T) {
	be := &S3{}
	ctx, cancel := be.withTimeout(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("a metadata call must carry a deadline")
	}
	if left := time.Until(deadline); left > s3MetadataTimeout+time.Second || left < s3MetadataTimeout-time.Second {
		t.Errorf("metadata deadline is %v, want ~%v", left, s3MetadataTimeout)
	}
	if s3MetadataTimeout >= time.Hour {
		t.Error("a control call must not carry an upload-sized timeout")
	}
	if be.idle() != s3IdleTimeout {
		t.Errorf("default idle window = %v, want %v", be.idle(), s3IdleTimeout)
	}
}
