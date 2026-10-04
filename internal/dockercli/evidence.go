package dockercli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

// Evidence is everything a node's Docker can say about how its containers are
// put together, at one moment (step 28): every container's inspect record, the
// networks and the volumes. In the 2026-10-04 recovery DockBack itself had to be
// rebuilt from `docker inspect` because its own folder was gone; this is that
// record for a whole node, taken before anything else is touched.
//
// Environment values are reduced to names, and logging options to their keys:
// those are where secrets live. Everything else is kept as Docker reports it.
type Evidence struct {
	Containers []json.RawMessage   `json:"containers"`
	Networks   []network.Inspect   `json:"networks"`
	Volumes    []*volume.Volume    `json:"volumes"`
	Skipped    []map[string]string `json:"skipped,omitempty"`
}

// CollectEvidence reads the node's containers, networks and volumes. A
// container that cannot be inspected is listed under Skipped with the reason,
// rather than failing the whole record.
func CollectEvidence(ctx context.Context, c *client.Client) (*Evidence, error) {
	list, err := c.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	ev := &Evidence{}
	for _, ctr := range list {
		_, raw, ierr := c.ContainerInspectWithRaw(ctx, ctr.ID, false)
		if ierr != nil {
			ev.Skipped = append(ev.Skipped, map[string]string{"id": ctr.ID, "reason": ierr.Error()})
			continue
		}
		redacted, rerr := redactInspect(raw)
		if rerr != nil {
			ev.Skipped = append(ev.Skipped, map[string]string{"id": ctr.ID, "reason": rerr.Error()})
			continue
		}
		ev.Containers = append(ev.Containers, redacted)
	}
	if ev.Networks, err = c.NetworkList(ctx, network.ListOptions{}); err != nil {
		return nil, fmt.Errorf("listing networks: %w", err)
	}
	vols, err := c.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing volumes: %w", err)
	}
	ev.Volumes = vols.Volumes
	return ev, nil
}

// redactInspect keeps an inspect record whole except for its secrets: each
// environment entry becomes its name, and each logging option keeps its key
// with the value removed. Pure.
func redactInspect(raw []byte) (json.RawMessage, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if cfg, ok := doc["Config"].(map[string]any); ok {
		if env, ok := cfg["Env"].([]any); ok {
			names := make([]any, 0, len(env))
			for _, entry := range env {
				if s, ok := entry.(string); ok {
					name, _, _ := strings.Cut(s, "=")
					names = append(names, name)
				}
			}
			cfg["Env"] = names
		}
	}
	if hc, ok := doc["HostConfig"].(map[string]any); ok {
		if lc, ok := hc["LogConfig"].(map[string]any); ok {
			if opts, ok := lc["Config"].(map[string]any); ok {
				for key := range opts {
					opts[key] = redactedValue
				}
			}
		}
	}
	return json.Marshal(doc)
}

// redactedValue stands in for a removed secret.
const redactedValue = "<removed>"
