package dockercli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
)

// F195 — the domain remap: the address mechanism for containers DockBack has no
// profile for.
//
// The motivating environment is webapp's, as found on a real host: an
// APP_URL naming the container's own domain, a CORS list carrying it again,
// and an OLLAMA_BASE_URL naming a DIFFERENT machine that must survive
// untouched. No rule over variable NAMES can separate those — both end in URL —
// which is why this is a literal from→to over values, like the IP remap.
func TestRemapContainerHostDomain(t *testing.T) {
	insp := types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{Name: "/webapp", HostConfig: &container.HostConfig{}},
		Config: &container.Config{Env: []string{
			"APP_URL=https://car.example.net",
			"CORS_ORIGINS=http://localhost:5000,https://car.example.net",
			"OLLAMA_BASE_URL=http://10.168.1.80:11434",
			"SOME_UPSTREAM=https://api.othercar.example.net.example.com/v1",
			"TZ=Europe/London",
		}},
	}
	raw, _ := json.Marshal(insp)
	out, n, err := RemapContainerHostDomain(raw, "car.example.net", "car.new-home.net")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("expected exactly the two carrying values to change, got %d", n)
	}
	var back types.ContainerJSON
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	env := strings.Join(back.Config.Env, "\n")
	if !strings.Contains(env, "APP_URL=https://car.new-home.net") {
		t.Errorf("APP_URL must carry the new domain, scheme intact:\n%s", env)
	}
	if !strings.Contains(env, "CORS_ORIGINS=http://localhost:5000,https://car.new-home.net") {
		t.Errorf("the CORS entry must change and its neighbour must not:\n%s", env)
	}
	// The upstream on another machine, the longer-name lookalike, and the
	// timezone are untouched.
	for _, keep := range []string{"OLLAMA_BASE_URL=http://10.168.1.80:11434", "api.othercar.example.net.example.com", "TZ=Europe/London"} {
		if !strings.Contains(env, keep) {
			t.Errorf("%q must survive verbatim:\n%s", keep, env)
		}
	}
}

// Boundary behaviour is the safety of this feature, so it is pinned case by
// case.
func TestDomainBoundaries(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		count int
	}{
		// Prefix and suffix lookalikes are different hosts.
		{"URL=https://oscar.example.uk", "URL=https://oscar.example.uk", 0},
		{"URL=https://car.example.uk.internal", "URL=https://car.example.uk.internal", 0},
		// A subdomain is a DIFFERENT host and deliberately not rewritten.
		{"URL=https://www.car.example.uk", "URL=https://www.car.example.uk", 0},
		// Scheme, port and path around the host survive.
		{"URL=http://car.example.uk:8080/api?x=1", "URL=http://car.example.uk.new:8080/api?x=1", 1},
		// Space- and comma-separated lists, including ADJACENT occurrences that
		// share a boundary character — the case a single regexp pass misses.
		{"HOSTS=car.example.uk car.example.uk", "HOSTS=car.example.uk.new car.example.uk.new", 2},
		{"HOSTS=car.example.uk,car.example.uk", "HOSTS=car.example.uk.new,car.example.uk.new", 2},
		// The whole value being exactly the host.
		{"HOST=car.example.uk", "HOST=car.example.uk.new", 1},
	}
	for _, c := range cases {
		re := domainBoundaryRegexp("car.example.uk")
		got, n := replaceDomain(c.in, "car.example.uk", "car.example.uk.new", re)
		if got != c.want || n != c.count {
			t.Errorf("replaceDomain(%q) = %q (%d), want %q (%d)", c.in, got, n, c.want, c.count)
		}
	}
}

// The endpoints reach a regexp and shell-adjacent surfaces, so the grammar is a
// security boundary, not a formality.
func TestValidRemapDomain(t *testing.T) {
	for _, ok := range []string{"cloud.example.com", "a.b", "x1-y2.z3.co", "sub.domain.example.co.uk"} {
		if err := ValidRemapDomain(ok); err != nil {
			t.Errorf("%q should be a valid endpoint: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", "   ", "https://cloud.example.com", "cloud.example.com:443",
		"cloud.example.com/path", "*.example.com", "example", "-bad.example.com",
		"a b.example.com", "$(rm).example.com", "exa mple.com", "..", "a..b",
	} {
		if err := ValidRemapDomain(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

// F197 — the machine's IP is known even for a node registered by hostname; it
// is one resolution away. Reported from a stack restore dialog whose "To
// (target host)" sat empty with the checkbox ticked — and the engine treats an
// empty endpoint as "no remap", so the tick did nothing, silently. The API now
// resolves hostnames and refuses when nothing resolves; this covers the
// resolver.
func TestResolveHostIP(t *testing.T) {
	ctx := context.Background()
	// An IP literal in any of the shapes node addresses take needs no lookup.
	for _, addr := range []string{"tcp://10.168.1.149:2375", "http://10.168.1.149:2375"} {
		if got := ResolveHostIP(ctx, addr); got != "10.168.1.149" {
			t.Errorf("ResolveHostIP(%q) = %q", addr, got)
		}
	}
	// A hostname resolves. localhost is the one name every test machine has.
	for _, addr := range []string{"tcp://localhost:2375", "localhost:2375"} {
		got := ResolveHostIP(ctx, addr)
		if got != "127.0.0.1" && got != "::1" {
			t.Errorf("ResolveHostIP(%q) = %q, want loopback", addr, got)
		}
	}
	// Nothing resolvable is empty — the caller turns that into a refusal.
	for _, addr := range []string{"", "unix:///var/run/docker.sock", "tcp://no.such.host.invalid:2375"} {
		if got := ResolveHostIP(ctx, addr); got != "" {
			t.Errorf("ResolveHostIP(%q) = %q, want empty", addr, got)
		}
	}
}
