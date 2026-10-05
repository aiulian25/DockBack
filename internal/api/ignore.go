package api

import (
	"fmt"
	"net/http"
	"strings"

	"dockback/internal/dockercli"
)

// Ignored containers and stacks. Some workloads are not worth a backup — a
// model server whose models download again, a cache, a scratch tool — and
// every never-backed-up warning, stale alert and whole-server run kept asking
// about them. An ignored container, or every member of an ignored stack, is
// left out of those, and out of whole-server backups (Full Server Backup and a
// schedule's whole-node target). A schedule that names it, and a backup run
// from its own page, still back it up: those were asked for explicitly.

// What can be ignored. Containers and stacks are kept by name, like schedule
// targets, so the choice survives a container being recreated.
const (
	ignoreKindContainer = "container"
	ignoreKindStack     = "stack"
)

// maxIgnoredNameLength bounds a name taken from the request path. Docker's own
// names are far shorter.
const maxIgnoredNameLength = 255

// ignoreSet is one node's ignored containers and stacks, keyed "kind:name".
type ignoreSet map[string]bool

func ignoreKey(kind, name string) string { return kind + ":" + name }

// has reports whether container c is ignored, by its own name or its stack's.
func (set ignoreSet) has(c *dockercli.Container) bool {
	if set[ignoreKey(ignoreKindContainer, c.Name)] {
		return true
	}
	return c.Stack != "" && set[ignoreKey(ignoreKindStack, c.Stack)]
}

func ignoredSettingKey(nodeID string) string { return "ignored:" + nodeID }

// ignoredOn returns what is ignored on a node.
func (s *Server) ignoredOn(nodeID string) ignoreSet {
	set, _ := s.loadKeySet(ignoredSettingKey(nodeID))
	return set
}

// wholeServerTakes reports whether a whole-server backup — Full Server Backup
// or a schedule's whole-node target — takes container c: running containers,
// stopped ones too when asked, never DockBack's own test clones (F219) or an
// ignored one. The one rule, so the runs and what the pages say about them
// agree.
func wholeServerTakes(c *dockercli.Container, includeStopped bool, ignored ignoreSet) bool {
	if isTestClone(c) || ignored.has(c) {
		return false
	}
	return c.State == "running" || includeStopped
}

// ignoredItem is one ignored container or stack as the node page lists it.
type ignoredItem struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Present says whether it is on the node now; an ignored container that
	// was removed stays listed so the choice can be undone.
	Present bool `json:"present"`
	// ContainerID links a present container to its page.
	ContainerID string `json:"container_id,omitempty"`
	// Detail is the image of a container, or how many services a stack has.
	Detail string `json:"detail,omitempty"`
}

// ignoredItems lists a node's ignored containers and stacks, described from
// the inventory cache.
func (s *Server) ignoredItems(nodeID string) []ignoredItem {
	var containers []*dockercli.Container
	if st := s.getStat(nodeID); st != nil {
		containers = st.Containers
	}
	out := []ignoredItem{}
	for _, key := range sortedKeys(s.ignoredOn(nodeID)) {
		kind, name, _ := strings.Cut(key, ":")
		out = append(out, describeIgnored(kind, name, containers))
	}
	return out
}

// describeIgnored fills in what the node's containers say about one ignored
// item. Pure.
func describeIgnored(kind, name string, containers []*dockercli.Container) ignoredItem {
	item := ignoredItem{Kind: kind, Name: name}
	services := 0
	for _, c := range containers {
		if kind == ignoreKindContainer && c.Name == name {
			item.Present, item.ContainerID, item.Detail = true, c.ID, c.Image
		}
		if kind == ignoreKindStack && c.Stack == name {
			services++
		}
	}
	if services > 0 {
		item.Present, item.Detail = true, fmt.Sprintf("%d service%s", services, plural(services))
	}
	return item
}

// handleListIgnored returns a node's ignored containers and stacks.
func (s *Server) handleListIgnored(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetNode(id); err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	writeJSON(w, http.StatusOK, s.ignoredItems(id))
}

// handleIgnore adds a container or stack to a node's ignore list (PUT) or takes
// it off again (DELETE).
func (s *Server) handleIgnore(w http.ResponseWriter, r *http.Request) {
	id, kind, name := r.PathValue("id"), r.PathValue("kind"), r.PathValue("name")
	node, err := s.store.GetNode(id)
	if err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	if kind != ignoreKindContainer && kind != ignoreKindStack {
		errJSON(w, http.StatusBadRequest, "only a container or a stack can be ignored")
		return
	}
	if name == "" || len(name) > maxIgnoredNameLength || strings.ContainsFunc(name, isControlRune) {
		errJSON(w, http.StatusBadRequest, "invalid name")
		return
	}
	ignore := r.Method == http.MethodPut

	s.ignoreMu.Lock()
	set := s.ignoredOn(id)
	key := ignoreKey(kind, name)
	if ignore {
		set[key] = true
	} else {
		delete(set, key)
	}
	s.saveKeySet(ignoredSettingKey(id), sortedKeys(set))
	s.ignoreMu.Unlock()

	action := "ignore.remove"
	if ignore {
		action = "ignore.add"
	}
	_ = s.store.Audit(userFrom(r), action, kind+" "+name, "node="+node.Name)
	writeJSON(w, http.StatusOK, s.ignoredItems(id))
}

// isControlRune reports a control character, which no container or stack name
// holds.
func isControlRune(r rune) bool {
	return r < ' ' || r == 0x7f
}
