package backup

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIsTransientErr(t *testing.T) {
	transient := []error{
		errors.New("server returned 502 Bad Gateway"),
		errors.New("503 service unavailable"),
		errors.New("read tcp: connection reset by peer"),
		errors.New("write: broken pipe"),
	}
	for _, e := range transient {
		if !isTransientErr(e) {
			t.Errorf("expected transient: %v", e)
		}
	}
	permanent := []error{
		nil,
		context.DeadlineExceeded, // a timeout won't get faster on retry
		context.Canceled,
		errors.New("403 forbidden"),
		errors.New("webdav: remote size mismatch"),
	}
	for _, e := range permanent {
		if isTransientErr(e) {
			t.Errorf("expected non-transient: %v", e)
		}
	}
}

// at builds a local time at HH:MM for window tests.
func at(h, m int) time.Time { return time.Date(2026, 1, 2, h, m, 0, 0, time.Local) }

// TestInUploadWindow covers the per-destination upload window (F14): normal and
// wrap-around windows, boundaries, and "no window = always allowed".
func TestInUploadWindow(t *testing.T) {
	cases := []struct {
		name       string
		start, end string
		now        time.Time
		want       bool
	}{
		{"no window allows any time", "", "", at(14, 0), true},
		{"daytime inside window", "01:00", "06:00", at(3, 30), true},
		{"daytime before window", "01:00", "06:00", at(0, 30), false},
		{"daytime after window", "01:00", "06:00", at(6, 30), false},
		{"start is inclusive", "01:00", "06:00", at(1, 0), true},
		{"end is exclusive", "01:00", "06:00", at(6, 0), false},
		{"wrap-around late night in", "22:00", "06:00", at(23, 0), true},
		{"wrap-around early morning in", "22:00", "06:00", at(5, 0), true},
		{"wrap-around midday out", "22:00", "06:00", at(12, 0), false},
		{"degenerate equal bounds = unrestricted", "03:00", "03:00", at(12, 0), true},
		{"malformed = unrestricted", "nope", "06:00", at(12, 0), true},
	}
	for _, c := range cases {
		if got := inUploadWindow(c.start, c.end, c.now); got != c.want {
			t.Errorf("%s: inUploadWindow(%q,%q,%s) = %v, want %v", c.name, c.start, c.end, c.now.Format("15:04"), got, c.want)
		}
	}
}

// TestParseUploadPolicy covers reading the per-destination policy from the sealed
// config map, including ignoring an invalid/zero cap (F14).
func TestParseUploadPolicy(t *testing.T) {
	p := parseUploadPolicy(map[string]string{"max_upload_mbps": "20", "upload_window_start": "01:00", "upload_window_end": "06:00"})
	if p.Mbps != 20 || p.Start != "01:00" || p.End != "06:00" {
		t.Fatalf("parsed policy = %+v", p)
	}
	// Empty/absent → unlimited, no window.
	if z := parseUploadPolicy(map[string]string{}); z.Mbps != 0 || z.Start != "" || z.End != "" {
		t.Fatalf("empty policy = %+v, want zero", z)
	}
	// A non-positive or garbage cap is treated as unlimited (0).
	for _, v := range []string{"0", "-5", "abc"} {
		if p := parseUploadPolicy(map[string]string{"max_upload_mbps": v}); p.Mbps != 0 {
			t.Errorf("cap %q parsed to %d, want 0", v, p.Mbps)
		}
	}
}

// TestPerDestLimiterConstruction covers building a per-destination limiter: a
// positive cap yields a limiter at the right byte rate; 0 yields nil (F14).
func TestPerDestLimiterConstruction(t *testing.T) {
	if lim := NewUploadLimiter(0); lim != nil {
		t.Fatal("0 Mbit/s must yield a nil (unlimited) limiter")
	}
	lim := NewUploadLimiter(8) // 8 Mbit/s = 1,000,000 bytes/sec
	if lim == nil {
		t.Fatal("positive cap must yield a limiter")
	}
	if got := float64(lim.Limit()); got < 999_000 || got > 1_001_000 {
		t.Errorf("8 Mbit/s limiter rate = %.0f bytes/s, want ~1,000,000", got)
	}
}
