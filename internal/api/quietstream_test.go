package api

import (
	"context"
	"testing"
	"time"

	"dockback/internal/dockercli"
)

// R5 §6: "while the 58 GB stream was running through the socket proxy, a plain
// `docker inspect` call timed out after 120 seconds. The proxy itself — a single
// haproxy in front of the socket — is saturated by the transfer, so all API
// access to the source is effectively lost for the duration."
//
// The poll would not merely be slow. It would fail, mark a healthy node offline,
// and alert an operator about the machine their backup is succeeding on.
func TestRefreshSkippedWhileStreaming(t *testing.T) {
	// lastKnown is the inventory a refresh must preserve rather than re-read.
	lastKnown := func() *nodeStat {
		return &nodeStat{
			Summary:    &dockercli.NodeSummary{Total: 7, Running: 6},
			Containers: []*dockercli.Container{{ID: "c1", Name: "nextcloud"}},
			Reachable:  true,
			UpdatedAt:  1000,
		}
	}

	t.Run("a running backup holds the node", func(t *testing.T) {
		s := &Server{store: testStore(t), reg: dockercli.NewRegistry(), stats: map[string]*nodeStat{}, jobs: map[string]*backupJob{}}
		// cancel non-nil = dispatched and streaming.
		s.jobs["b1"] = &backupJob{nodeID: "n1", cancel: func() {}}
		if !s.nodeStreaming("n1") {
			t.Fatal("a running backup must pause polling for its node")
		}
		if s.nodeStreaming("n2") {
			t.Fatal("another node must be unaffected")
		}
	})

	t.Run("a QUEUED backup does not", func(t *testing.T) {
		// cancel is nil until the dispatcher starts the job. Treating a queued job
		// as busy would freeze the dashboard for the length of a nightly window
		// over work that has not begun.
		s := &Server{store: testStore(t), reg: dockercli.NewRegistry(), stats: map[string]*nodeStat{}, jobs: map[string]*backupJob{}}
		s.jobs["b1"] = &backupJob{nodeID: "n1"}
		if s.nodeStreaming("n1") {
			t.Fatal("a queued backup is streaming nothing")
		}
	})

	t.Run("a running restore holds the node too", func(t *testing.T) {
		// A restore streams a whole archive INTO the same proxy.
		s := &Server{store: testStore(t), reg: dockercli.NewRegistry(), stats: map[string]*nodeStat{}}
		_, finish, ok := s.beginRestoreRun(context.Background(), "b1", "nextcloud", "n1", time.Minute)
		if !ok {
			t.Fatal("register")
		}
		if !s.nodeStreaming("n1") {
			t.Fatal("a running restore must pause polling for its target node")
		}
		finish()
		if s.nodeStreaming("n1") {
			t.Fatal("polling must resume once the restore deregisters")
		}
	})

	t.Run("the refresh no-ops and keeps the last inventory", func(t *testing.T) {
		s := &Server{store: testStore(t), reg: dockercli.NewRegistry(), stats: map[string]*nodeStat{}, jobs: map[string]*backupJob{}}
		s.setStat("n1", lastKnown())
		s.jobs["b1"] = &backupJob{nodeID: "n1", cancel: func() {}}

		// reg is nil: if the refresh tried to reach the node at all this panics,
		// which is a sharper assertion than checking the result afterwards.
		s.refreshNodeImpl("n1", true)

		got := s.getStat("n1")
		if got == nil || got.Summary == nil || got.Summary.Total != 7 {
			t.Fatalf("the last-known inventory must survive: %+v", got)
		}
		if len(got.Containers) != 1 || got.Containers[0].Name != "nextcloud" {
			t.Fatal("the container list must survive")
		}
		if !got.Streaming {
			t.Fatal("the node must be flagged as paused")
		}
		// The one thing this must never do: turn a running backup into an outage.
		if !got.Reachable || got.Error != "" {
			t.Fatalf("a busy node must not be reported as offline: reachable=%v err=%q", got.Reachable, got.Error)
		}
	})

	t.Run("a node with no snapshot yet is not invented", func(t *testing.T) {
		s := &Server{store: testStore(t), reg: dockercli.NewRegistry(), stats: map[string]*nodeStat{}, jobs: map[string]*backupJob{}}
		s.jobs["b1"] = &backupJob{nodeID: "n1", cancel: func() {}}
		s.refreshNodeImpl("n1", true)
		got := s.getStat("n1")
		if got == nil || got.Summary != nil {
			t.Fatalf("nothing to preserve means nothing is claimed: %+v", got)
		}
	})

	t.Run("the owed refresh waits for the LAST job on the node", func(t *testing.T) {
		// A stack backup runs several services against one node. The second one
		// finishing while the third still streams must not re-open the polling the
		// third is being protected from.
		s := &Server{store: testStore(t), reg: dockercli.NewRegistry(), stats: map[string]*nodeStat{}, jobs: map[string]*backupJob{}}
		s.setStat("n1", lastKnown())
		s.jobs["a"] = &backupJob{nodeID: "n1", cancel: func() {}}
		s.jobs["b"] = &backupJob{nodeID: "n1", cancel: func() {}}

		delete(s.jobs, "a")
		s.refreshAfterStreaming("n1")
		time.Sleep(100 * time.Millisecond) // the owed refresh is async by design
		if got := s.getStat("n1"); got == nil || got.Summary == nil || got.Summary.Total != 7 {
			t.Fatal("the refresh ran while another job still held the node")
		}
		if !s.nodeStreaming("n1") {
			t.Fatal("the remaining job must still hold the node")
		}
	})
}
