package backup

import (
	"strings"

	"dockback/internal/dockercli"
)

// Conversion between the runtime network spec and the manifest's on-disk form
// (F89). Two types rather than one so the manifest — the documented offline
// contract, read by the recovery tool — stays free of runtime dependencies, the
// same boundary VolumeRef already sits on.

func networkRefsFrom(specs []dockercli.NetworkSpec) []NetworkRef {
	if len(specs) == 0 {
		return nil
	}
	out := make([]NetworkRef, 0, len(specs))
	for _, s := range specs {
		r := NetworkRef{
			Name: s.Name, Driver: s.Driver, Scope: s.Scope,
			Internal: s.Internal, Attachable: s.Attachable, EnableIPv6: s.EnableIPv6,
			Subnet: s.Subnet, Gateway: s.Gateway, IPRange: s.IPRange,
			Options: s.Options, Labels: s.Labels,
			IPv4: s.IPv4, IPv6: s.IPv6, Aliases: s.Aliases,
		}
		for _, p := range s.Pools {
			r.Pools = append(r.Pools, NetworkPoolRef{Subnet: p.Subnet, Gateway: p.Gateway, IPRange: p.IPRange})
		}
		out = append(out, r)
	}
	return out
}

func networkSpecsFrom(refs []NetworkRef) []dockercli.NetworkSpec {
	if len(refs) == 0 {
		return nil
	}
	out := make([]dockercli.NetworkSpec, 0, len(refs))
	for _, r := range refs {
		s := dockercli.NetworkSpec{
			Name: r.Name, Driver: r.Driver, Scope: r.Scope,
			Internal: r.Internal, Attachable: r.Attachable, EnableIPv6: r.EnableIPv6,
			Subnet: r.Subnet, Gateway: r.Gateway, IPRange: r.IPRange,
			Options: r.Options, Labels: r.Labels,
			IPv4: r.IPv4, IPv6: r.IPv6, Aliases: r.Aliases,
		}
		for _, p := range r.Pools {
			s.Pools = append(s.Pools, dockercli.NetworkPool{Subnet: p.Subnet, Gateway: p.Gateway, IPRange: p.IPRange})
		}
		out = append(out, s)
	}
	return out
}

// networkSummary renders the recorded topology for one log line.
func networkSummary(refs []NetworkRef) string {
	parts := make([]string, 0, len(refs))
	for _, r := range refs {
		p := r.Name
		if r.Subnet != "" {
			p += " " + r.Subnet
		}
		if r.IPv4 != "" {
			p += " @" + r.IPv4
		}
		if r.Internal {
			p += " (internal)"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ", ")
}
