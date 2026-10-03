package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
)

// Machine page API (F105).
//
// Each probe spawns a short-lived container on the node, so the endpoint is
// STALE-WHILE-REVALIDATE: a request is answered from cache immediately and, when
// that cache is old, a refresh runs in the BACKGROUND. An HTTP request only ever
// waits for a probe when nothing has ever been recorded for that node.
//
// That shape matters for three reasons learned the hard way:
//
//   - Page load must never block on a container starting on a remote host.
//   - A probe tied to the request context died the moment the request returned,
//     so a slow node could never finish one — it timed out forever.
//   - A node that is briefly slow should show its last known hardware, not blank
//     out. Hardware barely changes; a two-minute-old CPU model is still correct.
//
// The last good probe is also PERSISTED, so an app restart doesn't send every
// node back to a cold, blocking first load.

const (
	// machineFresh is how long a probe is considered current. Older than this and
	// a background refresh is kicked off — the caller still gets the cached copy.
	machineFresh = 15 * time.Second
	// machineProbeBudget bounds a background probe. Generous, because it is not on
	// anyone's critical path.
	machineProbeBudget = 4 * time.Minute
	// machineCacheKey namespaces the persisted copy in the settings table.
	machineCacheKey = "machine.cache."
)

type machineEntry struct {
	info *dockercli.MachineInfo
	at   time.Time
	err  error // last probe error; kept so a failing node can say WHY
}

type machineCache struct {
	mu       sync.Mutex
	entries  map[string]*machineEntry
	inflight map[string]bool
}

func newMachineCache() *machineCache {
	return &machineCache{entries: map[string]*machineEntry{}, inflight: map[string]bool{}}
}

// persisted is the on-disk shape of a cached probe.
type persistedMachine struct {
	Info *dockercli.MachineInfo `json:"info"`
	At   int64                  `json:"at"`
}

// load returns the in-memory entry, falling back to the persisted copy so a
// restart doesn't cost every node a cold blocking probe. Caller holds no lock.
func (s *Server) machineLoad(nodeID string) *machineEntry {
	s.machines.mu.Lock()
	e := s.machines.entries[nodeID]
	s.machines.mu.Unlock()
	if e != nil {
		return e
	}
	raw, _ := s.store.GetSetting(machineCacheKey+nodeID, "")
	if raw == "" {
		return nil
	}
	var p persistedMachine
	if json.Unmarshal([]byte(raw), &p) != nil || p.Info == nil {
		return nil
	}
	e = &machineEntry{info: p.Info, at: time.Unix(p.At, 0)}
	s.machines.mu.Lock()
	s.machines.entries[nodeID] = e
	s.machines.mu.Unlock()
	return e
}

// machineStore records a probe result in memory, and persists it when it
// succeeded. A FAILURE never overwrites a good persisted probe: the hardware
// didn't change because the host was briefly unreachable, and throwing away a
// valid reading would leave the page with nothing to show.
func (s *Server) machineStore(nodeID string, info *dockercli.MachineInfo, err error) {
	s.machines.mu.Lock()
	prev := s.machines.entries[nodeID]
	if err != nil && prev != nil && prev.info != nil {
		// Keep the good data, record the fresh error, but do NOT advance the
		// timestamp — the page should keep saying how old the real reading is.
		prev.err = err
		s.machines.mu.Unlock()
		return
	}
	s.machines.entries[nodeID] = &machineEntry{info: info, at: time.Now(), err: err}
	s.machines.mu.Unlock()

	if err == nil && info != nil {
		if b, merr := json.Marshal(persistedMachine{Info: info, At: time.Now().Unix()}); merr == nil {
			_ = s.store.SetSetting(machineCacheKey+nodeID, string(b))
		}
	}
}

// machineRefresh runs a probe in the background, unless one is already running
// for this node. Deliberately uses context.Background(): a probe outliving the
// request that triggered it is the entire point.
func (s *Server) machineRefresh(nodeID string, cli *client.Client) bool {
	s.machines.mu.Lock()
	if s.machines.inflight[nodeID] {
		s.machines.mu.Unlock()
		return true
	}
	s.machines.inflight[nodeID] = true
	s.machines.mu.Unlock()

	go func() {
		defer func() {
			s.machines.mu.Lock()
			delete(s.machines.inflight, nodeID)
			s.machines.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), machineProbeBudget)
		defer cancel()
		info, err := dockercli.ProbeMachine(ctx, cli)
		s.machineStore(nodeID, info, err)
	}()
	return true
}

// machineScanning reports whether a probe is running for this node right now.
func (s *Server) machineScanning(nodeID string) bool {
	s.machines.mu.Lock()
	defer s.machines.mu.Unlock()
	return s.machines.inflight[nodeID]
}

// forget drops a node's cached probe (on node removal, so a recycled id can't
// inherit another machine's hardware).
func (s *Server) machineForget(nodeID string) {
	s.machines.mu.Lock()
	delete(s.machines.entries, nodeID)
	s.machines.mu.Unlock()
	_ = s.store.SetSetting(machineCacheKey+nodeID, "")
}

// machineResp carries the probe plus the Docker-side facts, so the page renders
// something useful even when the probe itself fails (an old daemon, a proxy that
// forbids container create, a host with no /sys).
type machineResp struct {
	NodeID   string                 `json:"node_id"`
	NodeName string                 `json:"node_name"`
	Host     *dockercli.HostInfo    `json:"host,omitempty"`
	Machine  *dockercli.MachineInfo `json:"machine,omitempty"`
	Docker   machineDockerInfo      `json:"docker"`
	Error    string                 `json:"error,omitempty"`
	// AgeSeconds is how old the shown reading is; Refreshing says a probe is
	// running right now. Together they let the page be honest about serving a
	// cached copy instead of pretending everything is live.
	AgeSeconds int  `json:"age_seconds"`
	Refreshing bool `json:"refreshing"`
	// Scanning means a FIRST reading is being taken and there is nothing to show
	// yet. It exists so the page can say "scanning this machine" rather than
	// leading with a failure it has not actually observed — a scan in progress is
	// not a scan that failed, and showing red before anything went wrong is just
	// alarming.
	Scanning bool `json:"scanning"`
	TTL      int  `json:"ttl_seconds"`
}

type machineDockerInfo struct {
	Version    string `json:"version,omitempty"`
	Containers int    `json:"containers"`
	Running    int    `json:"running"`
	Images     int    `json:"images"`
	Volumes    int    `json:"volumes"`
}

// handleNodeMachine returns a node's hardware + live utilisation.
//
// It answers from cache wherever possible and never blocks on a probe unless the
// node has never been probed at all.
func (s *Server) handleNodeMachine(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n, err := s.store.GetNode(id)
	if err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	cli, err := s.reg.Get(id)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}

	resp := machineResp{NodeID: n.ID, NodeName: n.Name, TTL: int(machineFresh / time.Second)}

	// Docker-side facts: cheap, always available, and what the page falls back to.
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if host, herr := dockercli.Info(ctx, cli); herr == nil {
		resp.Host = host
		resp.Docker.Version = host.DockerVersion
	}
	if st := s.getStat(id); st != nil && st.Summary != nil {
		resp.Docker.Containers = st.Summary.Total
		resp.Docker.Running = st.Summary.Running
		resp.Docker.Images = st.Summary.Images
		resp.Docker.Volumes = st.Summary.Volumes
	}

	force := r.URL.Query().Get("refresh") == "1"
	e := s.machineLoad(id)

	switch {
	case e != nil && e.info != nil:
		// Serve what we have IMMEDIATELY. Hardware barely changes, so a slightly
		// old reading is right; only the live counters age, and the page says how.
		resp.Machine = e.info
		resp.AgeSeconds = int(time.Since(e.at).Seconds())
		if e.err != nil {
			resp.Error = e.err.Error()
		}
		if force || time.Since(e.at) > machineFresh {
			resp.Refreshing = s.machineRefresh(id, cli)
		}

	case s.machineScanning(id):
		// A first reading is being taken. Report it as SCANNING, with no error:
		// nothing has failed, we simply do not know yet.
		resp.Scanning = true
		resp.Refreshing = true

	case e != nil && e.err != nil && !force:
		// A scan actually ran and actually failed. This is the only state that
		// warrants alarming the operator — and it does NOT auto-retry, because
		// re-probing on every 15s poll would hammer a host that is already
		// struggling. "Re-probe" is the way back.
		resp.Error = e.err.Error()

	default:
		// Never scanned (or an explicit Re-probe). Start it in the BACKGROUND and
		// return immediately — page load must never wait on a container starting
		// on a remote host, and the poll below picks up the result.
		resp.Scanning = true
		resp.Refreshing = s.machineRefresh(id, cli)
	}
	writeJSON(w, http.StatusOK, resp)
}
