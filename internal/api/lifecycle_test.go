package api

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestShutdownWaitsForOwnedWorkAndCancelsIt(t *testing.T) {
	s := &Server{store: testStore(t)}
	end := s.beginOp()
	root := s.rootContext()
	done := make(chan error, 1)
	go func() { done <- s.Shutdown(context.Background()) }()

	select {
	case <-root.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown must cancel what is running")
	}
	select {
	case err := <-done:
		t.Fatalf("shutdown returned before the work finished cleaning up: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	end()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a clean drain returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown never saw the work end")
	}
}

func TestShutdownReportsTheDeadlineApartFromACleanDrain(t *testing.T) {
	s := &Server{store: testStore(t)}
	defer s.beginOp()()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	if err := s.Shutdown(ctx); !errors.Is(err, ErrDrainDeadline) {
		t.Errorf("got %v, want ErrDrainDeadline", err)
	}
}

func TestShutdownWithNothingRunningReturnsAtOnce(t *testing.T) {
	s := &Server{store: testStore(t)}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Errorf("got %v", err)
	}
	if !s.isStopping() {
		t.Error("nothing should be claimed after shutdown begins")
	}
}

func TestNoJobIsClaimedOnceShutdownBegins(t *testing.T) {
	s := &Server{queue: []*queuedJob{{id: "j1", nodeID: "n1"}}}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if job, _, _ := s.takeEligible(); job != nil {
		t.Errorf("claimed %v after shutdown began", job.id)
	}
}

func TestARestoreCannotStartOnceShutdownBegins(t *testing.T) {
	s := &Server{store: testStore(t)}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.beginRestoreRun(context.Background(), "b1", "app", "n1", time.Minute, ""); ok {
		t.Error("a restore started after shutdown began")
	}
}

func TestShutdownCancelsARestoreRun(t *testing.T) {
	s := &Server{store: testStore(t)}
	ctx, finish, ok := s.beginRestoreRun(context.Background(), "b1", "app", "n1", time.Hour, "")
	if !ok {
		t.Fatal("the restore should start")
	}
	done := make(chan error, 1)
	go func() { done <- s.Shutdown(context.Background()) }()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the restore's context must end with the process")
	}
	finish()
	if err := <-done; err != nil {
		t.Errorf("got %v", err)
	}
}
