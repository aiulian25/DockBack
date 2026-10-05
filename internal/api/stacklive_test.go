package api

import (
	"strings"
	"testing"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// A stack backup reached no console. An app-consistent run's services log under
// ids the job queue never saw, so their lines carried no container, and a
// fan-out run never said it was over. The container's console filters on the
// first; the stack's console stops on the second.
func TestStackServiceLinesCarryTheirContainerUntilForgotten(t *testing.T) {
	s := &Server{store: testStore(t), jobs: map[string]*backupJob{}}
	tagged, forget := s.tagStackServices("n1", func(cid, name string) backup.Options {
		return backup.Options{NodeID: "n1", ContainerID: cid}
	})

	opts := tagged("c1", "web")
	if opts.BackupID == "" {
		t.Fatal("each service needs its backup id before the run writes a line")
	}
	if node, _, container := s.logSourceFor(opts.BackupID); node != "n1" || container != "c1" {
		t.Fatalf("a service's lines must carry its node and container, got %q/%q", node, container)
	}

	forget()
	if node, _, container := s.logSourceFor(opts.BackupID); node != "" || container != "" {
		t.Fatalf("a finished run's ids must be forgotten, still %q/%q", node, container)
	}
}

func TestStackBackupEndsOnItsVerdict(t *testing.T) {
	s := &Server{store: testStore(t), jobs: map[string]*backupJob{}}
	services := []*dockercli.Container{{ID: "c1", Name: "web"}, {ID: "c2", Name: "db"}}
	const queuedAt = 1_000_000
	backups := []*store.Backup{
		{ID: "b1", NodeID: "n1", TargetName: "web", Status: "success", CreatedAt: queuedAt},
		{ID: "b2", NodeID: "n1", TargetName: "db", Status: "success", CreatedAt: queuedAt - 1}, // before this run
	}
	for _, b := range backups {
		if err := s.store.CreateBackup(b); err != nil {
			t.Fatal(err)
		}
	}

	s.jobs["retry"] = &backupJob{nodeID: "n1", containerID: "c2"}
	if !s.backupsPendingFor("n1", map[string]bool{"c2": true}) {
		t.Fatal("a queued retry of a service keeps the stack backup open")
	}
	delete(s.jobs, "retry")

	s.reportStackBackup("n1", "shop", services, queuedAt)
	last := lastRunLogLine(t, s, "stack:shop")
	if last.Level != "ERR" || !strings.Contains(last.Msg, "db") || strings.Contains(last.Msg, "web") {
		t.Fatalf("an older backup is not this run's, so the stack failed for db alone: [%s] %s", last.Level, last.Msg)
	}

	if err := s.store.CreateBackup(&store.Backup{ID: "b3", NodeID: "n1", TargetName: "db", Status: "success", CreatedAt: queuedAt + 5}); err != nil {
		t.Fatal(err)
	}
	s.reportStackBackup("n1", "shop", services, queuedAt)
	last = lastRunLogLine(t, s, "stack:shop")
	if last.Level != "INFO" || !strings.HasPrefix(last.Msg, "Stack backup complete") {
		t.Fatalf("every service backed up in this run: [%s] %s", last.Level, last.Msg)
	}
}

func lastRunLogLine(t *testing.T, s *Server, runID string) store.RunLogLine {
	t.Helper()
	lines, err := s.store.GetRunLog(runID)
	if err != nil || len(lines) == 0 {
		t.Fatalf("no run log for %s (%v)", runID, err)
	}
	return lines[len(lines)-1]
}
