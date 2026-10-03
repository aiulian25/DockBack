package backup

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"golang.org/x/time/rate"
)

func TestNewUploadLimiter(t *testing.T) {
	if NewUploadLimiter(0) != nil || NewUploadLimiter(-5) != nil {
		t.Fatal("0/negative mbps should yield no limiter (unlimited)")
	}
	lim := NewUploadLimiter(8) // 8 mbps = 1,000,000 bytes/sec
	if lim == nil {
		t.Fatal("expected a limiter for 8 mbps")
	}
	if got := float64(lim.Limit()); got != 1_000_000 {
		t.Errorf("limit = %v bytes/sec, want 1000000", got)
	}
	if lim.Burst() < throttleChunk {
		t.Errorf("burst %d < throttleChunk %d", lim.Burst(), throttleChunk)
	}
}

func TestThrottledReaderCapsChunk(t *testing.T) {
	// A very high but FINITE rate means no real waiting; we only assert the
	// per-read cap. (rate.Inf is a deliberate passthrough — see the test below.)
	e := &Engine{UpLimiter: rate.NewLimiter(rate.Limit(1<<30), throttleChunk)}
	src := strings.NewReader(strings.Repeat("x", throttleChunk*4))
	r := e.throttle(context.Background(), src)

	buf := make([]byte, throttleChunk*2) // ask for more than a chunk
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n > throttleChunk {
		t.Errorf("read returned %d bytes, must be capped to %d", n, throttleChunk)
	}
}

func TestThrottlePassthroughWhenUnlimited(t *testing.T) {
	src := bytes.NewReader([]byte("hello"))
	// nil limiter (legacy unlimited) and an always-present rate.Inf limiter (F15)
	// both pass the reader through untouched (fast path — no chunk capping).
	for _, e := range []*Engine{{UpLimiter: nil}, {UpLimiter: NewSharedUploadLimiter()}} {
		if r := e.throttle(context.Background(), src); r != src {
			t.Error("unlimited engine should return the original reader unchanged")
		}
	}
}

func TestSetUploadLimit(t *testing.T) {
	e := &Engine{UpLimiter: NewSharedUploadLimiter()}
	// 8 Mbit/s → ~1,000,000 bytes/sec, and throttle now wraps (finite cap).
	e.SetUploadLimit(8)
	if got := float64(e.UpLimiter.Limit()); got < 999_000 || got > 1_001_000 {
		t.Errorf("SetUploadLimit(8) limit = %.0f, want ~1,000,000", got)
	}
	if r := e.throttle(context.Background(), bytes.NewReader([]byte("x"))); r == nil {
		t.Fatal("finite cap must wrap the reader")
	}
	// Back to unlimited → rate.Inf and passthrough.
	e.SetUploadLimit(0)
	if e.UpLimiter.Limit() != rate.Inf {
		t.Error("SetUploadLimit(0) must reset to unlimited (rate.Inf)")
	}
}

func TestThrottledReaderRespectsContext(t *testing.T) {
	// 1 byte/sec, burst 1: reading a chunk forces a WaitN that a canceled ctx
	// must abort rather than block.
	e := &Engine{UpLimiter: rate.NewLimiter(1, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := strings.NewReader(strings.Repeat("x", throttleChunk))
	r := e.throttle(ctx, src)
	if _, err := r.Read(make([]byte, throttleChunk)); err == nil {
		t.Error("expected an error from a canceled context while waiting for tokens")
	}
}
