package dockercli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

// Network topology capture & faithful restore (F89).
//
// Before this, a disaster-recovery restore recreated any missing network as a
// BARE bridge — `network.CreateOptions{Driver: "bridge"}` and nothing else — and
// reattached endpoints with aliases only. Three things were silently lost:
//
//   - the SUBNET, so addresses a service has pinned in its own config no longer
//     exist on the network it comes back on;
//   - the container's STATIC IP, so anything referencing it by address breaks;
//   - the `internal` flag, so a network deliberately cut off from the outside
//     came back with egress — a silent loss of isolation, which is the one that
//     matters most.
//
// This records each attached network's definition and the endpoint's address at
// backup time, and applies them on restore.

// NetworkPool is one IPAM pool. Most networks have exactly one; dual-stack and
// multi-pool networks have several, and none of them are dropped.
type NetworkPool struct {
	Subnet  string `json:"subnet,omitempty"`
	Gateway string `json:"gateway,omitempty"`
	IPRange string `json:"ip_range,omitempty"`
}

// NetworkSpec is one attached network's full definition plus THIS container's
// endpoint on it.
type NetworkSpec struct {
	Name       string `json:"name"`
	Driver     string `json:"driver,omitempty"`
	Scope      string `json:"scope,omitempty"`
	Internal   bool   `json:"internal,omitempty"`
	Attachable bool   `json:"attachable,omitempty"`
	EnableIPv6 bool   `json:"enable_ipv6,omitempty"`
	// Primary pool, flattened for display and for the common single-pool case.
	Subnet  string `json:"subnet,omitempty"`
	Gateway string `json:"gateway,omitempty"`
	IPRange string `json:"ip_range,omitempty"`
	// Pools carries EVERY pool when the network has more than one (dual-stack).
	// Populated only in that case, so a normal single-pool network records the
	// same thing it always would and the manifest stays free of duplication.
	Pools   []NetworkPool     `json:"pools,omitempty"`
	Options map[string]string `json:"options,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
	// This container's endpoint on the network.
	IPv4    string   `json:"ipv4,omitempty"`
	IPv6    string   `json:"ipv6,omitempty"`
	Aliases []string `json:"aliases,omitempty"`
}

// allPools returns every pool, whether the spec stored one or many.
func (s NetworkSpec) allPools() []NetworkPool {
	if len(s.Pools) > 0 {
		return s.Pools
	}
	if s.Subnet == "" && s.Gateway == "" && s.IPRange == "" {
		return nil
	}
	return []NetworkPool{{Subnet: s.Subnet, Gateway: s.Gateway, IPRange: s.IPRange}}
}

// InspectNetworks records the definition of every user-defined network a
// container is attached to, plus its endpoint on each.
//
// Best-effort by construction: a network that cannot be inspected is recorded
// from the endpoint alone rather than dropped, because knowing "it was on a
// network called X at 172.20.0.5" is still worth having.
//
// Docker's built-in modes (bridge/host/none) are skipped — they exist on every
// host and must never be recreated.
func InspectNetworks(ctx context.Context, c *client.Client, insp types.ContainerJSON) []NetworkSpec {
	if insp.NetworkSettings == nil || len(insp.NetworkSettings.Networks) == 0 {
		return nil
	}
	out := make([]NetworkSpec, 0, len(insp.NetworkSettings.Networks))
	for name, ep := range insp.NetworkSettings.Networks {
		if !isCustomNetwork(name) {
			continue
		}
		spec := NetworkSpec{Name: name}
		if ep != nil {
			spec.Aliases = cleanAliases(ep.Aliases, insp.Config.Hostname)
			// The STATIC address is the one the user asked for (IPAMConfig), not
			// the one the daemon happened to hand out. Recording the assigned
			// address instead would pin a lease that was never requested, and make
			// a dynamic container fail to restore when that address is taken.
			if ep.IPAMConfig != nil {
				spec.IPv4, spec.IPv6 = ep.IPAMConfig.IPv4Address, ep.IPAMConfig.IPv6Address
			}
		}
		if n, err := c.NetworkInspect(ctx, name, network.InspectOptions{}); err == nil {
			spec.Driver, spec.Scope = n.Driver, n.Scope
			spec.Internal, spec.Attachable, spec.EnableIPv6 = n.Internal, n.Attachable, n.EnableIPv6
			spec.Options, spec.Labels = copyMap(n.Options), copyMap(n.Labels)
			pools := make([]NetworkPool, 0, len(n.IPAM.Config))
			for _, cfg := range n.IPAM.Config {
				pools = append(pools, NetworkPool{Subnet: cfg.Subnet, Gateway: cfg.Gateway, IPRange: cfg.IPRange})
			}
			if len(pools) > 0 {
				spec.Subnet, spec.Gateway, spec.IPRange = pools[0].Subnet, pools[0].Gateway, pools[0].IPRange
			}
			if len(pools) > 1 {
				spec.Pools = pools
			}
		}
		out = append(out, spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func copyMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ensureNetworkSpec creates a network with its RECORDED properties, or reports
// how an existing one differs.
//
// It never MUTATES a network that already exists. Changing the subnet or the
// internal flag of a live network would disrupt every other service on it, and a
// restore of one container has no business doing that. Divergence is reported so
// the operator can decide.
func ensureNetworkSpec(ctx context.Context, c *client.Client, spec NetworkSpec) []string {
	if !isCustomNetwork(spec.Name) {
		return nil
	}
	if existing, err := c.NetworkInspect(ctx, spec.Name, network.InspectOptions{}); err == nil {
		return diffNetwork(spec, existing)
	}

	opts := network.CreateOptions{
		Driver:     firstNonEmptyStr(spec.Driver, "bridge"),
		Internal:   spec.Internal,
		Attachable: spec.Attachable,
		EnableIPv6: &spec.EnableIPv6,
		Options:    spec.Options,
		Labels:     spec.Labels,
	}
	if pools := spec.allPools(); len(pools) > 0 {
		cfg := make([]network.IPAMConfig, 0, len(pools))
		for _, p := range pools {
			cfg = append(cfg, network.IPAMConfig{Subnet: p.Subnet, Gateway: p.Gateway, IPRange: p.IPRange})
		}
		opts.IPAM = &network.IPAM{Config: cfg}
	}
	if _, err := c.NetworkCreate(ctx, spec.Name, opts); err != nil {
		// Fall back to a plain bridge rather than failing the restore outright: a
		// container on a default-subnet network is recoverable, a container that
		// never came back is not. The warning says exactly what was lost.
		if _, ferr := c.NetworkCreate(ctx, spec.Name, network.CreateOptions{Driver: "bridge"}); ferr == nil {
			return []string{fmt.Sprintf(
				"network %q could not be created with its recorded settings (%v) — created as a plain bridge instead; its subnet %s, internal=%v and driver options were NOT applied",
				spec.Name, err, orNone(spec.Subnet), spec.Internal)}
		}
		return []string{fmt.Sprintf("network %q could not be created (%v) — the container may not reach its stack", spec.Name, err)}
	}
	return nil
}

// diffNetwork reports the recorded properties an EXISTING network does not
// match. Report-only: nothing is changed.
func diffNetwork(spec NetworkSpec, existing network.Inspect) []string {
	var diffs []string
	note := func(what, want, got string) {
		if want != "" && want != got {
			diffs = append(diffs, fmt.Sprintf("%s (recorded %s, host has %s)", what, want, orNone(got)))
		}
	}
	note("driver", spec.Driver, existing.Driver)

	gotSubnet := ""
	if len(existing.IPAM.Config) > 0 {
		gotSubnet = existing.IPAM.Config[0].Subnet
	}
	note("subnet", spec.Subnet, gotSubnet)

	// `internal` is a bool, so "not captured" and "captured as false" look
	// identical. Driver is set whenever the network was actually inspected, so it
	// is the marker for "we know what this network was" — without this gate every
	// legacy backup warns that it lost an isolation flag it never recorded.
	if spec.Driver != "" && spec.Internal != existing.Internal {
		diffs = append(diffs, fmt.Sprintf("internal (recorded %v, host has %v)", spec.Internal, existing.Internal))
	}
	if len(diffs) == 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"network %q already exists on this host and differs: %s — left UNCHANGED (altering a live network would disrupt everything else on it); recreate it by hand if the difference matters",
		spec.Name, strings.Join(diffs, "; "))}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
