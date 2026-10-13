package api

import (
	"net/http"
	"time"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// A status summary for dashboard widgets. Homepage's customapi widget reads it
// field by field, so every value is flat and named for the tile it fills. It is
// the one route a widget-scoped token may call: the counts a dashboard shows,
// and nothing else DockBack holds — the token sits in a dashboard's config
// file, which is often kept in git.

const (
	// widgetPath is the summary's route.
	widgetPath = "/api/widget"
	// widgetFailedWindow is how far back failed_24h counts failures.
	widgetFailedWindow = 24 * time.Hour
	// maxWidgetFailingTargets bounds the failing list; failing counts them all.
	maxWidgetFailingTargets = 10
)

// widgetSummary is DockBack's state as a dashboard tile shows it.
type widgetSummary struct {
	Nodes          int            `json:"nodes"`
	NodesOffline   int            `json:"nodes_offline"`
	Backups        int            `json:"backups"`
	BackupsBytes   int64          `json:"backups_bytes"`
	Failed24h      int            `json:"failed_24h"`
	Failing        int            `json:"failing"`
	Unprotected    int            `json:"unprotected"`
	Running        int            `json:"running"`
	LastBackup     string         `json:"last_backup,omitempty"` // RFC 3339, which Homepage's relativeDate reads
	FailingTargets []widgetTarget `json:"failing_targets"`
}

// widgetTarget is one failing backup target; /backups?node=<node_id> lists it.
type widgetTarget struct {
	Name   string `json:"name"`
	Node   string `json:"node"`
	NodeID string `json:"node_id"`
}

func (s *Server) handleWidget(w http.ResponseWriter, r *http.Request) {
	summary, err := s.widgetSummary(time.Now())
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// widgetSummary reads the catalog and the coverage the dashboard shows, both
// from the database and the poller's cached inventory: no node is asked
// anything, so a dashboard polling every few seconds costs a few queries.
func (s *Server) widgetSummary(now time.Time) (widgetSummary, error) {
	tally, err := s.store.TallyBackups(now.Add(-widgetFailedWindow).Unix())
	if err != nil {
		return widgetSummary{}, err
	}
	failing, err := s.store.FailingTargets()
	if err != nil {
		return widgetSummary{}, err
	}
	coverage, err := s.computeCoverage(now)
	if err != nil {
		return widgetSummary{}, err
	}
	summary := widgetSummary{
		Nodes: len(coverage.Nodes), Backups: tally.Total, BackupsBytes: tally.Bytes,
		Failed24h: tally.FailedSince, Running: tally.Running,
		Unprotected:    coverage.UnprotectedTotal + coverage.StaleTotal,
		FailingTargets: []widgetTarget{},
	}
	nodeNames := map[string]string{}
	for _, n := range coverage.Nodes {
		nodeNames[n.NodeID] = n.NodeName
		if !n.Reachable {
			summary.NodesOffline++
		}
	}
	for _, target := range failing {
		nodeName, registered := nodeNames[target.NodeID]
		if !registered || !s.stillFailing(target) {
			continue
		}
		summary.Failing++
		if len(summary.FailingTargets) < maxWidgetFailingTargets {
			summary.FailingTargets = append(summary.FailingTargets, widgetTarget{Name: target.TargetName, Node: nodeName, NodeID: target.NodeID})
		}
	}
	if tally.LastSuccess > 0 {
		summary.LastBackup = time.Unix(tally.LastSuccess, 0).UTC().Format(time.RFC3339)
	}
	return summary, nil
}

// stillFailing says whether a failing target is one to act on: not ignored,
// and still there. A container its node's inventory no longer holds was
// removed, and the failure of something gone is not a failure to fix. A node
// with no inventory yet, and a standalone volume, keep the benefit of the doubt.
func (s *Server) stillFailing(target store.FailingTarget) bool {
	container := &dockercli.Container{Name: target.TargetName, Stack: target.Stack}
	if s.ignoredOn(target.NodeID).has(container) {
		return false
	}
	st := s.getStat(target.NodeID)
	if st == nil || len(st.Containers) == 0 || backup.IsVolumeOnlyBackup(&store.Backup{TargetName: target.TargetName}) {
		return true
	}
	for _, present := range st.Containers {
		if present.Name == target.TargetName {
			return true
		}
	}
	return false
}
