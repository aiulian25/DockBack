package backup

import (
	"fmt"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"
	units "github.com/docker/go-units"
	yaml "go.yaml.in/yaml/v3"
)

func TestComposeFromInspect(t *testing.T) {
	insp := types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{
			Name: "/karakeep-web-1",
			HostConfig: &container.HostConfig{
				RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
				PortBindings: nat.PortMap{
					"3000/tcp": []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "3000"}},
				},
				CapDrop: []string{"ALL"},
			},
		},
		Config: &container.Config{
			Image:      "ghcr.io/karakeep-app/karakeep:0.16.0",
			Env:        []string{"MEILI_ADDR=http://meili:7700", "DATA_DIR=/data"},
			WorkingDir: "/app",
			Labels: map[string]string{
				"com.docker.compose.service": "web",
				"com.docker.compose.project": "karakeep",
				"maintainer":                 "acme",
			},
		},
		Mounts: []types.MountPoint{
			{Type: "volume", Name: "karakeep_data", Destination: "/data", RW: true},
			{Type: "bind", Source: "/host/config", Destination: "/config", RW: false},
		},
		NetworkSettings: &types.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{
				"karakeep_default": {},
				"bridge":           {},
			},
		},
	}

	out, err := composeFromInspect(insp, "ghcr.io/karakeep-app/karakeep:0.16.0", nil, nil, nil)
	if err != nil {
		t.Fatalf("composeFromInspect: %v", err)
	}
	s := string(out)
	if !strings.HasPrefix(s, "# docker-compose.yml") {
		t.Fatalf("missing reconstruction header:\n%s", s)
	}

	// Must be valid YAML with the expected shape.
	var doc struct {
		Services map[string]struct {
			Image         string            `yaml:"image"`
			ContainerName string            `yaml:"container_name"`
			Restart       string            `yaml:"restart"`
			Environment   map[string]string `yaml:"environment"`
			Ports         []string          `yaml:"ports"`
			Volumes       []string          `yaml:"volumes"`
			Networks      []string          `yaml:"networks"`
			CapDrop       []string          `yaml:"cap_drop"`
			WorkingDir    string            `yaml:"working_dir"`
			Labels        map[string]string `yaml:"labels"`
		} `yaml:"services"`
		Networks map[string]struct {
			External bool `yaml:"external"`
		} `yaml:"networks"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("reconstructed compose is not valid YAML: %v\n%s", err, s)
	}

	svc, ok := doc.Services["web"]
	if !ok {
		t.Fatalf("expected service keyed by compose label 'web', got %v", doc.Services)
	}
	if svc.Image != "ghcr.io/karakeep-app/karakeep:0.16.0" {
		t.Errorf("image = %q", svc.Image)
	}
	if svc.ContainerName != "karakeep-web-1" {
		t.Errorf("container_name = %q", svc.ContainerName)
	}
	if svc.Restart != "unless-stopped" {
		t.Errorf("restart = %q", svc.Restart)
	}
	if svc.Environment["MEILI_ADDR"] != "http://meili:7700" || svc.Environment["DATA_DIR"] != "/data" {
		t.Errorf("environment = %v", svc.Environment)
	}
	if len(svc.Ports) != 1 || svc.Ports[0] != "3000:3000" {
		t.Errorf("ports = %v", svc.Ports)
	}
	if !contains(svc.Volumes, "karakeep_data:/data") || !contains(svc.Volumes, "/host/config:/config:ro") {
		t.Errorf("volumes = %v", svc.Volumes)
	}
	// The no-noise rule (#N5): this fixture's endpoints carry no aliases and no
	// static address, so the service-level attachment stays a plain NAME LIST.
	// The `Networks []string` field above is the assertion — a mapping would fail
	// to unmarshal into it — and this keeps existing reconstructions byte-stable
	// for the common case.
	if !contains(svc.Networks, "karakeep_default") || contains(svc.Networks, "bridge") {
		t.Errorf("networks = %v (bridge must be excluded)", svc.Networks)
	}
	if !doc.Networks["karakeep_default"].External {
		t.Errorf("attached network should be declared external: %v", doc.Networks)
	}
	if _, leaked := svc.Labels["com.docker.compose.project"]; leaked {
		t.Errorf("compose-managed labels must be stripped: %v", svc.Labels)
	}
	if svc.Labels["maintainer"] != "acme" {
		t.Errorf("user label dropped: %v", svc.Labels)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestComposeProjectLayout(t *testing.T) {
	cases := []struct {
		name     string
		labels   map[string]string
		wantDir  string
		wantFile string
	}{
		{"compose absolute", map[string]string{
			"com.docker.compose.project.working_dir":  "/opt/docker/app1",
			"com.docker.compose.project.config_files": "/opt/docker/app1/docker-compose.yml",
		}, "/opt/docker/app1", "docker-compose.yml"},
		{"config list first wins + basename", map[string]string{
			"com.docker.compose.project.working_dir":  "/srv/app",
			"com.docker.compose.project.config_files": "/srv/app/compose.yaml,/srv/app/compose.override.yaml",
		}, "/srv/app", "compose.yaml"},
		{"working dir only", map[string]string{
			"com.docker.compose.project.working_dir": "/srv/app",
		}, "/srv/app", ""},
		{"non-compose container", map[string]string{"maintainer": "x"}, "", ""},
		{"nil labels", nil, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, file := composeProjectLayout(tc.labels)
			if dir != tc.wantDir || file != tc.wantFile {
				t.Fatalf("composeProjectLayout(%v) = (%q,%q); want (%q,%q)", tc.labels, dir, file, tc.wantDir, tc.wantFile)
			}
		})
	}
}

// The netcup outage, as a test.
//
// The original compose gave the database `hostname: wiki-js-db`; Docker
// registers that in its embedded DNS on user-defined networks, which is the only
// reason the application's `DB_HOST=wiki-js-db` ever resolved. The
// reconstruction dropped it, so `docker compose up -d` from the generated file
// produced a stack that started cleanly and then crash-looped with
// `Database Connection Error: ENOTFOUND undefined:undefined`.
func TestComposeEmitsChosenHostname(t *testing.T) {
	// A real container id: 64 hex characters, of which Docker's default
	// hostname is the first twelve.
	const containerID = "abcdef123456789012345678901234567890123456789012345678901234"

	build := func(hostname, domainname, netMode string) map[string]any {
		t.Helper()
		insp := types.ContainerJSON{
			ContainerJSONBase: &types.ContainerJSONBase{
				ID:         containerID,
				Name:       "/Wiki.js-DB",
				HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(netMode)},
			},
			Config: &container.Config{
				Image:      "postgres:15",
				Hostname:   hostname,
				Domainname: domainname,
				Labels:     map[string]string{"com.docker.compose.service": "db"},
			},
		}
		out, err := composeFromInspect(insp, "postgres:15", nil, nil, nil)
		if err != nil {
			t.Fatalf("composeFromInspect: %v", err)
		}
		var doc struct {
			Services map[string]map[string]any `yaml:"services"`
		}
		if err := yaml.Unmarshal(out, &doc); err != nil {
			t.Fatalf("reconstructed compose is not valid YAML: %v\n%s", err, out)
		}
		svc, ok := doc.Services["db"]
		if !ok {
			t.Fatalf("expected a service keyed 'db', got %v", doc.Services)
		}
		return svc
	}

	t.Run("a chosen hostname is emitted", func(t *testing.T) {
		svc := build("wiki-js-db", "", "")
		if svc["hostname"] != "wiki-js-db" {
			t.Fatalf("hostname = %v, want wiki-js-db — this is the netcup outage", svc["hostname"])
		}
	})

	t.Run("Docker's default short-id hostname is not", func(t *testing.T) {
		// Every container has one of these. Emitting it would add a line to every
		// service and pin an id the recreated container will not have.
		svc := build("abcdef123456", "", "")
		if v, present := svc["hostname"]; present {
			t.Fatalf("the default short-id hostname must not be emitted, got %v", v)
		}
	})

	t.Run("no hostname at all emits nothing", func(t *testing.T) {
		if v, present := build("", "", "")["hostname"]; present {
			t.Fatalf("hostname = %v, want the key absent", v)
		}
	})

	t.Run("host networking borrows the machine's name", func(t *testing.T) {
		// The hostname is the HOST's, not this service's, and Docker refuses
		// --hostname with --network host — a file carrying both would not start.
		svc := build("wiki-js-db", "example.internal", "host")
		if v, present := svc["hostname"]; present {
			t.Errorf("hostname = %v, want absent under host networking", v)
		}
		if v, present := svc["domainname"]; present {
			t.Errorf("domainname = %v, want absent under host networking", v)
		}
	})

	t.Run("container networking borrows another container's name", func(t *testing.T) {
		svc := build("wiki-js-db", "", "container:abcdef123456789012345678901234567890123456789012345678901234")
		if v, present := svc["hostname"]; present {
			t.Fatalf("hostname = %v, want absent when the namespace is borrowed", v)
		}
	})

	t.Run("a domain name rides with the hostname", func(t *testing.T) {
		svc := build("wiki-js-db", "example.internal", "")
		if svc["hostname"] != "wiki-js-db" || svc["domainname"] != "example.internal" {
			t.Fatalf("hostname=%v domainname=%v", svc["hostname"], svc["domainname"])
		}
	})

	t.Run("the emitted file still parses as compose", func(t *testing.T) {
		// The whole point is a file somebody can run; a key in the wrong place
		// would be caught here rather than by `docker compose up`.
		svc := build("wiki-js-db", "", "")
		for _, required := range []string{"image", "container_name", "hostname"} {
			if _, ok := svc[required]; !ok {
				t.Errorf("service is missing %q: %v", required, svc)
			}
		}
	})
}

// The pure rule behind the emission, at its edges.
func TestDefaultHostname(t *testing.T) {
	const id = "abcdef123456789012345678901234567890123456789012345678901234"
	cases := []struct {
		name     string
		hostname string
		id       string
		want     bool
	}{
		{"empty is the default", "", id, true},
		{"the id's first twelve characters", "abcdef123456", id, true},
		{"a chosen name", "wiki-js-db", id, false},
		{"twelve characters that are not this id's", "0123456789ab", id, false},
		{"a prefix shorter than twelve", "abcdef", id, false},
		{"a name longer than twelve that starts the id", "abcdef1234567", id, false},
		// A fixture with no id: nothing can be the default of an unknown id, so a
		// chosen name still emits. Impossible for a real inspect, which always
		// carries one.
		{"no container id", "wiki-js-db", "", false},
		{"no container id and no hostname", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := defaultHostname(tc.hostname, tc.id); got != tc.want {
				t.Fatalf("defaultHostname(%q, id) = %v, want %v", tc.hostname, got, tc.want)
			}
		})
	}
}

func TestBorrowedNetworkNamespace(t *testing.T) {
	borrowed := []string{"host", "container:abc123", "container:"}
	own := []string{"", "bridge", "none", "wikijs_default", "hostnet", "my-host"}
	for _, m := range borrowed {
		if !borrowedNetworkNamespace(m) {
			t.Errorf("%q borrows a namespace and must suppress the hostname", m)
		}
	}
	for _, m := range own {
		if borrowedNetworkNamespace(m) {
			t.Errorf("%q is this container's own namespace — the hostname is its own", m)
		}
	}
}

// RESTORE findings #3 — the original stack declared `restart: on-failure:5` and
// the reconstruction emitted bare `on-failure`.
//
// The two are different policies, not two spellings of one: five attempts then
// stay down, versus retry forever. A crash-looping container that was meant to
// give up instead hammers its dependencies indefinitely, and nothing says so.
func TestComposeRestartRetryCount(t *testing.T) {
	restartOf := func(t *testing.T, policy container.RestartPolicy) (string, bool) {
		t.Helper()
		insp := types.ContainerJSON{
			ContainerJSONBase: &types.ContainerJSONBase{
				ID:         "abcdef123456789012345678901234567890123456789012345678901234",
				Name:       "/wiki-js",
				HostConfig: &container.HostConfig{RestartPolicy: policy},
			},
			Config: &container.Config{
				Image:  "requarks/wiki:2",
				Labels: map[string]string{"com.docker.compose.service": "wiki"},
			},
		}
		out, err := composeFromInspect(insp, "requarks/wiki:2", nil, nil, nil)
		if err != nil {
			t.Fatalf("composeFromInspect: %v", err)
		}
		var doc struct {
			Services map[string]map[string]any `yaml:"services"`
		}
		if err := yaml.Unmarshal(out, &doc); err != nil {
			t.Fatalf("reconstructed compose is not valid YAML: %v\n%s", err, out)
		}
		v, present := doc.Services["wiki"]["restart"]
		if !present {
			return "", false
		}
		return fmt.Sprint(v), true
	}

	t.Run("on-failure keeps its count", func(t *testing.T) {
		got, present := restartOf(t, container.RestartPolicy{Name: "on-failure", MaximumRetryCount: 5})
		if !present || got != "on-failure:5" {
			t.Fatalf("restart = %q (present=%v), want on-failure:5", got, present)
		}
	})

	t.Run("on-failure with no count stays bare", func(t *testing.T) {
		// Zero means "retry forever" — writing `on-failure:0` would say something
		// the source did not.
		got, _ := restartOf(t, container.RestartPolicy{Name: "on-failure"})
		if got != "on-failure" {
			t.Fatalf("restart = %q, want on-failure", got)
		}
	})

	t.Run("the other policies are unchanged", func(t *testing.T) {
		// Docker rejects a non-zero count on these at create time, so a recorded
		// one is impossible — but if a hand-edited inspect ever carried one, it
		// must not be emitted into a file Docker would then refuse.
		for _, policy := range []container.RestartPolicy{
			{Name: "unless-stopped"},
			{Name: "always"},
			{Name: "unless-stopped", MaximumRetryCount: 5},
			{Name: "always", MaximumRetryCount: 3},
		} {
			got, _ := restartOf(t, policy)
			if got != string(policy.Name) {
				t.Errorf("%+v: restart = %q, want %q", policy, got, policy.Name)
			}
		}
	})

	t.Run("no policy emits nothing", func(t *testing.T) {
		// `no` is Docker's default and compose's; emitting it would add a line to
		// every service that never asked for one.
		for _, policy := range []container.RestartPolicy{{}, {Name: "no"}} {
			if got, present := restartOf(t, policy); present {
				t.Errorf("%+v: restart = %q, want the key absent", policy, got)
			}
		}
	})
}

// RESTORE findings #2 — the service-level `networks:` was a bare name list,
// which structurally cannot carry an alias or an address, while the manifest had
// been recording both per network since F89 and the API restore had been
// applying them.
func TestComposeNetworkAliasesAndStaticIP(t *testing.T) {
	insp := types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{
			ID:         "abcdef123456789012345678901234567890123456789012345678901234",
			Name:       "/Wiki-js",
			HostConfig: &container.HostConfig{},
		},
		Config: &container.Config{
			Image:    "requarks/wiki:2",
			Hostname: "wiki-js",
			Labels:   map[string]string{"com.docker.compose.service": "web"},
		},
		NetworkSettings: &types.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{
				"proxy":  {},
				"appnet": {},
			},
		},
	}
	netDefs := []NetworkRef{
		// Every name Docker or compose re-registers on its own, plus one real
		// alias that must survive.
		{Name: "proxy", Aliases: []string{"wiki-js", "0123456789ab", "web", "Wiki-js", "public-name"}},
		{Name: "appnet", Subnet: "172.20.0.0/16", Driver: "bridge", IPv4: "172.20.0.5"},
	}

	out, err := composeFromInspect(insp, "requarks/wiki:2", netDefs, nil, nil)
	if err != nil {
		t.Fatalf("composeFromInspect: %v", err)
	}
	var doc struct {
		Services map[string]struct {
			Networks map[string]struct {
				Aliases     []string `yaml:"aliases"`
				IPv4Address string   `yaml:"ipv4_address"`
			} `yaml:"networks"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("mapping form is not valid YAML: %v\n%s", err, out)
	}
	svc := doc.Services["web"]

	t.Run("only the real alias survives", func(t *testing.T) {
		got := svc.Networks["proxy"].Aliases
		if len(got) != 1 || got[0] != "public-name" {
			t.Fatalf("proxy aliases = %v, want [public-name] — the hostname, the "+
				"service name, the container name and the short id are all re-registered elsewhere", got)
		}
	})

	t.Run("the requested static address is emitted", func(t *testing.T) {
		if got := svc.Networks["appnet"].IPv4Address; got != "172.20.0.5" {
			t.Fatalf("appnet ipv4_address = %q, want 172.20.0.5", got)
		}
	})

	t.Run("a network with nothing to say is still listed", func(t *testing.T) {
		// Once ANY endpoint carries configuration the whole attachment is a
		// mapping, so a quiet network appears as an empty entry rather than
		// vanishing from the service.
		if _, present := svc.Networks["appnet"]; !present {
			t.Fatal("appnet missing from the mapping")
		}
		if len(svc.Networks) != 2 {
			t.Fatalf("want both networks, got %v", svc.Networks)
		}
	})

	t.Run("the address falls back to the live inspect for a legacy backup", func(t *testing.T) {
		// No netDefs at all — the manifest predates F89, or came from the
		// app-consistent path before it recorded topology.
		legacy := insp
		legacy.NetworkSettings = &types.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{
				"appnet": {
					Aliases:    []string{"public-name", "0123456789ab"},
					IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: "172.20.0.9"},
					IPAddress:  "172.20.0.99", // the LEASE — must never be emitted
				},
			},
		}
		out, err := composeFromInspect(legacy, "requarks/wiki:2", nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		body := string(out)
		if !strings.Contains(body, "172.20.0.9") {
			t.Errorf("the requested address did not reach the file:\n%s", body)
		}
		if strings.Contains(body, "172.20.0.99") {
			t.Errorf("the LEASED address was emitted — pinning a lease nobody asked for "+
				"makes a dynamic container fail to start:\n%s", body)
		}
		if !strings.Contains(body, "public-name") || strings.Contains(body, "0123456789ab") {
			t.Errorf("alias filtering did not apply to the fallback path:\n%s", body)
		}
	})
}

func TestComposeNetworkModeHost(t *testing.T) {
	build := func(mode string) (map[string]any, map[string]any) {
		t.Helper()
		insp := types.ContainerJSON{
			ContainerJSONBase: &types.ContainerJSONBase{
				ID:         "abcdef123456789012345678901234567890123456789012345678901234",
				Name:       "/monitor",
				HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(mode)},
			},
			Config: &container.Config{
				Image:    "prom/node-exporter:latest",
				Hostname: "monitor-host",
				Labels:   map[string]string{"com.docker.compose.service": "exporter"},
			},
			NetworkSettings: &types.NetworkSettings{
				Networks: map[string]*network.EndpointSettings{"appnet": {}},
			},
		}
		out, err := composeFromInspect(insp, "prom/node-exporter:latest", nil, nil, nil)
		if err != nil {
			t.Fatalf("composeFromInspect: %v", err)
		}
		var doc struct {
			Services map[string]map[string]any `yaml:"services"`
			Networks map[string]any            `yaml:"networks"`
		}
		if err := yaml.Unmarshal(out, &doc); err != nil {
			t.Fatalf("not valid YAML: %v\n%s", err, out)
		}
		return doc.Services["exporter"], doc.Networks
	}

	for _, mode := range []string{"host", "none"} {
		t.Run(mode+" excludes networks entirely", func(t *testing.T) {
			// compose refuses a service that declares both, so emitting the list
			// beside network_mode would produce a file that cannot start.
			svc, topLevel := build(mode)
			if svc["network_mode"] != mode {
				t.Fatalf("network_mode = %v, want %s", svc["network_mode"], mode)
			}
			if v, present := svc["networks"]; present {
				t.Errorf("service networks = %v, want the key absent alongside network_mode", v)
			}
			if len(topLevel) != 0 {
				t.Errorf("top-level networks = %v, want none", topLevel)
			}
		})
	}

	t.Run("container mode is deliberately omitted", func(t *testing.T) {
		// The recorded value embeds the OLD container's full id, and the service
		// name it should reference cannot be derived offline. The API restore
		// reproduces it from HostConfig verbatim.
		svc, _ := build("container:abcdef123456789012345678901234567890123456789012345678901234")
		if v, present := svc["network_mode"]; present {
			t.Errorf("network_mode = %v, want absent for container: mode", v)
		}
	})

	t.Run("an ordinary mode still emits networks", func(t *testing.T) {
		svc, topLevel := build("appnet")
		if _, present := svc["network_mode"]; present {
			t.Error("network_mode must not be emitted for a user-defined network")
		}
		if _, present := svc["networks"]; !present {
			t.Error("networks must still be emitted")
		}
		if len(topLevel) == 0 {
			t.Error("the network declaration must still be emitted")
		}
	})
}

// RESTORE findings #4 — the diff against a real original showed `mem_limit`,
// `cpu_shares` and `security_opt` silently dropped.
//
// The last of those is why this matters beyond tidiness: a file that loses
// `no-new-privileges` describes a container hardened less than the one it claims
// to reproduce, and whoever rebuilds from it gets the weaker one with nothing
// saying so.
func TestComposeHostConfigFields(t *testing.T) {
	initTrue := true
	stopTimeout := 30

	svcOf := func(t *testing.T, mutate func(*container.HostConfig, *container.Config)) map[string]any {
		t.Helper()
		hc := &container.HostConfig{}
		cfg := &container.Config{
			Image:  "alpine:latest",
			Labels: map[string]string{"com.docker.compose.service": "app"},
		}
		mutate(hc, cfg)
		insp := types.ContainerJSON{
			ContainerJSONBase: &types.ContainerJSONBase{
				ID:   "abcdef123456789012345678901234567890123456789012345678901234",
				Name: "/lim-check", HostConfig: hc,
			},
			Config: cfg,
		}
		out, err := composeFromInspect(insp, "alpine:latest", nil, nil, nil)
		if err != nil {
			t.Fatalf("composeFromInspect: %v", err)
		}
		var doc struct {
			Services map[string]map[string]any `yaml:"services"`
		}
		if err := yaml.Unmarshal(out, &doc); err != nil {
			t.Fatalf("not valid YAML: %v\n%s", err, out)
		}
		return doc.Services["app"]
	}

	t.Run("the fields the findings measured", func(t *testing.T) {
		svc := svcOf(t, func(hc *container.HostConfig, _ *container.Config) {
			hc.Memory = 536870912
			hc.CPUShares = 768
			hc.SecurityOpt = []string{"no-new-privileges:true"}
			// Both of these are engine DEFAULTS stamped into every inspect.
			hc.ShmSize = 67108864
			hc.IpcMode = "private"
		})
		if svc["mem_limit"] != "512m" {
			t.Errorf("mem_limit = %v, want 512m", svc["mem_limit"])
		}
		if fmt.Sprint(svc["cpu_shares"]) != "768" {
			t.Errorf("cpu_shares = %v, want 768", svc["cpu_shares"])
		}
		if got := fmt.Sprint(svc["security_opt"]); got != "[no-new-privileges:true]" {
			t.Errorf("security_opt = %v", svc["security_opt"])
		}
		if v, present := svc["shm_size"]; present {
			t.Errorf("shm_size = %v — 64 MiB is the engine default and must not be emitted", v)
		}
		if v, present := svc["ipc"]; present {
			t.Errorf("ipc = %v — `private` is the engine default and must not be emitted", v)
		}
	})

	t.Run("hardening survives", func(t *testing.T) {
		svc := svcOf(t, func(hc *container.HostConfig, _ *container.Config) {
			hc.ReadonlyRootfs = true
			hc.Init = &initTrue
			hc.SecurityOpt = []string{"no-new-privileges:true", "seccomp=unconfined"}
			hc.GroupAdd = []string{"audio"}
		})
		if svc["read_only"] != true {
			t.Errorf("read_only = %v", svc["read_only"])
		}
		if svc["init"] != true {
			t.Errorf("init = %v", svc["init"])
		}
		if got := fmt.Sprint(svc["security_opt"]); got != "[no-new-privileges:true seccomp=unconfined]" {
			t.Errorf("security_opt = %v — every entry must survive verbatim", svc["security_opt"])
		}
		if got := fmt.Sprint(svc["group_add"]); got != "[audio]" {
			t.Errorf("group_add = %v", svc["group_add"])
		}
	})

	t.Run("a clean container emits none of it", func(t *testing.T) {
		// The no-inflation rule: an inspect with nothing set must add no keys.
		svc := svcOf(t, func(*container.HostConfig, *container.Config) {})
		for _, key := range []string{
			"mem_limit", "mem_reservation", "memswap_limit", "cpus", "cpu_shares",
			"cpuset", "security_opt", "extra_hosts", "devices", "shm_size",
			"read_only", "init", "sysctls", "group_add", "dns", "dns_search",
			"dns_opt", "tmpfs", "ulimits", "runtime", "pid", "ipc", "userns_mode",
			"stop_signal", "stop_grace_period",
		} {
			if v, present := svc[key]; present {
				t.Errorf("%s = %v on a container that asked for nothing", key, v)
			}
		}
	})

	t.Run("swap: a choice is kept, a derived default is not", func(t *testing.T) {
		// -1 means unlimited — a positive-only rule would drop it and leave the
		// file describing a limit the container does not have.
		unlimited := svcOf(t, func(hc *container.HostConfig, _ *container.Config) {
			hc.Memory = 536870912
			hc.MemorySwap = -1
		})
		if fmt.Sprint(unlimited["memswap_limit"]) != "-1" {
			t.Errorf("memswap_limit = %v, want -1", unlimited["memswap_limit"])
		}

		// Found in live testing: `docker run --memory 512m` alone yields
		// MemorySwap 1073741824 — exactly 2x, derived by Docker, chosen by nobody.
		// Emitting it pins a value computed from mem_limit.
		derived := svcOf(t, func(hc *container.HostConfig, _ *container.Config) {
			hc.Memory = 536870912
			hc.MemorySwap = 1073741824
		})
		if v, present := derived["memswap_limit"]; present {
			t.Errorf("memswap_limit = %v — 2x the memory limit is Docker's own default", v)
		}

		// A real, different choice survives.
		chosen := svcOf(t, func(hc *container.HostConfig, _ *container.Config) {
			hc.Memory = 536870912
			hc.MemorySwap = 805306368 // 768m — 1.5x, nothing derives this
		})
		if chosen["memswap_limit"] != "768m" {
			t.Errorf("memswap_limit = %v, want 768m", chosen["memswap_limit"])
		}

		// With no memory limit there is nothing to derive from, so any swap value
		// present was asked for.
		alone := svcOf(t, func(hc *container.HostConfig, _ *container.Config) {
			hc.MemorySwap = 1073741824
		})
		if alone["memswap_limit"] != "1g" {
			t.Errorf("memswap_limit = %v, want 1g when no memory limit is set", alone["memswap_limit"])
		}
	})

	t.Run("namespaces: only host is a choice", func(t *testing.T) {
		svc := svcOf(t, func(hc *container.HostConfig, _ *container.Config) {
			hc.PidMode, hc.IpcMode, hc.UsernsMode = "host", "host", "host"
		})
		for _, key := range []string{"pid", "ipc", "userns_mode"} {
			if svc[key] != "host" {
				t.Errorf("%s = %v, want host", key, svc[key])
			}
		}
		// `shareable` is a real value but not one compose expresses this way, and
		// the engine stamps `private` — neither is emitted.
		quiet := svcOf(t, func(hc *container.HostConfig, _ *container.Config) {
			hc.IpcMode, hc.PidMode = "shareable", ""
		})
		if v, present := quiet["ipc"]; present {
			t.Errorf("ipc = %v, want absent for a non-host mode", v)
		}
	})

	t.Run("devices, tmpfs and ulimits take their compose shapes", func(t *testing.T) {
		svc := svcOf(t, func(hc *container.HostConfig, _ *container.Config) {
			hc.Devices = []container.DeviceMapping{
				{PathOnHost: "/dev/dri", PathInContainer: "/dev/dri", CgroupPermissions: "rwm"},
				{PathOnHost: "/dev/ttyUSB0", PathInContainer: "/dev/ttyUSB0", CgroupPermissions: "rw"},
			}
			// A Go map has no order; the emitted list must be sorted or an
			// unchanged container reconstructs differently between captures.
			hc.Tmpfs = map[string]string{"/run": "", "/tmp": "size=64m"}
			hc.Ulimits = []*units.Ulimit{{Name: "nofile", Soft: 1024, Hard: 4096}, nil}
		})
		if got := fmt.Sprint(svc["devices"]); got != "[/dev/dri:/dev/dri /dev/ttyUSB0:/dev/ttyUSB0:rw]" {
			t.Errorf("devices = %v — rwm is the default and must be omitted", svc["devices"])
		}
		if got := fmt.Sprint(svc["tmpfs"]); got != "[/run /tmp:size=64m]" {
			t.Errorf("tmpfs = %v", svc["tmpfs"])
		}
		ul, ok := svc["ulimits"].(map[string]any)
		if !ok {
			t.Fatalf("ulimits = %T %v", svc["ulimits"], svc["ulimits"])
		}
		nofile, ok := ul["nofile"].(map[string]any)
		if !ok || fmt.Sprint(nofile["soft"]) != "1024" || fmt.Sprint(nofile["hard"]) != "4096" {
			t.Errorf("ulimits.nofile = %v", ul["nofile"])
		}
		if len(ul) != 1 {
			t.Errorf("a nil ulimit must be skipped, got %v", ul)
		}
	})

	t.Run("cpus, stop signal and grace period", func(t *testing.T) {
		svc := svcOf(t, func(hc *container.HostConfig, cfg *container.Config) {
			hc.NanoCPUs = 1500000000 // 1.5 CPUs
			hc.Runtime = "runc"      // the default — must not be emitted
			cfg.StopSignal = "SIGQUIT"
			cfg.StopTimeout = &stopTimeout
		})
		if svc["cpus"] != "1.5" {
			t.Errorf("cpus = %v, want 1.5", svc["cpus"])
		}
		if v, present := svc["runtime"]; present {
			t.Errorf("runtime = %v — runc is the default", v)
		}
		if svc["stop_signal"] != "SIGQUIT" {
			t.Errorf("stop_signal = %v", svc["stop_signal"])
		}
		if svc["stop_grace_period"] != "30s" {
			t.Errorf("stop_grace_period = %v, want 30s", svc["stop_grace_period"])
		}
	})

	t.Run("the logging driver is deliberately absent", func(t *testing.T) {
		// The daemon stamps its own default into every inspect, so nothing
		// distinguishes a choice from a default. Emitting would pin a non-choice
		// on every service.
		svc := svcOf(t, func(hc *container.HostConfig, _ *container.Config) {
			hc.LogConfig = container.LogConfig{Type: "json-file"}
		})
		for _, key := range []string{"logging", "log_driver", "log_opt"} {
			if v, present := svc[key]; present {
				t.Errorf("%s = %v, want absent", key, v)
			}
		}
	})
}

func TestComposeBytes(t *testing.T) {
	cases := map[int64]any{
		536870912:  "512m",
		1073741824: "1g",
		2147483648: "2g",
		67108864:   "64m",
		1024:       "1k",
		1000000:    int64(1000000), // not an exact multiple — raw, which compose accepts
		0:          int64(0),
		-1:         int64(-1), // "unlimited", passed through
	}
	for in, want := range cases {
		if got := composeBytes(in); got != want {
			t.Errorf("composeBytes(%d) = %v (%T), want %v (%T)", in, got, got, want, want)
		}
	}
}

// RESTORE findings #5 — the generated `environment:` carried PATH, HOME, PS1,
// NODE_ENV, LSIO_FIRST_PARTY and a dozen S6_* variables, because `docker
// inspect` merges the image's own environment into Config.Env.
//
// Beyond the noise it PINS them: a compose file that sets PATH overrides
// whatever the next image version bakes in, so an upgrade quietly runs with the
// previous image's PATH. No hand-written compose file behaves that way.
func TestComposeDropsImageBakedEnv(t *testing.T) {
	envOf := func(t *testing.T, env, imageEnv []string) map[string]string {
		t.Helper()
		insp := types.ContainerJSON{
			ContainerJSONBase: &types.ContainerJSONBase{
				ID:   "abcdef123456789012345678901234567890123456789012345678901234",
				Name: "/wiki-js", HostConfig: &container.HostConfig{},
			},
			Config: &container.Config{
				Image:  "lscr.io/linuxserver/wikijs:latest",
				Env:    env,
				Labels: map[string]string{"com.docker.compose.service": "wiki"},
			},
		}
		out, err := composeFromInspect(insp, "lscr.io/linuxserver/wikijs:latest", nil, nil, imageEnv)
		if err != nil {
			t.Fatalf("composeFromInspect: %v", err)
		}
		var doc struct {
			Services map[string]struct {
				Environment map[string]string `yaml:"environment"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(out, &doc); err != nil {
			t.Fatalf("not valid YAML: %v\n%s", err, out)
		}
		return doc.Services["wiki"].Environment
	}

	t.Run("the plan's table", func(t *testing.T) {
		got := envOf(t,
			[]string{"PATH=/usr/bin", "DB_HOST=db", "NODE_ENV=production", "S6_VERBOSITY=2"},
			[]string{"PATH=/usr/bin", "NODE_ENV=production", "S6_VERBOSITY=1"})
		if len(got) != 2 {
			t.Fatalf("environment = %v, want exactly DB_HOST and S6_VERBOSITY", got)
		}
		if got["DB_HOST"] != "db" {
			t.Errorf("the container's own variable was dropped: %v", got)
		}
		// The image bakes S6_VERBOSITY=1; this container says 2. A deliberate
		// override, and the whole reason the comparison is on key AND value.
		if got["S6_VERBOSITY"] != "2" {
			t.Errorf("an override of an image default must survive: %v", got)
		}
		for _, baked := range []string{"PATH", "NODE_ENV"} {
			if v, present := got[baked]; present {
				t.Errorf("%s=%s is the image's own value and must not be copied", baked, v)
			}
		}
	})

	t.Run("a nil imageEnv keeps everything", func(t *testing.T) {
		// The pre-existing behaviour, and what a caller that could not read the
		// image gets. A file listing too much beats one that dropped a variable
		// the application needed because a lookup failed.
		got := envOf(t,
			[]string{"PATH=/usr/bin", "DB_HOST=db", "NODE_ENV=production", "S6_VERBOSITY=2"},
			nil)
		if len(got) != 4 {
			t.Fatalf("environment = %v, want all four preserved", got)
		}
	})

	t.Run("the linuxserver shape from the screenshot", func(t *testing.T) {
		// What the reconstruction actually emitted for the wikijs container, and
		// what should survive: the operator's settings, none of the image's.
		got := envOf(t,
			[]string{
				"PATH=/lsiopy/bin:/usr/local/bin", "HOME=/app", "TERM=xterm",
				"LSIO_FIRST_PARTY=true", "NODE_ENV=production", "S6_STAGE2_HOOK=/docker-mods",
				"S6_VERBOSITY=1", "S6_CMD_WAIT_FOR_SERVICES_MAXTIME=0",
				"PS1=$(whoami)@$(hostname):$(pwd)\\$ ",
				"PUID=1026", "PGID=100", "DB_HOST=wiki-js-db", "DB_NAME=wikijs",
				"DB_TYPE=postgres", "DB_USER=wikiuser", "DB_PORT=5432",
			},
			[]string{
				"PATH=/lsiopy/bin:/usr/local/bin", "HOME=/app",
				"LSIO_FIRST_PARTY=true", "NODE_ENV=production", "S6_STAGE2_HOOK=/docker-mods",
				"S6_VERBOSITY=1", "S6_CMD_WAIT_FOR_SERVICES_MAXTIME=0",
				"PS1=$(whoami)@$(hostname):$(pwd)\\$ ",
			})
		for _, gone := range []string{"PATH", "HOME", "PS1", "LSIO_FIRST_PARTY", "NODE_ENV", "S6_STAGE2_HOOK", "S6_VERBOSITY", "S6_CMD_WAIT_FOR_SERVICES_MAXTIME"} {
			if v, present := got[gone]; present {
				t.Errorf("%s=%s is baked into the image and must not be copied", gone, v)
			}
		}
		for k, want := range map[string]string{
			"PUID": "1026", "PGID": "100", "DB_HOST": "wiki-js-db",
			"DB_NAME": "wikijs", "DB_TYPE": "postgres", "DB_USER": "wikiuser", "DB_PORT": "5432",
		} {
			if got[k] != want {
				t.Errorf("%s = %q, want %q — this is the operator's own configuration", k, got[k], want)
			}
		}
		// TERM comes from `-t`, not the image. Kept: minor noise, and honest.
		if got["TERM"] != "xterm" {
			t.Errorf("TERM should survive (it is not in the image env): %v", got["TERM"])
		}
	})

	t.Run("an empty environment emits no key at all", func(t *testing.T) {
		if got := envOf(t, []string{"PATH=/usr/bin"}, []string{"PATH=/usr/bin"}); len(got) != 0 {
			t.Fatalf("environment = %v, want the key absent entirely", got)
		}
	})
}

func TestContainerOwnEnv(t *testing.T) {
	t.Run("a malformed entry is skipped, not half-parsed", func(t *testing.T) {
		got := containerOwnEnv([]string{"NOEQUALS", "A=1"}, []string{"B=2"})
		if len(got) != 1 || got["A"] != "1" {
			t.Fatalf("containerOwnEnv = %v, want only A=1", got)
		}
	})
	t.Run("an empty value is a value", func(t *testing.T) {
		// `FOO=` is a real setting — it makes the variable exist and be empty,
		// which differs from not setting it at all.
		got := containerOwnEnv([]string{"FOO="}, []string{"BAR=1"})
		if v, present := got["FOO"]; !present || v != "" {
			t.Fatalf("containerOwnEnv = %v, want FOO present and empty", got)
		}
	})
	t.Run("nil in both directions", func(t *testing.T) {
		if got := containerOwnEnv(nil, nil); len(got) != 0 {
			t.Fatalf("= %v", got)
		}
		if got := containerOwnEnv([]string{"A=1"}, nil); len(got) != 1 {
			t.Fatalf("nil imageEnv must keep everything: %v", got)
		}
	})
}

// The 2026-10-04 recovery: reconstructions re-declared shared networks as the stack's
// own, so Compose made `<project>_npm` on the real network's subnet. Only a
// network with this project's label belongs to the stack, under the key Compose
// labelled it with; every other one is joined, as external.
func TestReconstructionKeepsSharedNetworksShared(t *testing.T) {
	insp := types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{Name: "/myproj-web-1", HostConfig: &container.HostConfig{}},
		Config: &container.Config{
			Image:  "alpine",
			Labels: map[string]string{"com.docker.compose.project": "myproj", "com.docker.compose.service": "web"},
		},
		NetworkSettings: &types.NetworkSettings{Networks: map[string]*network.EndpointSettings{
			"myproj_default": {}, "cloudflare": {}, "npm": {}, "otherproj_shared": {},
		}},
	}
	own := func(key string) map[string]string {
		return map[string]string{"com.docker.compose.project": "myproj", "com.docker.compose.network": key}
	}
	netDefs := []NetworkRef{
		{Name: "myproj_default", Driver: "bridge", Subnet: "172.29.0.0/16", Labels: own("default")},
		{Name: "cloudflare", Driver: "bridge", Subnet: "172.30.0.0/16", Labels: own("cloudflare")},
		{Name: "npm", Driver: "bridge", Subnet: "172.31.0.0/16"},
		{Name: "otherproj_shared", Driver: "bridge", Subnet: "172.28.0.0/16",
			Labels: map[string]string{"com.docker.compose.project": "otherproj", "com.docker.compose.network": "shared"}},
	}
	out, err := composeFromInspect(insp, "alpine", netDefs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]struct {
			Networks []string `yaml:"networks"`
		} `yaml:"services"`
		Networks map[string]map[string]any `yaml:"networks"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := fmt.Sprint(doc.Services["web"].Networks); got != "[cloudflare default npm otherproj_shared]" {
		t.Errorf("the service must refer to each network by its declared key, got %s", got)
	}
	if def := doc.Networks["default"]; def["name"] != nil || def["external"] != nil || def["driver"] != "bridge" {
		t.Errorf("the project's default network is its own, declared under `default`: %v", def)
	}
	if def := doc.Networks["cloudflare"]; def["name"] != "cloudflare" || def["external"] != nil {
		t.Errorf("an owned network with an explicit name keeps it, or Compose prefixes it: %v", def)
	}
	for _, joined := range []string{"npm", "otherproj_shared"} {
		def := doc.Networks[joined]
		if def["external"] != true || def["name"] != joined || def["ipam"] != nil {
			t.Errorf("%s is not the stack's, so it must be external by its real name: %v", joined, def)
		}
	}
	if _, prefixed := doc.Networks["myproj_default"]; prefixed {
		t.Errorf("declaring the real name makes Compose prefix it again: %v", doc.Networks)
	}
}

// Without a top-level declaration Compose refuses the file: "refers to undefined
// volume".
func TestReconstructionDeclaresNamedVolumes(t *testing.T) {
	insp := types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{Name: "/app", HostConfig: &container.HostConfig{}},
		Config:            &container.Config{Image: "alpine"},
		Mounts: []types.MountPoint{
			{Type: "volume", Name: "myproj_data", Destination: "/data", RW: true},
			{Type: "volume", Name: "myproj_data", Destination: "/again", RW: true},
			{Type: "bind", Source: "/srv/config", Destination: "/config", RW: true},
		},
	}
	out, err := composeFromInspect(insp, "alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Volumes map[string]map[string]any `yaml:"volumes"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Volumes) != 1 || doc.Volumes["myproj_data"]["external"] != true || doc.Volumes["myproj_data"]["name"] != "myproj_data" {
		t.Errorf("each named volume once, external by its real name, binds not at all: %v", doc.Volumes)
	}
}

// Every value is already resolved, so a `$` in it is literal. Written verbatim,
// Compose interpolated it: hawser's healthcheck lost `$TLS_CERT` and `${PORT}`,
// and an `$apr1$` hash came back mangled.
func TestReconstructionEscapesDollars(t *testing.T) {
	insp := types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{Name: "/hawser", HostConfig: &container.HostConfig{}},
		Config: &container.Config{
			Image:  "alpine",
			Env:    []string{"HASH=$apr1$abc$def", "DB_PASSWORD=plain"},
			Cmd:    []string{"sh", "-c", "echo $HOME"},
			Labels: map[string]string{"traefik.http.middlewares.auth.basicauth.users": "me:$apr1$x$y"},
			Healthcheck: &container.HealthConfig{
				Test: []string{"CMD-SHELL", "curl --cert $TLS_CERT https://localhost:${PORT}"},
			},
		},
	}
	out, err := composeFromInspect(insp, "alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`$$apr1$$abc$$def`, `echo $$HOME`, `me:$$apr1$$x$$y`, `--cert $$TLS_CERT https://localhost:$${PORT}`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// DockBack's own references to the .env are the only thing left to
	// interpolate, and they are added after the escaping.
	compose, env, moved := splitComposeSecrets(out)
	if moved != 1 || !strings.Contains(string(compose), "${DB_PASSWORD}") || !strings.Contains(string(env), "DB_PASSWORD=plain") {
		t.Errorf("the plain secret must move to the .env as ${DB_PASSWORD} (moved %d):\n%s\n%s", moved, compose, env)
	}
}

// What Compose can express is written; what it cannot is named on the service,
// so a reconstruction never quietly describes a lesser container.
func TestReconstructionKeepsOrListsWhatItCannotDrop(t *testing.T) {
	pids := int64(200)
	insp := types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{Name: "/gpu-app", HostConfig: &container.HostConfig{
			Resources: container.Resources{
				CPUQuota: 50000, CPUPeriod: 100000, PidsLimit: &pids,
				DeviceRequests: []container.DeviceRequest{{Driver: "nvidia", Count: -1, Capabilities: [][]string{{"gpu"}}}},
			},
			LogConfig: container.LogConfig{Type: "splunk", Config: map[string]string{"splunk-token": "s3cret", "splunk-url": "https://x"}},
		}},
		Config: &container.Config{Image: "alpine"},
	}
	out, err := composeFromInspect(insp, "alpine", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	svc := doc.Services["gpu-app"]
	if svc["cpu_quota"] != 50000 || svc["cpu_period"] != 100000 || svc["pids_limit"] != 200 {
		t.Errorf("CPU quota, period and pids limit must be written: %v", svc)
	}
	devices := fmt.Sprint(svc["deploy"])
	for _, want := range []string{"driver:nvidia", "count:all", "capabilities:[gpu]"} {
		if !strings.Contains(devices, want) {
			t.Errorf("the GPU request must be written (%s missing): %s", want, devices)
		}
	}
	notes := fmt.Sprint(svc[notWrittenKey])
	if !strings.Contains(notes, "logging: driver splunk with options splunk-token, splunk-url") {
		t.Errorf("a non-default logging driver must be named: %s", notes)
	}
	if strings.Contains(string(out), "s3cret") {
		t.Errorf("a logging option's value can be a token and must never be written:\n%s", out)
	}

	plain := insp
	plain.HostConfig = &container.HostConfig{LogConfig: container.LogConfig{Type: "json-file"}}
	out, _ = composeFromInspect(plain, "alpine", nil, nil, nil)
	if strings.Contains(string(out), notWrittenKey+":") {
		t.Errorf("the daemon's usual default driver is not worth a note:\n%s", out)
	}
}
