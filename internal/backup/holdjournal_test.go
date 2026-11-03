package backup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

const (
	priorStart = "2026-11-01T10:00:00Z"
	laterStart = "2026-11-02T10:00:00Z"
)

// This decision starts and unpauses other people's containers.
func TestDecideHoldRecovery(t *testing.T) {
	now := time.Now()
	intentAt := now.Add(-time.Hour)
	stopRow := func(state string) store.WorkloadHold {
		return store.WorkloadHold{Kind: holdKindStop, State: state, PriorStartedAt: priorStart, CreatedAt: intentAt.Unix()}
	}
	pauseRow := store.WorkloadHold{Kind: holdKindPause, State: holdStateAcquired, PriorStartedAt: priorStart, CreatedAt: intentAt.Unix()}
	stopped := func(startedAt string, finishedAt time.Time) holdObservation {
		return holdObservation{exists: true, startedAt: startedAt, finishedAt: finishedAt}
	}

	cases := []struct {
		name string
		row  store.WorkloadHold
		seen holdObservation
		want holdAction
	}{
		{"a container that is gone has nothing to undo", pauseRow, holdObservation{}, holdClear},
		{"paused under our pause: release", pauseRow, holdObservation{exists: true, paused: true, startedAt: priorStart}, holdRelease},
		{"a pause intent the daemon carried out: release", store.WorkloadHold{Kind: holdKindPause, State: holdStateIntent}, holdObservation{exists: true, paused: true}, holdRelease},
		{"a pause intent never carried out: nothing to do", store.WorkloadHold{Kind: holdKindPause, State: holdStateIntent}, holdObservation{exists: true, running: true}, holdClear},
		{"our stop, not run since: start it", stopRow(holdStateAcquired), stopped(priorStart, intentAt), holdRelease},
		{"already running again: nothing to do", stopRow(holdStateAcquired), holdObservation{exists: true, running: true, startedAt: laterStart}, holdClear},
		// It ran again after us, so this stop is somebody else's, and stays.
		{"restarted and stopped since: an operator's stop", stopRow(holdStateAcquired), stopped(laterStart, now), holdClear},
		{"a stop intent with the exit where it belongs: ours", stopRow(holdStateIntent), stopped(priorStart, intentAt.Add(10*time.Second)), holdRelease},
		{"a stop intent with the exit long after: cannot be proved ours", stopRow(holdStateIntent), stopped(priorStart, intentAt.Add(30*time.Minute)), holdReport},
		{"a stop intent never carried out: nothing to do", stopRow(holdStateIntent), holdObservation{exists: true, running: true, startedAt: priorStart}, holdClear},
		// Half-written data must not be started by anybody but the operator.
		{"an interrupted restore is only ever reported", store.WorkloadHold{Kind: holdKindRestore}, stopped(priorStart, intentAt), holdReport},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideHoldRecovery(tc.row, tc.seen); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// fakeContainers is a daemon holding the given containers' states; the only
// mutations it accepts are the two a reconcile may make.
type fakeContainers struct {
	states map[string]map[string]any
	calls  []string
	refuse map[string]bool // verbs the daemon answers with an error
}

func holdEngine(t *testing.T, states map[string]map[string]any) (*Engine, *fakeContainers, *[]string) {
	t.Helper()
	fake := &fakeContainers{states: states}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("API-Version", "1.44")
		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 2 || parts[len(parts)-3] != "containers" {
			return
		}
		id, verb := parts[len(parts)-2], parts[len(parts)-1]
		if r.Method == http.MethodPost {
			fake.calls = append(fake.calls, verb+" "+id)
			if fake.refuse[verb] {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"message":"forbidden by proxy"}`))
				return
			}
			if state, ok := fake.states[id]; ok && verb == "unpause" {
				state["Paused"], state["Running"] = false, true
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		state, ok := fake.states[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"no such container"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": id, "Name": "/" + id, "State": state})
	}))
	t.Cleanup(srv.Close)
	reg := dockercli.NewRegistry()
	reg.Set(dockercli.NodeConn{ID: "n1", Transport: dockercli.TransportTCPProxy, Address: "tcp://" + srv.Listener.Addr().String()})
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	alerts := &[]string{}
	return &Engine{Store: st, Reg: reg, Log: func(string, string, string) {},
		Notify: func(_, title, _ string) { *alerts = append(*alerts, title) }}, fake, alerts
}

func listHolds(t *testing.T, e *Engine) []store.WorkloadHold {
	t.Helper()
	rows, err := e.Store.ListWorkloadHolds()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// After SIGKILL: a hold the journal proves is released, its row goes, and doing it
// all again finds nothing more to do. A paused container with no row is left alone.
func TestReconcileReleasesOnlyOwnedHoldsAndIsRepeatable(t *testing.T) {
	e, fake, _ := holdEngine(t, map[string]map[string]any{
		"ours":     {"Running": true, "Paused": true, "StartedAt": priorStart},
		"stopped":  {"Running": false, "StartedAt": priorStart, "FinishedAt": priorStart},
		"operator": {"Running": true, "Paused": true, "StartedAt": priorStart},
	})
	for _, row := range []store.WorkloadHold{
		{ID: "op:ours", NodeID: "n1", ContainerID: "ours", Name: "ours", Kind: holdKindPause, State: holdStateAcquired, PriorStartedAt: priorStart},
		{ID: "op:stopped", NodeID: "n1", ContainerID: "stopped", Name: "stopped", Kind: holdKindStop, State: holdStateAcquired, PriorStartedAt: priorStart},
	} {
		if err := e.Store.AddWorkloadHold(row); err != nil {
			t.Fatal(err)
		}
	}

	if left := e.ReconcileWorkloadHolds(context.Background(), listHolds(t, e)); len(left) != 0 {
		t.Errorf("everything was reachable: %v", left)
	}

	want := []string{"unpause ours", "start stopped"}
	if !slices.Equal(fake.calls, want) {
		t.Errorf("calls = %v, want %v", fake.calls, want)
	}
	if rows := listHolds(t, e); len(rows) != 0 {
		t.Errorf("settled rows should be gone: %v", rows)
	}
	e.ReconcileWorkloadHolds(context.Background(), listHolds(t, e))
	if !slices.Equal(fake.calls, want) {
		t.Errorf("a repeat must do nothing: %v", fake.calls)
	}
}

func TestReconcileKeepsARowItCouldNotReach(t *testing.T) {
	e, fake, _ := holdEngine(t, nil)
	row := store.WorkloadHold{ID: "op:x", NodeID: "not-registered", ContainerID: "x", Kind: holdKindPause, State: holdStateAcquired}
	if err := e.Store.AddWorkloadHold(row); err != nil {
		t.Fatal(err)
	}

	left := e.ReconcileWorkloadHolds(context.Background(), listHolds(t, e))

	if len(left) != 1 || len(listHolds(t, e)) != 1 {
		t.Errorf("an unreachable node's row must survive to be retried: left %v", left)
	}
	if len(fake.calls) != 0 {
		t.Errorf("calls = %v", fake.calls)
	}
}

// A restart used to erase this. It is explained, and the container is not started.
func TestAnInterruptedRestoreIsReportedAndNotStarted(t *testing.T) {
	e, fake, alerts := holdEngine(t, map[string]map[string]any{"app": {"Running": false, "StartedAt": priorStart}})
	b := &store.Backup{ID: "b1", TargetName: "app"}
	e.journalRestore(b, "n1")
	e.noteRestoreSnapshot(b, "snap-123456789012")

	e.ReconcileWorkloadHolds(context.Background(), listHolds(t, e))

	if len(*alerts) != 1 || !strings.Contains((*alerts)[0], "Restore interrupted: app") {
		t.Errorf("alerts = %v", *alerts)
	}
	if slices.Contains(fake.calls, "start app") {
		t.Errorf("a half-restored container was started: %v", fake.calls)
	}
	if rows := listHolds(t, e); len(rows) != 0 {
		t.Errorf("reported once, then gone: %v", rows)
	}
}

func TestAFinishedRestoreLeavesNoRow(t *testing.T) {
	e, _, _ := holdEngine(t, nil)
	clear := e.journalRestore(&store.Backup{ID: "b1", TargetName: "app"}, "n1")
	if len(listHolds(t, e)) != 1 {
		t.Fatal("a running restore should have a row")
	}
	clear()
	if rows := listHolds(t, e); len(rows) != 0 {
		t.Errorf("rows = %v", rows)
	}
}

// A pause the daemon refused because it was never running gets no row at all, and
// a successful release clears the row only once it has happened.
func TestHoldClearsItsRowOnlyAfterRelease(t *testing.T) {
	e, fake, _ := holdEngine(t, map[string]map[string]any{"app": {"Running": true, "Paused": false, "StartedAt": priorStart}})
	cli, err := e.Reg.Get("n1")
	if err != nil {
		t.Fatal(err)
	}

	result, err := e.hold(context.Background(), cli, "n1", "app", "op", PausePause, true)

	if err != nil || result.resume == nil || result.achieved != PausePause {
		t.Fatalf("the pause should have taken: %+v %v", result, err)
	}
	rows := listHolds(t, e)
	if len(rows) != 1 || rows[0].State != holdStateAcquired || rows[0].PriorStartedAt != priorStart {
		t.Fatalf("rows = %+v", rows)
	}
	result.resume()
	if rows := listHolds(t, e); len(rows) != 0 {
		t.Errorf("released, so the row should be gone: %+v", rows)
	}
	if !slices.Equal(fake.calls, []string{"pause app", "unpause app"}) {
		t.Errorf("calls = %v", fake.calls)
	}
}

// A canceled context can abandon a stop that is still happening. The run carries on
// without a hold, so it must not leave one behind.
func TestACanceledHoldLeavesNothingBehind(t *testing.T) {
	e, _, _ := holdEngine(t, map[string]map[string]any{"app": {"Running": true, "Paused": false, "StartedAt": priorStart}})
	cli, err := e.Reg.Get("n1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, herr := e.hold(ctx, cli, "n1", "app", "op", PauseStop, true)

	if herr == nil || result.owned {
		t.Errorf("a canceled stop is not a hold: %+v %v", result, herr)
	}
	if rows := listHolds(t, e); len(rows) != 0 {
		t.Errorf("rows = %+v", rows)
	}
}

// A window left open by a dead process cannot always be closed straight away.
func TestReconcileOfAHookWindow(t *testing.T) {
	row := func(id string) store.WorkloadHold {
		return store.WorkloadHold{ID: "op:hooks:" + id, NodeID: "n1", ContainerID: id, Name: id, Kind: holdKindHooks, State: holdStateAcquired, Detail: "image:1"}
	}
	e, _, alerts := holdEngine(t, map[string]map[string]any{
		"paused":  {"Running": true, "Paused": true},
		"stopped": {"Running": false},
	})
	for _, id := range []string{"gone", "paused", "stopped"} {
		if err := e.Store.AddWorkloadHold(row(id)); err != nil {
			t.Fatal(err)
		}
	}

	left := e.ReconcileWorkloadHolds(context.Background(), listHolds(t, e))

	// Cannot exec into a paused container: kept, to try again once it is released.
	if len(left) != 1 || left[0].ContainerID != "paused" {
		t.Errorf("left = %+v", left)
	}
	if rows := listHolds(t, e); len(rows) != 1 || rows[0].ContainerID != "paused" {
		t.Errorf("rows = %+v", rows)
	}
	if len(*alerts) != 1 || !strings.Contains((*alerts)[0], "stopped") {
		t.Errorf("a stopped container's unrun post-hooks should be reported: %v", *alerts)
	}
}

// A required hold the daemon refuses is an error with what to do about it, and the
// run does not get to call what follows consistent.
func TestARefusedHoldIsAnError(t *testing.T) {
	for _, tc := range []struct{ pm, verb string }{{PauseStop, "stop"}, {PausePause, "pause"}} {
		e, fake, _ := holdEngine(t, map[string]map[string]any{"app": {"Running": true, "Paused": false, "StartedAt": priorStart}})
		fake.refuse = map[string]bool{tc.verb: true}
		cli, err := e.Reg.Get("n1")
		if err != nil {
			t.Fatal(err)
		}

		result, herr := e.hold(context.Background(), cli, "n1", "app", "op", tc.pm, false)

		if herr == nil || !strings.Contains(herr.Error(), "could not "+tc.verb) || !strings.Contains(herr.Error(), "Live copy") {
			t.Errorf("%s: got %v", tc.pm, herr)
		}
		if result.resume != nil {
			t.Errorf("%s: nothing was taken, so nothing is to be released", tc.pm)
		}
		if rows := listHolds(t, e); len(rows) != 0 {
			t.Errorf("%s: rows = %+v", tc.pm, rows)
		}
	}
}

// Somebody else holding it still serves the copy, and is not this run's to undo.
func TestAContainerAlreadyHeldIsNotAFailure(t *testing.T) {
	for state, want := range map[string]string{"paused": QuiesceAlreadyPaused, "stopped": QuiesceAlreadyStopped} {
		e, fake, _ := holdEngine(t, map[string]map[string]any{"app": {"Running": state == "paused", "Paused": state == "paused", "StartedAt": priorStart}})
		fake.refuse = map[string]bool{"pause": true}
		cli, err := e.Reg.Get("n1")
		if err != nil {
			t.Fatal(err)
		}

		result, herr := e.hold(context.Background(), cli, "n1", "app", "op", PausePause, false)

		if herr != nil || result.achieved != want || result.resume != nil {
			t.Errorf("%s: %+v %v", state, result, herr)
		}
	}
}

func TestAFailedReleaseRaisesAnAlert(t *testing.T) {
	e, fake, alerts := holdEngine(t, map[string]map[string]any{"app": {"Running": true, "Paused": false, "StartedAt": priorStart}})
	cli, err := e.Reg.Get("n1")
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.hold(context.Background(), cli, "n1", "app", "op", PausePause, false)
	if err != nil {
		t.Fatal(err)
	}
	fake.refuse = map[string]bool{"unpause": true}

	result.resume()

	if len(*alerts) != 1 || !strings.Contains((*alerts)[0], "left paused") {
		t.Errorf("alerts = %v", *alerts)
	}
	if rows := listHolds(t, e); len(rows) != 1 {
		t.Errorf("the row stays so the next start tries again: %+v", rows)
	}
}

func TestQuiesceEvidence(t *testing.T) {
	cases := []struct {
		pm, engine          string
		running             bool
		volumes             int
		requested, achieved string
	}{
		{PauseNone, "", true, 1, QuiesceLive, QuiesceLive},
		{PausePause, "postgres", true, 1, PausePause, QuiesceDatabaseDump},
		{PausePause, "", false, 1, PausePause, QuiesceAlreadyStopped},
		{PauseStop, "", true, 0, PauseStop, QuiesceNoVolumes},
	}
	for _, tc := range cases {
		requested, achieved := quiesceEvidence(tc.pm, tc.engine, tc.running, tc.volumes)
		if requested != tc.requested || achieved != tc.achieved {
			t.Errorf("%+v: got %s/%s", tc, requested, achieved)
		}
	}
}

// Failing here is what stops a stack claiming one point in time.
func TestARefusedHoldFailsTheRunAndTheGroupMember(t *testing.T) {
	engine, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(engine), "if herr != nil {\n\t\t\treturn id, e.fail(b, herr)") {
		t.Error("Run must fail on a refused hold")
	}
	consistent, err := os.ReadFile("consistent.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(consistent), "sc.captureErr = herr") {
		t.Error("a stack member must fail on a refused hold")
	}
}
