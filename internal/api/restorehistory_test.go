package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dockback/internal/store"
)

const (
	clientExecutionID = "0123456789abcdef0123456789abcdef"
	historyRunID      = "bk-1"
)

func historyServer(t *testing.T) *Server {
	t.Helper()
	return &Server{store: testStore(t), bcast: newBroadcaster(32)}
}

func beginHistoryRun(t *testing.T, s *Server, executionID string) (context.Context, func()) {
	t.Helper()
	ctx, finish, ok := s.beginRestoreRun(context.Background(), historyRunID, "nextcloud", "", time.Minute, executionID)
	if !ok {
		t.Fatal("could not begin the run")
	}
	return ctx, finish
}

func statusOf(t *testing.T, s *Server, query string) (*httptest.ResponseRecorder, store.RestoreExecution) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/restores/"+historyRunID+"/status"+query, nil)
	r.SetPathValue("id", historyRunID)
	rec := httptest.NewRecorder()
	s.handleRestoreStatus(rec, r)
	var execution store.RestoreExecution
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &execution); err != nil {
			t.Fatal(err)
		}
	}
	return rec, execution
}

// The finding: nothing but a live SSE listener could learn how a restore ended.
func TestARestoreOutcomeOutlivesTheRun(t *testing.T) {
	cases := []struct {
		name    string
		act     func(*Server)
		want    string
		message string
	}{
		{"success", func(s *Server) { s.restoreOutcome(historyRunID, nil, "Restore completed") }, runOutcomeOK, "Restore completed"},
		{"failure", func(s *Server) { s.restoreOutcome(historyRunID, errors.New("disk full"), "") }, runOutcomeFailed, "disk full"},
		{"operator cancel", func(s *Server) {
			s.cancelRestoreRun(historyRunID)
			s.restoreOutcome(historyRunID, errors.New("stopped"), "")
		}, runOutcomeCanceled, "stopped"},
		{"shutdown", func(s *Server) {
			s.life.stopping = true
			s.restoreOutcome(historyRunID, errors.New("context canceled"), "")
		}, runOutcomeInterrupted, "context canceled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := historyServer(t)
			_, finish := beginHistoryRun(t, s, "")
			c.act(s)
			finish()
			rec, got := statusOf(t, s, "")
			if rec.Code != http.StatusOK || got.Outcome != c.want || !strings.Contains(got.Message, c.message) || got.FinishedAt == 0 {
				t.Fatalf("%d %+v, want %s", rec.Code, got, c.want)
			}
		})
	}
}

func TestStatusSaysRunningWhileItIs(t *testing.T) {
	s := historyServer(t)
	_, finish := beginHistoryRun(t, s, clientExecutionID)
	defer finish()
	if _, got := statusOf(t, s, ""); got.Outcome != store.RestoreExecutionRunning || got.ExecutionID != clientExecutionID {
		t.Fatalf("%+v", got)
	}
	if running := s.runningRestores(); len(running) != 1 || running[0].ExecutionID != clientExecutionID {
		t.Fatalf("%+v", running)
	}
}

// The event is what a client reacts to by asking, so the answer has to be there first.
func TestTheOutcomeIsSavedBeforeItIsAnnounced(t *testing.T) {
	s := historyServer(t)
	_, finish := beginHistoryRun(t, s, clientExecutionID)
	defer finish()
	channel, _ := s.bcast.subscribe()
	defer s.bcast.unsubscribe(channel)

	s.restoreOutcome(historyRunID, nil, "Restore completed")
	for message := range channel {
		if message.event != "run.done" {
			continue
		}
		var event runDoneEvent
		if err := json.Unmarshal([]byte(message.data), &event); err != nil {
			t.Fatal(err)
		}
		if event.ExecutionID != clientExecutionID || event.Outcome != runOutcomeOK {
			t.Fatalf("%+v", event)
		}
		break
	}
	if _, got := statusOf(t, s, ""); got.Outcome != runOutcomeOK {
		t.Fatalf("%+v", got)
	}
}

func TestRepeatedRestoresOfOneBackupStayApart(t *testing.T) {
	s := historyServer(t)
	_, finish := beginHistoryRun(t, s, "")
	s.restoreOutcome(historyRunID, errors.New("first attempt failed"), "")
	_, first := statusOf(t, s, "")
	finish()

	_, finish = beginHistoryRun(t, s, clientExecutionID)
	defer finish()
	if first.ExecutionID == clientExecutionID || first.ExecutionID == "" {
		t.Fatalf("first execution: %+v", first)
	}
	if _, latest := statusOf(t, s, ""); latest.ExecutionID != clientExecutionID || latest.Outcome != store.RestoreExecutionRunning {
		t.Fatalf("the newest run is the answer: %+v", latest)
	}
	if _, earlier := statusOf(t, s, "?execution="+first.ExecutionID); earlier.Outcome != runOutcomeFailed {
		t.Fatalf("the earlier run keeps its answer: %+v", earlier)
	}
	if rec, _ := statusOf(t, s, "?execution=ffffffffffffffffffffffffffffffff"); rec.Code != http.StatusNotFound {
		t.Fatalf("%d", rec.Code)
	}
}

func TestAMalformedExecutionIDIsReplaced(t *testing.T) {
	for _, proposed := range []string{"", "short", "../../etc/passwd", strings.Repeat("A", 32), clientExecutionID + "00"} {
		if got := resolveExecutionID(proposed); got == proposed || !executionIDPattern.MatchString(got) {
			t.Errorf("%q became %q", proposed, got)
		}
	}
	if got := resolveExecutionID(clientExecutionID); got != clientExecutionID {
		t.Errorf("a well-formed id must be kept: %q", got)
	}
}

// A panic returns through finish with no outcome reported.
func TestARunThatReportedNothingIsInterruptedAndAnnounced(t *testing.T) {
	s := historyServer(t)
	_, finish := beginHistoryRun(t, s, clientExecutionID)
	channel, _ := s.bcast.subscribe()
	defer s.bcast.unsubscribe(channel)
	finish()
	if _, got := statusOf(t, s, ""); got.Outcome != runOutcomeInterrupted {
		t.Fatalf("%+v", got)
	}
	select {
	case message := <-channel:
		if message.event != "run.done" || !strings.Contains(message.data, runOutcomeInterrupted) {
			t.Fatalf("%+v", message)
		}
	default:
		t.Fatal("a console was following this run and must be told")
	}
}

func TestFinishDoesNotOverwriteTheRealOutcome(t *testing.T) {
	s := historyServer(t)
	_, finish := beginHistoryRun(t, s, "")
	s.restoreOutcome(historyRunID, nil, "Restore completed")
	finish()
	if _, got := statusOf(t, s, ""); got.Outcome != runOutcomeOK {
		t.Fatalf("%+v", got)
	}
}

// A process that dies cannot record anything, so the next one has to.
func TestARestartSettlesWhatTheLastProcessLeftRunning(t *testing.T) {
	s := historyServer(t)
	if err := s.store.BeginRestoreExecution(clientExecutionID, historyRunID, "nextcloud"); err != nil {
		t.Fatal(err)
	}
	finishedID := strings.Repeat("a", len(clientExecutionID))
	if err := s.store.BeginRestoreExecution(finishedID, "other", "app"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.FinishRestoreExecution(finishedID, runOutcomeOK, "fine"); err != nil {
		t.Fatal(err)
	}

	s.interruptAbandonedRestores()
	if _, got := statusOf(t, s, ""); got.Outcome != runOutcomeInterrupted || got.Message == "" {
		t.Fatalf("%+v", got)
	}
	other, err := s.store.GetRestoreExecution("other", "")
	if err != nil || other.Outcome != runOutcomeOK {
		t.Fatalf("a finished run keeps its answer: %+v %v", other, err)
	}
}

func TestStatusIsReadableByAReadToken(t *testing.T) {
	if !tokenAllows("read", http.MethodGet, "/api/restores/stack:blog/status") {
		t.Fatal("a read token should be able to ask how a restore ended")
	}
}
