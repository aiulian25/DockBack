package api

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
)

// Showing the operator where a restore will land, and how much room is left
// there, BEFORE they commit (#37, PLAYBOOK §7.6).
//
// The refusal lives in the engine and fires when a restore would take the
// destination filesystem below its margin. But R5 §3's own case is not a
// refusal — 58 GB onto 74 GB free leaves 16 GB, which clears a tenth of a 98 GB
// root — and it was still the wrong disk. What changed that operator's decision
// was seeing the numbers. So the dialog reports them whether or not the verdict
// is "no".

// fsUsageTTL bounds how stale a free-space reading may be in the dialog.
//
// The plan panel refetches on a 300 ms debounce while the operator types a remap
// path, and each reading costs a sidecar. Twenty seconds keeps the panel honest
// while collapsing a burst of edits onto one probe.
const fsUsageTTL = 20 * time.Second

type fsUsageEntry struct {
	usage dockercli.FSUsage
	at    time.Time
	err   error
}

// fsUsageCache remembers recent free-space readings per node and path.
type fsUsageCache struct {
	mu      sync.Mutex
	entries map[string]fsUsageEntry
}

func newFSUsageCache() *fsUsageCache {
	return &fsUsageCache{entries: map[string]fsUsageEntry{}}
}

// fsUsageFor reads the filesystem behind a path on a node, through the cache.
//
// Failures are cached too, and for the same reason: an unreachable node or a
// path df cannot describe would otherwise mean a fresh sidecar attempt on every
// keystroke.
func (s *Server) fsUsageFor(ctx context.Context, nodeID, hostPath string) (dockercli.FSUsage, error) {
	key := nodeID + "\x00" + hostPath
	now := time.Now()

	s.fsUsage.mu.Lock()
	if e, ok := s.fsUsage.entries[key]; ok && now.Sub(e.at) < fsUsageTTL {
		s.fsUsage.mu.Unlock()
		return e.usage, e.err
	}
	s.fsUsage.mu.Unlock()

	cli, err := s.reg.Get(nodeID)
	usage := dockercli.FSUsage{}
	if err == nil {
		usage, err = dockercli.FSFreeBytes(ctx, cli, hostPath)
	}

	s.fsUsage.mu.Lock()
	// Bounded: one entry per node and path an operator has actually asked about.
	// A dialog left open for an hour re-probes; it does not accumulate.
	if len(s.fsUsage.entries) > 256 {
		s.fsUsage.entries = map[string]fsUsageEntry{}
	}
	s.fsUsage.entries[key] = fsUsageEntry{usage: usage, at: now, err: err}
	s.fsUsage.mu.Unlock()
	return usage, err
}

// restoreCapacity sums what this selection will write and measures where it goes.
//
// Returns nil when there is nothing honest to show — no payload the manifests
// can account for, no target path, or a filesystem that could not be read. A
// panel of zeroes reads as "plenty of room", which is the failure this exists to
// prevent.
func (s *Server) restoreCapacity(ctx context.Context, targetNodeID string, entries []backup.StackPlanEntry, hostBase, fromPath, toPath string) *backup.RestoreCapacity {
	var payload int64
	estimated := false
	var targets []string

	for _, entry := range entries {
		b, err := s.store.GetBackup(entry.BackupID)
		if err != nil || b.ManifestJSON == "" {
			continue
		}
		var man backup.Manifest
		if json.Unmarshal([]byte(b.ManifestJSON), &man) != nil {
			continue
		}
		bytes, wasEstimated := s.engine.RestorePayloadBytes(b, &man)
		if bytes <= 0 {
			continue
		}
		payload += bytes
		estimated = estimated || wasEstimated
		if t := backup.RestoreTargetPath(&man, fromPath, toPath, hostBase); t != "" {
			targets = append(targets, t)
		}
	}
	if payload <= 0 {
		return nil
	}
	target := backup.CommonPathBase(targets)
	if strings.TrimSpace(target) == "" {
		return nil
	}
	usage, err := s.fsUsageFor(ctx, targetNodeID, target)
	if err != nil || usage.FreeBytes <= 0 {
		return nil
	}
	capacity := backup.EvaluateRestoreCapacity(payload, usage, target, estimated)
	return &capacity
}
