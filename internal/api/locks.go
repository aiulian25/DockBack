package api

import (
	"strings"
	"sync"

	"dockback/internal/backup"
)

// Stack-exclusive operation locking (PLAN §9.10).
//
// The data-safety hazard is a backup overlapping a RESTORE of the same stack: a
// restore overwrites volumes/re-imports databases while a backup would read torn
// state (or two restores would race). The waste hazard is two backups of the
// SAME container running at once (scheduled + manual + critical-RPO all firing).
//
// opLocks enforces a read/write discipline keyed by stack:
//   - a RESTORE takes the stack EXCLUSIVELY (no backup of that stack may run, no
//     other restore),
//   - a BACKUP takes the stack as a SHARED reader (a stack's services still back
//     up concurrently — no regression to stack-backup — but a restore can't begin
//     while any backup of the stack is in flight), and additionally takes a
//     per-container slot so the same container is never backed up twice at once.
//
// It is deliberately NON-blocking: callers try to acquire and, on failure, either
// leave the job queued (backups) or return 409 (restores). This keeps the
// dispatcher and HTTP handlers free of lock waits.
type opLocks struct {
	mu         sync.Mutex
	restoring  map[string]bool // stackKey -> a restore holds it exclusively
	backups    map[string]int  // stackKey -> in-flight backup (shared) count
	containers map[string]bool // containerKey -> a backup of this container is in flight
}

func newOpLocks() *opLocks {
	return &opLocks{
		restoring:  map[string]bool{},
		backups:    map[string]int{},
		containers: map[string]bool{},
	}
}

// canBackup reports (without acquiring) whether a backup could start now — used as
// the dispatcher's selection predicate so a blocked job is left queued, not spun.
func (l *opLocks) canBackup(stackKey, containerKey string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.restoring[stackKey] && !l.containers[containerKey]
}

// acquireBackup takes the shared stack lock + the per-container slot. Returns
// false if a restore of the stack is running or the same container is already
// being backed up.
func (l *opLocks) acquireBackup(stackKey, containerKey string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.restoring[stackKey] || l.containers[containerKey] {
		return false
	}
	l.backups[stackKey]++
	l.containers[containerKey] = true
	return true
}

func (l *opLocks) releaseBackup(stackKey, containerKey string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.backups[stackKey] > 0 {
		l.backups[stackKey]--
		if l.backups[stackKey] == 0 {
			delete(l.backups, stackKey)
		}
	}
	delete(l.containers, containerKey)
}

// acquireRestore takes the stack EXCLUSIVELY. Returns false (so the caller can
// 409) if a restore is already running or any backup of the stack is in flight.
func (l *opLocks) acquireRestore(stackKey string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.restoring[stackKey] || l.backups[stackKey] > 0 {
		return false
	}
	l.restoring[stackKey] = true
	return true
}

func (l *opLocks) releaseRestore(stackKey string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.restoring, stackKey)
}

// releaseRestoreAndDispatch releases the exclusive stack lock and then wakes
// the backup dispatcher.
//
// The wake is not optional. dispatchLoop parks on an unbuffered wait when
// nothing is runnable, and a backup left queued because canBackup() saw a
// restore holding its stack has NO other event coming: every other release
// path signals the queue, this one did not. Without it the job waits for an
// unrelated enqueue — on an idle fleet, potentially until the next scheduled
// window.
func (s *Server) releaseRestoreAndDispatch(stackKey string) {
	s.locks.releaseRestore(stackKey)
	s.signalQueue()
}

// stackKey identifies the exclusion domain: the compose project when present,
// else the single container's name. Both backup and restore paths must compute
// it the same way so they mutually exclude (PLAN §9.10). The node id scopes it so
// the same stack name on two hosts doesn't cross-lock.
func stackKey(nodeID, stack, name string) string {
	if stack != "" {
		return nodeID + "\x00@" + stack
	}
	return nodeID + "\x00$" + name
}

// containerKey scopes the per-container backup-dedup slot to a node.
func containerKey(nodeID, containerID string) string {
	return nodeID + "\x00#" + containerID
}

// backupExclusionKeys returns the (stack, container-dedup) lock keys for a backup
// job. A standalone-volume backup (F23) has no container — keying it off the empty
// ContainerID would wrongly collide with the first cached container (HasPrefix ""),
// so it is scoped by "volume:<name>" instead, giving each volume its own dedup slot.
func (s *Server) backupExclusionKeys(opts backup.Options) (stack, ctr string) {
	if opts.VolumeOnly != "" {
		vk := "volume:" + opts.VolumeOnly
		return stackKey(opts.NodeID, "", vk), containerKey(opts.NodeID, vk)
	}
	return s.stackKeyForContainer(opts.NodeID, opts.ContainerID), containerKey(opts.NodeID, opts.ContainerID)
}

// stackKeyForContainer resolves a container's stack-exclusion key from the
// inventory cache (no Docker round-trip), falling back to a container-scoped key
// when the cache doesn't know it yet. Used at enqueue time so the dispatcher can
// evaluate the lock without inspecting Docker.
func (s *Server) stackKeyForContainer(nodeID, containerID string) string {
	if st := s.getStat(nodeID); st != nil {
		for _, c := range st.Containers {
			if c.ID == containerID || strings.HasPrefix(c.ID, containerID) || c.Name == containerID {
				return stackKey(nodeID, c.Stack, c.Name)
			}
		}
	}
	// Unknown to the cache: scope by container id so two backups of it still
	// exclude; cross-stack restore exclusion will simply key by name on the
	// restore side (rare cache-miss edge, never a data-safety regression).
	return stackKey(nodeID, "", containerID)
}
