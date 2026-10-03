package api

import (
	"strings"
	"testing"
	"time"
)

// A log line that never arrives is one line missing from a console. The
// run-completion event is different: it is the one event a console cannot
// reconstruct, and missing it drops the console back to matching the TEXT of log
// lines — which the comments around that fallback already call unreliable. It
// was dropped on exactly the same terms as a log line.

func TestRunDoneWaitsForASlowSubscriber(t *testing.T) {
	b := newBroadcaster(10)
	ch, _ := b.subscribe()

	// Fill the subscriber's buffer so an immediate send cannot succeed.
	for len(ch) < cap(ch) {
		ch <- sseMsg{data: "filler"}
	}

	// A log line is still dropped rather than blocking the engine.
	before := len(ch)
	b.publish(LogLine{Msg: "an ordinary line"})
	if len(ch) != before {
		t.Error("a log line must still be dropped for a full subscriber")
	}
	if b.droppedEvents() != 0 {
		t.Error("a dropped log line is not a dropped terminal event")
	}

	// The terminal event waits. Drain one slot shortly after, and it lands.
	go func() {
		time.Sleep(30 * time.Millisecond)
		<-ch
	}()
	done := make(chan struct{})
	go func() {
		(&Server{bcast: b}).publishRunDone("b1", runOutcomeOK, "")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publishRunDone must not block indefinitely")
	}
	if b.droppedEvents() != 0 {
		t.Errorf("the event had room within the grace period and must not be counted as dropped")
	}

	// Drain and confirm the verdict is in there.
	found := false
	for len(ch) > 0 {
		m := <-ch
		if m.event == "run.done" && strings.Contains(m.data, `"b1"`) {
			found = true
		}
	}
	if !found {
		t.Error("the run's verdict never reached the console")
	}
}

// A console that has gone away must not hold up the run that is finishing, and
// the miss has to be countable rather than silent.
func TestRunDoneGivesUpAndCountsIt(t *testing.T) {
	b := newBroadcaster(10)
	ch, _ := b.subscribe()
	for len(ch) < cap(ch) {
		ch <- sseMsg{data: "filler"}
	}

	start := time.Now()
	(&Server{bcast: b}).publishRunDone("b1", runOutcomeFailed, "disk full")
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("a dead subscriber held the run for %v", elapsed)
	}
	if elapsed < runDoneGrace {
		t.Errorf("gave up after %v without waiting the %v grace period", elapsed, runDoneGrace)
	}
	if b.droppedEvents() != 1 {
		t.Errorf("dropped = %d, want 1 — a console fell back to matching log text and nothing recorded it", b.droppedEvents())
	}
}

// unsubscribe stops closing the channel, because a terminal event can be
// mid-delivery on it. Sending on a closed channel is a panic, and this one runs
// outside the lock.
func TestUnsubscribeDoesNotPanicAMidFlightSend(t *testing.T) {
	b := newBroadcaster(10)
	ch, _ := b.subscribe()
	for len(ch) < cap(ch) {
		ch <- sseMsg{data: "filler"}
	}

	// Unsubscribe WHILE a terminal event is waiting on that channel.
	go func() {
		time.Sleep(20 * time.Millisecond)
		b.unsubscribe(ch)
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&Server{bcast: b}).publishRunDone("b1", runOutcomeOK, "")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("delivery to a departing subscriber must finish")
	}
	// And a later publish to the now-empty subscriber set is fine.
	b.publish(LogLine{Msg: "after"})
}
