package api

import (
	"context"
	"errors"
	"sync"
)

// ErrDrainDeadline says shutdown gave up waiting for owned work. Whatever it was
// holding is in the hold journal, and the next start reconciles it.
var ErrDrainDeadline = errors.New("shutdown deadline passed with work still running")

// lifecycle is what stops with the process: the work that can hold a container or
// write to the catalog. A 202 request is no longer anything HTTP shutdown waits
// for, so without this the process exits underneath it, and the defers that resume
// a paused container never run.
type lifecycle struct {
	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	stopping bool
	active   int
	idle     chan struct{} // closed when draining and the last op has ended
}

// rootContext is the parent of owned work; shutdown cancels it.
func (s *Server) rootContext() context.Context {
	s.life.mu.Lock()
	defer s.life.mu.Unlock()
	if s.life.ctx == nil {
		s.life.ctx, s.life.cancel = context.WithCancel(context.Background())
	}
	return s.life.ctx
}

// isStopping reports whether shutdown has begun, so nothing new should be claimed.
func (s *Server) isStopping() bool {
	s.life.mu.Lock()
	defer s.life.mu.Unlock()
	return s.life.stopping
}

// beginOp counts one piece of owned work until the returned func runs. It never
// refuses: work that loses the race with shutdown finds its context canceled.
func (s *Server) beginOp() (end func()) {
	s.life.mu.Lock()
	defer s.life.mu.Unlock()
	s.life.active++
	return func() {
		s.life.mu.Lock()
		defer s.life.mu.Unlock()
		s.life.active--
		if s.life.active == 0 && s.life.idle != nil {
			close(s.life.idle)
			s.life.idle = nil
		}
	}
}

// Shutdown stops claiming work, cancels what is running, and waits for it to
// finish cleaning up, which is when a held container is let go. It returns
// ErrDrainDeadline if ctx ends first. The catalog must stay open until it returns.
func (s *Server) Shutdown(ctx context.Context) error {
	s.rootContext()
	s.life.mu.Lock()
	s.life.stopping = true
	s.life.cancel()
	if s.life.active == 0 {
		s.life.mu.Unlock()
		return nil
	}
	if s.life.idle == nil {
		s.life.idle = make(chan struct{})
	}
	idle := s.life.idle
	s.life.mu.Unlock()

	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ErrDrainDeadline
	}
}
