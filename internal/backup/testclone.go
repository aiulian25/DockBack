package backup

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
)

// F219 — a test restore that cleans up after itself.
//
// Restoring a backup as an isolated copy already existed, but it was a checkbox
// among fifteen options and the copy lived forever: you invented a name, then
// remembered — or did not — to delete the container AND the volumes Docker made
// for it. Weeks later a host is full of app-test-1, app-test-2, app-copy-final.
//
// So the clone carries its own expiry, as a label on the container itself. Not a
// row in the database: the thing that has to be cleaned up is on the host, and
// the record of when has to survive the control plane being restarted, restored
// from a backup, or pointed at a node it has not seen before. A host can always
// answer "what did you leave here and when does it expire" without asking
// DockBack anything.

// TestCloneLabel marks a container as a DockBack test clone; its value is the
// Unix second at which the clone may be removed.
//
// The com.dockback.* namespace, NOT dockback.*: the latter is the user-facing
// policy namespace parsed by parseDockbackLabels, where any key at all means
// "this container's protection is defined by labels" and locks the UI's controls
// for it. A marker DockBack writes for its own bookkeeping does not belong in
// the namespace the operator writes in.
const TestCloneLabel = "com.dockback.test_clone"

// errNotATestClone refuses to remove a container DockBack did not create as a
// test clone, however the id reached us.
var errNotATestClone = errors.New("that container is not a DockBack test clone")

// humanDuration renders a TTL the way the confirm dialog states it — whole
// hours, or days once there are enough of them to be worth reading as days.
func humanDuration(d time.Duration) string {
	h := int(d.Hours() + 0.5)
	if h >= 48 && h%24 == 0 {
		return strconv.Itoa(h/24) + " days"
	}
	if h == 1 {
		return "1 hour"
	}
	return strconv.Itoa(h) + " hours"
}

// TestCloneLabels builds the label set stamping a clone with its expiry.
func TestCloneLabels(expiry time.Time) map[string]string {
	return map[string]string{TestCloneLabel: strconv.FormatInt(expiry.Unix(), 10)}
}

// TestCloneExpiry reads a container's test-clone expiry. ok is false for a
// container that is not a test clone, and for one whose label cannot be read as
// a timestamp — an unreadable marker is never treated as "expired", because the
// only safe reading of "I do not understand this" is to leave it alone.
func TestCloneExpiry(labels map[string]string) (time.Time, bool) {
	raw, ok := labels[TestCloneLabel]
	if !ok {
		return time.Time{}, false
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || secs <= 0 {
		return time.Time{}, false
	}
	return time.Unix(secs, 0), true
}

// TestClone is a live test clone on a node, as the UI and the reaper see it.
type TestClone struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	NodeID    string `json:"node_id"`
	State     string `json:"state"`
	ExpiresAt int64  `json:"expires_at"`
}

// expiredTestClones picks the clones whose lifetime has run out. Pure (no live
// Docker) so the decision that DELETES CONTAINERS is exhaustively testable, and
// sorted by name so a sweep reads the same way twice.
//
// Deliberately conservative where it counts: a container with no marker is not
// ours, and a marker that does not parse is not a verdict — an unreadable label
// means leave it alone, never "delete it". At the boundary it takes the ordinary
// reading of an expiry timestamp: reaching it IS expiring, so now == expiry goes.
func expiredTestClones(list []*dockercli.Container, now int64) []*dockercli.Container {
	var out []*dockercli.Container
	for _, c := range list {
		if c == nil {
			continue
		}
		exp, ok := TestCloneExpiry(c.Labels)
		if !ok || exp.Unix() > now {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// liveTestClones lists every test clone on the node, expired or not, so the UI
// can show what is currently up and offer to remove it early. Sorted by name.
func liveTestClones(list []*dockercli.Container, nodeID string) []TestClone {
	out := []TestClone{}
	for _, c := range list {
		if c == nil {
			continue
		}
		exp, ok := TestCloneExpiry(c.Labels)
		if !ok {
			continue
		}
		out = append(out, TestClone{ID: c.ID, Name: c.Name, NodeID: nodeID, State: c.State, ExpiresAt: exp.Unix()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ListTestClones reports the test clones on one node.
func (e *Engine) ListTestClones(ctx context.Context, nodeID string) ([]TestClone, error) {
	cli, err := e.Reg.Get(nodeID)
	if err != nil {
		return nil, err
	}
	list, err := dockercli.ListContainers(ctx, cli)
	if err != nil {
		return nil, err
	}
	return liveTestClones(list, nodeID), nil
}

// ReapTestClones removes the expired test clones on one node, along with the
// anonymous volumes Docker created for them. Returns how many went.
//
// Errors from a single removal are swallowed on purpose: the next tick tries
// again, and one container that will not die must not stop the sweep of the
// others. An unreachable node is reported so the caller can stay quiet about it.
func (e *Engine) ReapTestClones(ctx context.Context, nodeID string) (int, error) {
	cli, err := e.Reg.Get(nodeID)
	if err != nil {
		return 0, err
	}
	list, err := dockercli.ListContainers(ctx, cli)
	if err != nil {
		return 0, err
	}
	byID := map[string]map[string]string{}
	for _, c := range list {
		if c != nil {
			byID[c.ID] = c.Labels
		}
	}
	n := 0
	for _, c := range expiredTestClones(list, time.Now().Unix()) {
		if rerr := dockercli.RemoveContainerAndAnonVolumes(ctx, cli, c.ID); rerr != nil {
			continue
		}
		n++
		e.logf("queue", "INFO", "Removed expired test clone %q on node %s (and the volumes created for it)", c.Name, nodeID)
		// #36: and the image the clone had to fetch, if nothing else uses it now.
		// After the container is gone, never before — an image is not removable
		// while something built on it still exists.
		if e.releaseCloneImage(ctx, cli, c.ID, byID[c.ID]) {
			e.logf("queue", "INFO", "Removed the image that clone had pulled — nothing else on node %s was using it", nodeID)
		}
	}
	return n, nil
}

// releaseCloneImage gives back an image a clone pulled for itself (#36).
//
// Silent about every reason not to. The usual answer is that the clone pulled
// nothing at all: it runs beside the original, whose image is already here, so
// there is no label and nothing to reclaim. An image another container has since
// been built on is not garbage either.
func (e *Engine) releaseCloneImage(ctx context.Context, cli *client.Client, containerID string, labels map[string]string) bool {
	imageID := dockercli.ClonePulledImage(labels)
	if imageID == "" {
		return false
	}
	return dockercli.ReleaseDrillImage(ctx, cli, dockercli.DrillImage{ID: imageID, PulledHere: true}, containerID)
}

// RemoveTestClone removes ONE test clone now, by container id. It refuses a
// container that is not marked as a test clone — this is reachable from an API
// route, and "delete this container" is not what the button says.
func (e *Engine) RemoveTestClone(ctx context.Context, nodeID, containerID string) error {
	cli, err := e.Reg.Get(nodeID)
	if err != nil {
		return err
	}
	list, err := dockercli.ListContainers(ctx, cli)
	if err != nil {
		return err
	}
	for _, c := range list {
		if c == nil || c.ID != containerID {
			continue
		}
		if _, ok := TestCloneExpiry(c.Labels); !ok {
			return errNotATestClone
		}
		if rerr := dockercli.RemoveContainerAndAnonVolumes(ctx, cli, c.ID); rerr != nil {
			return rerr
		}
		// #36: same as the reaper — after the container, and only what this clone
		// fetched for itself.
		e.releaseCloneImage(ctx, cli, c.ID, c.Labels)
		return nil
	}
	return errNotATestClone
}
