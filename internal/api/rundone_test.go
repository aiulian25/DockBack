package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// The structured terminal event (#N10).
//
// Every console used to learn a run's outcome by matching the TEXT of its log
// lines. That produced three separate defects in one investigation — a pattern
// ending `restored$` closing the stream after the first of five services, a
// pattern reading `stack .* restored` calling a partial failure a success, and
// 27 lines logged at "ERROR" reaching nothing at all — and each was a message
// this codebase can reword at any time.
func TestRestoreOutcomePublishesRunDone(t *testing.T) {
	// captured pulls the run.done frames out of a real broadcaster.
	captured := func(t *testing.T, run func(*Server)) []runDoneEvent {
		t.Helper()
		s := &Server{store: testStore(t), bcast: newBroadcaster(32)}
		ch, _ := s.bcast.subscribe()
		defer s.bcast.unsubscribe(ch)
		run(s)
		var out []runDoneEvent
		for {
			select {
			case m := <-ch:
				if m.event != "run.done" {
					continue
				}
				var ev runDoneEvent
				if err := json.Unmarshal([]byte(m.data), &ev); err != nil {
					t.Fatalf("run.done is not valid JSON: %v", err)
				}
				out = append(out, ev)
			default:
				return out
			}
		}
	}

	t.Run("success", func(t *testing.T) {
		got := captured(t, func(s *Server) {
			s.restoreOutcome("stack:paperlessngx", nil, "Stack restore completed")
		})
		if len(got) != 1 {
			t.Fatalf("want one run.done, got %v", got)
		}
		if got[0].RunID != "stack:paperlessngx" || got[0].Outcome != runOutcomeOK {
			t.Fatalf("event = %+v", got[0])
		}
		if got[0].Message != "Stack restore completed" {
			t.Errorf("message = %q — the console shows this without re-deriving one", got[0].Message)
		}
	})

	t.Run("failure carries the reason", func(t *testing.T) {
		got := captured(t, func(s *Server) {
			s.restoreOutcome("b1", errTest("service \"db\": disk full"), "unused")
		})
		if len(got) != 1 || got[0].Outcome != runOutcomeFailed {
			t.Fatalf("event = %+v", got)
		}
		if !strings.Contains(got[0].Message, "disk full") {
			t.Errorf("message = %q, want the reason", got[0].Message)
		}
	})

	t.Run("a run with no id publishes nothing", func(t *testing.T) {
		if got := captured(t, func(s *Server) { s.publishRunDone("", runOutcomeOK, "x") }); len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("a server with no broadcaster still runs", func(t *testing.T) {
		// Every log path already tolerates this; the event must too.
		s := &Server{store: testStore(t)}
		s.restoreOutcome("b1", nil, "Restore completed") // must not panic
	})

	t.Run("run.done is NOT replayed to a late subscriber", func(t *testing.T) {
		// Named deltas are live-only by design — which is exactly why every
		// console keeps its line matching as a fallback.
		s := &Server{store: testStore(t), bcast: newBroadcaster(32)}
		s.restoreOutcome("b1", nil, "Restore completed")
		_, hist := s.bcast.subscribe()
		for _, m := range hist {
			if m.event == "run.done" {
				t.Fatal("run.done must not be replayed — a late console would end a run it never saw start")
			}
		}
	})
}

type errTest string

func (e errTest) Error() string { return string(e) }
