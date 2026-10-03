package backup

import (
	"encoding/json"
	"strings"
	"testing"

	"dockback/internal/dockercli"
)

// The manifest is the documented offline contract, so the topology has to survive
// a round trip through it unchanged — and a backup taken before this existed must
// restore exactly as it did before.

func TestNetworkRefRoundTrip(t *testing.T) {
	specs := []dockercli.NetworkSpec{{
		Name: "stack_backend", Driver: "bridge", Scope: "local",
		Internal: true, Attachable: true, EnableIPv6: true,
		Subnet: "172.20.0.0/16", Gateway: "172.20.0.1", IPRange: "172.20.5.0/24",
		Pools: []dockercli.NetworkPool{
			{Subnet: "172.20.0.0/16", Gateway: "172.20.0.1"},
			{Subnet: "fd00::/64"},
		},
		Options: map[string]string{"com.docker.network.bridge.name": "br-stack"},
		Labels:  map[string]string{"com.docker.compose.network": "backend"},
		IPv4:    "172.20.0.5", IPv6: "fd00::5",
		Aliases: []string{"db", "postgres"},
	}}

	refs := networkRefsFrom(specs)
	// Through JSON, because that is how it actually travels.
	blob, err := json.Marshal(refs)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []NetworkRef
	if err := json.Unmarshal(blob, &decoded); err != nil {
		t.Fatal(err)
	}
	got := networkSpecsFrom(decoded)
	if len(got) != 1 {
		t.Fatalf("want 1 network, got %d", len(got))
	}
	g := got[0]
	if g.Name != "stack_backend" || g.Subnet != "172.20.0.0/16" || g.IPv4 != "172.20.0.5" {
		t.Fatalf("core fields lost: %+v", g)
	}
	if !g.Internal || !g.Attachable || !g.EnableIPv6 {
		t.Fatalf("flags lost: internal=%v attachable=%v ipv6=%v", g.Internal, g.Attachable, g.EnableIPv6)
	}
	if len(g.Pools) != 2 || g.Pools[1].Subnet != "fd00::/64" {
		t.Fatalf("dual-stack pools lost: %+v", g.Pools)
	}
	if g.Options["com.docker.network.bridge.name"] != "br-stack" {
		t.Fatalf("driver options lost: %+v", g.Options)
	}
	if len(g.Aliases) != 2 || g.Aliases[0] != "db" {
		t.Fatalf("aliases lost: %+v", g.Aliases)
	}
}

// A pre-F89 backup has no networks field at all. It must convert to nothing, so
// the restore falls through to the original behaviour.
func TestNetworkRefLegacyBackupConvertsToNothing(t *testing.T) {
	var man Manifest
	if err := json.Unmarshal([]byte(`{"version":1,"target_name":"app"}`), &man); err != nil {
		t.Fatal(err)
	}
	if man.Networks != nil {
		t.Fatalf("a legacy manifest must have no networks, got %+v", man.Networks)
	}
	if got := networkSpecsFrom(man.Networks); got != nil {
		t.Fatalf("nothing recorded must convert to nothing, got %+v", got)
	}
	if got := networkRefsFrom(nil); got != nil {
		t.Fatalf("no specs must convert to nothing, got %+v", got)
	}
}

// A container with no recorded networks must not add the key to the manifest at
// all, so a backup of a host-network container is byte-identical to before.
func TestManifestOmitsEmptyNetworks(t *testing.T) {
	blob, err := json.Marshal(Manifest{Version: 1, TargetName: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "networks") {
		t.Fatalf("an empty topology must be omitted entirely: %s", blob)
	}
}

func TestNetworkSummaryReadsWell(t *testing.T) {
	got := networkSummary([]NetworkRef{
		{Name: "backend", Subnet: "172.20.0.0/16", IPv4: "172.20.0.5", Internal: true},
		{Name: "proxy"},
	})
	for _, want := range []string{"backend", "172.20.0.0/16", "@172.20.0.5", "(internal)", "proxy"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary must contain %q: %s", want, got)
		}
	}
}
