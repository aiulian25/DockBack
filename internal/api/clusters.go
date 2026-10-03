package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"dockback/internal/store"
)

// Cluster management API (F104).
//
// Clusters group nodes into failure domains. Membership still lives on the node
// (nodes.cluster holds the NAME), so these endpoints manage identity and the
// per-cluster policy tier — they never touch a node's connection, its
// containers, or any backup.
//
// The delete contract is the one that matters and is stated explicitly in the
// error text the user sees: removing a cluster removes a GROUPING. Nodes,
// containers, backups and archives are untouched; a cluster holding nodes is
// refused unless the caller says where those nodes should go.

// clusterListResp is the registry plus the fleet-wide unassigned count, so the
// UI can render the picker and the settings table from one cheap request.
type clusterListResp struct {
	Clusters []*store.Cluster `json:"clusters"`
}

// handleListClusters returns every registered cluster with its node count.
//
// Deliberately cheap — two indexed queries, no Docker calls and no backup
// aggregation. The Dashboard's per-cluster rollup is computed in the browser
// from the nodes + coverage payloads it already loads, so grouping the fleet by
// cluster costs zero extra server work.
func (s *Server) handleListClusters(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListClusters()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, clusterListResp{Clusters: list})
}

type clusterReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Color       string `json:"color"`
}

// handleCreateCluster registers a new, empty cluster. Nodes are assigned to it
// afterwards from the node form — creating a cluster changes nothing on its own.
func (s *Server) handleCreateCluster(w http.ResponseWriter, r *http.Request) {
	var req clusterReq
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if err := store.ValidateClusterName(req.Name); err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.CreateCluster(req.Name, strings.TrimSpace(req.Description), req.Color); err != nil {
		if errors.Is(err, store.ErrClusterExists) {
			errJSON(w, http.StatusConflict, err.Error())
			return
		}
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "cluster.create", req.Name, "")
	c, err := s.store.GetCluster(req.Name)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// handleUpdateCluster edits a cluster's description/colour and, when the posted
// name differs, renames it — moving its nodes and its policy scope atomically.
func (s *Server) handleUpdateCluster(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	current, err := s.store.GetCluster(name)
	if err != nil {
		errJSON(w, http.StatusNotFound, "cluster not found")
		return
	}
	var req clusterReq
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		req.Name = current.Name
	}
	if req.Name != current.Name {
		if err := s.store.RenameCluster(current.Name, req.Name); err != nil {
			if errors.Is(err, store.ErrClusterExists) {
				errJSON(w, http.StatusConflict, err.Error())
				return
			}
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		_ = s.store.Audit(userFrom(r), "cluster.rename", current.Name, "to "+req.Name)
	}
	if err := s.store.UpdateCluster(req.Name, strings.TrimSpace(req.Description), req.Color); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.Name == current.Name {
		_ = s.store.Audit(userFrom(r), "cluster.update", req.Name, "")
	}
	c, err := s.store.GetCluster(req.Name)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// handleDeleteCluster removes a cluster from the registry.
//
// It deletes NOTHING else: no node is disconnected, no container is touched, no
// backup or archive is removed. What goes away is the grouping and the
// cluster's policy override — nodes that were inheriting from it fall back to
// the global policy (their own node override, if any, still applies).
//
// A cluster with nodes is refused with 409 and the member count, unless
// ?reassign_to=<cluster> names where those nodes should move; the move and the
// delete then happen in one transaction, so a node is never left pointing at a
// cluster that no longer exists.
func (s *Server) handleDeleteCluster(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	current, err := s.store.GetCluster(name)
	if err != nil {
		errJSON(w, http.StatusNotFound, "cluster not found")
		return
	}
	reassign := strings.TrimSpace(r.URL.Query().Get("reassign_to"))

	moved, err := s.store.DeleteCluster(current.Name, reassign)
	if err != nil {
		if errors.Is(err, store.ErrClusterInUse) {
			errJSON(w, http.StatusConflict, fmt.Sprintf(
				"%q still has %d node%s. Move them to another cluster first — removing a cluster never deletes nodes, containers or backups.",
				current.Name, current.Nodes, plural(current.Nodes)))
			return
		}
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	detail := "grouping removed; no nodes or backups deleted"
	if moved > 0 {
		detail = fmt.Sprintf("%d node%s reassigned to %q; no nodes or backups deleted", moved, plural(moved), reassign)
	}
	_ = s.store.Audit(userFrom(r), "cluster.delete", current.Name, detail)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": current.Name, "nodes_reassigned": moved})
}

// clusterMember is one server as shown on the cluster screen — enough to
// identify it, never its credential.
type clusterMember struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Address   string `json:"address"`
	Transport string `json:"transport"`
	Cluster   string `json:"cluster"`
	Reachable bool   `json:"reachable"`
}

// handleClusterMembers returns this cluster's servers plus every other server in
// the fleet, so the cluster screen can both LIST its members and offer the ones
// available to move in — without the caller having to fetch and cross-reference
// the whole node list itself.
func (s *Server) handleClusterMembers(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.GetCluster(r.PathValue("name"))
	if err != nil {
		errJSON(w, http.StatusNotFound, "cluster not found")
		return
	}
	nodes, err := s.store.ListNodes()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	members, available := []clusterMember{}, []clusterMember{}
	for _, n := range nodes {
		m := clusterMember{ID: n.ID, Name: n.Name, Address: n.Address, Transport: n.Transport, Cluster: n.Cluster}
		if st := s.getStat(n.ID); st != nil {
			m.Reachable = st.Reachable
		}
		if strings.EqualFold(n.Cluster, c.Name) {
			members = append(members, m)
		} else {
			available = append(available, m)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": members, "available": available})
}

// handleAssignClusterMembers moves servers INTO this cluster.
//
// It changes one column on each named node and nothing else — not the transport,
// not the address, and never the sealed credential. That isolation is the point:
// re-grouping a fleet from the cluster screen must not be able to disturb a
// working connection.
func (s *Server) handleAssignClusterMembers(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.GetCluster(r.PathValue("name"))
	if err != nil {
		errJSON(w, http.StatusNotFound, "cluster not found")
		return
	}
	var req struct {
		NodeIDs []string `json:"node_ids"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	moved := 0
	for _, id := range req.NodeIDs {
		n, err := s.store.GetNode(id)
		if err != nil {
			errJSON(w, http.StatusNotFound, "server not found: "+id)
			return
		}
		if strings.EqualFold(n.Cluster, c.Name) {
			continue // already here — not an error, just nothing to do
		}
		if err := s.store.SetNodeCluster(id, c.Name); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = s.store.Audit(userFrom(r), "cluster.assign", n.Name, "moved to cluster "+c.Name)
		moved++
	}
	writeJSON(w, http.StatusOK, map[string]any{"moved": moved})
}

// clusterPolicyResp mirrors the node-policy response shape so the UI can reuse
// the same override editor: the global default, this cluster's override, and
// the resolved result.
type clusterPolicyResp struct {
	Global   Policy               `json:"global"`
	Override store.PolicyOverride `json:"override"`
	Resolved Policy               `json:"resolved"`
	Nodes    int                  `json:"nodes"`
}

// clusterResolved applies a cluster's own override to the global policy. This is
// what its nodes inherit BEFORE their own node/container overrides — shown so a
// user can see what the cluster tier contributes on its own.
func (s *Server) clusterResolved(name string) (Policy, store.PolicyOverride) {
	p := s.loadPolicy()
	ov, ok := s.store.GetPolicyOverride(store.ClusterScope(name))
	if ok {
		applyOverride(&p, ov)
	}
	return p, ov
}

// handleGetClusterPolicy returns the cluster tier of the inheritance chain.
func (s *Server) handleGetClusterPolicy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c, err := s.store.GetCluster(name)
	if err != nil {
		errJSON(w, http.StatusNotFound, "cluster not found")
		return
	}
	resolved, ov := s.clusterResolved(c.Name)
	writeJSON(w, http.StatusOK, clusterPolicyResp{
		Global: s.loadPolicy(), Override: ov, Resolved: resolved, Nodes: c.Nodes,
	})
}

// handleSetClusterPolicy saves (or clears) a cluster's policy override. Clearing
// every flag deletes the row, so "inherit everything" leaves no trace — the same
// contract SetPolicyOverride already gives node and container scopes.
func (s *Server) handleSetClusterPolicy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c, err := s.store.GetCluster(name)
	if err != nil {
		errJSON(w, http.StatusNotFound, "cluster not found")
		return
	}
	var ov store.PolicyOverride
	if err := readJSON(r, &ov); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	// Clamp negatives, matching the global and node policy validation.
	for _, v := range []*int{&ov.Generations, &ov.KeepDaily, &ov.KeepWeekly, &ov.KeepMonthly, &ov.KeepYearly} {
		if *v < 0 {
			*v = 0
		}
	}
	if err := s.store.SetPolicyOverride(store.ClusterScope(c.Name), ov); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "cluster.policy.update", c.Name,
		fmt.Sprintf("dests=%v retention=%v", ov.OverrideDestinations, ov.OverrideRetention))
	s.handleGetClusterPolicy(w, r)
}

// resolveNodeCluster canonicalizes a submitted cluster name and registers it if
// new, returning the spelling to store on the node. Called by the add and edit
// node handlers so a node can never point at an unregistered cluster and so
// `prod` typed next to an existing `Prod` joins it instead of forking the fleet.
func (s *Server) resolveNodeCluster(name string) (string, error) {
	canonical, err := s.store.EnsureCluster(name)
	if err != nil {
		return "", err
	}
	return canonical, nil
}
