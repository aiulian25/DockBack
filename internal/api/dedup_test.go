package api

import (
	"testing"

	"dockback/internal/backup"
)

// Repeat "Initiate Backup Now" clicks must coalesce onto the already-pending
// run instead of queueing extra full backups (the per-container lock only
// serializes them, so every duplicate click used to cost a whole re-backup).

func dedupServer() *Server {
	return &Server{jobs: map[string]*backupJob{}}
}

func TestActiveBackupEmpty(t *testing.T) {
	s := dedupServer()
	if id, state := s.activeBackup(backup.Options{NodeID: "n1", ContainerID: "c1"}); id != "" || state != "" {
		t.Fatalf("empty queue: got (%q,%q), want none", id, state)
	}
}

func TestActiveBackupQueuedInteractive(t *testing.T) {
	s := dedupServer()
	opts := backup.Options{NodeID: "n1", ContainerID: "c1"}
	_, ck := s.backupExclusionKeys(opts)
	s.jobs["b1"] = &backupJob{nodeID: "n1", containerID: "c1"}
	s.queue = append(s.queue, &queuedJob{id: "b1", nodeID: "n1", ctrKey: ck, priority: prioInteractive})

	id, state := s.activeBackup(opts)
	if id != "b1" || state != "queued" {
		t.Fatalf("queued interactive job: got (%q,%q), want (b1,queued)", id, state)
	}
	// A DIFFERENT container on the same node is unaffected.
	if id, _ := s.activeBackup(backup.Options{NodeID: "n1", ContainerID: "c2"}); id != "" {
		t.Fatalf("other container must not match, got %q", id)
	}
	// Same container id on another node is unaffected (keys are node-scoped).
	if id, _ := s.activeBackup(backup.Options{NodeID: "n2", ContainerID: "c1"}); id != "" {
		t.Fatalf("other node must not match, got %q", id)
	}
}

// A jittered nightly waiting in the queue must NOT swallow an explicit manual
// "back up now" — only interactive queue entries dedup.
func TestActiveBackupIgnoresQueuedScheduled(t *testing.T) {
	s := dedupServer()
	opts := backup.Options{NodeID: "n1", ContainerID: "c1"}
	_, ck := s.backupExclusionKeys(opts)
	s.jobs["b1"] = &backupJob{nodeID: "n1", containerID: "c1"}
	s.queue = append(s.queue, &queuedJob{id: "b1", nodeID: "n1", ctrKey: ck, priority: prioScheduled})

	if id, state := s.activeBackup(opts); id != "" || state != "" {
		t.Fatalf("queued scheduled job must not dedup a manual run, got (%q,%q)", id, state)
	}
}

// A dispatched job (in the jobs map, no longer in the queue) is running — a
// click during it returns that run's id.
func TestActiveBackupRunning(t *testing.T) {
	s := dedupServer()
	s.jobs["b1"] = &backupJob{nodeID: "n1", containerID: "c1"}

	id, state := s.activeBackup(backup.Options{NodeID: "n1", ContainerID: "c1"})
	if id != "b1" || state != "running" {
		t.Fatalf("running job: got (%q,%q), want (b1,running)", id, state)
	}
}

// A canceled-while-queued job must not block a fresh manual run.
func TestActiveBackupIgnoresCanceled(t *testing.T) {
	s := dedupServer()
	s.jobs["b1"] = &backupJob{nodeID: "n1", containerID: "c1", canceled: true}

	if id, state := s.activeBackup(backup.Options{NodeID: "n1", ContainerID: "c1"}); id != "" || state != "" {
		t.Fatalf("canceled job must not dedup, got (%q,%q)", id, state)
	}
}

// Standalone-volume backups (F23) key by volume name, never by the empty
// container id — a volume job must not collide with a container job.
func TestActiveBackupVolumeScoped(t *testing.T) {
	s := dedupServer()
	vopts := backup.Options{NodeID: "n1", VolumeOnly: "vol1"}
	_, ck := s.backupExclusionKeys(vopts)
	s.jobs["v1"] = &backupJob{nodeID: "n1", containerID: ""}
	s.queue = append(s.queue, &queuedJob{id: "v1", nodeID: "n1", ctrKey: ck, priority: prioInteractive})

	if id, state := s.activeBackup(vopts); id != "v1" || state != "queued" {
		t.Fatalf("same volume: got (%q,%q), want (v1,queued)", id, state)
	}
	if id, _ := s.activeBackup(backup.Options{NodeID: "n1", VolumeOnly: "vol2"}); id != "" {
		t.Fatalf("other volume must not match, got %q", id)
	}
	// A container backup on the node never matches the volume job's empty
	// container id.
	if id, _ := s.activeBackup(backup.Options{NodeID: "n1", ContainerID: "c1"}); id != "" {
		t.Fatalf("container backup must not match a volume job, got %q", id)
	}
}
