package backup

import (
	"context"
	"strings"
	"testing"

	"golang.org/x/time/rate"
)

// F76 per-destination upload caps: the effective cap is min(global, destination),
// realized by CHAINING the two limiters in the upload path — these tests pin the
// pure composition math and the limiter construction, timing-free.

func TestEffectiveRateMin(t *testing.T) {
	cases := []struct{ global, dest, want int }{
		{0, 0, 0},    // both unlimited
		{0, 20, 20},  // only the destination caps
		{10, 0, 10},  // only the global caps
		{10, 20, 10}, // global tighter — 10 wins (the acceptance case)
		{20, 10, 10}, // destination tighter
		{-1, 20, 20}, // negative treated as unlimited
	}
	for _, c := range cases {
		if got := effectiveMbps(c.global, c.dest); got != c.want {
			t.Errorf("effectiveMbps(%d, %d) = %d, want %d", c.global, c.dest, got, c.want)
		}
	}
}

func TestRateLimiterConstruction(t *testing.T) {
	// 8 Mbit/s = 1,000,000 bytes/sec.
	lim := NewUploadLimiter(8)
	if lim == nil || lim.Limit() != rate.Limit(1_000_000) {
		t.Fatalf("NewUploadLimiter(8).Limit() = %v, want 1e6 bytes/sec", lim.Limit())
	}
	// 0/negative = unlimited = no limiter at all (passthrough in throttleWith).
	if NewUploadLimiter(0) != nil || NewUploadLimiter(-5) != nil {
		t.Fatal("a 0/negative cap must yield a nil limiter")
	}
	r := strings.NewReader("data")
	if throttleWith(context.Background(), r, nil) != r {
		t.Fatal("throttleWith(nil limiter) must be a passthrough")
	}
	// With a limiter the reader is wrapped — chained wrapping composes both caps.
	wrapped := throttleWith(context.Background(), r, lim)
	if wrapped == r {
		t.Fatal("throttleWith must wrap when a limiter is set")
	}
	if _, ok := wrapped.(*throttledReader); !ok {
		t.Fatalf("wrapped reader is %T, want *throttledReader", wrapped)
	}
	chained := throttleWith(context.Background(), wrapped, NewUploadLimiter(4))
	inner, ok := chained.(*throttledReader)
	if !ok || inner.r != wrapped {
		t.Fatal("chaining must layer the second limiter around the first")
	}
}

func TestRateGlobalReadback(t *testing.T) {
	e := &Engine{}
	if e.uploadMbps() != 0 {
		t.Fatal("nil limiter must read back as unlimited")
	}
	e.SetUploadLimit(10)
	if got := e.uploadMbps(); got != 10 {
		t.Fatalf("uploadMbps after SetUploadLimit(10) = %d", got)
	}
	e.SetUploadLimit(0)
	if got := e.uploadMbps(); got != 0 {
		t.Fatalf("uploadMbps after SetUploadLimit(0) = %d, want 0 (unlimited)", got)
	}
}
