package dockercli

import (
	"context"
	"testing"
	"time"
)

// The post-restore health gate waits inside WaitForHealthy, so an operator's
// Cancel (or the run's deadline) has to end that wait immediately instead of
// sitting out the remaining health timeout — the "stuck on Verifying…" case.

// An already-canceled context returns at once, without touching Docker.
func TestWaitForHealthyReturnsOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if WaitForHealthy(ctx, nil, "some-container", 10*time.Minute) {
		t.Fatal("a canceled wait must not report healthy")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("cancel must end the wait promptly, took %s", el)
	}
}

// The between-polls sleep is interruptible, so a cancel mid-wait doesn't have
// to wait out the poll interval (let alone the whole health timeout).
func TestSleepOrDoneIsInterruptible(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	if sleepOrDone(ctx, 30*time.Second) {
		t.Fatal("a canceled sleep must report not-completed")
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("cancel must interrupt the sleep, took %s", el)
	}

	// An uncanceled sleep still completes normally.
	if !sleepOrDone(context.Background(), 10*time.Millisecond) {
		t.Fatal("an uninterrupted sleep must report completed")
	}
}
