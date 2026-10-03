package dockercli

import (
	"strings"
	"testing"

	"github.com/docker/docker/api/types/network"
)

// A DR restore used to recreate every missing network as a bare bridge, dropping
// the subnet, the container's pinned address, and the `internal` isolation flag.
// These tests pin the three properties that fix depends on: the recorded
// definition is carried into CreateOptions, an existing network is never
// mutated, and a container that was NOT pinned stays dynamic.

func TestNetworkSpecPoolsFlattenAndExpand(t *testing.T) {
	// The common case: one pool, recorded flat, no duplication in the manifest.
	single := NetworkSpec{Name: "web", Subnet: "172.20.0.0/16", Gateway: "172.20.0.1"}
	pools := single.allPools()
	if len(pools) != 1 || pools[0].Subnet != "172.20.0.0/16" || pools[0].Gateway != "172.20.0.1" {
		t.Fatalf("a flat single pool must expand to one pool: %+v", pools)
	}

	// Dual-stack: every pool is kept, and Pools wins over the flattened primary.
	dual := NetworkSpec{
		Name: "web", Subnet: "172.20.0.0/16",
		Pools: []NetworkPool{{Subnet: "172.20.0.0/16"}, {Subnet: "fd00::/64"}},
	}
	if got := dual.allPools(); len(got) != 2 || got[1].Subnet != "fd00::/64" {
		t.Fatalf("a dual-stack network must keep both pools: %+v", got)
	}

	// A network with no IPAM at all claims none.
	if got := (NetworkSpec{Name: "plain"}).allPools(); len(got) != 0 {
		t.Fatalf("a network with no IPAM must report no pools, got %+v", got)
	}
}

// The isolation flag is the property whose silent loss matters most: a network
// deliberately cut off from the outside must not come back with egress.
func TestDiffNetworkReportsLostIsolation(t *testing.T) {
	recorded := NetworkSpec{Name: "backend", Driver: "bridge", Subnet: "172.20.0.0/16", Internal: true}
	existing := network.Inspect{
		Name: "backend", Driver: "bridge", Internal: false,
		IPAM: network.IPAM{Config: []network.IPAMConfig{{Subnet: "172.20.0.0/16"}}},
	}
	warns := diffNetwork(recorded, existing)
	if len(warns) != 1 {
		t.Fatalf("a differing network must produce exactly one warning, got %v", warns)
	}
	w := warns[0]
	for _, want := range []string{"backend", "internal", "recorded true", "host has false", "left UNCHANGED"} {
		if !strings.Contains(w, want) {
			t.Fatalf("the warning must state %q: %s", want, w)
		}
	}
}

func TestDiffNetworkReportsSubnetMismatch(t *testing.T) {
	recorded := NetworkSpec{Name: "web", Subnet: "172.20.0.0/16"}
	existing := network.Inspect{
		Name: "web", IPAM: network.IPAM{Config: []network.IPAMConfig{{Subnet: "10.5.0.0/24"}}},
	}
	warns := diffNetwork(recorded, existing)
	if len(warns) != 1 || !strings.Contains(warns[0], "172.20.0.0/16") || !strings.Contains(warns[0], "10.5.0.0/24") {
		t.Fatalf("both subnets must be named: %v", warns)
	}
}

// An identical network must be silent — a warning on every restore would train
// the operator to ignore them.
func TestDiffNetworkSilentWhenMatching(t *testing.T) {
	recorded := NetworkSpec{Name: "web", Driver: "bridge", Subnet: "172.20.0.0/16", Internal: true}
	existing := network.Inspect{
		Name: "web", Driver: "bridge", Internal: true,
		IPAM: network.IPAM{Config: []network.IPAMConfig{{Subnet: "172.20.0.0/16"}}},
	}
	if w := diffNetwork(recorded, existing); len(w) != 0 {
		t.Fatalf("a matching network must produce no warning, got %v", w)
	}
	// A legacy record (nothing known) must also stay silent rather than warning
	// about every property it simply didn't capture.
	if w := diffNetwork(NetworkSpec{Name: "web"}, existing); len(w) != 0 {
		t.Fatalf("an empty record must not warn: %v", w)
	}
}

// A container that was never pinned must stay dynamic. Pinning a lease it never
// asked for would make it fail to restore the moment that address is in use.
func TestEndpointIPAMOnlyForRecordedStaticAddresses(t *testing.T) {
	if ipam := endpointIPAM(NetworkSpec{Name: "web"}); ipam != nil {
		t.Fatalf("a dynamic container must get no address config, got %+v", ipam)
	}
	ipam := endpointIPAM(NetworkSpec{Name: "web", IPv4: "172.20.0.5"})
	if ipam == nil || ipam.IPv4Address != "172.20.0.5" {
		t.Fatalf("a pinned address must be carried: %+v", ipam)
	}
	if ipam.IPv6Address != "" {
		t.Fatalf("an absent IPv6 address must stay empty: %+v", ipam)
	}
	// IPv6-only pinning is still pinning.
	if ipam := endpointIPAM(NetworkSpec{Name: "web", IPv6: "fd00::5"}); ipam == nil || ipam.IPv6Address != "fd00::5" {
		t.Fatalf("an IPv6-only pin must be carried: %+v", ipam)
	}
}

// Docker's built-in modes exist on every host and must never be recreated.
func TestEnsureNetworkSpecSkipsBuiltIns(t *testing.T) {
	for _, name := range []string{"bridge", "host", "none", "default", ""} {
		if w := ensureNetworkSpec(nil, nil, NetworkSpec{Name: name}); w != nil {
			t.Fatalf("%q is a built-in and must be skipped without touching Docker, got %v", name, w)
		}
	}
}
