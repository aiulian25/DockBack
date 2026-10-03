package api

import (
	"net/http"

	"dockback/internal/dockercli"
)

// Backup coverage / "unprotected containers" (Fable-UI-UX B2). This joins the
// cached per-node container inventory (PLAN §4.13) with backup history and the
// schedule to answer the single most valuable question a backup tool can: which
// RUNNING containers have no protection at all.
//
// A running container is PROTECTED when it has a successful backup on record OR
// is covered by the enabled schedule (a whole-node target, or a specific target
// matched by name). Everything is computed from data already in memory + SQLite
// (no Docker calls), so it's cheap enough to poll.
//
// F13 adds a second, quieter bucket: STOPPED containers that still hold a named
// data volume yet have no backup and aren't schedule-covered. A stopped container
// is usually intentionally off, so it never inflates the running-unprotected
// number — but a stopped app with real data and no backup is exactly what you can
// lose, so it's surfaced separately.

type coverageContainer struct {
	ContainerID string `json:"container_id"`
	Name        string `json:"name"`
	Stack       string `json:"stack,omitempty"`
	Image       string `json:"image,omitempty"`
}

type coverageNode struct {
	NodeID    string `json:"node_id"`
	NodeName  string `json:"node_name"`
	Cluster   string `json:"cluster"` // F104: lets the client roll coverage up per cluster
	Reachable bool   `json:"reachable"`
	Running   int    `json:"running"`
	Protected int    `json:"protected"`
	// LastBackupAt is the newest successful backup on this node (0 = never). Read
	// from the ProtectedTargets map already fetched below, so the per-cluster
	// "oldest backup" rollup costs no extra query (F104).
	LastBackupAt  int64               `json:"last_backup_at"`
	Unprotected   []coverageContainer `json:"unprotected"`
	StoppedAtRisk []coverageContainer `json:"stopped_at_risk"` // stopped + named data volume + no backup (F13)
}

type coverageResp struct {
	UnprotectedTotal   int            `json:"unprotected_total"`
	RunningTotal       int            `json:"running_total"`
	StoppedAtRiskTotal int            `json:"stopped_at_risk_total"`
	Nodes              []coverageNode `json:"nodes"`
}

// hasNamedDataVolume reports whether any mount is a NAMED docker volume — real,
// persistent app state worth backing up. Anonymous volumes (a 64-hex id Docker
// assigns) and tmpfs/bind mounts don't count: an anonymous volume is throwaway
// scratch, so a stopped container with only those isn't flagged as at-risk (F13).
func hasNamedDataVolume(mounts []dockercli.Mount) bool {
	for _, m := range mounts {
		if m.Type == "volume" && m.Name != "" && !isAnonymousVolumeName(m.Name) {
			return true
		}
	}
	return false
}

// isAnonymousVolumeName reports whether a volume name is a Docker-assigned
// anonymous-volume id (64 lowercase hex chars) rather than a user-chosen name.
func isAnonymousVolumeName(name string) bool {
	if len(name) != 64 {
		return false
	}
	for _, r := range name {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// handleCoverage computes the fleet's backup coverage (B2).
func (s *Server) handleCoverage(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.store.ListNodes()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Schedule coverage: the union of targets across all enabled named schedules
	// (F6). Legacy id-only targets can't be name-matched here; they fall back to
	// the backup-history check, so a scheduled-by-id container reads as protected
	// once it has run at least once.
	wholeNode, specific, schedOn := s.scheduleCoverage()

	resp := coverageResp{Nodes: []coverageNode{}}
	for _, n := range nodes {
		cn := coverageNode{NodeID: n.ID, NodeName: n.Name, Cluster: n.Cluster, Unprotected: []coverageContainer{}, StoppedAtRisk: []coverageContainer{}}
		st := s.getStat(n.ID)
		if st != nil {
			cn.Reachable = st.Reachable
		}
		// Resolved BEFORE the no-inventory shortcut below: an unreachable node has
		// no cached containers but may well have backups, and reporting it as
		// "never backed up" would poison the per-cluster rollup (F104).
		backedUp, _ := s.store.ProtectedTargets(n.ID) // container name -> newest success ts
		for _, ts := range backedUp {
			if ts > cn.LastBackupAt {
				cn.LastBackupAt = ts
			}
		}
		if st == nil || len(st.Containers) == 0 {
			resp.Nodes = append(resp.Nodes, cn)
			continue
		}
		for _, c := range st.Containers {
			// F219: a test clone is DockBack's own, temporary, and about to be
			// removed. Counting it as an unprotected container would ask the
			// operator to go and protect something that will not exist tomorrow.
			if isTestClone(c) {
				continue
			}
			_, hasBackup := backedUp[c.Name]
			scheduled := schedOn && (wholeNode[n.ID] || specific[n.ID+"\x00"+c.Name])
			if c.State != "running" {
				// A stopped container is usually intentionally off, so it never counts
				// toward the running-unprotected number. But if it still holds a NAMED
				// data volume and has neither a backup nor schedule coverage, its data
				// is genuinely at risk — surface it in the separate, quieter bucket (F13).
				if !hasBackup && !scheduled && hasNamedDataVolume(c.Mounts) {
					cn.StoppedAtRisk = append(cn.StoppedAtRisk, coverageContainer{
						ContainerID: c.ID, Name: c.Name, Stack: c.Stack, Image: c.Image,
					})
				}
				continue
			}
			cn.Running++
			if hasBackup || scheduled {
				cn.Protected++
				continue
			}
			cn.Unprotected = append(cn.Unprotected, coverageContainer{
				ContainerID: c.ID, Name: c.Name, Stack: c.Stack, Image: c.Image,
			})
		}
		resp.RunningTotal += cn.Running
		resp.UnprotectedTotal += len(cn.Unprotected)
		resp.StoppedAtRiskTotal += len(cn.StoppedAtRisk)
		resp.Nodes = append(resp.Nodes, cn)
	}
	writeJSON(w, http.StatusOK, resp)
}
