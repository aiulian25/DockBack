package notify

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHeartbeatPingsOnVerifiedSuccess(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	cfg := Config{Heartbeat: HeartbeatConfig{Enabled: true, URL: srv.URL}}
	d := New(func() (Config, error) { return cfg, nil }, nil)

	// A verified backup success pings the monitor.
	d.Send(KindBackupSuccess, "ok", "verified")
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("expected 1 heartbeat on success, got %d", hits)
	}

	// A failure must NOT ping (absence-of-heartbeat is what catches failure).
	d.Send(KindBackupFailed, "fail", "boom")
	d.Send(KindVerifyFailed, "fail", "bad")
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("failures must not ping; hits=%d", hits)
	}

	// Disabled = no ping.
	cfg.Heartbeat.Enabled = false
	d.Send(KindBackupSuccess, "ok", "verified")
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("disabled heartbeat must not ping; hits=%d", hits)
	}
}

func TestHeartbeatTickInterval(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	cfg := Config{Heartbeat: HeartbeatConfig{Enabled: true, URL: srv.URL, IntervalMinutes: 60}}
	d := New(func() (Config, error) { return cfg, nil }, nil)

	// First tick after startup pings immediately (lastBeat == 0).
	d.HeartbeatTick()
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("first tick should ping, got %d", hits)
	}
	// A second tick well within the interval must NOT ping again.
	d.HeartbeatTick()
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("within-interval tick must not ping; hits=%d", hits)
	}
}

func TestHeartbeatTestChannel(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&hits, 1) }))
	defer srv.Close()
	d := New(func() (Config, error) { return Config{}, nil }, nil)
	if err := d.TestChannel(Config{Heartbeat: HeartbeatConfig{Enabled: true, URL: srv.URL}}, "heartbeat"); err != nil {
		t.Fatalf("test channel: %v", err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("test should ping once, got %d", hits)
	}
}

// TestHeartbeatErrorSurfacesResponseBody proves a failed ping reports the server's
// ACTIONABLE reason, not just a bare status code — the exact Uptime Kuma
// paused/unknown-token case that otherwise logs an opaque "heartbeat HTTP 404".
func TestHeartbeatErrorSurfacesResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"ok":false,"msg":"Monitor not found or not active."}`))
	}))
	defer srv.Close()

	d := New(func() (Config, error) { return Config{}, nil }, nil)
	err := d.TestChannel(Config{Heartbeat: HeartbeatConfig{Enabled: true, URL: srv.URL}}, "heartbeat")
	if err == nil {
		t.Fatal("expected an error on HTTP 404")
	}
	if !strings.Contains(err.Error(), "HTTP 404") || !strings.Contains(err.Error(), "Monitor not found or not active") {
		t.Fatalf("error should include the status AND the response body, got: %q", err.Error())
	}
}

// TestHTTPStatusError covers the helper's edge cases: empty body → status only
// (no trailing colon), whitespace/newlines collapsed to one tidy line, and an
// oversized body truncated with an ellipsis so it can't bloat a log line.
func TestHTTPStatusError(t *testing.T) {
	mk := func(code int, body string) *http.Response {
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body))}
	}
	if got := httpStatusError("gotify", mk(500, "")).Error(); got != "gotify HTTP 500" {
		t.Fatalf("empty body should give status only, got %q", got)
	}
	if got := httpStatusError("webhook", mk(400, "bad\n  request\n")).Error(); got != "webhook HTTP 400: bad request" {
		t.Fatalf("whitespace should collapse, got %q", got)
	}
	got := httpStatusError("heartbeat", mk(413, strings.Repeat("x", 400))).Error()
	if !strings.HasSuffix(got, "…") || len([]rune(got)) > 240 {
		t.Fatalf("oversized body should truncate with an ellipsis, got len=%d: %q", len([]rune(got)), got)
	}
}
