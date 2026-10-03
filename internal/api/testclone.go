package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"dockback/internal/backup"
	"dockback/internal/dockercli"
)

// F219 — "Test this backup": an isolated clone the app names, and removes.
//
// Restore-as-a-copy could already do this, but only as a checkbox among fifteen
// options, with a name to invent and a container plus its volumes to remember to
// delete afterwards. Proving a backup is a routine act; it should not require
// planning your own cleanup.

// isTestClone reports whether a discovered container is one of DockBack's own
// throwaway copies.
//
// The rest of the app has to know, because a container DockBack created and will
// delete tomorrow must not be treated as somebody's workload: a whole-node
// schedule would back it up, and the coverage panel would report it as an
// unprotected container to go and protect. Both would be true statements about
// something that is not real.
func isTestClone(c *dockercli.Container) bool {
	if c == nil {
		return false
	}
	_, ok := backup.TestCloneExpiry(c.Labels)
	return ok
}

// defaultTestCloneTTLHours is how long a test clone lives unless configured
// otherwise. A day: long enough to come back to it tomorrow morning, short
// enough that forgetting costs one day of disk rather than a quarter.
const defaultTestCloneTTLHours = 24

// testCloneReapInterval is how often each known node is swept.
const testCloneReapInterval = 10 * time.Minute

// testCloneName derives the clone's name: <container>-test-<MMDD>, with a -2,
// -3 suffix when that name is taken — which it is the second time you test the
// same backup on the same day, the commonest case there is.
//
// Date rather than a timestamp because the name is read by a person on a
// `docker ps` at some later point, and "when did I make this" to the day is what
// they need. Uniqueness is what the suffix is for.
func (s *Server) testCloneName(ctx context.Context, nodeID, target string) (string, error) {
	base := fmt.Sprintf("%s-test-%s", target, time.Now().Format("0102"))
	// A long container name plus the suffix must still be a legal name; if the
	// base is already unusable there is nothing to salvage, so say so plainly.
	if !validContainerName(base) {
		return "", fmt.Errorf("cannot build a valid test-clone name from %q", target)
	}
	cli, err := s.reg.Get(nodeID)
	if err != nil {
		// The node is unreachable; the restore itself is about to fail on that.
		// Hand back the unsuffixed name rather than inventing a second error for
		// the same cause — RecreateContainer replaces a stale same-named clone
		// anyway, so this can only ever collide with a previous test clone.
		return base, nil
	}
	name := base
	for i := 2; i <= 20; i++ {
		if _, taken := dockercli.FindContainerByName(ctx, cli, name); !taken {
			return name, nil
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return "", errors.New("too many test clones of this container already exist — remove some before making another")
}

// startTestCloneReaper sweeps expired test clones off every known node.
//
// A background loop rather than a timer per clone: the expiry lives on the
// container, so the sweep is correct after a restart, after the control plane is
// restored from a backup, and for a node this instance has never seen before.
// Ten minutes is well inside the granularity anyone cares about for a 24-hour
// lifetime, and costs one container list per node.
func (s *Server) startTestCloneReaper() {
	go func() {
		t := time.NewTicker(testCloneReapInterval)
		defer t.Stop()
		for range t.C {
			s.reapTestClones()
		}
	}()
}

func (s *Server) reapTestClones() {
	defer guardPanic("test-clone reaper", "queue", func() {})
	nodes, err := s.store.ListNodes()
	if err != nil {
		return
	}
	for _, n := range nodes {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		// A node that is down is simply skipped: its clones expire against the
		// clock, not against this loop, so they go on the first sweep that reaches
		// it. Nothing is logged for it — an offline node must not fill the log with
		// a line every ten minutes.
		_, _ = s.engine.ReapTestClones(ctx, n.ID)
		cancel()
	}
}

// handleListTestClones reports the test clones currently on a node, so the
// drawer can show what a test left running and offer to remove it early.
func (s *Server) handleListTestClones(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetNode(id); err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	clones, err := s.engine.ListTestClones(ctx, id)
	if err != nil {
		// An unreachable node has no answer, not an empty one. Reported as such so
		// the UI can stay silent instead of claiming there are no clones.
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"clones": clones, "ttl_hours": s.testCloneTTLHours()})
}

type deleteTestCloneReq struct {
	NodeID      string `json:"node_id"`
	ContainerID string `json:"container_id"`
}

// handleDeleteTestClone removes one test clone now. The engine refuses any
// container that is not marked as one, so this route cannot be used as a
// general-purpose "delete that container" — which is not what the button says
// and not what the operator agreed to.
func (s *Server) handleDeleteTestClone(w http.ResponseWriter, r *http.Request) {
	var req deleteTestCloneReq
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.NodeID == "" || req.ContainerID == "" {
		errJSON(w, http.StatusBadRequest, "node_id and container_id are required")
		return
	}
	if _, err := s.store.GetNode(req.NodeID); err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	if err := s.engine.RemoveTestClone(ctx, req.NodeID, req.ContainerID); err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "restore.test_clone.remove", req.ContainerID, "node="+req.NodeID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// testCloneTTLHours is the effective lifetime, for the confirm dialog's wording.
func (s *Server) testCloneTTLHours() int {
	return s.settingInt("restore.test_clone_ttl_hours", defaultTestCloneTTLHours)
}
