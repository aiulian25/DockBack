package api

import "time"

// offsiteDrainInterval is how often pending offsite copies are looked at. The check
// costs one query when nothing is pending, and a window opens at minute precision.
const offsiteDrainInterval = time.Minute

// startOffsiteDrain sends pending offsite copies in the background, and finishes
// deletes that left a copy behind (DBK-029). One pass runs at a time, under the
// lifecycle: shutdown cancels an upload in flight, and the copy stays pending for
// the next process to send.
func (s *Server) startOffsiteDrain() {
	go func() {
		t := time.NewTicker(offsiteDrainInterval)
		defer t.Stop()
		for {
			select {
			case <-s.rootContext().Done():
				return
			case now := <-t.C:
				s.drainOffsiteOnce(now)
			}
		}
	}()
}

func (s *Server) drainOffsiteOnce(now time.Time) {
	if s.isStopping() {
		return
	}
	defer s.beginOp()()
	s.engine.DrainOffsite(s.rootContext(), now)
	s.engine.RetryPendingDeletions(s.rootContext(), now)
}
