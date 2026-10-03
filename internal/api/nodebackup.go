package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"dockback/internal/dockercli"
)

// F221 — "Back up this node now", server-side.
//
// The button already existed. What it did was loop from the BROWSER: one HTTP
// request per container, each one hardcoding balanced compression and dropping
// whatever that container had remembered, with no shared-bind dedup, failures
// swallowed, and the whole run abandoned if the tab was closed halfway through.
// On a forty-container node that is forty round trips to produce a set of
// backups that do not match what the schedule would have produced for the same
// containers.
//
// One call now does it on the server, resolving each container's options exactly
// the way a scheduled run does, and deduplicating shared host folders per stack
// the way a stack backup does. "Back up everything before I touch this host" and
// "the nightly run" should capture the same thing.

type nodeBackupAllReq struct {
	// IncludeStopped mirrors the schedule's own flag (F9): a stopped container is
	// usually off on purpose, but one that crashed, or an app you run
	// occasionally, still holds the data you would miss.
	IncludeStopped bool `json:"include_stopped"`
	// Destinations is the optional per-run target selection, as the node page's
	// picker has always offered. nil = each container's policy resolution.
	Destinations *[]string `json:"destinations"`
}

type nodeBackupAllResp struct {
	Status string `json:"status"`
	// Count is how many backups this call STARTED. Skipped is how many were
	// already queued or running and were therefore coalesced rather than
	// duplicated — reported separately so "nothing happened" and "it was already
	// happening" never look the same.
	Count   int      `json:"count"`
	Skipped int      `json:"skipped"`
	Names   []string `json:"names"`
}

// nodeBackupTargets picks the containers a whole-node run should capture.
//
// Pure (no live Docker, no store) so the decision about WHAT gets backed up is
// unit-testable, and sorted so two runs of the same node enqueue in the same
// order — the queue is FIFO within a priority, so the order is what the operator
// watches happen.
func nodeBackupTargets(list []*dockercli.Container, includeStopped bool) []*dockercli.Container {
	out := []*dockercli.Container{}
	for _, c := range list {
		if c == nil || c.ID == "" {
			continue
		}
		// F219: DockBack's own test clones are throwaway copies it will delete
		// tomorrow. Backing one up would store a copy of a copy under a name that
		// stops existing, and it would land in the catalog beside the real thing.
		if isTestClone(c) {
			continue
		}
		if c.State != "running" && !includeStopped {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// nodeInventory returns the node's containers, preferring the poller's cache and
// falling back to a live list. The cache is what every other read on this page
// already trusts; the fallback exists for a node added moments ago that has not
// been polled yet, so the button is not dead on a fresh connection.
func (s *Server) nodeInventory(ctx context.Context, id string) ([]*dockercli.Container, error) {
	if st := s.getStat(id); st != nil && len(st.Containers) > 0 {
		return st.Containers, nil
	}
	cli, err := s.reg.Get(id)
	if err != nil {
		return nil, err
	}
	return dockercli.ListContainers(ctx, cli)
}

// handleBackupNodeAll enqueues a backup of every eligible container on one node.
func (s *Server) handleBackupNodeAll(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	node, err := s.store.GetNode(id)
	if err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	var req nodeBackupAllReq
	if r.ContentLength != 0 {
		if err := readJSON(r, &req); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid request")
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	list, err := s.nodeInventory(ctx, id)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	targets := nodeBackupTargets(list, req.IncludeStopped)
	if len(targets) == 0 {
		errJSON(w, http.StatusNotFound, "no containers to back up on this node")
		return
	}

	dests := s.effectivePolicy(id).Destinations
	if req.Destinations != nil {
		dests = *req.Destinations
	}

	// F83: dedup shared host binds PER STACK, exactly as a stack backup does. Two
	// services of the same project mounting one media folder must capture it once
	// — a whole-node run is the case where doing it twice costs the most, and
	// where nobody is watching closely enough to notice.
	//
	// Per stack rather than across the node, because that is the unit the
	// annotation and the coverage map are built for: an unrelated container that
	// happens to mount the same path is not a member of anything and keeps its
	// own capture.
	byStack := map[string][]*dockercli.Container{}
	for _, c := range targets {
		if c.Stack != "" {
			byStack[c.Stack] = append(byStack[c.Stack], c)
		}
	}
	dedupInc := map[string][]string{}
	owners := map[string]bool{}
	projects := make([]string, 0, len(byStack))
	for p := range byStack {
		projects = append(projects, p)
	}
	sort.Strings(projects) // deterministic log order
	for _, p := range projects {
		inc, own, logs := s.sharedBindDedup(id, byStack[p])
		for k, v := range inc {
			dedupInc[k] = v
		}
		for k := range own {
			owners[k] = true
		}
		for _, line := range logs {
			s.logSink("queue", "INFO", line)
		}
	}

	// F83: owners of a shared bind go first. The queue is FIFO within a priority,
	// so the owner's fresh capture is underway before a non-owner's skip is
	// annotated as covered — the same ordering handleBackupStack applies.
	ordered := make([]*dockercli.Container, 0, len(targets))
	for _, c := range targets {
		if owners[c.Name] {
			ordered = append(ordered, c)
		}
	}
	for _, c := range targets {
		if !owners[c.Name] {
			ordered = append(ordered, c)
		}
	}

	started := []string{}
	skipped := 0
	for _, c := range ordered {
		opts := s.scheduledBackupOptions(id, c.ID, c.Name, dests)
		if inc, ok := dedupInc[c.Name]; ok {
			opts.IncludeMounts = inc
			opts.SelectionEphemeral = true // one run only — never overwrite the remembered selection
		}
		// Coalesce, container by container, against the same check a manual "back
		// up now" uses. Serialized with createMu so two clicks a moment apart
		// cannot race two full runs of the same container past the check — which
		// on a large volume means two sequential full backups, not two fast ones.
		s.createMu.Lock()
		if jid, _ := s.activeBackup(opts); jid != "" {
			s.createMu.Unlock()
			skipped++
			continue
		}
		s.runBackupAsync(node.Name, opts)
		s.createMu.Unlock()
		started = append(started, c.Name)
	}

	if len(started) == 0 {
		// Everything was already in flight. Not an error — the operator asked for
		// backups of this node and backups of this node are running — but it must
		// not report a count of zero as if it had done something.
		writeJSON(w, http.StatusAccepted, nodeBackupAllResp{Status: "already_running", Count: 0, Skipped: skipped, Names: []string{}})
		return
	}
	s.logSink("queue", "INFO", fmt.Sprintf("Whole-node backup of %s: queued %d container(s)%s", node.Name, len(started), skippedNote(skipped)))
	_ = s.store.Audit(userFrom(r), "node.backup.all", node.Name,
		fmt.Sprintf("queued=%d already_running=%d include_stopped=%v", len(started), skipped, req.IncludeStopped))
	writeJSON(w, http.StatusAccepted, nodeBackupAllResp{Status: "started", Count: len(started), Skipped: skipped, Names: started})
}

// skippedNote renders the coalesced-count clause for the log line, or nothing.
func skippedNote(n int) string {
	if n == 0 {
		return ""
	}
	if n == 1 {
		return " (1 was already running or queued)"
	}
	return fmt.Sprintf(" (%d were already running or queued)", n)
}
