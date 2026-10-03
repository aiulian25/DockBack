package dockercli

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

// Compose labels Docker writes on stack members (PLAN §2.14 discovery).
const (
	labelProject     = "com.docker.compose.project"
	labelService     = "com.docker.compose.service"
	labelWorkingDir  = "com.docker.compose.project.working_dir"
	labelConfigFiles = "com.docker.compose.project.config_files"
)

// Container is a discovered container, trimmed to what the UI/backup need.
type Container struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Image     string            `json:"image"`
	ImageID   string            `json:"image_id"`
	State     string            `json:"state"`  // running|exited|paused|restarting|...
	Status    string            `json:"status"` // human status string
	Ports     string            `json:"ports"`
	Stack     string            `json:"stack"`   // compose project ("" if standalone)
	Service   string            `json:"service"` // compose service
	CreatedAt int64             `json:"created_at"`
	Mounts    []Mount           `json:"mounts"`
	Labels    map[string]string `json:"-"`
}

// Mount is a volume/bind attached to a container (drives volume backup, PLAN §4).
type Mount struct {
	Type        string `json:"type"` // volume|bind|tmpfs
	Name        string `json:"name"` // volume name (volumes only)
	Source      string `json:"source"`
	Destination string `json:"destination"`
	RW          bool   `json:"rw"`
}

// Stack groups containers by compose project.
type Stack struct {
	Name        string       `json:"name"`
	WorkingDir  string       `json:"working_dir"`
	ConfigFiles string       `json:"config_files"`
	Containers  []*Container `json:"containers"`
	Running     int          `json:"running"`
	Total       int          `json:"total"`
}

// NodeSummary is the per-node dashboard card payload (PLAN §5.4 hub(5)).
type NodeSummary struct {
	Running    int `json:"running"`
	Stopped    int `json:"stopped"`
	Paused     int `json:"paused"`
	Restarting int `json:"restarting"`
	Total      int `json:"total"`
	Images     int `json:"images"`
	Volumes    int `json:"volumes"`
	Networks   int `json:"networks"`
	Stacks     int `json:"stacks"`

	// Stack health breakdown (running / stopped / errored).
	StacksRunning int `json:"stacks_running"`
	StacksStopped int `json:"stacks_stopped"`
	StacksErrored int `json:"stacks_errored"`

	// Live resource usage (CPU % across cores, memory used vs host total).
	CPUPercent float64 `json:"cpu_percent"`
	MemUsed    int64   `json:"mem_used"`
	MemTotal   int64   `json:"mem_total"`
	MemPercent float64 `json:"mem_percent"`

	// Cumulative container network RX/TX bytes (UI derives live rates by diffing).
	NetRx int64 `json:"net_rx"`
	NetTx int64 `json:"net_tx"`

	// Docker events.
	EventsToday int `json:"events_today"`
	EventsTotal int `json:"events_total"`
}

// ListContainers returns all containers on a node (running + stopped).
func ListContainers(ctx context.Context, c *client.Client) ([]*Container, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	raw, err := c.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	out := make([]*Container, 0, len(raw))
	for _, rc := range raw {
		name := ""
		if len(rc.Names) > 0 {
			name = strings.TrimPrefix(rc.Names[0], "/")
		}
		cc := &Container{
			ID:        rc.ID,
			Name:      name,
			Image:     rc.Image,
			ImageID:   rc.ImageID,
			State:     rc.State,
			Status:    rc.Status,
			Ports:     formatPorts(rc),
			Stack:     rc.Labels[labelProject],
			Service:   rc.Labels[labelService],
			CreatedAt: rc.Created,
			Labels:    rc.Labels,
		}
		for _, m := range rc.Mounts {
			cc.Mounts = append(cc.Mounts, Mount{
				Type:        string(m.Type),
				Name:        m.Name,
				Source:      m.Source,
				Destination: m.Destination,
				RW:          m.RW,
			})
		}
		out = append(out, cc)
	}
	sort.Slice(out, func(i, j int) bool { return lessName(out[i].Name, out[j].Name) })
	return out, nil
}

// lessName orders names alphabetically, case-insensitively (so "PaperlessNGX"
// sorts among the p's, not ahead of lowercase "docker"), with a case-sensitive
// tie-break so the order stays deterministic for names that differ only by case.
func lessName(a, b string) bool {
	la, lb := strings.ToLower(a), strings.ToLower(b)
	if la != lb {
		return la < lb
	}
	return a < b
}

// GroupStacks groups containers into compose stacks (standalone containers go
// under the empty-name stack handled by callers).
func GroupStacks(cs []*Container) []*Stack {
	byProj := map[string]*Stack{}
	for _, c := range cs {
		proj := c.Stack
		if proj == "" {
			continue // standalone; listed separately by the UI
		}
		st, ok := byProj[proj]
		if !ok {
			st = &Stack{
				Name:        proj,
				WorkingDir:  c.Labels[labelWorkingDir],
				ConfigFiles: c.Labels[labelConfigFiles],
			}
			byProj[proj] = st
		}
		st.Containers = append(st.Containers, c)
		st.Total++
		if c.State == "running" {
			st.Running++
		}
	}
	out := make([]*Stack, 0, len(byProj))
	for _, st := range byProj {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return lessName(out[i].Name, out[j].Name) })
	return out
}

// Inventory builds the dashboard node summary with a handful of cheap API
// calls (no per-container stats — PLAN §4.13 says don't hammer the daemon).
func Inventory(ctx context.Context, c *client.Client) (*NodeSummary, error) {
	_, _, sum, err := InventorySnapshot(ctx, c, true)
	return sum, err
}

// InventorySnapshot lists a node's containers once and derives the full cached
// inventory from that single pass: the container list, the grouped compose
// stacks, and the dashboard summary (PLAN §4.13). When withStats is false the
// expensive per-container CPU/memory/network sampling and the 30-day event count
// are skipped — used by the event-driven "lite" refresh, which carries the last
// sampled stats forward so stats stay interval-sampled, not recomputed per event.
func InventorySnapshot(ctx context.Context, c *client.Client, withStats bool) ([]*Container, []*Stack, *NodeSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	cs, err := ListContainers(ctx, c)
	if err != nil {
		return nil, nil, nil, err
	}
	sum := &NodeSummary{Total: len(cs)}
	var runningIDs []string
	for _, cc := range cs {
		switch cc.State {
		case "running":
			sum.Running++
			runningIDs = append(runningIDs, cc.ID)
		case "paused":
			sum.Paused++
		case "restarting":
			sum.Restarting++
		default:
			sum.Stopped++
		}
	}

	// Stacks + health breakdown (running = all members running).
	stacks := GroupStacks(cs)
	sum.Stacks = len(stacks)
	for _, st := range stacks {
		switch {
		case st.Running == st.Total && st.Total > 0:
			sum.StacksRunning++
		case st.Running == 0:
			sum.StacksStopped++
		default:
			sum.StacksErrored++ // partially up
		}
	}

	if imgs, err := c.ImageList(ctx, image.ListOptions{}); err == nil {
		sum.Images = len(imgs)
	}
	if vols, err := c.VolumeList(ctx, volume.ListOptions{}); err == nil {
		sum.Volumes = len(vols.Volumes)
	}
	if nets, err := c.NetworkList(ctx, network.ListOptions{}); err == nil {
		sum.Networks = len(nets)
	}

	if withStats {
		// Live CPU/memory/network (best-effort). Event counts are NOT taken from the
		// daemon here — its event log is capped at ~256, so a 30-day query is
		// meaningless; the API layer overlays a real persisted counter instead
		// (PLAN §5.4, fed by the per-node event-stream watcher).
		sum.CPUPercent, sum.MemUsed, sum.MemTotal, sum.NetRx, sum.NetTx = NodeStats(ctx, c, runningIDs)
		if sum.MemTotal > 0 {
			sum.MemPercent = float64(sum.MemUsed) / float64(sum.MemTotal) * 100.0
		}
	}
	return cs, stacks, sum, nil
}

// PublishedHostPorts reads the HOST ports out of a Container's formatted Ports
// string (F144) — "80:80, 443:443, 3000" yields [80, 443]; an entry with no
// host side is not published and cannot conflict with anything.
//
// Parsing the display string rather than re-listing containers is deliberate:
// the caller answers "is this port already taken on the target?" from the
// CACHED inventory, so opening a restore dialog costs no Docker round-trip on a
// production host.
func PublishedHostPorts(ports string) []int {
	var out []int
	for _, part := range strings.Split(ports, ",") {
		part = strings.TrimSpace(part)
		host, _, ok := strings.Cut(part, ":")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(host)); err == nil && n > 0 && n <= 65535 {
			out = append(out, n)
		}
	}
	return out
}

func formatPorts(c types.Container) string {
	var parts []string
	seen := map[string]struct{}{}
	for _, p := range c.Ports {
		var s string
		if p.PublicPort != 0 {
			s = strconv.Itoa(int(p.PublicPort)) + ":" + strconv.Itoa(int(p.PrivatePort))
		} else {
			s = strconv.Itoa(int(p.PrivatePort))
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}
