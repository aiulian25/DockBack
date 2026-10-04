package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dockback/internal/backup"
	"dockback/internal/crypto"
	"dockback/internal/dockercli"
	"dockback/internal/egress"
	"dockback/internal/storage"
	"dockback/internal/store"
	"dockback/internal/version"
)

// ---------------- Nodes ----------------

type nodeView struct {
	*store.Node
	Summary   *dockercli.NodeSummary `json:"summary,omitempty"`
	Reachable bool                   `json:"reachable"`
	Error     string                 `json:"error,omitempty"`
	// HostKeyChanged (F67): the connection is refused on an SSH pin mismatch —
	// the UI shows a dedicated critical state pointing at "Reset pinned key".
	HostKeyChanged bool `json:"host_key_changed,omitempty"`
	// Streaming (#40): a bulk transfer holds this node, so the figures above are
	// the last ones taken before it started. Says "paused", never "offline" — the
	// node is working, and telling an operator it is down mid-backup is the
	// failure this exists to prevent.
	Streaming bool `json:"streaming,omitempty"`
	// SSHAuth tells the edit form which credential an SSH node uses ("key" |
	// "password") so it prefills the right tab. Derived server-side; never the
	// secret itself. Empty for non-SSH nodes.
	SSHAuth string `json:"ssh_auth,omitempty"`
}

// sshAuthMethod reports how an SSH node authenticates ("key" | "password") for the
// edit form. It reads only which credential is present, never exposing the secret.
// Empty for non-SSH nodes or an unset credential.
func (s *Server) sshAuthMethod(n *store.Node) string {
	if n == nil || n.Transport != dockercli.TransportSSH || len(n.SecretEnc) == 0 {
		return ""
	}
	c := dockercli.ParseSSHCreds(OpenNodeSecret(n.SecretEnc, s.cfg.EncryptionKey))
	if len(c.Key) == 0 && c.Password != "" {
		return "password"
	}
	return "key"
}

// handleListNodes returns every node with its cached inventory snapshot. The
// snapshot is refreshed in the background (PLAN §4.13), so this responds
// instantly and scales to many nodes regardless of Docker latency.
func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.store.ListNodes()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	nodes = applyNodeOrder(nodes, s.storedNodeOrder())
	s.statMu.RLock()
	views := make([]*nodeView, len(nodes))
	for i, n := range nodes {
		nv := &nodeView{Node: n, SSHAuth: s.sshAuthMethod(n)}
		if st, ok := s.stats[n.ID]; ok {
			nv.Summary, nv.Reachable, nv.Error, nv.HostKeyChanged = st.Summary, st.Reachable, st.Error, st.HostKeyChanged
			nv.Streaming = st.Streaming
		} else {
			nv.Error = "refreshing…" // not polled yet; kick an async refresh
			go s.refreshNode(n.ID)
		}
		views[i] = nv
	}
	s.statMu.RUnlock()
	writeJSON(w, http.StatusOK, views)
}

type addNodeReq struct {
	// ID is set only when EDITING an existing node (Test Connection / Update), so a
	// blank credential field can fall back to the stored one instead of probing with
	// an empty key. Absent on the add flow.
	ID         string `json:"id,omitempty"`
	Name       string `json:"name"`
	Cluster    string `json:"cluster"`
	Transport  string `json:"transport"`
	Address    string `json:"address"`
	Secret     string `json:"secret"`     // ssh key PEM or mtls bundle
	Passphrase string `json:"passphrase"` // ssh key passphrase (optional)
	Password   string `json:"password"`   // ssh password (alternative to a key)
	// AuthMethod selects which SSH credential to use: "key" | "password". Empty is
	// inferred (a submitted password ⇒ password, else key), so older clients and the
	// add flow still work.
	AuthMethod string `json:"auth_method,omitempty"`
}

// sshSecretBlob encodes an SSH credential (a private key + its passphrase, and/or
// a password) as the JSON blob stored on the node. The blob is sealed at rest with
// the master key (F2, SealNodeSecret), so no plaintext credential is persisted.
func sshSecretBlob(key, passphrase, password string) []byte {
	if key == "" && passphrase == "" && password == "" {
		return nil // no credential — store nothing (SealNodeSecret keeps it empty)
	}
	b, _ := json.Marshal(struct {
		Key        string `json:"key,omitempty"`
		Passphrase string `json:"passphrase,omitempty"`
		Password   string `json:"password,omitempty"`
	}{key, passphrase, password})
	return b
}

// secretFor builds the stored credential blob for a transport.
func secretFor(transport, secret, passphrase, password string) []byte {
	if transport == dockercli.TransportSSH {
		return sshSecretBlob(secret, passphrase, password)
	}
	return []byte(secret)
}

// sshAuthMethodFor picks the SSH auth method ("key" | "password") to resolve for
// a node edit/test: the explicit request value when valid, otherwise inferred —
// a submitted password (or a stored password with no stored key) ⇒ "password",
// else "key". Pure/testable.
func sshAuthMethodFor(requested, submittedSecret, submittedPassword string, stored dockercli.SSHCreds) string {
	if requested == "key" || requested == "password" {
		return requested
	}
	switch {
	case submittedPassword != "":
		return "password"
	case submittedSecret != "":
		return "key"
	case len(stored.Key) == 0 && stored.Password != "":
		return "password"
	default:
		return "key"
	}
}

// resolveEditSecret returns the effective PLAINTEXT credential blob for a node
// test/edit. For SSH it resolves against the chosen auth method (key or password),
// falling back per-field to the value stored on `existing` when a field is left
// blank ("keep stored") — so re-testing or editing another field never requires
// re-pasting the credential, and switching methods clears the other. `existing ==
// nil` (add flow) uses the submitted values as-is. Used by BOTH Test and Save so
// they resolve identically.
func (s *Server) resolveEditSecret(transport, secret, passphrase, password, authMethod string, existing *store.Node) []byte {
	// Switching TRANSPORT (e.g. socket-proxy → SSH, or SSH → mTLS): the stored secret
	// belongs to the OLD transport and can't be "kept", so resolve like the add flow.
	// This drops the old, now-unused credential (a no-secret transport like tcp ends
	// up with an empty secret) and requires fresh credentials for the new transport.
	if existing != nil && existing.Transport != transport {
		existing = nil
	}
	if transport != dockercli.TransportSSH {
		if existing != nil && secret == "" {
			return OpenNodeSecret(existing.SecretEnc, s.cfg.EncryptionKey) // keep stored (e.g. mTLS bundle)
		}
		return []byte(secret)
	}
	var stored dockercli.SSHCreds
	if existing != nil {
		stored = dockercli.ParseSSHCreds(OpenNodeSecret(existing.SecretEnc, s.cfg.EncryptionKey))
	}
	if sshAuthMethodFor(authMethod, secret, password, stored) == "password" {
		pw := password
		if pw == "" {
			pw = stored.Password
		}
		return sshSecretBlob("", "", pw) // password auth — key/passphrase cleared
	}
	key := secret
	if key == "" {
		key = string(stored.Key)
	}
	pass := passphrase
	if pass == "" {
		pass = stored.Passphrase
	}
	return sshSecretBlob(key, pass, "") // key auth — password cleared
}

// handleAddNode registers a node connection (PLAN §2.14 "Connect New Node").
func (s *Server) handleAddNode(w http.ResponseWriter, r *http.Request) {
	var req addNodeReq
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Name == "" || req.Transport == "" || req.Address == "" {
		errJSON(w, http.StatusBadRequest, "name, transport and address are required")
		return
	}
	// F104: canonicalize the cluster (registering it if new) so the node can never
	// point at an unregistered cluster, and so a differently-cased spelling joins
	// the existing cluster instead of forking the fleet in two.
	cluster, err := s.resolveNodeCluster(req.Cluster)
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	n := &store.Node{
		ID:        randToken()[:12],
		Name:      req.Name,
		Cluster:   cluster,
		Transport: req.Transport,
		Address:   req.Address,
		Status:    "unknown",
		// Sealed at rest with the master key (F2); the registry is handed the
		// unsealed plaintext below.
		SecretEnc: SealNodeSecret(s.resolveEditSecret(req.Transport, req.Secret, req.Passphrase, req.Password, req.AuthMethod, nil), s.cfg.EncryptionKey),
	}
	if err := s.store.UpsertNode(n); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.reg.Set(dockercli.NodeConn{ID: n.ID, Transport: n.Transport, Address: n.Address, Secret: OpenNodeSecret(n.SecretEnc, s.cfg.EncryptionKey)})
	go s.refreshNode(n.ID) // populate the cache immediately
	go s.manageWatchers()  // start its event-stream watcher (PLAN §4.13)
	_ = s.store.Audit(userFrom(r), "node.add", n.Name, n.Transport+" "+n.Address)
	writeJSON(w, http.StatusOK, n)
}

// handleUpdateNode edits an existing node connection (PLAN §5.4 edit action).
// A blank secret preserves the stored credential so users needn't re-paste keys.
func (s *Server) handleUpdateNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.store.GetNode(id)
	if err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	var req addNodeReq
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Name == "" || req.Transport == "" || req.Address == "" {
		errJSON(w, http.StatusBadRequest, "name, transport and address are required")
		return
	}
	cluster, err := s.resolveNodeCluster(req.Cluster) // F104: canonicalize + register
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	existing.Name = req.Name
	existing.Cluster = cluster
	// Resolve the credential the same way Test Connection does: blank fields keep the
	// stored value (per SSH auth method), a new value replaces it, and switching SSH
	// method (key↔password) clears the other. Sealed at rest with the master key (F2).
	existing.SecretEnc = SealNodeSecret(s.resolveEditSecret(req.Transport, req.Secret, req.Passphrase, req.Password, req.AuthMethod, existing), s.cfg.EncryptionKey)
	existing.Transport = req.Transport
	existing.Address = req.Address
	if err := s.store.UpsertNode(existing); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.reg.Set(dockercli.NodeConn{ID: existing.ID, Transport: existing.Transport, Address: existing.Address, Secret: OpenNodeSecret(existing.SecretEnc, s.cfg.EncryptionKey)})
	s.stopWatcher(existing.ID)    // restart the watcher against the new connection
	go s.refreshNode(existing.ID) // re-poll with the new connection
	go s.manageWatchers()
	_ = s.store.Audit(userFrom(r), "node.update", existing.Name, existing.Transport+" "+existing.Address)
	writeJSON(w, http.StatusOK, existing)
}

// handleTestNode pings a (possibly not-yet-saved) connection (PLAN §5.4).
func (s *Server) handleTestNode(w http.ResponseWriter, r *http.Request) {
	var req addNodeReq
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	// Editing (id set): a blank credential means "keep the stored one", so fall
	// back to the saved secret instead of probing with an empty key (which fails
	// "ssh: no key found"). Same resolution the Save path uses.
	var existing *store.Node
	if req.ID != "" {
		if n, gerr := s.store.GetNode(req.ID); gerr == nil {
			existing = n
		}
	}
	secret := s.resolveEditSecret(req.Transport, req.Secret, req.Passphrase, req.Password, req.AuthMethod, existing)
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// For SSH, probe directly so the real failure (auth, refused, missing
	// socket) is surfaced instead of the Docker client's generic message. The
	// probe also captures the host-key fingerprint to show the user (F1); it does
	// NOT pin — a Test Connection is not a commitment.
	hostKeyFP := ""
	if req.Transport == dockercli.TransportSSH {
		fp, err := dockercli.ProbeSSH(ctx, req.Address, secret)
		if err != nil {
			// Return the fingerprint even on failure (it's captured before auth), so
			// the user can verify the host key regardless of a wrong key/password.
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "host_key_fingerprint": fp})
			return
		}
		hostKeyFP = fp
	}

	tmpID := "test-" + randToken()[:8]
	s.reg.Set(dockercli.NodeConn{ID: tmpID, Transport: req.Transport, Address: req.Address, Secret: secret})
	defer s.reg.Remove(tmpID)
	if err := s.reg.Ping(ctx, tmpID); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	cli, _ := s.reg.Get(tmpID)
	sum, _ := dockercli.Inventory(ctx, cli)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "summary": sum, "host_key_fingerprint": hostKeyFP})
}

// handleResetHostKey forgets a node's pinned SSH host key so the next connect
// re-pins whatever the host presents — for a legitimate host re-key (F1).
func (s *Server) handleResetHostKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetNode(id); err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	if err := s.store.DeleteHostKey(id); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.reg.Drop(id) // force a fresh connect (and re-pin) on next use
	// F67: clear the "host key changed" state immediately — the operator has
	// explicitly accepted the re-key; the refresh below re-pins on connect.
	s.statMu.Lock()
	if st, ok := s.stats[id]; ok && st.HostKeyChanged {
		cp := *st
		cp.HostKeyChanged = false
		s.stats[id] = &cp
	}
	s.statMu.Unlock()
	go s.refreshNode(id)
	_ = s.store.Audit(userFrom(r), "node.hostkey.reset", id, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
}

// handleDeleteNode forgets a node (never touches the host, PLAN §5.4). The
// removal is a DURABLE soft-delete: the node is tombstoned (hidden everywhere
// and its live connection dropped) the instant this returns, so it can't
// resurrect on a page refresh — while a short undo window can still restore it
// via handleRestoreNode. A background sweep (startNodePurge) hard-deletes the
// row and its derived state once that window elapses.
func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.SoftDeleteNode(id); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Stop using the connection immediately; the retained row + inventory/events/
	// policy/host-key are dropped by the purge sweep so Undo can fully restore.
	s.reg.Remove(id)
	s.stopWatcher(id)
	s.dropNodeCache(id)
	_ = s.store.Audit(userFrom(r), "node.delete", id, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleRestoreNode clears a forgotten node's tombstone (Undo), then re-warms its
// connection. Fails with 404 if the undo window already elapsed and the node was
// purged.
func (s *Server) handleRestoreNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n, err := s.store.GetNode(id)
	if err != nil {
		errJSON(w, http.StatusNotFound, "node no longer exists (undo window elapsed)")
		return
	}
	if err := s.store.RestoreNode(id); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	go s.refreshNode(id) // reconnect + repopulate stats
	s.manageWatchers()   // restart its event-stream watcher
	_ = s.store.Audit(userFrom(r), "node.restore", id, n.Name)
	writeJSON(w, http.StatusOK, map[string]string{"status": "restored"})
}

// handleListContainers lists containers + grouped stacks for a node, with
// server-side search/filter/pagination so a node with hundreds of containers
// stays usable (PLAN §4.13). It serves the event-driven cached inventory (PLAN
// §4.13 bullet 2) so a page load never re-lists Docker; it only falls back to a
// live fetch on a cold cache miss (a node just added and not yet polled).
//
// Query params (all optional — absent = current behavior, full list):
//
//	q          substring match on name/image/stack/service (case-insensitive)
//	state      running | stopped | paused | restarting (default: all)
//	page       1-based page number (default 1)
//	page_size  page size; >0 enables pagination, else the full filtered list
func (s *Server) handleListContainers(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	state := r.URL.Query().Get("state")
	page := atoiDefault(r.URL.Query().Get("page"), 1)
	pageSize := atoiDefault(r.URL.Query().Get("page_size"), 0)

	if st := s.getStat(id); st != nil && st.Containers != nil {
		resp := buildContainerPage(st.Containers, q, state, page, pageSize)
		resp["counts"] = countsFromSummary(st.Summary, st.Containers)
		resp["cached_at"] = st.UpdatedAt
		resp["reachable"] = st.Reachable
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Cold miss: fetch live once, then kick a background refresh to warm the cache.
	cli, err := s.reg.Get(id)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	cs, err := dockercli.ListContainers(ctx, cli)
	if err != nil {
		s.reg.Drop(id)
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	go s.refreshNode(id)
	resp := buildContainerPage(cs, q, state, page, pageSize)
	resp["counts"] = countsFromSummary(nil, cs)
	writeJSON(w, http.StatusOK, resp)
}

// buildContainerPage filters the cached containers by search/state, groups the
// filtered set into stacks, and (when pageSize>0) returns just the requested
// page along with the filtered total (PLAN §4.13).
func buildContainerPage(all []*dockercli.Container, q, state string, page, pageSize int) map[string]any {
	filtered := make([]*dockercli.Container, 0, len(all))
	for _, c := range all {
		if !matchState(c.State, state) {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(c.Name+" "+c.Image+" "+c.Stack+" "+c.Service), q) {
			continue
		}
		filtered = append(filtered, c)
	}
	total := len(filtered)
	items := filtered
	if pageSize > 0 {
		if page < 1 {
			page = 1
		}
		start := (page - 1) * pageSize
		if start > total {
			start = total
		}
		end := start + pageSize
		if end > total {
			end = total
		}
		items = filtered[start:end]
	}
	return map[string]any{
		"containers": items,
		"stacks":     dockercli.GroupStacks(filtered),
		"total":      total,
		"page":       page,
		"page_size":  pageSize,
	}
}

// matchState reports whether a container state passes the state filter. An empty
// or "all" filter matches everything; "stopped" means any non-running state
// (matching the existing UI's two-way All/Running/Stopped semantics).
func matchState(cState, filter string) bool {
	switch filter {
	case "", "all":
		return true
	case "running":
		return cState == "running"
	case "stopped":
		return cState != "running"
	case "paused":
		return cState == "paused"
	case "restarting":
		return cState == "restarting"
	default:
		return true
	}
}

// countsFromSummary returns whole-node state counts for the UI header, preferring
// the cached summary (accurate for the whole node) and falling back to counting
// the provided list (cold-miss live path).
func countsFromSummary(sum *dockercli.NodeSummary, cs []*dockercli.Container) map[string]int {
	if sum != nil {
		return map[string]int{
			"total": sum.Total, "running": sum.Running, "stopped": sum.Stopped,
			"paused": sum.Paused, "restarting": sum.Restarting,
		}
	}
	m := map[string]int{"total": len(cs)}
	for _, c := range cs {
		switch c.State {
		case "running":
			m["running"]++
		case "paused":
			m["paused"]++
		case "restarting":
			m["restarting"]++
		default:
			m["stopped"]++
		}
	}
	return m
}

// atoiDefault parses s as an int, returning def on empty/invalid input.
func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

// handleContainerMounts lists a container's backup-candidate mounts with sizes
// and the current/default selection, so the UI can let the user choose exactly
// what to back up (and auto-skip large media binds).
func (s *Server) handleContainerMounts(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cid := r.PathValue("cid")
	cli, err := s.reg.Get(id)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	// Allow time for the size scan (du) on slow nodes; it's bounded internally.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	ms, err := s.engine.ListMounts(ctx, cli, id, cid)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	// F83: shared-bind context — "also mounted in <names>" and "backed up via
	// <owner>" — from the inventory cache; no extra Docker round-trips.
	s.annotateSharedMounts(id, cid, ms)
	writeJSON(w, http.StatusOK, ms)
}

// annotateSharedMounts fills MountInfo.SharedWith/CoveredBy (F83) for a
// container's bind mounts: which OTHER containers on the node mount the same
// host Source (inventory cache), and — for a bind left unselected — which
// container's recent backups already capture it (catalog), so the picker can
// say "backed up via <owner>" instead of the large-bind nag.
func (s *Server) annotateSharedMounts(nodeID, cid string, ms []backup.MountInfo) {
	st := s.getStat(nodeID)
	if st == nil {
		return
	}
	var self string
	for _, c := range st.Containers {
		if c != nil && (c.ID == cid || strings.HasPrefix(c.ID, cid) || c.Name == cid) {
			self = c.Name
			break
		}
	}
	sharers := map[string][]string{} // host source -> other container names
	for _, c := range st.Containers {
		if c == nil || c.Name == self {
			continue
		}
		seen := map[string]bool{}
		for _, m := range c.Mounts {
			if m.Type == "bind" && m.Source != "" && !seen[m.Source] {
				seen[m.Source] = true
				sharers[m.Source] = append(sharers[m.Source], c.Name)
			}
		}
	}
	var covered map[string]string // lazy: catalog scan only when a shared bind exists
	for i := range ms {
		if ms[i].Type != "bind" || ms[i].Source == "" {
			continue
		}
		names := sharers[ms[i].Source]
		if len(names) == 0 {
			continue
		}
		sort.Strings(names)
		ms[i].SharedWith = names
		if !ms[i].Selected {
			if covered == nil {
				covered = s.engine.CoveredSources(nodeID, self)
			}
			ms[i].CoveredBy = covered[ms[i].Source]
		}
	}
}

// handleContainerDetail returns one container's live detail + its backup
// history summary for the backup-management page (PLAN §5.4 hub(1)).
func (s *Server) handleContainerDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cid := r.PathValue("cid")
	cli, err := s.reg.Get(id)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 18*time.Second)
	defer cancel()
	d, err := dockercli.InspectContainer(ctx, cli, cid)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	// Backups for this container, queried BY TARGET rather than filtered out of a
	// node-wide window — on a busy node the container's own rows fall outside that
	// window and the page reports "No backups yet" for a container that has them.
	mine, _ := s.store.ListBackupsForTarget(id, d.Name, 500)
	var totalBytes int64
	for _, b := range mine {
		totalBytes += b.SizeBytes
	}
	// Carry the user-defined node name (the same container may run on many hosts,
	// so the UI must show which machine this is).
	nodeName := id
	if n, err := s.store.GetNode(id); err == nil {
		nodeName = n.Name
	}
	// Per-container custom hooks + auto-detected presets (PLAN §9.5).
	var hooks backup.SavedHooks
	if js, _ := s.store.GetSetting(backup.HooksSettingKey(id, d.Name), ""); js != "" {
		_ = json.Unmarshal([]byte(js), &hooks)
	}
	prof := s.engine.ExportProfileFor(d.Image, id, d.Name)
	// F145: the effective default is the one the ENGINE will use, not the shipped
	// one — an application that declares its own quiesce mode must not have the
	// settings page showing "pause" for a container that will not be paused.
	pauseDefaultMode, pauseDefaultWhy := backup.AppPauseDefault(d.Image)
	pauseFallback := backup.PausePause
	if pauseDefaultMode != "" {
		pauseFallback = pauseDefaultMode
	}
	pauseMode, _ := s.store.GetSetting(backup.PauseModeKey(id, d.Name), pauseFallback)
	// Remembered manual backup options so the form reflects the user's last choice
	// (and matches what scheduled runs will use), F3. Defaults: balanced/off.
	backupOpts := backup.SavedBackupOptions{Compression: "balanced"}
	if js, _ := s.store.GetSetting(backup.BackupOptionsKey(id, d.Name), ""); js != "" {
		_ = json.Unmarshal([]byte(js), &backupOpts)
	}
	// F22: how many SQLite databases the most recent backup captured CONSISTENTLY,
	// read straight from that backup's manifest — cheap (no live sidecar scan on a
	// page load), and it reflects what was actually captured.
	sqliteFiles := 0
	newestConfigFP := "" // F73: the newest success's captured-config fingerprint
	for _, b := range mine {
		if b.Status != "success" || b.ManifestJSON == "" {
			continue
		}
		var man backup.Manifest
		if json.Unmarshal([]byte(b.ManifestJSON), &man) == nil {
			sqliteFiles = len(man.SQLiteDumps)
			newestConfigFP = man.ConfigFP
			break // newest successful backup wins
		}
	}

	// F73: config drift vs the newest successful backup — a fingerprint compare
	// only, so the page stays cheap; the /drift endpoint yields the field
	// breakdown on demand. nil when undecidable (legacy backup without a
	// fingerprint, or no backup yet) so the UI stays quiet rather than guessing.
	var drift map[string]any
	if newestConfigFP != "" {
		if liveFP := backup.ConfigFingerprint(d.Raw); liveFP != "" {
			drift = map[string]any{"changed": liveFP != newestConfigFP}
		}
	}

	// F19: is this container's protection defined declaratively by dockback.* labels?
	// If so, expose the parsed policy so the UI can show a read-only banner and lock
	// the governed controls (labels win — UI edits are disabled to avoid drift).
	labelPolicy, labelManaged := parseDockbackLabels(d.Labels)

	// F34's PITR-readiness probe used to run HERE, on a page the UI polls every
	// six seconds — a docker exec into a production database ten times a minute
	// for a read-only panel. The same probe already runs in handleCritical, whose
	// result this page's database card polls at a fifteen-second cadence, so the
	// panel reads it from there and this one is gone. One probe, one definition,
	// and nothing exec's into a database just because a page is open.

	writeJSON(w, http.StatusOK, map[string]any{
		"container":     d,
		"backups":       mine,
		"backup_count":  len(mine),
		"total_bytes":   totalBytes,
		"sqlite_files":  sqliteFiles,
		"label_managed": labelManaged,
		"label_policy":  labelPolicy,
		"storage":       s.engine.Storage.Name(),
		"node_id":       id,
		"node_name":     nodeName,
		"hooks":         hooks,
		"auto_hooks":    backup.AutoHookLabels(d.Image, d.IsDatabase),
		"pause_mode":    pauseMode,
		// F145: the quiesce mode this application declares for itself, and why.
		// Shown rather than silently applied — a default that differs from the
		// shipped one is a decision, and one made invisibly cannot be disagreed
		// with. Empty for the images that declare none, which is nearly all.
		"pause_default":     pauseDefaultMode,
		"pause_default_why": pauseDefaultWhy,
		"backup_options":    backupOpts,
		// Event-triggered "back up before changes" toggle (F7), keyed by name.
		"autosnap": s.loadAutosnap()[autosnapKey(id, d.Name)],
		// F69: reason of an active mass-change retention hold ("" = none) so the
		// page can show the review banner + clear button.
		"tripwire_hold": s.tripwireHoldReason(id, d.Name),
		// F73: {"changed": bool} when the live config's fingerprint differs from
		// the newest successful backup's; null when undecidable.
		"drift": drift,
		// Large-bind cutoff (F12): the GiB threshold that actually applies here
		// (per-container override, else global) plus the raw override (0 = none),
		// so the mount picker can show the effective value and let it be overridden.
		"regenerable": backup.RegenerablePathsFor(d.Image),
		// F151: whether the app-native export directory is emptied after a
		// successful capture. Shown beside the export toggle it belongs to.
		"cleanup_export": s.engine.CleanupExport(id, d.Name),
		// F163: whether this container refuses to be backed up without write-only
		// encryption, and — if it does — the reason its next run would be refused
		// right now. Empty means the next run proceeds.
		"require_write_only":        s.engine.RequireWriteOnly(id, d.Name),
		"write_only_blocked_reason": s.engine.WriteOnlyRequirementFor(id, d.Name, d.Image),
		// F206: whether an in-place restore of this container demands fresh proof
		// of the password, what the derived default would be, and whether an
		// operator has made an explicit choice — so the control can say "following
		// the default" instead of implying somebody decided.
		"restore_step_up":         s.restoreStepUpRequired(id, d.Name),
		"restore_step_up_default": s.restoreStepUpDefault(id, d.Name),
		"restore_step_up_set":     s.restoreStepUpIsExplicit(id, d.Name),
		// F205: WHETHER a Redis password is recorded for this container — never
		// the password. There is no endpoint that returns the value. db_engine
		// travels beside it so the UI can offer the field only where it applies,
		// rather than asking every container about Redis.
		"db_engine":           backup.DBEngine(d.Image),
		"redis_auth_set":      s.engine.HasRedisAuth(id, d.Name),
		"exclude_regenerable": s.engine.ExcludeRegenerable(id, d.Name),
		// F184: the uid:gid an operator pinned for this container's restores, ""
		// when none is set and detection decides. Beside it, what detection WOULD
		// decide, so the field can show the value it is overriding.
		"restore_ownership":      s.engine.RestoreOwnershipSpec(id, d.Name),
		"bind_skip_gib":          s.engine.BindSkipGiBEffective(id, d.Name),
		"bind_skip_gib_override": bindSkipOverrideOrZero(s.engine, id, d.Name),
		// Post-restore health-gate timeout (F30): the seconds that actually apply
		// here (per-container override, else global) + the raw override (0 = none).
		"restore_health_timeout_seconds":  s.engine.RestoreHealthTimeoutEffective(id, d.Name),
		"restore_health_timeout_override": restoreTimeoutOverrideOrZero(s.engine, id, d.Name),
		"app_export": map[string]any{
			"available":  prof.Available,
			"tool":       prof.Tool,
			"dir":        prof.Dir,
			"export_cmd": strings.Join(prof.ExportCmd, " "),
			"import_cmd": strings.Join(prof.ImportCmd, " "),
			// F152: the application's own post-import check, when it has one that
			// can actually fail.
			"verify_cmd": strings.Join(prof.VerifyCmd, " "),
			"user":       prof.User,
		},
	})
}

// resolveContainerForEdit is the preamble thirteen per-container PUT/POST
// handlers were each carrying verbatim: read {id}/{cid} from the path, get the
// node's client, inspect the container under a bounded timeout, and return its
// stable NAME (every per-container setting is keyed by name, not id, so it
// survives a recreate).
//
// On false it has already written the response, so callers just return.
func (s *Server) resolveContainerForEdit(w http.ResponseWriter, r *http.Request) (nodeID, name, image string, ok bool) {
	nodeID = r.PathValue("id")
	cid := r.PathValue("cid")
	cli, err := s.reg.Get(nodeID)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return "", "", "", false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	insp, err := cli.ContainerInspect(ctx, cid)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return "", "", "", false
	}
	if insp.Config != nil {
		image = insp.Config.Image
	}
	return nodeID, strings.TrimPrefix(insp.Name, "/"), image, true
}

// handleSetContainerHooks saves a container's custom pre/post backup hooks.
func (s *Server) handleSetContainerHooks(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cid := r.PathValue("cid")
	cli, err := s.reg.Get(id)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	d, err := dockercli.InspectContainer(ctx, cli, cid)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	var hooks backup.SavedHooks
	if err := readJSON(r, &hooks); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	js, _ := json.Marshal(hooks)
	if err := s.store.SetSetting(backup.HooksSettingKey(id, d.Name), string(js)); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "hooks.update", d.Name, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// bindSkipOverrideOrZero returns a container's per-container large-bind override in
// GiB, or 0 when none is set (F12) — a small adapter so the JSON stays a plain int.
func bindSkipOverrideOrZero(e *backup.Engine, nodeID, name string) int {
	if n, ok := e.BindSkipGiBOverride(nodeID, name); ok {
		return n
	}
	return 0
}

// handleSetBindThreshold sets (gib > 0) or clears (gib <= 0) a container's
// per-container large-bind cutoff override (F12). The engine clamps a set value to
// handleSetRestoreOwnership pins the uid:gid this container's restored data is
// owned by (F184).
//
// Detection covers images that ANNOUNCE which user they drop to. An image
// running as a baked-in user announces nothing, so there is nothing to detect —
// and that is exactly the case where data captured on a NAS that numbered its
// users differently needs the answer stated outright. Auth + CSRF gated and
// audited, like every other per-container option.
func (s *Server) handleSetRestoreOwnership(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Ownership string `json:"ownership"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	// Validated at the boundary with the same rule that stores it, so a bad
	// value is refused here rather than saved and discovered at restore time.
	if spec := strings.TrimSpace(body.Ownership); spec != "" {
		if _, _, ok := backup.ParseOwnership(spec); !ok {
			errJSON(w, http.StatusBadRequest, "ownership must be written as uid:gid — two numbers, for example 1000:1000")
			return
		}
	}
	id, name, _, ok := s.resolveContainerForEdit(w, r)
	if !ok {
		return
	}
	if err := s.engine.SetRestoreOwnership(id, name, body.Ownership); err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "restore.ownership.set", name, "ownership="+strings.TrimSpace(body.Ownership))
	writeJSON(w, http.StatusOK, map[string]any{"restore_ownership": s.engine.RestoreOwnershipSpec(id, name)})
}

// handleSetExcludeRegenerable stores whether this container's app-declared
// regenerable directories are left out of its backups (F132).
//
// Offered only for images that declare any — Jellyfin's 11 GB of trickplay
// previews being the motivating case, at 11 GB of a 12 GB archive. Auth + CSRF
// gated + audited, like every other per-container backup option.
func (s *Server) handleSetExcludeRegenerable(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cid := r.PathValue("cid")
	var body struct {
		Exclude bool `json:"exclude"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	cli, err := s.reg.Get(id)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	insp, err := cli.ContainerInspect(ctx, cid)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	name := strings.TrimPrefix(insp.Name, "/")
	// Refuse for an image that declares nothing regenerable, rather than storing
	// a setting that would silently never apply.
	if insp.Config == nil || len(backup.RegenerablePathsFor(insp.Config.Image)) == 0 {
		errJSON(w, http.StatusBadRequest, "this image declares no regenerable directories")
		return
	}
	if err := s.engine.SetExcludeRegenerable(id, name, body.Exclude); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "regenerable.set", name, fmt.Sprintf("exclude=%v", body.Exclude))
	writeJSON(w, http.StatusOK, map[string]any{"exclude_regenerable": s.engine.ExcludeRegenerable(id, name)})
}

// handleSetRequireWriteOnly stores whether this container refuses to be backed
// up unless write-only encryption is on (F163).
//
// Accepted for any container — the reasoning generalises to anything holding
// credentials to other systems, and DockBack's registry does not know every such
// application. Turning it ON while write-only is currently OFF is allowed and
// deliberately so: the response says the next run would be refused, which is the
// operator learning it now rather than from a failed schedule at three in the
// morning.
//
// Auth + CSRF gated and audited, like every other per-container backup option.
func (s *Server) handleSetRequireWriteOnly(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Require bool `json:"require"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	id, name, image, ok := s.resolveContainerForEdit(w, r)
	if !ok {
		return
	}
	if err := s.engine.SetRequireWriteOnly(id, name, body.Require); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "writeonly.require.set", name, fmt.Sprintf("require=%v", body.Require))
	writeJSON(w, http.StatusOK, map[string]any{
		"require_write_only":        s.engine.RequireWriteOnly(id, name),
		"write_only_blocked_reason": s.engine.WriteOnlyRequirementFor(id, name, image),
	})
}

// handleSetCleanupExport stores whether this container's app-native export
// directory is emptied after a successful capture (F151).
//
// Offered only for a container that HAS a usable export profile, rather than
// storing a setting that would silently never apply. Auth + CSRF gated and
// audited, like every other per-container backup option — and audited
// particularly deliberately here, because this is the one option that deletes
// something.
func (s *Server) handleSetCleanupExport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Cleanup bool `json:"cleanup"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	id, name, image, ok := s.resolveContainerForEdit(w, r)
	if !ok {
		return
	}
	if !s.engine.ExportProfileFor(image, id, name).Available {
		errJSON(w, http.StatusBadRequest, "this container has no app-native export profile, so it has no export directory to empty")
		return
	}
	if err := s.engine.SetCleanupExport(id, name, body.Cleanup); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "export.cleanup.set", name, fmt.Sprintf("cleanup=%v", body.Cleanup))
	writeJSON(w, http.StatusOK, map[string]any{"cleanup_export": s.engine.CleanupExport(id, name)})
}

// a sane range. Auth + CSRF gated + audited.
func (s *Server) handleSetBindThreshold(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GiB int `json:"gib"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	id, name, _, ok := s.resolveContainerForEdit(w, r)
	if !ok {
		return
	}
	if err := s.engine.SetBindSkipGiBOverride(id, name, body.GiB); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "bind_threshold.set", name, fmt.Sprintf("gib=%d", body.GiB))
	writeJSON(w, http.StatusOK, map[string]any{
		"bind_skip_gib":          s.engine.BindSkipGiBEffective(id, name),
		"bind_skip_gib_override": bindSkipOverrideOrZero(s.engine, id, name),
	})
}

// restoreTimeoutOverrideOrZero returns a container's per-container restore-health
// timeout override in seconds, or 0 when none is set (F30).
func restoreTimeoutOverrideOrZero(e *backup.Engine, nodeID, name string) int {
	if n, ok := e.RestoreHealthTimeoutOverride(nodeID, name); ok {
		return n
	}
	return 0
}

// handleSetRestoreTimeout sets (seconds > 0) or clears (seconds <= 0) a container's
// per-container post-restore health-timeout override (F30) — so a heavy app isn't
// force-rolled-back mid-migration. The engine clamps a set value. Auth+CSRF+audited.
func (s *Server) handleSetRestoreTimeout(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Seconds int `json:"seconds"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	id, name, _, ok := s.resolveContainerForEdit(w, r)
	if !ok {
		return
	}
	if err := s.engine.SetRestoreHealthTimeoutOverride(id, name, body.Seconds); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "restore_timeout.set", name, fmt.Sprintf("seconds=%d", body.Seconds))
	writeJSON(w, http.StatusOK, map[string]any{
		"restore_health_timeout_seconds":  s.engine.RestoreHealthTimeoutEffective(id, name),
		"restore_health_timeout_override": restoreTimeoutOverrideOrZero(s.engine, id, name),
	})
}

// handleSetPauseMode saves a container's quiesce-during-volume-backup behavior
// (none|pause|stop), remembered so scheduled/bulk backups honor it (PLAN §4.2).
func (s *Server) handleSetPauseMode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cid := r.PathValue("cid")
	cli, err := s.reg.Get(id)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	d, err := dockercli.InspectContainer(ctx, cli, cid)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	switch req.Mode {
	case backup.PauseNone, backup.PausePause, backup.PauseStop:
	default:
		errJSON(w, http.StatusBadRequest, "mode must be none, pause, or stop")
		return
	}
	if err := s.store.SetSetting(backup.PauseModeKey(id, d.Name), req.Mode); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "pause_mode.update", d.Name, req.Mode)
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// handleSetBackupOptions (F80) updates a container's remembered SavedBackupOptions
// WITHOUT starting a backup — the stack panel's inline toggles write here, the
// same settings key the manual panel and scheduled/stack runs read, so there is
// one source of truth. Partial-update semantics: omitted fields keep their
// stored values (mirroring handleCreateBackup's remembering block).
// handleSetMountSelection stores a container's mount selection without running a
// backup (F115), so the stack-backup panel can offer the same picker the
// container page has instead of sending the operator away to a different screen
// (and losing the destination choices they had already made).
//
// It writes the SAME setting a backup run writes — one source of truth, so the
// container page agrees the moment it is opened.
func (s *Server) handleSetMountSelection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cid := r.PathValue("cid")
	cli, err := s.reg.Get(id)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	// Mounts is a pointer so "omitted" is distinguishable from an explicit empty
	// list: [] means "capture none of this container's mounts" (legitimate when a
	// sibling covers them), whereas null clears the selection back to the
	// size-based default.
	var req struct {
		Mounts *[]string `json:"mounts"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
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

	if req.Mounts == nil {
		_ = s.engine.SetMountSelection(id, name, nil, nil)
		writeJSON(w, http.StatusOK, map[string]any{"mounts": []string{}, "total": len(backup.CandidateMountDestsOf(insp))})
		return
	}

	// Every destination must be a REAL backup-candidate mount of THIS container.
	// An unvalidated string would be stored happily and then match nothing at
	// capture time, quietly shrinking the backup while the UI showed a tick.
	offered := backup.CandidateMountDestsOf(insp)
	allowed := setOfStrings(offered)
	clean := make([]string, 0, len(*req.Mounts))
	seen := map[string]bool{}
	for _, d := range *req.Mounts {
		d = strings.TrimSpace(d)
		if d == "" || seen[d] {
			continue
		}
		if !allowed[d] {
			errJSON(w, http.StatusBadRequest, "not a backup-candidate mount of this container: "+d)
			return
		}
		seen[d] = true
		clean = append(clean, d)
	}
	// The candidate set travels with the choice: a mount that becomes selectable
	// later must read as newly offered, not as one this operator excluded.
	if err := s.engine.SetMountSelection(id, name, clean, offered); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mounts": clean, "total": len(allowed)})
}

// setOfStrings is a small membership helper for the validation above.
func setOfStrings(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

func (s *Server) handleSetBackupOptions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cid := r.PathValue("cid")
	cli, err := s.reg.Get(id)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	d, err := dockercli.InspectContainer(ctx, cli, cid)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	// Pointers distinguish "omitted" (keep stored) from an explicit false/empty.
	var req struct {
		Compression          *string `json:"compression"`
		AppExport            *bool   `json:"app_export"`
		SaveImage            *bool   `json:"save_image"`
		Incremental          *bool   `json:"incremental"`
		IncrementalFullEvery *int    `json:"incremental_full_every"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Compression != nil && !backup.ValidCompression(*req.Compression) {
		errJSON(w, http.StatusBadRequest, "invalid compression")
		return
	}
	var sb backup.SavedBackupOptions
	if cur, _ := s.store.GetSetting(backup.BackupOptionsKey(id, d.Name), ""); cur != "" {
		_ = json.Unmarshal([]byte(cur), &sb)
	}
	if req.Compression != nil {
		sb.Compression = *req.Compression
	}
	if req.AppExport != nil {
		sb.AppExport = *req.AppExport
	}
	if req.SaveImage != nil {
		sb.SaveImage = *req.SaveImage
	}
	if req.Incremental != nil {
		sb.Incremental = *req.Incremental
	}
	if req.IncrementalFullEvery != nil {
		sb.IncrementalFullEvery = backup.ClampFullEvery(*req.IncrementalFullEvery)
	}
	if sb.Incremental && sb.IncrementalFullEvery == 0 {
		sb.IncrementalFullEvery = backup.ClampFullEvery(0) // default cadence
	}
	js, jerr := json.Marshal(sb)
	if jerr != nil {
		errJSON(w, http.StatusInternalServerError, jerr.Error())
		return
	}
	if err := s.store.SetSetting(backup.BackupOptionsKey(id, d.Name), string(js)); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "container.backup_options", d.Name, string(js))
	writeJSON(w, http.StatusOK, sb)
}

// handleSetExportProfile saves a container's custom app-native export profile.
func (s *Server) handleSetExportProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cid := r.PathValue("cid")
	cli, err := s.reg.Get(id)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	d, err := dockercli.InspectContainer(ctx, cli, cid)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	var sp backup.SavedExportProfile
	if err := readJSON(r, &sp); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	js, _ := json.Marshal(sp)
	if err := s.store.SetSetting(backup.ExportProfileKey(id, d.Name), string(js)); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "export_profile.update", d.Name, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// handleExportProfileExport returns a container's EFFECTIVE app-native export
// profile as a portable SavedExportProfile JSON (F32) — the saved custom profile
// merged over any built-in preset — so a working profile can be copied to another
// container or node.
func (s *Server) handleExportProfileExport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cid := r.PathValue("cid")
	cli, err := s.reg.Get(id)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	d, err := dockercli.InspectContainer(ctx, cli, cid)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.engine.ExportProfileFor(d.Image, id, d.Name).Saved())
}

// handleExportProfileImport stores a SavedExportProfile onto a container (F32) — the
// import side of copy/paste-a-profile. Malformed JSON is rejected (400) up front,
// before any Docker round-trip; an empty profile is refused so a paste mistake can't
// silently blank the target. Auth+CSRF gated, and — like the PUT saver — it stores
// an operator-authored shell recipe that runs in the container with its privileges
// (an intentional operator-level capability, SEC-7), not an external vector.
func (s *Server) handleExportProfileImport(w http.ResponseWriter, r *http.Request) {
	var sp backup.SavedExportProfile
	if err := readJSON(r, &sp); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid export profile JSON")
		return
	}
	sp.Dir = strings.TrimSpace(sp.Dir)
	sp.ExportCmd = strings.TrimSpace(sp.ExportCmd)
	sp.ImportCmd = strings.TrimSpace(sp.ImportCmd)
	sp.User = strings.TrimSpace(sp.User)
	if sp.Dir == "" && sp.ExportCmd == "" && sp.ImportCmd == "" {
		errJSON(w, http.StatusBadRequest, "export profile is empty — it needs at least a directory or a command")
		return
	}
	id := r.PathValue("id")
	cid := r.PathValue("cid")
	cli, err := s.reg.Get(id)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	d, err := dockercli.InspectContainer(ctx, cli, cid)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	js, _ := json.Marshal(sp)
	if err := s.store.SetSetting(backup.ExportProfileKey(id, d.Name), string(js)); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "export_profile.import", d.Name, "")
	// Return the effective profile so the UI can reflect the merged result.
	writeJSON(w, http.StatusOK, s.engine.ExportProfileFor(d.Image, id, d.Name).Saved())
}

// ---------------- Backups ----------------

type createBackupReq struct {
	NodeID      string `json:"node_id"`
	ContainerID string `json:"container_id"`
	// ContainerName (F222) targets by NAME instead of id, resolved to the current
	// id here. An id dies on every recreate; the name is what survives one, which
	// is why every other targeting surface in this app — schedules, per-container
	// options, pause modes — has always been name-keyed. Ignored when an id is
	// supplied, so every existing caller is byte-for-byte unchanged.
	ContainerName string    `json:"container_name"`
	StopApp       bool      `json:"stop_app"`
	Compression   string    `json:"compression"`
	Destinations  *[]string `json:"destinations"` // nil = all enabled; [] = local only
	AppExport     bool      `json:"app_export"`   // use app-native export (Paperless etc.)
	SaveImage     bool      `json:"save_image"`   // bundle `docker save` image tarball (air-gapped, PLAN §8.4)
	PauseMode     string    `json:"pause_mode"`   // ""|none|pause|stop quiesce during volume copy (PLAN §4.2)
	Mounts        *[]string `json:"mounts"`       // nil = remembered/default; else explicit destinations
	Databases     []string  `json:"databases"`    // nil/empty = whole cluster; else only these DBs (F8)
	// Incremental volume capture (F61): remembered per container. IncrementalFullEvery
	// is clamped [2,30] (0 = default 7). Incremental is a pointer so an omitted field
	// on a partial request leaves the remembered choice untouched.
	Incremental          *bool `json:"incremental"`
	IncrementalFullEvery int   `json:"incremental_full_every"`
}

// handleCreateBackup starts an async backup and returns immediately (PLAN §4.11).
func (s *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	var req createBackupReq
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.NodeID == "" || (req.ContainerID == "" && strings.TrimSpace(req.ContainerName) == "") {
		errJSON(w, http.StatusBadRequest, "node_id and container_id (or container_name) are required")
		return
	}
	// F8: the selected names end up as arguments to a client run as root inside
	// the database container. Every builder quotes them, so a hostile name is
	// already inert — but refusing it HERE is where the caller can be told what
	// is wrong, instead of a dump that fails minutes later inside the container
	// with whatever the client chose to say about it.
	if bad := backup.InvalidDBNames(req.Databases); len(bad) > 0 {
		errJSON(w, http.StatusBadRequest, fmt.Sprintf(
			"these are not usable database names: %s — a name may contain letters, digits, underscore, hyphen and dot, and may not begin with a hyphen or a dot",
			strings.Join(bad, ", ")))
		return
	}
	node, err := s.store.GetNode(req.NodeID)
	if err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	// F222: resolve the name to whatever id it has RIGHT NOW. The 404 names the
	// container, because "not found" about a name the operator chose is a
	// different problem from "not found" about an id they never saw.
	if req.ContainerID == "" {
		cid, rerr := s.resolveContainerID(req.NodeID, req.ContainerName)
		if rerr != nil {
			errJSON(w, http.StatusNotFound, rerr.Error())
			return
		}
		req.ContainerID = cid
	}
	opts := backup.Options{
		NodeID: req.NodeID, ContainerID: req.ContainerID, StopApp: req.StopApp, Compression: req.Compression,
		AppExport: req.AppExport, SaveImage: req.SaveImage, PauseMode: req.PauseMode,
		Databases: req.Databases, // per-database selection (F8); nil/empty = whole cluster
		// F84: "balanced" is the manual panel's pre-seeded default, so only a
		// non-default value marks a deliberate choice that suppresses autotune.
		CompressionExplicit: req.Compression != "" && req.Compression != "balanced",
	}
	if req.Destinations != nil {
		opts.Destinations = *req.Destinations
		opts.DestinationsExplicit = true
	}
	if req.Mounts != nil {
		opts.IncludeMounts = *req.Mounts
	}
	// Fast-path duplicate check BEFORE the options persist below, so a repeat
	// click leaves no side effects at all. Unlocked — the authoritative re-check
	// under createMu further down closes the double-submit race; this one only
	// spares the common case a needless resolve/persist.
	if id, state := s.activeBackup(opts); id != "" {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "already_" + state, "backup_id": id})
		return
	}
	// Remember the chosen compression / app-native export / save-image / incremental
	// settings so scheduled and whole-node runs reuse them (F3/F61). Keyed by
	// container NAME, like the remembered pause mode. Persisted BEFORE the async run
	// so this very run's incremental decision reads the just-chosen setting. Best-
	// effort: a lookup/persist failure never affects the backup that starts next.
	if name, nerr := s.resolveContainerName(req.NodeID, req.ContainerID); nerr == nil && name != "" {
		sb := backup.SavedBackupOptions{Compression: req.Compression, AppExport: req.AppExport, SaveImage: req.SaveImage}
		if req.Incremental != nil {
			sb.Incremental = *req.Incremental
			sb.IncrementalFullEvery = backup.ClampFullEvery(req.IncrementalFullEvery)
		} else if cur, _ := s.store.GetSetting(backup.BackupOptionsKey(req.NodeID, name), ""); cur != "" {
			// Field omitted: preserve the remembered incremental choice.
			var prev backup.SavedBackupOptions
			if json.Unmarshal([]byte(cur), &prev) == nil {
				sb.Incremental, sb.IncrementalFullEvery = prev.Incremental, prev.IncrementalFullEvery
			}
		}
		if js, jerr := json.Marshal(sb); jerr == nil {
			_ = s.store.SetSetting(backup.BackupOptionsKey(req.NodeID, name), string(js))
		}
	}
	// Coalesce repeat clicks: if an interactive backup of this container is
	// already queued or running, return ITS id instead of enqueueing another
	// full run. Check + enqueue are serialized so a double-click can't race two
	// jobs past the check. No side effects on the duplicate path — nothing
	// started, so nothing is audited.
	s.createMu.Lock()
	if id, state := s.activeBackup(opts); id != "" {
		s.createMu.Unlock()
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "already_" + state, "backup_id": id})
		return
	}
	id := s.runBackupAsync(node.Name, opts)
	s.createMu.Unlock()
	_ = s.store.Audit(userFrom(r), "backup.start", req.ContainerID, node.Name)
	// "started" is kept verbatim for existing API-token consumers; backup_id is
	// additive and lets the UI track this exact run to completion.
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started", "backup_id": id})
}

// handleCancelBackup stops an in-flight backup. The worker marks the record as
// "canceled" once the run unwinds.
func (s *Server) handleCancelBackup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.cancelBackup(id) {
		errJSON(w, http.StatusNotFound, "no running backup with that id")
		return
	}
	_ = s.store.Audit(userFrom(r), "backup.cancel", id, "")
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "canceling"})
}

// handleListBackups lists backups, optionally filtered by ?node_id=.
// handleListBackups returns backup history. With no pagination params it returns
// a plain array (recent window, backward-compatible). When page_size>0 it returns
// a server-side paged/searched/filtered envelope {items,total,page,page_size} so
// the history view scales to thousands of rows (PLAN §4.13).
//
// Query params: node_id, q (target/stack search), status, verified, page, page_size.
func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	pageSize := atoiDefault(qp.Get("page_size"), 0)
	// slim=1 (perf Fix 8): the updated UI asks for summaries instead of raw
	// blobs. Callers WITHOUT the flag (older scripts/API tokens) keep the fat
	// shape for one release before the default flips.
	slim := qp.Get("slim") == "1"

	if pageSize <= 0 {
		// Legacy/simple path: recent window as a plain array.
		list, err := s.store.ListBackups(qp.Get("node_id"), 200)
		if err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		dm, sbm := s.drillMap(), s.standbyMap()
		for i := range list {
			s.annotateKeyMismatch(list[i])
			man, locs := s.annotateConfidence(list[i], dm, sbm)
			if slim {
				summarizeListRow(list[i], man, locs)
			}
		}
		writeJSON(w, http.StatusOK, list)
		return
	}

	page := atoiDefault(qp.Get("page"), 1)
	if page < 1 {
		page = 1
	}
	if pageSize > 500 {
		pageSize = 500
	}
	list, total, err := s.store.ListBackupsPage(store.BackupFilter{
		NodeID:   qp.Get("node_id"),
		Query:    qp.Get("q"),
		Status:   qp.Get("status"),
		Verified: qp.Get("verified"),
		Limit:    pageSize,
		Offset:   (page - 1) * pageSize,
	})
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	dm, sbm := s.drillMap(), s.standbyMap()
	for i := range list {
		s.annotateKeyMismatch(list[i])
		man, locs := s.annotateConfidence(list[i], dm, sbm)
		if slim {
			summarizeListRow(list[i], man, locs)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": list, "total": total, "page": page, "page_size": pageSize,
	})
}

// annotateKeyMismatch flags a backup whose master-key fingerprint differs from
// the current key, so the UI can warn it's "encrypted with a key you no longer
// have" (PLAN §3.3). Computed from the manifest; never persisted.
func (s *Server) annotateKeyMismatch(b *store.Backup) {
	if b == nil || b.ManifestJSON == "" {
		return
	}
	var m struct {
		KeyFingerprint string `json:"key_fingerprint"`
	}
	if json.Unmarshal([]byte(b.ManifestJSON), &m) == nil && m.KeyFingerprint != "" && m.KeyFingerprint != s.engine.MasterKeyFP() {
		b.KeyMismatch = true
	}
}

// handleGetBackup returns one backup record (manifest + verification report).
func (s *Server) handleGetBackup(w http.ResponseWriter, r *http.Request) {
	b, err := s.store.GetBackup(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.annotateKeyMismatch(b)
	s.annotateConfidence(b, s.drillMap(), s.standbyMap())
	// F63: how many live incremental backups depend on this one — the drawer
	// shows a "baseline of N deltas" chip and delete warns before orphaning.
	if pm, err := s.store.BackupParentMap(b.NodeID, b.TargetName); err == nil {
		b.ChainDependents = len(backup.ChainDescendants(b.ID, pm))
	}
	writeJSON(w, http.StatusOK, b)
}

// imageBundleAdvice is the actionable next step when a backup's image can't be
// obtained for a restore (F11): re-back up with the existing "bundle image" option
// so the image travels inside the archive and restores fully offline.
const imageBundleAdvice = "Restore image unavailable — bundle the image (image.tar) on the next backup."

// restoreReadinessResp reports whether a backup could be restored right now with
// respect to its container IMAGE (F11): the image must be obtainable — present
// locally, pullable from a registry by its recorded digest/tag, or bundled inside
// the archive (image.tar) for a fully offline restore. Surfaced up front so a
// gone/unpullable image is caught before a restore fails partway through recreate.
type restoreReadinessResp struct {
	ImageOK     bool   `json:"image_ok"`
	HasImageTar bool   `json:"has_image_tar"`
	Image       string `json:"image,omitempty"`
	ImageDigest string `json:"image_digest,omitempty"`
	Detail      string `json:"detail"`
	// Portability (F94): what the TARGET host cannot honor, when restoring
	// somewhere other than where the backup came from. Informational — it never
	// blocks a restore, because the operator may know something we cannot.
	Portability []string `json:"portability,omitempty"`
	// ImageDrift (F95): what the image about to run declares that this backup's
	// configuration does not provide. Also informational.
	ImageDrift []string `json:"image_drift,omitempty"`
	// AppPreconditions (F110): what the APPLICATION needs from wherever it lands
	// — the container mount destinations its database has baked into its own
	// rows, and any path that must be on local disk. Distinct from Portability,
	// which asks whether the host can run the container at all; this asks whether
	// the app will mean anything once it does. Informational here; the storage
	// half of it is enforced at restore time.
	AppPreconditions *backup.AppRestorePreconditions `json:"app_preconditions,omitempty"`
	// StepUpRequired (F206): an in-place restore of this container will demand
	// fresh proof of the password. Reported here so the dialog can say so BEFORE
	// the operator commits, rather than only when the request comes back asking.
	// Restoring as a copy is never gated, and the flag says nothing about it.
	StepUpRequired bool `json:"step_up_required,omitempty"`
	// SharedData (F119): other containers on the target node that mount the same
	// host directories this restore writes into. Advisory — restoring one half of
	// a pair that shares data is legitimate, but it should be a decision rather
	// than a surprise.
	SharedData []string `json:"shared_data,omitempty"`
	// PortConflicts (F144): host ports this container publishes that something
	// else on the target already holds. Advisory — the holder may be exactly what
	// this restore is about to replace.
	PortConflicts []string `json:"port_conflicts,omitempty"`
	// Certificates (F143): the TLS certificates travelling inside this backup, so
	// an operator can see whether the copy they are about to restore still holds
	// a valid one before they find out from a browser.
	Certificates []backup.CertRef `json:"certificates,omitempty"`
	// RestoreBlock (F174): a problem in the container's OWN configuration that
	// would stop this restore — a database whose environment cannot initialize an
	// empty data directory, which is what restoring from a dump requires it to
	// do. Manifest-derived, so it costs nothing and applies to a same-host
	// restore exactly as much as a cross-host one.
	RestoreBlock string `json:"restore_block,omitempty"`
	// AddressVars (F177): environment variables this container records an address
	// in. Names only — the manifest carries no values — and only for a restore
	// onto a different node, where they are the ones a move invalidates.
	AddressVars []string `json:"address_vars,omitempty"`
}

// handleRestoreReadiness checks, on demand, whether a backup's image can still be
// obtained for a restore (F11). A bundled image is always ready (no registry
// needed); otherwise it does a cheap, no-pull availability check on the node
// (local presence, then a registry manifest peek). Read-only and idempotent, so
// it is auth-gated but not CSRF-gated (GET).
func (s *Server) handleRestoreReadiness(w http.ResponseWriter, r *http.Request) {
	b, err := s.store.GetBackup(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	var m backup.Manifest
	_ = json.Unmarshal([]byte(b.ManifestJSON), &m)
	resp := restoreReadinessResp{Image: m.Image, ImageDigest: m.ImageDigest, HasImageTar: m.ImageTar != nil}

	// F94: cross-host portability. Computed before the image check so it is
	// reported even for a bundled image, which returns early below.
	resp.Portability = s.portabilityFor(r.Context(), &m, b.NodeID, r.URL.Query().Get("target_node"))
	// F110: application preconditions. Purely manifest-derived — no node call —
	// so it costs nothing and is reported for same-host restores too, where an
	// edited compose file can just as easily have moved a mount.
	resp.AppPreconditions = appPreconditionsFor(&m)
	// F174: also manifest-derived, and for the same reason — the container's
	// configuration is what decides this, not the host it lands on.
	resp.RestoreBlock = backup.DBInitBlock(&m)
	// F177: only when this is actually a move. On the origin node the recorded
	// address is still right, and a warning shown every time is one nobody reads
	// by the time it matters.
	if tn := r.URL.Query().Get("target_node"); tn != "" && tn != b.NodeID {
		resp.AddressVars = backup.AddressEnvKeys(&m)
	}
	// F119: what else on the target node uses the directories this restore
	// writes into. Cached inventory only — no Docker call, no cost to opening
	// the dialog.
	sharedNode := r.URL.Query().Get("target_node")
	if sharedNode == "" {
		sharedNode = b.NodeID
	}
	resp.SharedData = s.sharedDataWarnings(sharedNode, &m, b.TargetName)
	// F144: what on the target already holds the ports this container publishes.
	// Same cached inventory, same absence of cost.
	resp.PortConflicts = s.portConflicts(sharedNode, &m, b.TargetName)
	// F143: manifest-derived, so it costs nothing and applies to every restore.
	resp.Certificates = m.Certificates
	// F206: resolved against the node this restore would land on — a container
	// marked critical on one node says nothing about a same-named container
	// somewhere else, and the gate itself resolves it the same way.
	resp.StepUpRequired = s.restoreStepUpRequired(sharedNode, b.TargetName)

	// A bundled image restores with no registry at all — always ready, no node call.
	if resp.HasImageTar {
		resp.ImageOK = true
		resp.Detail = "The container image is bundled in this backup (image.tar) — it restores fully offline."
		writeJSON(w, http.StatusOK, resp)
		return
	}
	// Otherwise verify the image can still be obtained on the node the restore
	// lands on (step 27): a target that cannot reach the registry, through its
	// egress rules for instance, is the one that matters, not the origin.
	cli, err := s.reg.Get(sharedNode)
	if err != nil {
		resp.Detail = "Node is unreachable, so image availability can't be verified now: " + err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	ok, detail := dockercli.ImagePullable(ctx, cli, m.ImageDigest, m.Image)
	resp.ImageOK = ok
	if ok {
		resp.Detail = detail
	} else {
		resp.Detail = detail + " " + imageBundleAdvice
	}
	// F95: what the image that will ACTUALLY run expects. Only meaningful when
	// the restore will not use the exact recorded digest — a digest-pinned
	// restore runs the identical image and cannot drift.
	resp.ImageDrift = s.imageDriftFor(ctx, cli, &m)
	writeJSON(w, http.StatusOK, resp)
}

// handleDeleteBackup removes a backup record and its artifacts.
// deleteBackupByID removes a single backup's artifacts from EVERY location
// (local + destinations) and its catalog row, then audits it. Shared by the
// single and bulk delete handlers so behaviour is identical: an immutable/WORM
// offsite copy is enforced by the destination and survives per §9.1, and every
// deletion is individually recorded in the append-only audit trail. Returns
// errNotFound when the id doesn't exist.
func (s *Server) deleteBackupByID(ctx context.Context, actor, id string) error {
	b, err := s.store.GetBackup(id)
	if err != nil {
		return errNotFound
	}
	// Chain guard (F63): deleting the ancestor of a live incremental backup
	// would strand every descendant ("backup chain is broken" on restore).
	// Refuse with the dependent ids; cascade paths delete newest-first, so by
	// the time each ancestor reaches this guard its children are already gone.
	// Fail CLOSED: if the parent map can't be read, refuse the delete rather
	// than risk orphaning a chain.
	pm, perr := s.store.BackupParentMap(b.NodeID, b.TargetName)
	if perr != nil {
		return fmt.Errorf("cannot verify incremental-chain dependents: %w", perr)
	}
	if deps := backup.ChainDescendants(id, pm); len(deps) > 0 {
		return &errHasDependents{ids: deps}
	}
	s.engine.DeleteArtifacts(ctx, b) // best-effort per location; WORM copies stay locked
	if err := s.store.DeleteBackup(id); err != nil {
		return err
	}
	_ = s.store.Audit(actor, "backup.delete", id, b.TargetName)
	return nil
}

var errNotFound = errors.New("not found")

// errHasDependents (F63): the target is the chain ancestor of live incremental
// backups — deleting it would make them unrestorable.
type errHasDependents struct{ ids []string }

func (e *errHasDependents) Error() string {
	return fmt.Sprintf("is the baseline of %d newer incremental backup(s)", len(e.ids))
}

// deleteChainCascade deletes id AND its whole descendant subtree, leaves-first
// (F63): repeated passes over the remaining set delete every row whose children
// are already gone, so no intermediate orphan state ever exists. Bounded by the
// chain-depth cap. Returns the ids actually deleted, in deletion order.
func (s *Server) deleteChainCascade(actor, id string, deps []string) ([]string, error) {
	remaining := append(append([]string{}, deps...), id)
	var deleted []string
	for pass := 0; pass < backup.IncrementalFullEveryMax+2 && len(remaining) > 0; pass++ {
		var next []string
		progress := false
		for _, did := range remaining {
			dctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			err := s.deleteBackupByID(dctx, actor, did)
			cancel()
			switch {
			case err == nil, errors.Is(err, errNotFound):
				deleted = append(deleted, did)
				progress = true
			default:
				var dep *errHasDependents
				if errors.As(err, &dep) {
					next = append(next, did) // children still pending — later pass
					continue
				}
				return deleted, err
			}
		}
		remaining = next
		if !progress {
			break // defensive: no pass progress means a cycle/corruption — stop
		}
	}
	if len(remaining) > 0 {
		return deleted, fmt.Errorf("could not delete %d chain member(s): %v", len(remaining), remaining)
	}
	return deleted, nil
}

func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Optional body {"cascade":true} (F63): delete the backup AND every
	// incremental descendant in one audited operation.
	var req struct {
		Cascade bool `json:"cascade"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(r, &req); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid request")
			return
		}
	}
	dctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	actor := userFrom(r)
	err := s.deleteBackupByID(dctx, actor, id)
	var dep *errHasDependents
	if errors.As(err, &dep) {
		if !req.Cascade {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":      "this backup " + dep.Error() + " — deleting it would make them unrestorable",
				"dependents": dep.ids,
			})
			return
		}
		deleted, cerr := s.deleteChainCascade(actor, id, dep.ids)
		if cerr != nil {
			errJSON(w, http.StatusInternalServerError, cerr.Error())
			return
		}
		_ = s.store.Audit(actor, "backup.delete.cascade", id, fmt.Sprintf("chain of %d backup(s) deleted", len(deleted)))
		writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "cascade": true, "deleted": deleted})
		return
	}
	if err != nil {
		if errors.Is(err, errNotFound) {
			errJSON(w, http.StatusNotFound, "not found")
			return
		}
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleSetBackupPin pins/unpins a backup as "keep forever" — a pinned backup is
// exempt from all retention pruning (F2). Auth + CSRF gated + audited.
func (s *Server) handleSetBackupPin(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := s.store.GetBackup(id)
	if errors.Is(err, store.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req struct {
		Pinned bool `json:"pinned"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := s.store.SetBackupPin(id, req.Pinned); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "backup.pin", id, fmt.Sprintf("%s pinned=%v", b.TargetName, req.Pinned))
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "pinned": req.Pinned})
}

// handleSetBackupLabel sets/clears a backup's free-text label (F2). Auth + CSRF
// gated + audited. The label is length-capped so it can't bloat the row/UI.
func (s *Server) handleSetBackupLabel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetBackup(id); errors.Is(err, store.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "not found")
		return
	} else if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req struct {
		Label string `json:"label"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	label := strings.TrimSpace(req.Label)
	if len(label) > 200 {
		label = label[:200]
	}
	if err := s.store.SetBackupLabel(id, label); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "backup.label", id, label)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "label": label})
}

// handleBulkDeleteBackup deletes many backups in one request. Each id runs
// through the same audited, WORM-respecting path as a single delete, with its
// own timeout and a detached context so a client disconnect can't leave the set
// half-deleted. Failures are collected per id rather than aborting the batch.
func (s *Server) handleBulkDeleteBackup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	if len(req.IDs) == 0 {
		errJSON(w, http.StatusBadRequest, "no backup ids provided")
		return
	}
	if len(req.IDs) > 1000 {
		errJSON(w, http.StatusBadRequest, "too many ids in one request (max 1000)")
		return
	}
	actor := userFrom(r)
	type failItem struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	deleted := 0
	failed := []failItem{}
	// Chain-aware ordering (F63): repeated passes let descendants in THIS
	// request delete before their ancestors regardless of the ids' order. An
	// id still blocked at the end has dependents OUTSIDE the request — that's
	// a genuine per-id failure (the caller must include or cascade them).
	remaining := req.IDs
	for pass := 0; pass < backup.IncrementalFullEveryMax+2 && len(remaining) > 0; pass++ {
		var next []string
		progress := false
		for _, id := range remaining {
			dctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			err := s.deleteBackupByID(dctx, actor, id)
			cancel()
			if err == nil {
				deleted++
				progress = true
				continue
			}
			var dep *errHasDependents
			if errors.As(err, &dep) {
				next = append(next, id)
				continue
			}
			failed = append(failed, failItem{ID: id, Error: err.Error()})
		}
		remaining = next
		if !progress {
			break
		}
	}
	for _, id := range remaining {
		// Dependents outside the request set — refuse, never orphan.
		pmMsg := "is the baseline of newer incremental backups — include them in the deletion or use the chain delete"
		failed = append(failed, failItem{ID: id, Error: pmMsg})
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted, "failed": failed})
}

type restoreReq struct {
	// F206: an in-place restore of a PROTECTED container demands fresh proof of
	// the password before it wipes anything. The credentials ride in the same
	// body, exactly as they do for an export grant or a token mint.
	stepUpBody
	NodeID   string `json:"node_id"`
	TargetID string `json:"target_id"`
	Volumes  bool   `json:"volumes"`
	Database bool   `json:"database"`
	Confirm  bool   `json:"confirm"`
	Snapshot bool   `json:"snapshot"` // snapshot current state before overwrite (PLAN §3.7)
	Recreate bool   `json:"recreate"` // "Revert update": recreate the container from the backup's image digest, rolling a bad upgrade back to this backup's version
	Source   string `json:"source"`   // "" auto | "local" | destination ID
	// ConfirmIncompatible overrides the restore-time engine/extension compatibility
	// gate (F10): set only after the user explicitly acknowledges a cross-family
	// mismatch (e.g. a pgvecto.rs dump into a VectorChord image).
	ConfirmIncompatible bool `json:"confirm_incompatible"`
	// ConfirmUnverified overrides the known-bad gate (F218): set only after the
	// user acknowledges that this backup's LAST VERIFICATION FAILED. Never set by
	// default, and never inferred — the whole point is that it is a decision.
	ConfirmUnverified bool `json:"confirm_unverified"`
	// AllowDifferentImage lets a recreate run the tag's current image when the
	// image the backup ran is gone (step 22). Set only after the operator chose
	// it: a newer version on older data can migrate it beyond going back.
	AllowDifferentImage bool `json:"allow_different_image"`
	// ConfirmMissingNetworks lets a cross-host restore create, as plain
	// bridges, shared networks the target does not have (step 27).
	ConfirmMissingNetworks bool `json:"confirm_missing_networks"`
	// ConfirmMissingDevices lets a cross-host restore proceed although the
	// target lacks a device the container needs (step 27).
	ConfirmMissingDevices bool `json:"confirm_missing_devices"`
	// FilesOnly puts back only the files a stack keeps on the host — compose,
	// .env, project files, missing single-file binds — with no container
	// stopped or changed and nothing that exists overwritten (step 23).
	FilesOnly bool `json:"files_only"`
	// PrivateKey is the offline X25519 key for a WRITE-ONLY backup (F86). It is
	// SECRET: used for this one restore, held in memory, never persisted and never
	// written to the audit trail or a log line — the audit records only that a key
	// was supplied.
	PrivateKey string `json:"private_key,omitempty"`
	// AsName + Isolated request "restore as a copy" (F10): bring the backup up as a
	// NEW, isolated container of this name instead of overwriting the original.
	AsName   string `json:"as_name"`
	Isolated bool   `json:"isolated"`
	// TestClone (F219) is the one-click version of the two above: the server picks
	// the name, forces isolation, and stamps the clone with an expiry so it is
	// removed automatically. Set INSTEAD of as_name/isolated, not alongside.
	TestClone bool `json:"test_clone"`
	// ReconstructHost rebuilds the on-host stack directory + compose file when the
	// container is recreated (disaster recovery onto a fresh machine). Opt-in.
	// HostBaseDir is the fallback base dir for a non-compose container's folder; when
	// empty the last-used value (setting restore.host_base_dir) is reused, and a
	// provided value is remembered for next time.
	ReconstructHost bool `json:"reconstruct_host"`
	// PromoteRestartPolicy sets a policy that will not survive a reboot to
	// `unless-stopped` (#8). Opt-in: the capture-time finding is the default
	// behaviour, because on a host whose package manager starts its own stacks
	// the recorded policy is correct and rewriting it unasked is a deviation.
	PromoteRestartPolicy bool `json:"promote_restart_policy"`
	// InjectHealthchecks adds a readiness probe to a recreated database container
	// that has none (#16). Opt-in, and never over an existing probe.
	InjectHealthchecks bool   `json:"inject_healthchecks"`
	HostBaseDir        string `json:"host_base_dir"`
	// RemapIP rewrites a source machine IP to a target machine IP in the recreated
	// config + compose (cross-host restore). RemapFromIP/RemapToIP override the
	// auto-derived origin/target node IPs; blank fields fall back to those.
	RemapIP     bool   `json:"remap_ip"`
	RemapFromIP string `json:"remap_from_ip"`
	RemapToIP   string `json:"remap_to_ip"`
	// RemapPath (F81) moves bind-mount sources, the reconstructed compose, and
	// the stack folder from the source machine's base directory to the target's.
	// A blank from-base falls back to the backup's recorded compose parent; a
	// blank to-base to the remembered reconstruction base.
	// RemapDomain (F195) rewrites the source machine's DOMAIN to the target's in
	// every recreated environment value that carries it — the address mechanism
	// for containers without a profile. Both fields are required when enabled;
	// there is no meaningful default for either.
	RemapDomain     bool   `json:"remap_domain"`
	RemapFromDomain string `json:"remap_from_domain"`
	RemapToDomain   string `json:"remap_to_domain"`
	RemapPath       bool   `json:"remap_path"`
	RemapFromPath   string `json:"remap_from_path"`
	RemapToPath     string `json:"remap_to_path"`

	// NewSiteAddress (F114) is the address the application will be reached at
	// after this restore. Blank — the normal case — means the address is not
	// changing and nothing about it is touched.
	NewSiteAddress string `json:"new_site_address"`

	// NewUpstreamAddress (F160) is the address of a service this application
	// DEPENDS ON, when that service is the thing that moved. Blank — the normal
	// case — means the dependency has not moved and nothing is touched.
	NewUpstreamAddress string `json:"new_upstream_address"`
}

// handleRestore restores volume/db state into a target container (PLAN §3.7/§4.8).
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req restoreReq
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	if !req.Confirm {
		errJSON(w, http.StatusBadRequest, "restore is destructive; set confirm=true")
		return
	}
	if req.NodeID == "" {
		errJSON(w, http.StatusBadRequest, "node_id is required")
		return
	}
	// The restore target node may differ from the backup's origin (cross-host
	// portability, PLAN §4.8) — validate it exists for a clear error.
	if _, err := s.store.GetNode(req.NodeID); err != nil {
		errJSON(w, http.StatusNotFound, "target node not found")
		return
	}
	// Fetch the backup up front — its manifest drives the compatibility gate and
	// its stack keys the exclusive lock below.
	b, berr := s.store.GetBackup(id)
	if berr != nil {
		errJSON(w, http.StatusNotFound, "backup not found")
		return
	}
	// F218: a backup whose LAST VERIFICATION FAILED is known-bad — it was re-read
	// and did not come back intact. Restoring it anyway is still the operator's
	// call (a damaged copy of yesterday can beat nothing at all, and the failure
	// may be in one copy rather than the data), but it has to be a DECISION.
	//
	// Merely-unverified is deliberately NOT gated: that is what this endpoint has
	// always allowed, an adopted backup is unverified by construction, and the UI
	// now offers to verify first rather than refusing. Only a recorded failure is
	// refused, and only until it is acknowledged.
	//
	// Checked before the volume dispatch below, because a standalone volume's
	// archive rots exactly like a container's, and before the lock, because a
	// refused restore must hold nothing.
	if b.Verified == "failed" && !req.ConfirmUnverified {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":         "this backup's last verification FAILED — it did not re-read intact. Restoring it can restore corrupt data; confirm explicitly to proceed.",
			"verify_failed": true,
		})
		return
	}
	// Step 23: files-only puts files on the host and changes nothing else, so
	// it has no copy, no revert and no standalone-volume form. It restores no
	// volume and no database, which keeps it clear of the gates that guard them.
	if req.FilesOnly {
		if req.AsName != "" || req.TestClone || req.Recreate || strings.HasPrefix(b.TargetName, "volume:") {
			errJSON(w, http.StatusBadRequest, "a files-only restore puts back the stack's files and changes no container, so it cannot be a copy, a revert, or a standalone volume")
			return
		}
		req.Volumes, req.Database = false, false
		req.ReconstructHost = true // the stack folder is the point; its remembered base directory applies
	}
	// F23: a standalone-volume backup restores by recreating the named volume — no
	// container target, so skip the container-specific validation and gates below.
	if strings.HasPrefix(b.TargetName, "volume:") {
		s.restoreVolumeBackup(w, r, b, req)
		return
	}
	// Container restores need a target container.
	if req.TargetID == "" {
		errJSON(w, http.StatusBadRequest, "target_id is required")
		return
	}

	// F219: "Test this backup" is a clone whose name and lifetime the SERVER
	// decides. Resolved here, before the clone validation below, so from this
	// point on it is an ordinary restore-as-a-copy and every gate, lock, audit
	// line and engine option already written for one applies to it unchanged.
	testCloneTTL := 0
	if req.TestClone {
		if req.AsName != "" {
			errJSON(w, http.StatusBadRequest, "test_clone picks its own name — do not send as_name with it")
			return
		}
		name, nerr := s.testCloneName(r.Context(), req.NodeID, b.TargetName)
		if nerr != nil {
			errJSON(w, http.StatusConflict, nerr.Error())
			return
		}
		req.AsName = name
		req.Isolated = true // never in place: this is a test, and it proves nothing if it destroys the original
		testCloneTTL = s.testCloneTTLHours()
	}
	// Restore as a copy (F10): validate the clone name. It must be a valid Docker
	// container name AND must differ from the original — cloning into the original's
	// name would overwrite it, defeating the whole point (and risking data loss).
	clone := req.AsName != ""
	if clone {
		if !validContainerName(req.AsName) {
			errJSON(w, http.StatusBadRequest, "invalid new container name — use letters, digits, and _ . -")
			return
		}
		if req.AsName == b.TargetName {
			errJSON(w, http.StatusBadRequest, "the copy's name must differ from the original container name")
			return
		}
	}

	// Restore-time engine/extension compatibility gate (F10): a database dump that
	// needs a specific vector-extension family (e.g. pgvecto.rs `vectors`) restored
	// into a different-family image (VectorChord, or plain Postgres) fails on import
	// or silently corrupts vector search. Warn and require an explicit override
	// instead of proceeding blindly. Only relevant when restoring the database, and
	// checked BEFORE the lock so a refused restore holds nothing. A CLONE is exempt:
	// it recreates from the backup's OWN image, so it can never be a cross-family
	// mismatch.
	if req.Database && !req.ConfirmIncompatible && !clone {
		var man backup.Manifest
		if b.ManifestJSON != "" {
			_ = json.Unmarshal([]byte(b.ManifestJSON), &man)
		}
		if len(man.Databases) > 0 {
			// The target's image is the current container's (an in-place restore may
			// hit a container whose image was upgraded to a different family); for a
			// recreate/disaster-recovery target that doesn't exist yet, fall back to
			// the backup's own image, which is the same family by construction.
			targetImage := man.Image
			targetVersion := ""
			var targetExts []string // F41: nil = couldn't probe → fall back to the image-name guess
			if cli, cerr := s.reg.Get(req.NodeID); cerr == nil {
				ictx, icancel := context.WithTimeout(r.Context(), 12*time.Second)
				insp, ierr := cli.ContainerInspect(ictx, req.TargetID)
				icancel()
				var env []string
				running := ierr == nil && insp.State != nil && insp.State.Running
				if ierr == nil && insp.Config != nil {
					if insp.Config.Image != "" {
						targetImage = insp.Config.Image
					}
					env = insp.Config.Env
				}
				// F36: probe the target's LIVE engine version so a version downgrade (a
				// newer dump into an older engine) is caught before the destructive
				// re-init. Best-effort: a stopped/unprobeable target leaves it empty, so
				// the gate simply skips the version check (never a false block).
				if cmd := backup.DBVersionCmd(backup.DBEngine(targetImage)); cmd != nil {
					vctx, vcancel := context.WithTimeout(r.Context(), 10*time.Second)
					if out, verr := dockercli.ExecCapture(vctx, cli, req.TargetID, cmd); verr == nil {
						targetVersion, _, _ = strings.Cut(strings.TrimSpace(string(out)), "\n")
					}
					vcancel()
				}
				// F41: probe the target's REAL installed extensions (Postgres, running
				// only) so the vector-family check compares measured-to-measured instead
				// of guessing from the image name. Best-effort: on any failure targetExts
				// stays nil and the gate falls back to the name guess.
				if running && backup.DBEngine(targetImage) == "postgres" {
					ectx, ecancel := context.WithTimeout(r.Context(), 10*time.Second)
					if out, eerr := dockercli.ExecCapture(ectx, cli, req.TargetID, backup.PGExtensionsCmd(env)); eerr == nil {
						targetExts = backup.ParsePGExtensions(string(out))
					}
					ecancel()
				}
			}
			if warnings, blocking := backup.RestoreCompatibility(&man, targetImage, targetVersion, targetExts); blocking {
				exts := []string{}
				for _, d := range man.Databases {
					exts = append(exts, d.Extensions...)
				}
				writeJSON(w, http.StatusConflict, map[string]any{
					"error":          "restore blocked: database engine, version, or extension incompatible with the target image",
					"compat_warning": strings.Join(warnings, " "),
					"extensions":     exts,
				})
				return
			}
		}
	}

	// F114: the operator is moving the app to a different address. Validated HERE
	// — before the lock, before anything is stopped — because a malformed address
	// must cost nothing, and because this value ends up in a trust list and in
	// commands run inside the container. Blank is the normal case and means the
	// address is not changing.
	newSiteAddress := strings.TrimSpace(req.NewSiteAddress)
	if newSiteAddress != "" {
		if err := backup.ValidSiteAddress(newSiteAddress); err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	// F160: and the same up front for a dependency's new address, which is
	// written into a configuration file inside the container and so is held to
	// exactly the same shape.
	newUpstreamAddress := strings.TrimSpace(req.NewUpstreamAddress)
	if newUpstreamAddress != "" {
		if err := backup.ValidSiteAddress(newUpstreamAddress); err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// Application restore gate (F108 + F110). The database gate above only ever
	// fires for a database CONTAINER — it reads man.Databases, which an app with
	// an embedded SQLite database never populates. So an application reached the
	// restore path with no compatibility check at all, and the two ways it breaks
	// are both silent:
	//
	//   - restored into an OLDER image of an app whose schema migrates one way,
	//     which fails at start AFTER the data has been written; and
	//   - restored onto a network filesystem, where the embedded database keeps
	//     working for days before it corrupts.
	//
	// Skipped for a CLONE (recreated from the backup's own image onto fresh
	// anonymous volumes — neither failure is reachable) and for a RECREATE (which
	// restores the recorded digest, so the version is the backup's by
	// construction). Checked BEFORE the lock, so a refusal holds nothing.
	if req.Volumes && !req.ConfirmIncompatible && !clone && !req.Recreate {
		var man backup.Manifest
		if b.ManifestJSON != "" {
			_ = json.Unmarshal([]byte(b.ManifestJSON), &man)
		}
		if profile := backup.ProfileFor(man.Image); profile != nil {
			var warnings []string
			blocking := false

			// F108: what version will actually be running after this restore?
			// Best-effort — an image with no version label yields no verdict,
			// because a gate that guessed would block valid restores far more
			// often than it caught a real downgrade.
			backupVersion := ""
			if man.ImageConfig != nil {
				backupVersion = man.ImageConfig.Version
			}
			if cli, cerr := s.reg.Get(req.NodeID); cerr == nil && backupVersion != "" {
				ictx, icancel := context.WithTimeout(r.Context(), 12*time.Second)
				insp, ierr := cli.ContainerInspect(ictx, req.TargetID)
				icancel()
				if ierr == nil && insp.Config != nil && insp.Config.Image != "" {
					vctx, vcancel := context.WithTimeout(r.Context(), 15*time.Second)
					if cfg, verr := dockercli.InspectImageConfig(vctx, cli, insp.Config.Image); verr == nil {
						// Only a BLOCKING verdict is collected here. The
						// forward-migration warning is already shown as a standing
						// precondition in the restore dialog (F110), so repeating it
						// as a confirm-to-continue prompt would train the operator to
						// click through the one prompt that also carries the block.
						if v := backup.AppVersionCompatibility(profile, backupVersion, cfg.Version); v.Blocking {
							warnings = append(warnings, v.Warning)
							blocking = true
						}
					}
					vcancel()
				}
			}

			// F110: is a path that must be on local disk actually on a network
			// share on this target? Only a MEASURED network filesystem blocks.
			if fsWarn, fsBlock := localOnlyStorageVerdict(&man, s.targetMountFSTypes(r.Context(), req.NodeID, req.TargetID)); len(fsWarn) > 0 {
				warnings = append(warnings, fsWarn...)
				blocking = blocking || fsBlock
			}

			// #11: does this target still hold the key the backup's data was
			// encrypted under? A volumes-only restore does not change the
			// container's environment, so data restored under a different APP_KEY
			// arrives permanently unreadable — with the application starting and
			// logging in normally, which is what makes it worth stopping for.
			//
			// Values are compared inside changedIrreplaceableSecrets and never
			// leave it; only the KEY NAMES reach this warning.
			if changed := s.changedIrreplaceableSecrets(r.Context(), b, &man, profile, req.NodeID, req.TargetID); len(changed) > 0 {
				warnings = append(warnings, backup.ChangedSecretWarning(profile.Name, changed))
				blocking = true
			}

			if blocking {
				writeJSON(w, http.StatusConflict, map[string]any{
					"error":          "restore blocked: the target cannot safely run this application's data",
					"compat_warning": strings.Join(warnings, " "),
				})
				return
			}
		}
	}

	// F86: a write-only backup needs its offline private key. Checked HERE — before
	// the lock, before anything is stopped or snapshotted — so a missing or wrong
	// key costs nothing and leaves the running container untouched. The check
	// compares public-key fingerprints only; it never decrypts.
	if wm := manifestOf(b); backup.IsWriteOnly(wm) {
		if err := backup.CheckPrivateKey(wm, req.PrivateKey); err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// F206: the last gate before anything is taken or touched — fresh proof of the
	// password to OVERWRITE a protected container.
	//
	// Placed here, after every check that can refuse for free, for the same reason
	// the export grant checks the backup exists before prompting: asking someone to
	// re-authenticate for a restore that is about to be blocked as incompatible
	// wastes their time and teaches them the prompt is noise.
	//
	// "In place" is AsName == "" — the same test the lock below uses to decide
	// whether the ORIGINAL stack is at risk, so the two can never disagree about
	// what is destructive. A request that sets isolated without a name still
	// overwrites the original, and is treated as in-place accordingly.
	// Step 27: a device the target verifiably lacks stops the restore before
	// anything is written, unless the operator says it will be there.
	if !req.FilesOnly && !req.ConfirmMissingDevices {
		if missing := s.missingDevicesOn(r.Context(), manifestOf(b), b.NodeID, req.NodeID); len(missing) > 0 {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":           fmt.Sprintf("the target machine has no %s, which this container needs; it would be created and then fail to start. Attach the device, or confirm that it will be there.", strings.Join(missing, ", ")),
				"missing_devices": missing,
			})
			return
		}
	}
	// Step 27: a shared network the target lacks would be invented as a plain
	// bridge and leave the container unreachable. Stop until it exists there, or
	// the operator accepts the bridge.
	if !req.FilesOnly && !req.ConfirmMissingNetworks {
		if missing := s.sharedNetworksMissingOn(r.Context(), manifestOf(b), b.Stack, b.NodeID, req.NodeID); len(missing) > 0 {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":            fmt.Sprintf("the target machine has no %s network, which this container joins but its stack does not own (a proxy network, say). DockBack would create it as a plain bridge, cutting the container off from what it reaches through it. Create it on the target as it should be, or confirm the plain bridge.", strings.Join(missing, ", ")),
				"missing_networks": missing,
			})
			return
		}
	}
	// Files-only overwrites nothing, so it needs no fresh proof of the password.
	if !clone && !req.FilesOnly && s.restoreStepUpRequired(req.NodeID, b.TargetName) {
		if !s.requireFreshAuth(w, r, req.stepUpBody) {
			return
		}
	}

	// Exclusive lock (PLAN §9.10): a normal restore takes the ORIGINAL stack
	// exclusively (it's destructive). A CLONE touches nothing on the original, so it
	// keys the lock by the CLONE's own name instead — serializing concurrent clones
	// into the same name without blocking (or being blocked by) the original's
	// backups/restores.
	lockKey := stackKey(req.NodeID, b.Stack, b.TargetName)
	if clone {
		lockKey = stackKey(req.NodeID, "", req.AsName)
	}
	if !s.locks.acquireRestore(lockKey) {
		errJSON(w, http.StatusConflict, "a backup or restore of this stack is already in progress — try again once it finishes")
		return
	}
	// Every refusal between here and the goroutine below has to give the lock
	// back. opLocks has no expiry, so one leaked exclusive lock blocks every
	// later backup AND restore of this stack until DockBack restarts — one typo
	// in the restore dialog was enough to do it.
	lockHeld := true
	defer func() {
		if lockHeld {
			s.releaseRestoreAndDispatch(lockKey)
		}
	}()
	action := "restore.start"
	if clone {
		action = "restore.clone"
	}
	if req.TestClone {
		action = "restore.test_clone"
	}
	// Host-side stack reconstruction (opt-in DR): remember/reuse the base directory
	// used for a non-compose container's folder so the user needn't retype it.
	baseDir := strings.TrimSpace(req.HostBaseDir)
	if req.ReconstructHost {
		if baseDir != "" {
			if !strings.HasPrefix(baseDir, "/") {
				errJSON(w, http.StatusBadRequest, "host base directory must be an absolute path")
				return
			}
			_ = s.store.SetSetting("restore.host_base_dir", baseDir)
		} else {
			baseDir, _ = s.store.GetSetting("restore.host_base_dir", "")
		}
	}
	// Host-IP remap (opt-in, cross-host): resolve the source/target machine IPs,
	// defaulting to the origin/target node addresses when a field is left blank.
	var remapFrom, remapTo string
	if req.RemapIP {
		// F197: a blank field is derived from the node's address — including a
		// node registered by HOSTNAME, which is resolved. The machine's IP is
		// known either way; leaving the operator to type it was the gap.
		remapFrom = strings.TrimSpace(req.RemapFromIP)
		if remapFrom == "" {
			if origin, oerr := s.store.GetNode(b.NodeID); oerr == nil {
				remapFrom = dockercli.ResolveHostIP(r.Context(), origin.Address)
			}
		}
		remapTo = strings.TrimSpace(req.RemapToIP)
		if remapTo == "" {
			if target, terr := s.store.GetNode(req.NodeID); terr == nil {
				remapTo = dockercli.ResolveHostIP(r.Context(), target.Address)
			}
		}
		if (remapFrom != "" && net.ParseIP(remapFrom) == nil) || (remapTo != "" && net.ParseIP(remapTo) == nil) {
			errJSON(w, http.StatusBadRequest, "IP remap needs valid IPv4/IPv6 addresses")
			return
		}
		// The remap was ASKED for. An endpoint that cannot be derived must be a
		// refusal here, where the operator can fill the field — the engine
		// treats an empty endpoint as "no remap", which turned a ticked checkbox
		// into a silent no-op.
		if remapFrom == "" || remapTo == "" {
			errJSON(w, http.StatusBadRequest, "IP remap: could not derive the source or target machine IP from the node addresses — fill both fields in the dialog")
			return
		}
	}
	// Domain remap (F195, opt-in): both endpoints are required and validated to
	// the plain-hostname grammar — these values end up inside a regexp applied
	// to every environment value the container carries.
	var remapFromDomain, remapToDomain string
	if req.RemapDomain {
		remapFromDomain = strings.TrimSpace(req.RemapFromDomain)
		remapToDomain = strings.TrimSpace(req.RemapToDomain)
		if err := dockercli.ValidRemapDomain(remapFromDomain); err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := dockercli.ValidRemapDomain(remapToDomain); err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	// Host-path remap (F81, opt-in): blank fields default to this backup's
	// recorded compose parent / the remembered reconstruction base; the target
	// base is validated fail-closed against the protected system roots.
	var remapFromPath, remapToPath string
	if req.RemapPath {
		remapFromPath = strings.TrimSpace(req.RemapFromPath)
		if remapFromPath == "" && b.ManifestJSON != "" {
			var man backup.Manifest
			if json.Unmarshal([]byte(b.ManifestJSON), &man) == nil && man.StackWorkingDir != "" {
				remapFromPath = filepath.Dir(man.StackWorkingDir)
			}
		}
		remapToPath = strings.TrimSpace(req.RemapToPath)
		if remapToPath == "" {
			remapToPath, _ = s.store.GetSetting("restore.host_base_dir", "")
		}
		var verr error
		remapFromPath, remapToPath, verr = validateRemapPathBases(remapFromPath, remapToPath)
		if verr != nil {
			errJSON(w, http.StatusBadRequest, verr.Error())
			return
		}
	}
	// F86: record THAT an offline key was supplied, never the key itself. The
	// audit trail is readable by any admin and is exported in app backups, so the
	// one place a write-only private key must never reach is this line.
	detail := "node=" + req.NodeID + " target=" + req.TargetID
	if req.PrivateKey != "" {
		detail += " (write-only key supplied)"
	}
	// F206: record that the overwrite guard was satisfied, so the trail
	// distinguishes a protected container's restore from an ordinary one.
	if !clone && s.restoreStepUpRequired(req.NodeID, b.TargetName) {
		detail += " (protected: step-up ok)"
	}
	// F218: restoring over a FAILED verification is a deliberate override of a
	// refusal. The trail has to say so — "why is this container full of corrupt
	// data" is answered here or nowhere.
	if req.ConfirmUnverified && b.Verified == "failed" {
		detail += " (verification FAILED — override confirmed)"
	}
	// F215: remember the cross-host choices for this route too. Keyed by the
	// PROJECT when this container belongs to one, so a choice made while
	// restoring a single member is offered when the whole stack is restored next
	// — the drawer and the stack dialog are two doors onto the same decision, and
	// remembering it in only one of them is how the operator ends up retyping it.
	memKey := b.Stack
	if memKey == "" {
		memKey = b.TargetName
	}
	s.rememberCrossRestore(memKey, req.NodeID, crossRestoreMemory{
		RemapIP: req.RemapIP, RemapFromIP: remapFrom, RemapToIP: remapTo,
		RemapDomain: req.RemapDomain, RemapFromDomain: strings.TrimSpace(req.RemapFromDomain), RemapToDomain: strings.TrimSpace(req.RemapToDomain),
		RemapPath: req.RemapPath, RemapFromPath: strings.TrimSpace(req.RemapFromPath), RemapToPath: strings.TrimSpace(req.RemapToPath),
		ReconstructHost: req.ReconstructHost, HostBaseDir: baseDir,
		NewSiteAddress: newSiteAddress, NewUpstreamAddress: newUpstreamAddress,
	})
	_ = s.store.Audit(userFrom(r), action, id, detail)
	// Handed over: the run owns the lock from here and releases it when it ends.
	lockHeld = false
	go func() {
		defer guardPanic("restore", id, func() { s.logSink(id, "ERR", "Restore failed: internal error (panic)") })
		defer s.releaseRestoreAndDispatch(lockKey)
		// Registered as cancelable under the backup id — the same id the UI
		// streams this restore's log on (see handleCancelRestore).
		ctx, finish, ok := s.beginRestoreRun(context.Background(), id, b.TargetName, req.NodeID, 2*time.Hour)
		if !ok {
			s.logSink(id, "ERR", "Restore failed: another restore of this backup is already running")
			return
		}
		defer finish()
		err := s.engine.Restore(ctx, backup.RestoreOptions{
			BackupID: id, NodeID: req.NodeID, TargetID: req.TargetID,
			Volumes: req.Volumes, Database: req.Database, Source: req.Source,
			Snapshot: req.Snapshot, Recreate: req.Recreate, PromoteRestartPolicy: req.PromoteRestartPolicy,
			AllowDifferentImage: req.AllowDifferentImage,
			FilesOnly:           req.FilesOnly,
			InjectHealthchecks:  req.InjectHealthchecks,
			AsName:              req.AsName, Isolated: req.Isolated,
			TestCloneTTL:    time.Duration(testCloneTTL) * time.Hour,
			ReconstructHost: req.ReconstructHost, HostBaseDir: baseDir,
			RemapFromIP: remapFrom, RemapToIP: remapTo,
			RemapFromDomain: remapFromDomain, RemapToDomain: remapToDomain, // F195
			RemapFromPath: remapFromPath, RemapToPath: remapToPath,
			NewSiteAddress:     newSiteAddress,     // F114 — validated up front; blank = address unchanged
			NewUpstreamAddress: newUpstreamAddress, // F160 — a DEPENDENCY's new address; blank = it has not moved
			PrivateKey:         req.PrivateKey,     // F86 — in-memory for this restore only
		})
		s.restoreOutcome(id, err, "Restore completed")
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

// validContainerName reports whether s is a syntactically valid Docker container
// name (first char alphanumeric, then letters/digits/_.-) — so a "restore as a
// copy" name (F10) can't collide with a daemon/shell edge or inject.
func validContainerName(s string) bool {
	if len(s) < 1 || len(s) > 100 {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		alnum := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')
		if i == 0 {
			if !alnum {
				return false
			}
			continue
		}
		if !alnum && ch != '_' && ch != '.' && ch != '-' {
			return false
		}
	}
	return true
}

// handleMirrorBackup re-runs ONLY the offsite mirror step for an existing local
// backup — reusing the stored encrypted archive so a DEGRADED backup's 3-2-1
// copies (or a local-only backup) can be pushed to destination(s) without a full
// re-capture. Runs async and logs under the backup id so the drawer's log stream
// shows progress. Optional body {destinations:[ids]} narrows the target set.
func (s *Server) handleMirrorBackup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := s.store.GetBackup(id)
	if err != nil {
		errJSON(w, http.StatusNotFound, "not found")
		return
	}
	if b.Status != "success" {
		errJSON(w, http.StatusBadRequest, "only a completed backup can be mirrored")
		return
	}
	var req struct {
		Destinations []string `json:"destinations"`
	}
	// The body is optional — absent means "every enabled destination that needs a
	// copy" — but MALFORMED is not the same as absent. Discarding the error left
	// Destinations nil, so a request naming two destinations and getting the JSON
	// wrong silently mirrored to all of them instead.
	if err := readJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	// One mirror per backup at a time.
	if _, busy := s.mirrorInFlight.LoadOrStore(id, struct{}{}); busy {
		errJSON(w, http.StatusConflict, "this backup is already being mirrored")
		return
	}
	_ = s.store.Audit(userFrom(r), "backup.mirror", id, b.TargetName)
	go func() {
		defer s.mirrorInFlight.Delete(id)
		defer guardPanic("mirror", id, func() { s.logSink(id, "ERR", "Mirror failed: internal error (panic)") })
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		if err := s.engine.MirrorExisting(ctx, id, req.Destinations); err != nil {
			s.logSink(id, "ERR", "Mirror failed: "+err.Error())
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "mirroring"})
}

// handleDownload streams the decrypted, decompressed tar archive (PLAN §9.3/§9.14).
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// F199: a one-shot, step-up-issued ticket, redeemed before a single byte is
	// decrypted. This handler streams the archive in PLAINTEXT — every secret the
	// container held — which made it the most dangerous GET in the app and, until
	// now, the least protected one.
	if !s.requireExportTicket(w, r, exportPurposeDownload, id) {
		return
	}
	_ = s.store.Audit(userFrom(r), "export.download", id, "decrypted archive streamed to the browser")
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "dockback-"+id+".tar"))
	if err := s.engine.DecryptTo(r.Context(), id, w); err != nil {
		// Headers may already be sent; log and stop.
		s.logSink(id, "ERR", "Download failed: "+err.Error())
	}
}

// handleBackupEntries lists the files inside a backup's volume payload so the UI
// can browse them without downloading the whole archive (F21). It streams the
// archive server-side to read tar headers; nothing is buffered beyond the list.
func (s *Server) handleBackupEntries(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := s.store.GetBackup(id)
	if err != nil {
		errJSON(w, http.StatusNotFound, "backup not found")
		return
	}
	// Reading through a large archive can take a while; give it room but bound it.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	entries, err := s.engine.ListEntries(ctx, b, r.URL.Query().Get("source"))
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// deferredAttachment writes the download headers (200 + Content-Disposition) lazily
// on the first data byte, so the handler can still answer a clean 400 when the
// requested file isn't in the backup (nothing was streamed yet).
type deferredAttachment struct {
	w        http.ResponseWriter
	filename string
	wrote    bool
}

func (d *deferredAttachment) writeHeaders() {
	if d.wrote {
		return
	}
	d.wrote = true
	d.w.Header().Set("Content-Type", "application/octet-stream")
	d.w.Header().Set("X-Content-Type-Options", "nosniff")
	// Quote the filename and strip anything that could break the header or mislead.
	safe := strings.NewReplacer("\"", "", "\\", "", "\r", "", "\n", "").Replace(filepath.Base(d.filename))
	d.w.Header().Set("Content-Disposition", `attachment; filename="`+safe+`"`)
}

func (d *deferredAttachment) Write(p []byte) (int, error) {
	d.writeHeaders()
	return d.w.Write(p)
}

// handleBackupExtract streams exactly one file from a backup's volume payload,
// decrypted (F21). The path is matched against the archive's own tar entries — it
// never touches the filesystem by path — and a path not present returns 400.
func (s *Server) handleBackupExtract(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path := r.URL.Query().Get("path")
	if path == "" {
		errJSON(w, http.StatusBadRequest, "path is required")
		return
	}
	b, err := s.store.GetBackup(id)
	if err != nil {
		errJSON(w, http.StatusNotFound, "backup not found")
		return
	}
	// F199: same gate as the whole-archive download. One file out of a backup is
	// less data than all of it, but it is the same KIND of data — a config file
	// with the database password in it is a complete compromise on its own.
	if !s.requireExportTicket(w, r, exportPurposeExtract, id) {
		return
	}
	_ = s.store.Audit(userFrom(r), "export.extract", id, "decrypted file streamed to the browser: "+safeLabel(path, 200))
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	dw := &deferredAttachment{w: w, filename: path}
	eerr := s.engine.ExtractOne(ctx, b, r.URL.Query().Get("source"), path, dw)
	switch {
	case errors.Is(eerr, backup.ErrEntryNotFound):
		errJSON(w, http.StatusBadRequest, "no such file in this backup")
	case eerr != nil && !dw.wrote:
		errJSON(w, http.StatusBadGateway, eerr.Error())
	case eerr != nil:
		// Failure mid-stream — headers already sent, so we can only log.
		s.logSink(id, "ERR", "File extract failed: "+eerr.Error())
	case !dw.wrote:
		dw.writeHeaders() // a zero-byte file still gets a clean 200 + headers
	}
}

// backupRate computes the 30-day backup success rate for a node (nodeID="" =
// whole fleet) from the catalog (PLAN §4.3/§4.6).
func (s *Server) backupRate(nodeID string) map[string]any {
	// Slim projection (perf Fix 6): only CreatedAt/Status/Verified are read.
	list, _ := s.store.ListBackupSummaries(nodeID, 5000)
	cutoff := time.Now().AddDate(0, 0, -30).Unix()
	var total, verified int
	for _, b := range list {
		if b.CreatedAt < cutoff || b.Status == "running" {
			continue
		}
		total++
		if b.Verified == "verified" {
			verified++
		}
	}
	rate := 100.0
	if total > 0 {
		rate = float64(verified) / float64(total) * 100.0
	}
	return map[string]any{"backup_success_rate": rate, "backups_30d": total, "backups_verified": verified}
}

// handleStats returns fleet-wide metrics for the dashboard footer (PLAN §5.4).
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.backupRate(""))
}

// handleGetNode returns one node with its host details (pulled live from THAT
// node's Docker daemon), cached resource summary, and per-node backup rate —
// powering the node-detail stat header (PLAN §5.4).
// handleNodeHealth returns a node's connection-health transitions and its uptime %
// over the last 7 and 30 days (F40), so a flapping node is visible as a pattern and
// a missed backup can be correlated with the node being down at the time.
func (s *Server) handleNodeHealth(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetNode(id); errors.Is(err, store.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	days := 30
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d > 0 && d <= 90 {
		days = d
	}
	all, err := s.store.NodeHealth(id, 0) // newest-first, all retained transitions
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := time.Now().Unix()
	windowStart := now - int64(days)*86400
	display := make([]store.NodeHealthRow, 0, len(all))
	for _, h := range all {
		if h.Ts >= windowStart {
			display = append(display, h)
		}
	}
	// F67: whether the CURRENT unreachability is a refused host-key pin
	// mismatch — a security state, not an outage.
	hostKeyChanged := false
	if st := s.getStat(id); st != nil {
		hostKeyChanged = st.HostKeyChanged
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rows":             display, // newest-first, within the requested window
		"uptime_pct_7d":    uptimePct(all, now-7*86400, now),
		"uptime_pct_30d":   uptimePct(all, now-30*86400, now),
		"days":             days,
		"host_key_changed": hostKeyChanged,
	})
}

// uptimePct computes the fraction of [from, to] a node was reachable, as a
// percentage rounded to one decimal, from its transition rows (newest-first). The
// state entering the window is the newest transition at or before `from`; if none,
// the oldest known state is assumed to have held. No history at all → 100 (nothing
// was ever observed down). Pure, so it is unit-testable.
func uptimePct(rows []store.NodeHealthRow, from, to int64) float64 {
	if to <= from || len(rows) == 0 {
		return 100
	}
	// State at `from`: the newest row with ts <= from (rows are newest-first), else
	// fall back to the oldest known state (assume it held before the window).
	state := rows[len(rows)-1].Reachable
	for _, h := range rows {
		if h.Ts <= from {
			state = h.Reachable
			break
		}
	}
	segStart := from
	var upSecs int64
	// Walk the transitions strictly inside the window, oldest-first.
	for i := len(rows) - 1; i >= 0; i-- {
		h := rows[i]
		if h.Ts <= from || h.Ts >= to {
			continue
		}
		if state {
			upSecs += h.Ts - segStart
		}
		segStart = h.Ts
		state = h.Reachable
	}
	if state {
		upSecs += to - segStart
	}
	pct := float64(upSecs) / float64(to-from) * 100
	return float64(int64(pct*10+0.5)) / 10 // one decimal
}

func (s *Server) handleGetNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n, err := s.store.GetNode(id)
	if errors.Is(err, store.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// F88: the pinned volume-sidecar image. Public information (a content
	// address), read from the store — no Docker call, so it costs the page nothing.
	resp := map[string]any{"node": n, "backups": s.backupRate(id), "sidecar": s.sidecarPinView(id)}

	s.statMu.RLock()
	st := s.stats[id]
	s.statMu.RUnlock()
	if st != nil {
		resp["reachable"], resp["summary"], resp["error"] = st.Reachable, st.Summary, st.Error
	}
	if cli, err := s.reg.Get(id); err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		if hi, err := dockercli.Info(ctx, cli); err == nil {
			resp["host"] = hi
		}
		cancel()
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------------- External backup destinations (PLAN §4.9) ----------------

type destView struct {
	*store.Destination
	FreeBytes  uint64 `json:"free_bytes"`
	TotalBytes uint64 `json:"total_bytes"`
	UsedBytes  uint64 `json:"used_bytes"` // shown when total is unknown (e.g. unlimited quota)
	Reachable  bool   `json:"reachable"`  // live liveness probe (drives the status dot)
	Forecast          // capacity trend + projected fill-up (PLAN §9.13)
}

type destReq struct {
	Name   string            `json:"name"`
	Type   string            `json:"type"`
	Config map[string]string `json:"config"`
}

// handleListDestinations lists destinations with capacity where available.
// Credentials are never returned.
func (s *Server) handleListDestinations(w http.ResponseWriter, r *http.Request) {
	ds, err := s.store.ListDestinations()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]*destView, len(ds))
	var wg sync.WaitGroup
	for i, d := range ds {
		out[i] = &destView{Destination: d}
		cfg, err := s.decryptDestConfig(d)
		if err != nil {
			continue
		}
		b, err := storage.NewFromConfig(d.Type, cfg)
		if err != nil {
			continue
		}
		// Probe destinations concurrently — one slow/unreachable provider must not
		// serialize (and stall) the whole list. Each probe is bounded.
		wg.Add(1)
		go func(v *destView, b storage.Backend) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
			defer cancel()
			// Liveness first (cheap, writes nothing) → drives the status dot.
			v.Reachable = storage.Reachable(cctx, b) == nil
			// Capacity only if reachable (a down provider can't report it).
			if v.Reachable {
				if cap, ok := b.(storage.Capacity); ok {
					v.FreeBytes, _ = cap.FreeBytes(cctx)
					v.TotalBytes, _ = cap.TotalBytes(cctx)
				} else {
					v.FreeBytes, _ = b.FreeBytes(cctx)
				}
				// Usage is reported even when total is unknown (unlimited quota).
				if u, ok := b.(storage.Usage); ok {
					v.UsedBytes, _ = u.UsedBytes(cctx)
				}
			}
		}(out[i], b)
	}
	wg.Wait()
	// Capacity forecast per destination (PLAN §9.13) — from stored history + the
	// live reading just probed. Cheap (DB only), so it never slows the list.
	for _, v := range out {
		v.Forecast = s.destForecast(v.ID, v.TotalBytes, v.FreeBytes)
	}
	writeJSON(w, http.StatusOK, out)
}

// normalizeSealManifests validates the per-destination sidecar-sealing flag
// (F78): only the literals "true"/"false" are stored; anything else is dropped
// so the destination falls back to the global manifest.encrypt setting.
func normalizeSealManifests(cfg map[string]string) {
	if v, ok := cfg["seal_manifests"]; ok && v != "true" && v != "false" {
		delete(cfg, "seal_manifests")
	}
}

// normalizeRetentionLock validates the per-destination filesystem retention lock
// (F97): a clean integer 1..3650 is stored; 0/empty/invalid is DROPPED, so a
// malformed value can never be read as "lock forever". Above-range clamps to
// 3650 (~10 years) — a lock cannot be lifted from the UI, so an unbounded period
// is a way to accidentally make a destination permanently unprunable.
func normalizeRetentionLock(cfg map[string]string) {
	v, ok := cfg["retention_lock_days"]
	if !ok {
		return
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		delete(cfg, "retention_lock_days")
		return
	}
	if n > 3650 {
		n = 3650
	}
	cfg["retention_lock_days"] = strconv.Itoa(n)
}

// normalizeDestMbps validates the per-destination upload cap (F14/F76):
// a clean integer 0..100000 is stored normalized; 0/empty/invalid is dropped
// (= inherit the global aggregate only); above-range clamps to 100000.
func normalizeDestMbps(cfg map[string]string) {
	v, ok := cfg["max_upload_mbps"]
	if !ok {
		return
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		delete(cfg, "max_upload_mbps")
		return
	}
	if n > 100000 {
		n = 100000
	}
	cfg["max_upload_mbps"] = strconv.Itoa(n)
}

// handleAddDestination stores a destination (credentials encrypted at rest).
func (s *Server) handleAddDestination(w http.ResponseWriter, r *http.Request) {
	var req destReq
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Name == "" || req.Type == "" {
		errJSON(w, http.StatusBadRequest, "name and type are required")
		return
	}
	normalizeSealManifests(req.Config) // F78
	normalizeDestMbps(req.Config)      // F76
	normalizeRetentionLock(req.Config) // F97
	// F66: an SFTP destination pins its host key at save time (TOFU) — refuse
	// the save when the host can't be reached to pin.
	if err := s.pinSFTPHostKey(r.Context(), req.Type, req.Config); err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := storage.NewFromConfig(req.Type, req.Config); err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	enc, err := s.encryptDestConfig(req.Config)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	d := &store.Destination{ID: randToken()[:12], Name: req.Name, Type: req.Type, Enabled: true, Status: "unknown", ConfigEnc: enc}
	if err := s.store.CreateDestination(d); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "destination.add", req.Name, req.Type)
	writeJSON(w, http.StatusOK, d)
}

// handleTestDestination does a real write/read/delete round-trip.
func (s *Server) handleTestDestination(w http.ResponseWriter, r *http.Request) {
	var req destReq
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	b, err := storage.NewFromConfig(req.Type, req.Config)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := probeDestination(ctx, b); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	resp := map[string]any{"ok": true}
	if cap, ok := b.(storage.Capacity); ok {
		free, _ := cap.FreeBytes(ctx)
		total, _ := cap.TotalBytes(ctx)
		resp["free_bytes"], resp["total_bytes"] = free, total
	}
	addObjectLockResult(ctx, resp, b)
	addSFTPTestFingerprint(resp, b, req.Config) // F66: show what would be pinned
	writeJSON(w, http.StatusOK, resp)
}

// probeDestination verifies a destination works before saving. For a normal
// destination it does a full write/read/delete round-trip (Probe). For an
// IMMUTABLE (WORM) destination it uses a non-destructive liveness check instead
// (PLAN §9.1): a write would create a locked probe object the bucket won't let us
// delete — polluting the bucket — and an append-only key can't delete anyway. The
// WORM assurance comes from VerifyObjectLock, which writes nothing.
func probeDestination(ctx context.Context, b storage.Backend) error {
	// The skip applies only to SERVER-enforced object lock, which is what
	// ObjectLockVerifier identifies. A filesystem retention lock (F97) is applied
	// by DockBack to the backup objects it uploads — a probe object is never
	// locked and is fully deletable, so those destinations keep the full
	// write/read/delete round-trip that actually proves they work.
	if _, worm := b.(storage.ObjectLockVerifier); worm {
		if im, ok := b.(storage.Immutabler); ok && im.Immutable() {
			return storage.Reachable(ctx, b)
		}
	}
	return storage.Probe(ctx, b)
}

// addObjectLockResult runs the non-destructive WORM preflight (PLAN §9.1) when
// the backend supports it and folds the verdict into a Test-connection response,
// so the UI can confirm a bucket is genuinely immutable — or warn loudly when a
// destination is configured immutable but the bucket wouldn't enforce it.
func addObjectLockResult(ctx context.Context, resp map[string]any, b storage.Backend) {
	v, ok := b.(storage.ObjectLockVerifier)
	if !ok {
		return
	}
	requested := false
	if im, ok := b.(storage.Immutabler); ok {
		requested = im.Immutable()
	}
	st := v.VerifyObjectLock(ctx)
	resp["object_lock_requested"] = requested
	resp["object_lock_enforced"] = st.Enforced
	resp["object_lock_checked"] = st.Checked
	resp["object_lock_detail"] = st.Detail
	// The dangerous case: the operator asked for immutability but the bucket
	// definitively doesn't enforce it — surface it as a loud warning.
	if requested && st.Checked && !st.Enforced {
		resp["object_lock_warning"] = "You enabled Object Lock for this destination, but the bucket does NOT enforce it — backups here would NOT be immutable."
	}
}

// handleDeleteDestination removes a destination (does not touch its data).
func (s *Server) handleDeleteDestination(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteDestination(id); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.DeleteDestSamples(id) // drop its capacity history too (PLAN §9.13)
	_ = s.store.Audit(userFrom(r), "destination.delete", id, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// secretDestKeys are credential fields that are NEVER returned to the client; on
// edit, a blank value for one of these means "keep the stored secret".
var secretDestKeys = map[string]bool{"password": true, "secret_key": true,
	// F66 SFTP: the private key + its passphrase are credentials; the pinned
	// host key is server-managed (never edited by hand — see reset-hostkey).
	"private_key": true, "key_passphrase": true, "host_key": true,
	// An access key is not a secret in the way its partner is — but it is one
	// half of a credential pair, and this endpoint's own contract is that it
	// returns no secrets. Sending it to the browser put it in history, in
	// whatever an extension can read, and on the screen of whoever is standing
	// behind the operator, for nothing: the edit form never needed it back.
	"access_key": true}

// mergeDestConfig overlays submitted fields onto the stored config, keeping the
// stored secret whenever a secret field is left blank — so the credential never
// has to leave the server to edit a destination.
func mergeDestConfig(cur, in map[string]string) map[string]string {
	m := map[string]string{}
	for k, v := range cur {
		m[k] = v
	}
	for k, v := range in {
		if secretDestKeys[k] && strings.TrimSpace(v) == "" {
			continue
		}
		m[k] = v
	}
	return m
}

// handleGetDestinationConfig returns a destination's NON-SECRET config to pre-fill
// the edit form. Secret fields (password, secret_key) are stripped — they are
// never sent to the browser; the form leaves them blank ("keep current").
func (s *Server) handleGetDestinationConfig(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.GetDestination(r.PathValue("id"))
	if err != nil {
		errJSON(w, http.StatusNotFound, "destination not found")
		return
	}
	cfg, err := s.decryptDestConfig(d)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "could not read destination")
		return
	}
	out := map[string]string{}
	for k, v := range cfg {
		if !secretDestKeys[k] {
			out[k] = v
		}
	}
	// F66: the pinned SSH host key itself stays server-side, but its fingerprint
	// is safe to show ("Host key pinned: SHA256:…" + reset button).
	if hk := strings.TrimSpace(cfg["host_key"]); hk != "" {
		if _, _, fp, ok := dockercli.ParseHostKey([]byte(hk)); ok {
			out["host_key_fp"] = fp
		}
	}
	// Whether a credential is stored, never its value — the same shape the
	// notification settings use for the Gotify token and the SMTP password. The
	// form needs this to say "leave blank to keep current" rather than making the
	// operator guess whether a blank field means unset.
	writeJSON(w, http.StatusOK, map[string]any{
		"id": d.ID, "name": d.Name, "type": d.Type, "config": out,
		"access_key_set": strings.TrimSpace(cfg["access_key"]) != "",
		"secret_key_set": strings.TrimSpace(cfg["secret_key"]) != "",
	})
}

// handleUpdateDestination edits a destination in place. Secrets left blank are
// kept from storage (mergeDestConfig), so the stored password/secret never leaves
// the server. Re-encrypts with the master key and resets status so a stale
// active/error result clears (PLAN §3.8).
func (s *Server) handleUpdateDestination(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.GetDestination(r.PathValue("id"))
	if err != nil {
		errJSON(w, http.StatusNotFound, "destination not found")
		return
	}
	var req destReq
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Name == "" {
		errJSON(w, http.StatusBadRequest, "name is required")
		return
	}
	cur, err := s.decryptDestConfig(d)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "could not read destination")
		return
	}
	merged := mergeDestConfig(cur, req.Config)
	normalizeSealManifests(merged) // F78
	normalizeDestMbps(merged)      // F76
	normalizeRetentionLock(merged) // F97
	// F66: re-pin on edit if the pin is somehow absent (host must be reachable).
	if err := s.pinSFTPHostKey(r.Context(), d.Type, merged); err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	// Type is fixed on edit (a different type would mean a different config shape).
	if _, err := storage.NewFromConfig(d.Type, merged); err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	enc, err := s.encryptDestConfig(merged)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	d.Name = req.Name
	d.ConfigEnc = enc
	d.Status = "unknown" // config changed — clear any stale active/error result
	if err := s.store.UpdateDestination(d); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "destination.edit", d.Name, d.Type)
	writeJSON(w, http.StatusOK, map[string]string{"id": d.ID})
}

// handleTestDestinationByID tests an EXISTING destination using its stored config
// merged with any edited fields (so a blank secret reuses the stored one) — lets
// the edit form verify changes before saving without re-typing the password.
func (s *Server) handleTestDestinationByID(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.GetDestination(r.PathValue("id"))
	if err != nil {
		errJSON(w, http.StatusNotFound, "destination not found")
		return
	}
	var req destReq
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	cur, err := s.decryptDestConfig(d)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "could not read destination")
		return
	}
	merged := mergeDestConfig(cur, req.Config)
	b, err := storage.NewFromConfig(d.Type, merged)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := probeDestination(ctx, b); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// F66: a stored SFTP destination whose pin was still absent (saved while the
	// host was offline, or reset while unreachable) gains it on this successful
	// test — persisted sealed immediately.
	s.persistLearnedDestHostKey(d, b, cur)
	resp := map[string]any{"ok": true}
	if cap, ok := b.(storage.Capacity); ok {
		free, _ := cap.FreeBytes(ctx)
		total, _ := cap.TotalBytes(ctx)
		resp["free_bytes"], resp["total_bytes"] = free, total
	}
	addObjectLockResult(ctx, resp, b)
	addSFTPTestFingerprint(resp, b, merged)
	writeJSON(w, http.StatusOK, resp)
}

// handleAdoptDestination scans a destination for `.dback` archives that verify
// under the current master key but aren't in the catalog, and re-registers them
// (F20) — recovering backups after a DockBack DB loss. Never overwrites existing
// rows; a manifest sealed/signed with a different key is skipped.
func (s *Server) handleAdoptDestination(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.GetDestination(r.PathValue("id"))
	if err != nil {
		errJSON(w, http.StatusNotFound, "destination not found")
		return
	}
	cfg, err := s.decryptDestConfig(d)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "could not read destination")
		return
	}
	be, err := storage.NewFromConfig(d.Type, cfg)
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	// Listing a large bucket + reading each sidecar can take a while — give it room.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	adopted, skipped, aerr := s.engine.AdoptFromBackend(ctx, be, d.Name, d.ID, d.Type)
	if aerr != nil {
		errJSON(w, http.StatusBadGateway, aerr.Error())
		return
	}
	// F102: the per-archive report goes to the operator, not the audit trail —
	// the audit records WHAT HAPPENED (how many, and how many were foreign-key
	// skips, the number worth noticing after a rotation), while the object keys
	// themselves belong in the response the operator is reading right now.
	foreign := 0
	for _, sk := range skipped {
		if sk.Reason == backup.SkipForeignKey {
			foreign++
		}
	}
	_ = s.store.Audit(userFrom(r), "destination.adopt", d.Name,
		fmt.Sprintf("adopted=%d skipped=%d foreign_key=%d", adopted, len(skipped), foreign))
	writeJSON(w, http.StatusOK, map[string]any{
		"adopted": adopted,
		"skipped": len(skipped),
		"skips":   skipped,
	})
}

func (s *Server) encryptDestConfig(cfg map[string]string) ([]byte, error) {
	b, _ := json.Marshal(cfg)
	return crypto.SealString(string(b), s.cfg.EncryptionKey)
}

func (s *Server) decryptDestConfig(d *store.Destination) (map[string]string, error) {
	js, err := crypto.OpenString(d.ConfigEnc, s.cfg.EncryptionKey)
	if err != nil {
		return nil, err
	}
	var cfg map[string]string
	return cfg, json.Unmarshal([]byte(js), &cfg)
}

// ---------------- Audit / Settings / Logs ----------------

// handleAudit lists audit entries. With no pagination params it returns a plain
// array (newest 200 — backward-compatible). When page_size>0 it returns a
// server-side paged/searched/date-ranged envelope {items,total,page,page_size}
// so the audit page can browse the whole (unbounded) log (F7).
//
// Query params: q (search actor/action/target/detail), from, to (unix seconds),
// page, page_size.
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	pageSize := atoiDefault(qp.Get("page_size"), 0)
	if pageSize <= 0 {
		entries, err := s.store.ListAudit(200)
		if err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, entries)
		return
	}
	page := atoiDefault(qp.Get("page"), 1)
	if page < 1 {
		page = 1
	}
	if pageSize > 500 {
		pageSize = 500
	}
	list, total, err := s.store.ListAuditPage(auditFilterFrom(qp, pageSize, (page-1)*pageSize))
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list, "total": total, "page": page, "page_size": pageSize})
}

// auditFilterFrom builds an AuditFilter from query params (shared by the paged
// list and the export).
func auditFilterFrom(qp url.Values, limit, offset int) store.AuditFilter {
	from, _ := strconv.ParseInt(qp.Get("from"), 10, 64)
	to, _ := strconv.ParseInt(qp.Get("to"), 10, 64)
	return store.AuditFilter{Query: qp.Get("q"), From: from, To: to, Limit: limit, Offset: offset}
}

// csvSafe neutralizes spreadsheet formula injection: a field beginning with one
// of = + - @ (or a leading tab/CR) can execute as a formula when the exported CSV
// is opened in Excel/Sheets. Prefixing a single quote defuses it while keeping the
// value legible — important because audit fields carry user-influenced text and an
// export is handed to third parties.
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// handleAuditExport streams ALL matching audit rows (honoring q/from/to) as a CSV
// or JSON attachment for compliance/hand-off (F7). The export itself is audited.
// auditExportFlushRows is how often a streamed CSV export is pushed to the
// client, so neither the writer's buffer nor the operator's wait grows with the
// size of the trail.
const auditExportFlushRows = 1000

func (s *Server) handleAuditExport(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	format := qp.Get("format")
	if format != "json" {
		format = "csv"
	}
	// Streamed a page at a time rather than assembled in memory: the audit table
	// is append-only and unbounded, so "export everything" grew with the age of
	// the deployment until it met the container's memory limit.
	filter := auditFilterFrom(qp, 0, 0)
	stamp := time.Now().UTC().Format("20060102-150405")
	rows := 0

	if format == "json" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="dockback-audit-%s.json"`, stamp))
		enc := json.NewEncoder(w)
		// The array is written by hand so each row can be encoded and flushed as
		// it is read, rather than every row being held to marshal one value.
		fmt.Fprint(w, "[")
		werr := s.store.WalkAuditFiltered(filter, func(e *store.AuditEntry) bool {
			if rows > 0 {
				fmt.Fprint(w, ",")
			}
			fmt.Fprint(w, "\n  ")
			rows++
			return enc.Encode(e) == nil
		})
		fmt.Fprint(w, "\n]\n")
		s.auditTheExport(r, format, rows, qp.Get("q"), werr)
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="dockback-audit-%s.csv"`, stamp))
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"timestamp_utc", "unix", "actor", "action", "target", "detail"})
	werr := s.store.WalkAuditFiltered(filter, func(e *store.AuditEntry) bool {
		_ = cw.Write([]string{
			time.Unix(e.TS, 0).UTC().Format(time.RFC3339),
			strconv.FormatInt(e.TS, 10),
			csvSafe(e.Actor), csvSafe(e.Action), csvSafe(e.Target), csvSafe(e.Detail),
		})
		rows++
		// Flush each page so a large export starts arriving immediately and the
		// writer's own buffer does not become the thing that grows.
		if rows%auditExportFlushRows == 0 {
			cw.Flush()
		}
		return true
	})
	cw.Flush()
	s.auditTheExport(r, format, rows, qp.Get("q"), werr)
}

// auditTheExport records what left the machine. The headers are already written
// by this point, so a read failure part-way cannot be turned into an error
// response — it is recorded instead, which is the honest thing for a trail whose
// purpose is to say what happened.
func (s *Server) auditTheExport(r *http.Request, format string, rows int, query string, err error) {
	detail := fmt.Sprintf("rows=%d q=%q", rows, query)
	if err != nil {
		detail += " INCOMPLETE: " + err.Error()
	}
	_ = s.store.Audit(userFrom(r), "audit.export", format, detail)
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	// The number the ENGINE keeps, not a second opinion. This endpoint answered
	// 10 while the engine kept 3, so Settings showed a policy the app was not
	// applying.
	retention, _ := s.store.GetSetting("retention.generations", backup.DefaultGenerations)
	autoprune, _ := s.store.GetSetting("retention.autoprune", "false")
	autosnapKeep, _ := s.store.GetSetting("retention.autosnap_keep", "3")
	encManifest, _ := s.store.GetSetting("manifest.encrypt", "false")
	verifyDeep, _ := s.store.GetSetting("verify.deep", "false")
	scrubDays, _ := s.store.GetSetting("scrub.interval_days", "0")
	drillDays, _ := s.store.GetSetting("drill.interval_days", "0")
	appDrillDays, _ := s.store.GetSetting("appbackup.drill_interval_days", "7")
	autotune, _ := s.store.GetSetting("backup.autotune_compression", "true")
	failCorrupt, _ := s.store.GetSetting("backup.fail_on_corrupt_db", "true")
	syntheticFull, _ := s.store.GetSetting("backup.synthetic_full", "true")
	tripwireEnabled, _ := s.store.GetSetting("tripwire.enabled", "true")
	indexAlways, _ := s.store.GetSetting("index.always", "true")
	// F65: surface whether /metrics is currently gated (locked the moment ANY
	// metrics-scoped token exists — including an expired one, which keeps the
	// endpoint closed rather than silently re-opening it).
	metricsLocked := "false"
	if n, _ := s.store.CountAPITokensWithScope("metrics"); n > 0 {
		metricsLocked = "true"
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"metrics_locked":                metricsLocked,
		"backup.autotune_compression":   autotune,
		"backup.fail_on_corrupt_db":     failCorrupt,
		"backup.synthetic_full":         syntheticFull,
		"retention.generations":         retention,
		"retention.autoprune":           autoprune,
		"retention.autosnap_keep":       autosnapKeep,
		"manifest.encrypt":              encManifest,
		"verify.deep":                   verifyDeep,
		"scrub.interval_days":           scrubDays,
		"drill.interval_days":           drillDays,
		"appbackup.drill_interval_days": appDrillDays,
		// Performance & tuning knobs, with the env-derived config value as default
		// so the UI shows the effective setting even when never overridden (F15).
		"db.ready_timeout_seconds":       strconv.Itoa(s.dbReadyTimeoutSeconds()),
		"schedule.jitter_seconds":        strconv.Itoa(s.scheduleJitter()),
		"upload.max_mbps":                strconv.Itoa(s.uploadMbps()),
		"backup.bind_skip_gib":           strconv.Itoa(s.engine.BindSkipGiBGlobal()),
		"scrub.per_cycle":                strconv.Itoa(s.scrubPerCycle()),
		"alert.dest_full_pct":            strconv.Itoa(s.settingInt("alert.dest_full_pct", defaultDestFullPct)),
		"alert.forecast_days":            strconv.Itoa(s.forecastWarnDays()),
		"backup.sidecar_image":           dockercli.SidecarImage(),
		"backup.max_concurrent":          strconv.Itoa(s.maxConcurrent()),
		"backup.max_concurrent_per_node": strconv.Itoa(s.maxConcurrentPerNode()),
		"restore.health_timeout_seconds": strconv.Itoa(s.engine.RestoreHealthTimeoutGlobal()),
		"drill.boot_wait_seconds":        strconv.Itoa(dockercli.DrillBootWaitSeconds()),
		"critical.rpo_min_seconds":       strconv.Itoa(s.criticalRPOMin()),
		"critical.tick_seconds":          strconv.Itoa(s.criticalTickSeconds()),
		"critical.fail_limit":            strconv.Itoa(s.criticalFailLimit()),
		"alert.anomaly_factor":           strconv.Itoa(s.anomalyFactor()),
		// Universal file index (F70): non-incremental backups also store the file
		// listing, powering browse-without-truncation, file search and diff.
		"index.always": indexAlways,
		// Ransomware tripwire (F69): mass-change detection on incremental deltas.
		"tripwire.enabled":     tripwireEnabled,
		"tripwire.min_files":   strconv.Itoa(s.settingInt("tripwire.min_files", 200)),
		"tripwire.changed_pct": strconv.Itoa(s.settingInt("tripwire.changed_pct", 60)),
		"tripwire.deleted_pct": strconv.Itoa(s.settingInt("tripwire.deleted_pct", 40)),
		// Outbound allow-list (F39): the in-app override if set, else the env default,
		// so the UI shows the effective list even when never overridden.
		"security.egress_allow": s.egressAllowEffective(),
		// F207: whether the allow-list is currently OBSERVED rather than enforced.
		"security.egress_audit": boolWord(egress.AuditMode()),
		// F203: authentication policy, reported as the EFFECTIVE value — the
		// in-app override when set, otherwise the env default, already clamped.
		// The UI shows what is actually enforced rather than what happens to be
		// stored, which for a security control is the only useful number.
		"security.session_ttl_hours":    strconv.Itoa(int(s.sessionTTL().Hours())),
		"security.session_idle_minutes": strconv.Itoa(s.sessionIdleMinutes()),
		"security.min_password_len":     strconv.Itoa(s.minPasswordLength()),
		"key_fingerprint":               s.engine.MasterKeyFP(),
		"storage":                       s.engine.Storage.Name(),
	})
}

// egressAllowEffective returns the effective outbound allow-list (F39): the in-app
// override when set, otherwise the DOCKBACK_EGRESS_ALLOW env default.
func (s *Server) egressAllowEffective() string {
	if v, _ := s.store.GetSetting("security.egress_allow", ""); strings.TrimSpace(v) != "" {
		return v
	}
	return strings.Join(s.cfg.EgressAllow, ",")
}

// applyEgress installs the outbound allow-list live via an atomic policy swap (F39).
// An empty override falls back to the env default, so clearing the setting restores
// the boot behavior without a restart.
func (s *Server) applyEgress(v string) {
	entries := splitCleanList(v)
	if len(entries) == 0 {
		entries = s.cfg.EgressAllow
	}
	egress.Configure(entries)
}

// settingInt reads an integer setting, returning def when it's unset or malformed
// (F15 — the env-derived config value is passed as def, so a knob falls back to
// the environment when no in-app override exists).
func (s *Server) settingInt(key string, def int) int {
	v, _ := s.store.GetSetting(key, "")
	if strings.TrimSpace(v) == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}

// Tuning resolvers: the persisted setting when present, else the env/config value.
func (s *Server) dbReadyTimeoutSeconds() int {
	return s.settingInt("db.ready_timeout_seconds", s.cfg.DBReadyTimeout)
}
func (s *Server) uploadMbps() int { return s.settingInt("upload.max_mbps", s.cfg.MaxUploadMbps) }
func (s *Server) scheduleJitter() int {
	if n := s.settingInt("schedule.jitter_seconds", s.cfg.ScheduleJitter); n > 0 {
		return n
	}
	return 0
}

// settableKeys is the allow-list of setting keys writable via POST /api/settings,
// each with coercion/bounds. Any other key is rejected so a typo'd or hostile key
// can't pollute the settings table (SCAN_NOTES #51 / F15). Keys with dedicated
// endpoints (policy, notifications, schedules, …) are intentionally NOT here.
func coerceSetting(key, val string) (string, bool) {
	switch key {
	// F207: security.egress_audit is the dry run — the allow-list is evaluated
	// and observed but never acted on. A boolean like the rest.
	case "manifest.encrypt", "verify.deep", "backup.autotune_compression", "backup.synthetic_full", "tripwire.enabled", "index.always", "backup.fail_on_corrupt_db", "security.egress_audit":
		if val == "true" {
			return "true", true
		}
		return "false", true
	case "tripwire.min_files":
		// Ransomware tripwire (F69): minimum changed/deleted files before either
		// mass-change rule can trip, so small volumes never false-positive.
		return clampIntSetting(val, 200, 10, 100000), true
	case "tripwire.changed_pct":
		// F69: percentage of the parent index changed in one run that trips the wire.
		return clampIntSetting(val, 60, 10, 100), true
	case "tripwire.deleted_pct":
		// F69: percentage of the parent index deleted in one run that trips the wire.
		return clampIntSetting(val, 40, 10, 100), true
	case "scrub.interval_days", "drill.interval_days":
		return clampIntSetting(val, 0, 0, 3650), true
	case "drill.per_cycle":
		return clampIntSetting(val, 1, 1, 5), true
	case "drill.scope":
		if val == "newest_per_week" {
			return "newest_per_week", true
		}
		return "newest", true
	case "db.ready_timeout_seconds":
		return clampIntSetting(val, 300, 10, 3600), true
	// F203: authentication policy. The bounds here are the security control —
	// this row is written by whoever holds an admin session, so what stops a
	// weakened policy from being an arbitrary one is that it cannot leave the
	// range. The minimum password length clamps UP to the baseline: 12 is the
	// shortest this app accepts in any configuration.
	case "security.session_ttl_hours":
		return clampIntSetting(val, 12, minSessionTTLHours, maxSessionTTLHours), true
	case "security.session_idle_minutes":
		return clampIntSetting(val, 30, minSessionIdleMinutes, maxSessionIdleMinutes), true
	case "security.min_password_len":
		return clampIntSetting(val, minPasswordLen, minPasswordLen, maxPasswordLen), true
	case "schedule.jitter_seconds":
		return clampIntSetting(val, 30, 0, 3600), true
	case "upload.max_mbps":
		return clampIntSetting(val, 0, 0, 100000), true
	case "backup.bind_skip_gib":
		// Large-bind cutoff in GiB (F12): binds bigger than this are excluded by
		// default. Clamp to a sane range (1 GiB avoids auto-including every bind).
		return clampIntSetting(val, 5, 1, 1024), true
	case "scrub.per_cycle":
		// Backups re-verified per scrub cycle (F18), parity with drill.per_cycle.
		return clampIntSetting(val, 3, 1, 5), true
	case "retention.autosnap_keep":
		// Newest automatic ("auto:") snapshots kept per container, separate from GFS
		// (F48). 0 = treat auto snapshots like any other backup; clamp 0..50.
		return clampIntSetting(val, 3, 0, 50), true
	case "appbackup.drill_interval_days":
		// App-backup integrity-drill cadence in days (F58). 0 = off; clamp 0..365.
		return clampIntSetting(val, 7, 0, 365), true
	case "alert.dest_full_pct":
		// "Almost full" alert threshold as a used percentage (F18).
		return clampIntSetting(val, 90, 50, 99), true
	case "alert.forecast_days":
		// "Filling up" forecast horizon in days (F18).
		return clampIntSetting(val, 30, 1, 3650), true
	case "backup.sidecar_image":
		// Volume-sidecar image ref (F25). Trim; reject empty/whitespace/garbage so a
		// bad value can't break every volume backup. A valid ref is stored verbatim;
		// to return to the built-in default, set it back to "alpine:3.20".
		v := strings.TrimSpace(val)
		if !validSidecarImageRef(v) {
			return "", false
		}
		return v, true
	case "backup.max_concurrent":
		// Fleet-wide concurrent-backup cap (F29). Default 3 (the shipped env default).
		return clampIntSetting(val, 3, 1, 64), true
	case "backup.max_concurrent_per_node":
		// Per-node concurrent-backup cap (F29). Default 2 (the shipped env default).
		return clampIntSetting(val, 2, 1, 64), true
	case "restore.health_timeout_seconds":
		// Post-restore health-gate timeout before auto-rollback (F30/F4).
		return clampIntSetting(val, 300, 30, 3600), true
	case "restore.test_clone_ttl_hours":
		// How long a one-click test clone lives before the reaper removes it and
		// the volumes Docker made for it (F219). Clamped 1..168 (a week): the floor
		// keeps a clone alive long enough to look at, the ceiling keeps "temporary"
		// meaning something — a clone nobody sweeps is the problem this replaced.
		return clampIntSetting(val, defaultTestCloneTTLHours, 1, 168), true
	case "drill.boot_wait_seconds":
		// How long a volume drill waits for the throwaway app to come up (F30/F5).
		return clampIntSetting(val, 45, 10, 600), true
	case "critical.rpo_min_seconds":
		// Low-RPO floor for critical DBs (F31), so aggressive re-dumps can't self-DoS.
		return clampIntSetting(val, 300, 60, 3600), true
	case "critical.tick_seconds":
		// How often the low-RPO loop evaluates due dumps (F31).
		return clampIntSetting(val, 60, 30, 600), true
	case "critical.fail_limit":
		// Consecutive verification failures that pause a critical DB's auto-backups (F31).
		return clampIntSetting(val, 3, 1, 10), true
	case "alert.anomaly_factor":
		// Backup drift factor (F28): a run more than Nx its usual duration/size warns.
		return clampIntSetting(val, 3, 2, 100), true
	case "security.egress_allow":
		// Live outbound allow-list override (F39). Empty clears the override, so the
		// DOCKBACK_EGRESS_ALLOW env default applies again. A non-empty list must
		// contain ONLY valid entries — every one must parse to a real rule (an exact
		// host, *.suffix domain, IP, or CIDR) — so a typo can't silently store a list
		// that refuses a legitimate destination. Stored as a normalized CSV.
		entries := splitCleanList(val)
		if len(entries) == 0 {
			return "", true // cleared → env default
		}
		for _, e := range entries {
			// egress.Parse is deliberately lenient (it treats an arbitrary token as an
			// exact host), so also require each entry to be a plausible host/IP/CIDR —
			// no whitespace — and to round-trip to an enabled rule. A garbage entry
			// that parses to nothing usable is rejected so a typo can't silently store
			// a list that refuses a legitimate destination.
			if strings.ContainsAny(e, " \t\r\n") || !egress.Parse([]string{e}).Enabled() {
				return "", false
			}
		}
		return strings.Join(entries, ","), true
	default:
		return "", false
	}
}

// validSidecarImageRef reports whether s is a plausible container image reference
// for the volume sidecar (F25): non-empty, no whitespace, bounded length, and only
// the characters a registry/repo/tag/digest ref uses — so a typo or hostile value
// can't be stored (it's a Docker API field, not a shell string). Dependency-free,
// mirroring validContainerName (F10).
func validSidecarImageRef(s string) bool {
	if s == "" || len(s) > 512 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-' || r == '/' || r == ':' || r == '@':
		default:
			return false
		}
	}
	return true
}

// clampInt clamps n to [min,max].
func clampInt(n, min, max int) int {
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

// clampIntSetting parses val, falling back to def on garbage, then clamps to
// [min,max] and re-serializes — so a stored numeric setting is always sane.
func clampIntSetting(val string, def, min, max int) string {
	n, err := strconv.Atoi(strings.TrimSpace(val))
	if err != nil {
		n = def
	}
	if n < min {
		n = min
	}
	if n > max {
		n = max
	}
	return strconv.Itoa(n)
}

func (s *Server) handleSetSettings(w http.ResponseWriter, r *http.Request) {
	var req map[string]string
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	// F203: changing the authentication policy is a security action, not an
	// operational one, so it is gated behind fresh proof of the password the same
	// way minting a token or revealing the encryption key already is. Whoever sits
	// down at a signed-in laptop could otherwise stretch the session lifetime to a
	// month — quietly extending their own access — with no credential at all.
	//
	// The gate is conditional on an auth-policy key actually being present, so an
	// ordinary settings save is untouched and nobody is asked for a password to
	// change a compression setting. The step-up credentials travel as reserved
	// keys in the same body and are removed before validation, so they are never
	// mistaken for settings and never stored.
	stepUp := stepUpBody{Password: req[stepUpPasswordKey], Code: req[stepUpCodeKey]}
	delete(req, stepUpPasswordKey)
	delete(req, stepUpCodeKey)
	if authPolicyChange(req) && !s.requireFreshAuth(w, r, stepUp) {
		return
	}
	// Validate + coerce every key against the allow-list up front; reject the whole
	// request on the first unknown key so a typo is surfaced, not silently stored.
	clean := make(map[string]string, len(req))
	for k, v := range req {
		if k == "key_fingerprint" || k == "storage" {
			continue // read-only, ignore if echoed back
		}
		cv, ok := coerceSetting(k, v)
		if !ok {
			errJSON(w, http.StatusBadRequest, "unknown or unsettable key: "+k)
			return
		}
		clean[k] = cv
	}
	for k, v := range clean {
		_ = s.store.SetSetting(k, v)
	}
	// Apply the live-tunable knobs immediately (F15) so a change takes effect
	// without a restart; the others (concurrency caps) apply on next restart.
	if v, ok := clean["db.ready_timeout_seconds"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			dockercli.SetDBReadyTimeout(n)
		}
	}
	if v, ok := clean["upload.max_mbps"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			s.engine.SetUploadLimit(n)
		}
	}
	if v, ok := clean["backup.sidecar_image"]; ok {
		dockercli.SetSidecarImage(v) // live-apply so the next volume backup uses it (F25)
	}
	if v, ok := clean["backup.max_concurrent"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			s.setMaxConcurrent(n) // live-resize the fleet-wide cap (F29)
		}
	}
	if v, ok := clean["backup.max_concurrent_per_node"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			s.setMaxConcurrentPerNode(n) // live-resize per-node caps (F29)
		}
	}
	if v, ok := clean["drill.boot_wait_seconds"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			dockercli.SetDrillBootWait(n) // live-apply so the next drill uses it (F30)
		}
	}
	if v, ok := clean["security.egress_allow"]; ok {
		s.applyEgress(v) // live atomic swap — no restart (F39)
	}
	if v, ok := clean["security.egress_audit"]; ok {
		// F207: live, and AUDITED. Turning audit mode ON stops a security control
		// from acting — that is its purpose, and it is exactly the kind of change
		// that should leave a record rather than being inferred later from a
		// setting nobody remembers flipping.
		on := v == "true"
		egress.SetAuditMode(on)
		if on {
			_ = s.store.Audit(userFrom(r), "egress.audit.on", "",
				"outbound allow-list is now OBSERVED, not enforced — connections it would refuse are recorded and allowed")
		} else {
			_ = s.store.Audit(userFrom(r), "egress.audit.off", "", "outbound allow-list is enforced again")
		}
	}
	if v, ok := clean["security.session_idle_minutes"]; ok {
		// F203: live-apply so a shortened idle window takes effect on the next
		// request rather than at the next restart — the direction that matters is
		// tightening, and a tightening that waits for a restart is not a control.
		// The absolute lifetime and the password minimum need no live-apply: both
		// are read fresh at the moment they are used (session creation, password
		// change), so the next sign-in already has the new value.
		if n, err := strconv.Atoi(v); err == nil {
			store.SetSessionIdleTTL(time.Duration(n) * time.Minute)
		}
	}
	// restore.health_timeout_seconds needs no live-apply: the gate reads the setting
	// fresh on every restore (backup.Engine.restoreHealthTimeout).
	_ = s.store.Audit(userFrom(r), "settings.update", "", fmt.Sprintf("%v", clean))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleEgressTest reports whether a host would be permitted by the current
// outbound allow-list (F39), so the operator can test a destination before saving
// it. Read-only — it evaluates the live policy, connects to nothing.
func (s *Server) handleEgressTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host string `json:"host"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	host := strings.TrimSpace(req.Host)
	if host == "" {
		errJSON(w, http.StatusBadRequest, "host is required")
		return
	}
	if err := egress.Default().Check(host); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleLogStream streams live log lines via Server-Sent Events (PLAN §4.11).
func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		errJSON(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, hist := s.bcast.subscribe()
	defer s.bcast.unsubscribe(ch)

	// A named event (node.summary / backup.status) is delivered via the SSE
	// "event:" field so only listeners for that type receive it; an unnamed
	// message stays the default log-line event, so the Logs page is unchanged.
	send := func(m sseMsg) {
		if m.event != "" {
			fmt.Fprintf(w, "event: %s\n", m.event)
		}
		fmt.Fprintf(w, "data: %s\n\n", m.data)
		flusher.Flush()
	}
	for _, m := range hist {
		send(m)
	}
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case m, ok := <-ch:
			if !ok {
				return
			}
			send(m)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// ---------------- Health & metrics (PLAN §9.12) ----------------

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleVersion returns the build-stamped app version (PLAN: no hardcoded UI
// values). "dev" for unstamped local builds.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": version.Version})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	// F45: /metrics is open by default, but the moment ANY metrics-scoped token
	// exists it is locked to one — so an operator can protect the endpoint simply by
	// minting a metrics token. A read/backup token does NOT grant metrics access.
	if n, _ := s.store.CountAPITokensWithScope("metrics"); n > 0 {
		ok := false
		if bt := bearerToken(r); bt != "" {
			// F65: an expired metrics token no longer opens the gate — but its
			// mere existence still keeps /metrics locked (expiry must never
			// silently re-open the endpoint to the public).
			// F201: the source pin applies here too — and this endpoint is the
			// reason the feature exists. A metrics token is the one most likely to
			// be pinned ("only Prometheus may scrape"), and it is resolved on this
			// path rather than through the auth middleware, so gating only the
			// middleware would have left exactly the motivating case open.
			if t, err := s.store.GetAPITokenByHash(sha256Hex(bt)); err == nil && scopeSet(t.Scopes)["metrics"] &&
				(t.ExpiresAt == 0 || time.Now().Unix() <= t.ExpiresAt) {
				ip := ""
				if t.AllowedCIDRs != "" {
					ip = s.clientIP(r)
				}
				if !tokenSourceAllowed(t.AllowedCIDRs, ip) {
					_ = s.store.Audit("token:"+t.Name, "token.denied_ip", t.Name,
						"metrics scrape from "+ip+", which is outside this token's allowed source addresses")
				} else {
					_ = s.store.TouchAPIToken(t.ID)
					ok = true
				}
			}
		}
		if !ok {
			errJSON(w, http.StatusUnauthorized, "metrics requires a valid metrics-scoped API token")
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	last := s.metrics.lastSuccessTS.Load()
	age := int64(-1)
	if last > 0 {
		age = time.Now().Unix() - last
	}
	fmt.Fprintf(w, "# HELP dockback_backups_total Total backups attempted.\n")
	fmt.Fprintf(w, "# TYPE dockback_backups_total counter\n")
	fmt.Fprintf(w, "dockback_backups_total %d\n", s.metrics.backupsTotal.Load())
	fmt.Fprintf(w, "# HELP dockback_backups_failed_total Total failed backups.\n")
	fmt.Fprintf(w, "# TYPE dockback_backups_failed_total counter\n")
	fmt.Fprintf(w, "dockback_backups_failed_total %d\n", s.metrics.backupsFailed.Load())
	fmt.Fprintf(w, "# HELP dockback_last_successful_backup_age_seconds Age of last successful backup.\n")
	fmt.Fprintf(w, "# TYPE dockback_last_successful_backup_age_seconds gauge\n")
	fmt.Fprintf(w, "dockback_last_successful_backup_age_seconds %d\n", age)
	fmt.Fprintf(w, "# HELP dockback_sse_terminal_events_dropped_total Run-completion events a live console could not be given.\n")
	fmt.Fprintf(w, "# TYPE dockback_sse_terminal_events_dropped_total counter\n")
	fmt.Fprintf(w, "dockback_sse_terminal_events_dropped_total %d\n", s.bcast.droppedEvents())
	fmt.Fprintf(w, "# HELP dockback_backups_queued Backups waiting for a concurrency slot.\n")
	fmt.Fprintf(w, "# TYPE dockback_backups_queued gauge\n")
	fmt.Fprintf(w, "dockback_backups_queued %d\n", s.queueDepth())
	// Staleness of the least-recently re-verified backup (scrub, PLAN §9.4/§9.12).
	fmt.Fprintf(w, "# HELP dockback_oldest_verified_age_seconds Age of the least-recently verified successful backup (-1 if none).\n")
	fmt.Fprintf(w, "# TYPE dockback_oldest_verified_age_seconds gauge\n")
	verAge := int64(-1)
	if at, ok := s.store.OldestVerified(); ok {
		verAge = time.Now().Unix() - at
	}
	fmt.Fprintf(w, "dockback_oldest_verified_age_seconds %d\n", verAge)

	// Restore drills (PLAN §9.4/§9.12): staleness of the least-recently drilled
	// stack and how many stacks last failed their drill.
	drillAge := int64(-1)
	drillFailing := 0
	if drills, err := s.store.ListDrills(); err == nil {
		var oldest int64
		for _, d := range drills {
			if oldest == 0 || d.RanAt < oldest {
				oldest = d.RanAt
			}
			if !d.OK {
				drillFailing++
			}
		}
		if oldest > 0 {
			drillAge = time.Now().Unix() - oldest
		}
	}
	fmt.Fprintf(w, "# HELP dockback_oldest_restore_drill_age_seconds Age of the least-recently drilled stack (-1 if none).\n")
	fmt.Fprintf(w, "# TYPE dockback_oldest_restore_drill_age_seconds gauge\n")
	fmt.Fprintf(w, "dockback_oldest_restore_drill_age_seconds %d\n", drillAge)
	fmt.Fprintf(w, "# HELP dockback_restore_drills_failing Stacks whose most recent restore drill failed.\n")
	fmt.Fprintf(w, "# TYPE dockback_restore_drills_failing gauge\n")
	fmt.Fprintf(w, "dockback_restore_drills_failing %d\n", drillFailing)

	// Standby readiness (F62 pilot-light rehearsals, exported for alerting — F75):
	// configured count, currently-failing count (a rehearsal ran and failed), and
	// the age of the least-recently proven standby. Store-only, no probes.
	sbConfigured, sbFailing := 0, 0
	var sbOldestRun int64
	if sbs, err := s.store.ListStandby(); err == nil {
		for _, sb := range sbs {
			sbConfigured++
			if sb.LastRun > 0 && !sb.LastOK {
				sbFailing++
			}
			if sb.LastRun > 0 && (sbOldestRun == 0 || sb.LastRun < sbOldestRun) {
				sbOldestRun = sb.LastRun
			}
		}
	}
	sbAge := int64(-1)
	if sbOldestRun > 0 {
		sbAge = time.Now().Unix() - sbOldestRun
	}
	fmt.Fprintf(w, "# HELP dockback_standby_configured Containers with a pilot-light standby configured.\n")
	fmt.Fprintf(w, "# TYPE dockback_standby_configured gauge\n")
	fmt.Fprintf(w, "dockback_standby_configured %d\n", sbConfigured)
	fmt.Fprintf(w, "# HELP dockback_standby_failing Standbys whose most recent rehearsal failed.\n")
	fmt.Fprintf(w, "# TYPE dockback_standby_failing gauge\n")
	fmt.Fprintf(w, "dockback_standby_failing %d\n", sbFailing)
	fmt.Fprintf(w, "# HELP dockback_oldest_standby_age_seconds Age of the least-recently rehearsed standby (-1 if none rehearsed).\n")
	fmt.Fprintf(w, "# TYPE dockback_oldest_standby_age_seconds gauge\n")
	fmt.Fprintf(w, "dockback_oldest_standby_age_seconds %d\n", sbAge)

	// Projected days until each destination fills up (PLAN §9.13/§9.12). Computed
	// from stored capacity history only (no live probe), so a scrape stays cheap.
	fmt.Fprintf(w, "# HELP dockback_destination_days_to_full Projected days until a destination fills up (-1 = not projected).\n")
	fmt.Fprintf(w, "# TYPE dockback_destination_days_to_full gauge\n")
	if ds, err := s.store.ListDestinations(); err == nil {
		for _, d := range ds {
			f := s.destForecast(d.ID, 0, 0) // 0,0 ⇒ use newest stored sample
			fmt.Fprintf(w, "dockback_destination_days_to_full{destination=\"%s\"} %d\n", promLabel(d.Name), f.DaysToFull)
		}
	}

	// Critical databases whose newest successful backup is older than their target
	// RPO (or have none) — the low-RPO/PITR alternative (PLAN §9.7/§9.12).
	breaches, critTotal := s.criticalRPOBreaches()
	fmt.Fprintf(w, "# HELP dockback_critical_databases Databases marked for low-RPO protection.\n")
	fmt.Fprintf(w, "# TYPE dockback_critical_databases gauge\n")
	fmt.Fprintf(w, "dockback_critical_databases %d\n", critTotal)
	fmt.Fprintf(w, "# HELP dockback_rpo_breaches Critical databases currently exceeding their target RPO.\n")
	fmt.Fprintf(w, "# TYPE dockback_rpo_breaches gauge\n")
	fmt.Fprintf(w, "dockback_rpo_breaches %d\n", breaches)

	// Per-node, node-keyed gauges (PLAN §4.13). Derived from the inventory cache +
	// store, so a scrape is cheap and never blocks on Docker.
	nodes, _ := s.store.ListNodes()
	lastByNode, _ := s.store.LastSuccessfulByNode()
	now := time.Now().Unix()
	day := todayKey()
	type nrow struct {
		name, id, cluster string
		reachable, runnng int
		eventsTotal       int
		lastAge           int64
	}
	rows := make([]nrow, 0, len(nodes))
	for _, n := range nodes {
		r := nrow{name: n.Name, id: n.ID, cluster: n.Cluster, lastAge: -1}
		if st := s.getStat(n.ID); st != nil {
			if st.Reachable {
				r.reachable = 1
			}
			if st.Summary != nil {
				r.runnng = st.Summary.Running
			}
		}
		r.eventsTotal, _, _ = s.store.GetNodeEvents(n.ID, day)
		if t, ok := lastByNode[n.ID]; ok && t > 0 {
			r.lastAge = now - t
		}
		rows = append(rows, r)
	}
	// F104: the cluster is an ADDED label on the existing series, not a new series
	// name — anything already summing these gauges keeps working and gains a free
	// `by (cluster)` split.
	lbl := func(r nrow) string {
		return fmt.Sprintf("{node=%q,node_id=%q,cluster=%q}", promLabel(r.name), r.id, promLabel(r.cluster))
	}

	fmt.Fprintf(w, "# HELP dockback_node_reachable Whether a node is currently reachable (1/0).\n# TYPE dockback_node_reachable gauge\n")
	for _, r := range rows {
		fmt.Fprintf(w, "dockback_node_reachable%s %d\n", lbl(r), r.reachable)
	}
	fmt.Fprintf(w, "# HELP dockback_node_containers_running Running containers on a node.\n# TYPE dockback_node_containers_running gauge\n")
	for _, r := range rows {
		fmt.Fprintf(w, "dockback_node_containers_running%s %d\n", lbl(r), r.runnng)
	}
	fmt.Fprintf(w, "# HELP dockback_node_events_total Docker events observed on a node since monitoring started.\n# TYPE dockback_node_events_total counter\n")
	for _, r := range rows {
		fmt.Fprintf(w, "dockback_node_events_total%s %d\n", lbl(r), r.eventsTotal)
	}
	fmt.Fprintf(w, "# HELP dockback_node_last_successful_backup_age_seconds Age of the newest successful backup on a node (-1 if none).\n# TYPE dockback_node_last_successful_backup_age_seconds gauge\n")
	for _, r := range rows {
		fmt.Fprintf(w, "dockback_node_last_successful_backup_age_seconds%s %d\n", lbl(r), r.lastAge)
	}
}

// promLabel escapes a Prometheus label value (backslash, quote, newline).
func promLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// handleRepinSidecar clears a node's pinned volume-sidecar image so the next use
// records whatever is present (F88).
//
// This is the deliberate "yes, I changed that image" action. It does NOT pin a
// digest supplied by the caller: the pin must always be something DockBack
// observed itself, or the refusal could be waved through with an attacker's
// value. Clearing is safe because the next sidecar run re-pins under the same
// trust-on-first-use rule that established the original.
func (s *Server) handleRepinSidecar(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n, err := s.store.GetNode(id)
	if err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	old, _ := s.store.GetSidecarPin(id)
	if err := s.store.SetSidecarPin(id, ""); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "node.sidecar.repin", n.Name,
		"cleared the pinned volume sidecar image (was "+old+"); it re-pins on next use")
	writeJSON(w, http.StatusOK, map[string]any{"status": "repinned", "previous": old})
}

// sidecarPinView reports a node's pinned sidecar image for the node page.
func (s *Server) sidecarPinView(nodeID string) map[string]any {
	pin, ok := s.store.GetSidecarPin(nodeID)
	ref, digest := dockercli.SplitSidecarPin(pin)
	if ref == "" {
		ref = dockercli.SidecarImage()
	}
	return map[string]any{"image": ref, "digest": digest, "pinned": ok}
}
