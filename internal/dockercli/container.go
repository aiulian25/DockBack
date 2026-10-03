package dockercli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/docker/docker/client"
)

// ContainerDetail is the per-container payload for the backup-management page
// (PLAN §5.4 hub(1)). All fields come from a live inspect + one stats read.
type ContainerDetail struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Image       string  `json:"image"`
	ImageDigest string  `json:"image_digest"`
	State       string  `json:"state"`
	Uptime      string  `json:"uptime"`
	StartedAt   string  `json:"started_at"`
	IP          string  `json:"ip"`
	Health      string  `json:"health"`
	Description string  `json:"description"`
	Command     string  `json:"command"`
	Stack       string  `json:"stack"`
	Service     string  `json:"service"`
	Ports       string  `json:"ports"`
	CreatedAt   int64   `json:"created_at"`
	CPUPercent  float64 `json:"cpu_percent"`
	MemUsed     int64   `json:"mem_used"`
	MemLimit    int64   `json:"mem_limit"`
	VolumeCount int     `json:"volume_count"`
	IsDatabase  bool    `json:"is_database"`
	// RunAs is the "PUID/PGID=1000:1000" this image ANNOUNCES it drops privileges
	// to, or "" for an image that announces nothing (F184). Computed here, where
	// the inspect is already in hand, so the container page can show what the
	// ownership override would be replacing without a second round-trip.
	RunAs string `json:"run_as,omitempty"`
	// Labels carries the container's raw labels for server-side use (e.g. the F19
	// dockback.* policy parse). Not serialized to the client.
	Labels map[string]string `json:"-"`
	// Raw is the full inspect JSON for server-side config-drift comparison (F73).
	// Not serialized to the client.
	Raw []byte `json:"-"`
}

// InspectContainer returns the detail for one container.
func InspectContainer(ctx context.Context, c *client.Client, id string) (*ContainerDetail, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	insp, err := c.ContainerInspect(ctx, id)
	if err != nil {
		return nil, err
	}
	d := &ContainerDetail{
		ID:        insp.ID,
		Name:      strings.TrimPrefix(insp.Name, "/"),
		Image:     insp.Config.Image,
		Command:   strings.Join(insp.Config.Cmd, " "),
		StartedAt: insp.State.StartedAt,
	}
	// Raw (F73): the full inspect JSON for server-side config-drift comparison —
	// never serialized to the client (json:"-").
	d.Raw, _ = json.Marshal(insp)
	d.State = insp.State.Status
	if uid, gid, key, ok := RunAsIDs(insp.Config.Env); ok {
		d.RunAs = fmt.Sprintf("%s=%d:%d", key, uid, gid)
	}
	d.Labels = insp.Config.Labels
	d.Stack = insp.Config.Labels["com.docker.compose.project"]
	d.Service = insp.Config.Labels["com.docker.compose.service"]
	d.Description = firstNonEmptyStr(
		insp.Config.Labels["org.opencontainers.image.description"],
		insp.Config.Labels["com.docker.compose.service"],
	)
	if t, e := time.Parse(time.RFC3339, insp.Created); e == nil {
		d.CreatedAt = t.Unix()
	}
	if insp.State.Running {
		if t, e := time.Parse(time.RFC3339Nano, insp.State.StartedAt); e == nil {
			d.Uptime = humanDur(time.Since(t))
		}
	}
	if insp.State.Health != nil {
		d.Health = insp.State.Health.Status // healthy|unhealthy|starting
	}
	if insp.NetworkSettings != nil {
		for _, n := range insp.NetworkSettings.Networks {
			if n.IPAddress != "" {
				d.IP = n.IPAddress
				break
			}
		}
		var ports []string
		for p := range insp.NetworkSettings.Ports {
			ports = append(ports, string(p))
		}
		d.Ports = strings.Join(ports, ", ")
	}
	for _, m := range insp.Mounts {
		if string(m.Type) == "volume" {
			d.VolumeCount++
		}
	}
	d.IsDatabase = isDBImage(insp.Config.Image)

	// Image digest for restore-by-digest display.
	if img, _, e := c.ImageInspectWithRaw(ctx, insp.Image); e == nil && len(img.RepoDigests) > 0 {
		d.ImageDigest = img.RepoDigests[0]
	}

	// Live CPU/memory (single read).
	if insp.State.Running {
		d.CPUPercent, d.MemUsed, d.MemLimit = ContainerStat(ctx, c, id)
	}
	return d, nil
}

// ContainerStat reads CPU% and memory for a single container.
func ContainerStat(ctx context.Context, c *client.Client, id string) (cpu float64, memUsed, memLimit int64) {
	resp, err := c.ContainerStats(ctx, id, false)
	if err != nil {
		return 0, 0, 0
	}
	var s statsJSON
	derr := json.NewDecoder(resp.Body).Decode(&s)
	resp.Body.Close()
	if derr != nil {
		return 0, 0, 0
	}
	return calcCPU(&s), int64(calcMem(&s)), int64(s.MemoryStats.Limit)
}

func isDBImage(image string) bool {
	i := strings.ToLower(image)
	return strings.Contains(i, "postgres") || strings.Contains(i, "mysql") ||
		strings.Contains(i, "mariadb") || strings.Contains(i, "mongo") || strings.Contains(i, "redis")
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func humanDur(d time.Duration) string {
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	m := int(d.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("%dd %02dh %02dm", days, h, m)
	}
	if h > 0 {
		return fmt.Sprintf("%dh %02dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}
