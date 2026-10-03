package api

import "sync"

// dynSem is a counting semaphore whose limit can change at runtime, so the backup
// concurrency caps can be resized live from Settings without a restart (F29). A
// channel-buffered semaphore (the previous approach) can't be resized — its buffer
// capacity is fixed at creation — so the dispatcher uses this instead. Shrinking
// the limit never preempts in-flight work; it just stops NEW acquisitions until
// enough slots release. Construct with newDynSem; the zero value is unusable.
type dynSem struct {
	mu    sync.Mutex
	cond  *sync.Cond
	limit int
	used  int
}

func newDynSem(limit int) *dynSem {
	if limit < 1 {
		limit = 1
	}
	s := &dynSem{limit: limit}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// acquire blocks until a slot is free, then takes it.
func (s *dynSem) acquire() {
	s.mu.Lock()
	for s.used >= s.limit {
		s.cond.Wait()
	}
	s.used++
	s.mu.Unlock()
}

// tryAcquire takes a slot without blocking, reporting whether it got one.
func (s *dynSem) tryAcquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used < s.limit {
		s.used++
		return true
	}
	return false
}

// release returns a slot and wakes one waiter. Idempotent-safe against underflow.
func (s *dynSem) release() {
	s.mu.Lock()
	if s.used > 0 {
		s.used--
	}
	s.cond.Signal()
	s.mu.Unlock()
}

// hasSlot reports whether a slot is currently free. It's a peek and may race a
// concurrent acquire, so callers that must actually take the slot re-check with
// tryAcquire (the dispatcher does exactly this).
func (s *dynSem) hasSlot() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used < s.limit
}

// setLimit changes the cap (clamped >=1). Growing wakes any waiters to re-check;
// shrinking below the in-use count leaves running work alone and simply blocks new
// acquisitions until it drains.
func (s *dynSem) setLimit(n int) {
	if n < 1 {
		n = 1
	}
	s.mu.Lock()
	s.limit = n
	s.cond.Broadcast()
	s.mu.Unlock()
}

// currentLimit reports the effective cap (for tests/introspection).
func (s *dynSem) currentLimit() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limit
}
