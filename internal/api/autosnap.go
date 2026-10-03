package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
)

// Backup-before-change: event-triggered protective snapshot (F7).
//
// DockBack already watches each node's Docker event stream to refresh its cached
// inventory. This opt-in per-container feature reuses that same stream: when a
// PROTECTED container is about to be recreated/updated/destroyed — a destructive
// container lifecycle event (die/kill/stop/destroy) or an image pull of the image
// it runs (the Watchtower "update incoming" signal) — DockBack enqueues an
// immediate, labeled "auto: pre-change" snapshot so a bad `docker compose up -d`
// or an auto-update always has a fresh, verified rollback point.
//
// It snapshots at the STOP/DIE/PULL moment (while the container and its named
// volumes still exist) rather than at `destroy` (too late — the container is
// already gone), and coalesces the burst of events from one recreate into a
// single snapshot via a short per-container cooldown.

// autosnapSetting stores the enabled set as one JSON blob (node\x00name -> true),
// mirroring how critical databases are stored, so the hot event path can check
// membership without a per-container settings scan.
const autosnapSetting = "autosnap.enabled"

// autosnapCooldown coalesces the kill+die+destroy burst of a single recreate (and
// rapid repeated changes) into one snapshot per container.
const autosnapCooldown = 90 * time.Second

func autosnapKey(node, name string) string { return node + "\x00" + name }

func (s *Server) loadAutosnap() map[string]bool {
	out := map[string]bool{}
	js, _ := s.store.GetSetting(autosnapSetting, "{}")
	_ = json.Unmarshal([]byte(js), &out)
	return out
}

func (s *Server) saveAutosnap(m map[string]bool) error {
	b, _ := json.Marshal(m)
	return s.store.SetSetting(autosnapSetting, string(b))
}

// autosnapEventQualifies reports whether a change event is a "destructive /
// pre-change" signal worth a protective snapshot. Pure, so it is unit-testable.
func autosnapEventQualifies(ev dockercli.ChangeEvent) bool {
	switch ev.Type {
	case "container":
		switch ev.Action {
		// The container still exists at die/kill/stop (exited but present, named
		// volumes intact) — snapshottable. `destroy` is included so a plain
		// `docker rm` (no prior stop event seen) is still caught; the cooldown
		// dedups it against a preceding die.
		case "die", "kill", "stop", "destroy":
			return true
		}
	case "image":
		return ev.Action == "pull"
	}
	return false
}

// autosnapTarget identifies a container to protectively snapshot.
type autosnapTarget struct {
	Name        string
	ContainerID string
}

// autosnapTargets returns the containers a change event should snapshot: for a
// container die/kill/stop/destroy, the container itself when autosnap is enabled
// for it; for an image pull, every autosnap-enabled container running that image
// (the incoming-update case). Pure (no Docker/DB) so it is unit-testable.
func autosnapTargets(ev dockercli.ChangeEvent, enabled map[string]bool, containers []*dockercli.Container, nodeID string) []autosnapTarget {
	if !autosnapEventQualifies(ev) || len(enabled) == 0 {
		return nil
	}
	var out []autosnapTarget
	switch ev.Type {
	case "container":
		if ev.Name == "" || !enabled[autosnapKey(nodeID, ev.Name)] {
			return nil
		}
		// Prefer the live inventory id (stable/current); fall back to the event's
		// own container id.
		cid := ev.ID
		for _, c := range containers {
			if c != nil && c.Name == ev.Name && c.ID != "" {
				cid = c.ID
				break
			}
		}
		out = append(out, autosnapTarget{Name: ev.Name, ContainerID: cid})
	case "image":
		seen := map[string]bool{}
		for _, c := range containers {
			if c == nil || c.Name == "" || seen[c.Name] {
				continue
			}
			if imageMatches(c.Image, ev.Image) && enabled[autosnapKey(nodeID, c.Name)] {
				seen[c.Name] = true
				out = append(out, autosnapTarget{Name: c.Name, ContainerID: c.ID})
			}
		}
	}
	return out
}

// imageRepo strips the tag and digest from an image reference, leaving the repo
// (incl. any registry host:port) so an update pull of the same repo matches the
// running container even when the tag moved.
func imageRepo(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndexByte(ref, ':'); i >= 0 && !strings.Contains(ref[i+1:], "/") {
		ref = ref[:i]
	}
	return ref
}

func imageMatches(containerImage, eventImage string) bool {
	if containerImage == "" || eventImage == "" {
		return false
	}
	return imageRepo(containerImage) == imageRepo(eventImage)
}

// maybeAutosnap fires an event-triggered protective snapshot for any protected
// container the change event puts at risk. Called from the event-stream watcher,
// so it must stay cheap: it early-returns before any DB read unless the event is
// a destructive signal and at least one container has autosnap enabled.
func (s *Server) maybeAutosnap(nodeID string, ev dockercli.ChangeEvent) {
	if !autosnapEventQualifies(ev) {
		return
	}
	enabled := s.loadAutosnap()
	if len(enabled) == 0 {
		return
	}
	var containers []*dockercli.Container
	if st := s.getStat(nodeID); st != nil {
		containers = st.Containers
	}
	targets := autosnapTargets(ev, enabled, containers, nodeID)
	if len(targets) == 0 {
		return
	}
	node, err := s.store.GetNode(nodeID)
	if err != nil {
		return
	}
	now := time.Now()
	dests := s.effectivePolicy(nodeID).Destinations
	for _, t := range targets {
		if t.ContainerID == "" {
			continue
		}
		k := autosnapKey(nodeID, t.Name)
		s.autosnapMu.Lock()
		if now.Sub(s.autosnapLast[k]) < autosnapCooldown {
			s.autosnapMu.Unlock()
			continue // coalesce the recreate's event burst into one snapshot
		}
		s.autosnapLast[k] = now
		s.autosnapMu.Unlock()

		s.enqueueBackup(node.Name, backup.Options{
			NodeID: nodeID, ContainerID: t.ContainerID, Compression: "balanced",
			Destinations: dests, DestinationsExplicit: true,
			Label:     "auto: pre-change",
			ForceFull: true, // a pre-change rollback point stays self-contained, never a delta (F61)
		}, prioInteractive)
		s.logSink("autosnap", "INFO", fmt.Sprintf("Pre-change snapshot queued for %q on %s (triggered by %s %s)", t.Name, node.Name, ev.Type, ev.Action))
	}
}

// handleSetAutosnap enables/disables event-triggered pre-change snapshots for a
// container (mirrors handleSetCritical). Keyed by stable container NAME so it
// survives recreates.
func (s *Server) handleSetAutosnap(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cid := r.PathValue("cid")
	var body struct {
		Enabled bool `json:"enabled"`
	}
	// readJSON, like every other handler: it caps the body and refuses unknown
	// fields, and a decoder wired by hand here got neither.
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid body")
		return
	}
	cli, err := s.reg.Get(id)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	insp, err := cli.ContainerInspect(ctx, cid)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	name := strings.TrimPrefix(insp.Name, "/")

	m := s.loadAutosnap()
	k := autosnapKey(id, name)
	if body.Enabled {
		m[k] = true
	} else {
		delete(m, k)
	}
	if err := s.saveAutosnap(m); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Reset the cooldown so a re-enable can snapshot on the next event immediately.
	s.autosnapMu.Lock()
	delete(s.autosnapLast, k)
	s.autosnapMu.Unlock()

	_ = s.store.Audit(userFrom(r), "autosnap.set", name, fmt.Sprintf("enabled=%v", body.Enabled))
	writeJSON(w, http.StatusOK, map[string]any{"enabled": body.Enabled})
}
