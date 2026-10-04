package api

import (
	"net/http"
	"time"

	"dockback/internal/dockercli"
	"dockback/internal/storage"
	"dockback/internal/store"
)

// Backup coverage / "unprotected containers" (Fable-UI-UX B2). This joins the
// cached per-node container inventory (PLAN §4.13) with backup history and the
// schedule to answer the single most valuable question a backup tool can: which
// RUNNING containers have no protection at all.
//
// A running container is PROTECTED when its newest successful backup is recent:
// younger than twice the interval of the schedule that covers it, or than
// defaultStaleAfter when no schedule does. Older than that it is STALE, and
// with no backup at all it is UNPROTECTED, scheduled or not. The 2026-10-04 recovery
// found stacks counted as protected on the strength of backups weeks old, or of
// a schedule alone. Everything is computed from data already in memory +
// SQLite (no Docker calls), so it's cheap enough to poll.
//
// F13 adds a second, quieter bucket: STOPPED containers that still hold a named
// data volume yet have no backup and aren't schedule-covered. A stopped container
// is usually intentionally off, so it never inflates the running-unprotected
// number — but a stopped app with real data and no backup is exactly what you can
// lose, so it's surfaced separately.

type coverageContainer struct {
	ContainerID  string `json:"container_id"`
	Name         string `json:"name"`
	Stack        string `json:"stack,omitempty"`
	Image        string `json:"image,omitempty"`
	LastBackupAt int64  `json:"last_backup_at,omitempty"`
	Scheduled    bool   `json:"scheduled,omitempty"`
}

// defaultStaleAfter is how old a container's newest backup may grow, with no
// schedule covering it, before it no longer counts as protection.
const defaultStaleAfter = 8 * 24 * time.Hour

// staleIntervals is how many of its schedule's intervals a backup may age: one
// missed run is tolerated, a second is not.
const staleIntervals = 2

// offMachineDestinations counts the enabled destinations that leave this
// machine. A "local" destination is a folder on it, so it does not count. Pure.
func offMachineDestinations(dests []*store.Destination) int {
	n := 0
	for _, d := range dests {
		if d.Enabled && d.Type != storage.TypeLocal {
			n++
		}
	}
	return n
}

// backupRecency is where a container's newest backup stands.
type backupRecency int

const (
	backupNever backupRecency = iota
	backupStale
	backupRecent
)

// recencyOf judges a newest successful backup (unix seconds, 0 for none)
// against the age its schedule allows. Pure.
func recencyOf(lastBackup int64, now time.Time, interval time.Duration, scheduled bool) backupRecency {
	if lastBackup <= 0 {
		return backupNever
	}
	allowed := defaultStaleAfter
	if scheduled && interval > 0 {
		allowed = staleIntervals * interval
	}
	if now.Sub(time.Unix(lastBackup, 0)) > allowed {
		return backupStale
	}
	return backupRecent
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
	Unprotected   []coverageContainer `json:"unprotected"`     // running, never backed up
	Stale         []coverageContainer `json:"stale"`           // running, newest backup too old
	StoppedAtRisk []coverageContainer `json:"stopped_at_risk"` // stopped + named data volume + no backup (F13)
}

type coverageResp struct {
	// OffMachineDestinations counts the enabled container-backup destinations
	// that are not a folder on this machine. Zero means every copy of every
	// backup is on the one machine a disk failure would take with it.
	OffMachineDestinations    int            `json:"off_machine_destinations"`
	AppOffMachineDestinations int            `json:"app_off_machine_destinations"`
	UnprotectedTotal          int            `json:"unprotected_total"`
	StaleTotal                int            `json:"stale_total"`
	RunningTotal              int            `json:"running_total"`
	StoppedAtRiskTotal        int            `json:"stopped_at_risk_total"`
	Nodes                     []coverageNode `json:"nodes"`
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

// handleCoverage reports the fleet's backup coverage (B2).
func (s *Server) handleCoverage(w http.ResponseWriter, r *http.Request) {
	resp, err := s.computeCoverage(time.Now())
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// computeCoverage is the fleet's backup coverage, shared by the page, the
// hourly alert check and the daily digest so all three agree.
func (s *Server) computeCoverage(now time.Time) (coverageResp, error) {
	nodes, err := s.store.ListNodes()
	if err != nil {
		return coverageResp{}, err
	}
	cover := s.scheduleCoverage(now)

	resp := coverageResp{Nodes: []coverageNode{}}
	if dests, err := s.store.ListDestinations(); err == nil {
		resp.OffMachineDestinations = offMachineDestinations(dests)
	}
	if dests, err := s.store.ListAppDestinations(); err == nil {
		resp.AppOffMachineDestinations = offMachineDestinations(dests)
	}
	for _, n := range nodes {
		cn := coverageNode{NodeID: n.ID, NodeName: n.Name, Cluster: n.Cluster, Unprotected: []coverageContainer{}, Stale: []coverageContainer{}, StoppedAtRisk: []coverageContainer{}}
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
			interval, scheduled := cover.interval(n.ID, c)
			lastBackup := backedUp[c.Name]
			item := coverageContainer{ContainerID: c.ID, Name: c.Name, Stack: c.Stack, Image: c.Image, LastBackupAt: lastBackup, Scheduled: scheduled}
			if c.State != "running" {
				// A stopped container is usually intentionally off, so it never counts
				// toward the running-unprotected number. But if it still holds a NAMED
				// data volume and has neither a backup nor schedule coverage, its data
				// is genuinely at risk — surface it in the separate, quieter bucket (F13).
				if lastBackup == 0 && !scheduled && hasNamedDataVolume(c.Mounts) {
					cn.StoppedAtRisk = append(cn.StoppedAtRisk, item)
				}
				continue
			}
			cn.Running++
			switch recencyOf(lastBackup, now, interval, scheduled) {
			case backupRecent:
				cn.Protected++
			case backupStale:
				cn.Stale = append(cn.Stale, item)
			default:
				cn.Unprotected = append(cn.Unprotected, item)
			}
		}
		resp.RunningTotal += cn.Running
		resp.UnprotectedTotal += len(cn.Unprotected)
		resp.StaleTotal += len(cn.Stale)
		resp.StoppedAtRiskTotal += len(cn.StoppedAtRisk)
		resp.Nodes = append(resp.Nodes, cn)
	}
	return resp, nil
}
