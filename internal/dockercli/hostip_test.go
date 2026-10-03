package dockercli

import (
	"encoding/json"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
)

func TestHostIPFromDockerAddress(t *testing.T) {
	cases := map[string]string{
		"ssh://user@10.168.1.178:22":  "10.168.1.178",
		"tcp://10.0.0.5:2375":         "10.0.0.5",
		"tcp://socket-proxy:2375":     "", // name, not an IP
		"unix:///var/run/docker.sock": "",
		"":                            "",
		"ssh://box.lan:22":            "",
	}
	for addr, want := range cases {
		if got := HostIPFromDockerAddress(addr); got != want {
			t.Errorf("HostIPFromDockerAddress(%q) = %q; want %q", addr, got, want)
		}
	}
}

func TestRemapTextHostIP_Boundary(t *testing.T) {
	// The critical safety property: remapping .1 must NOT touch .10/.100.
	in := []byte("A=10.168.1.1\nB=10.168.1.10\nC=10.168.1.100\nURL=http://10.168.1.1:8080/x")
	out, n := RemapTextHostIP(in, "10.168.1.1", "10.0.0.9")
	if n != 2 { // A and URL only
		t.Fatalf("expected 2 substitutions, got %d: %s", n, out)
	}
	s := string(out)
	if want := "A=10.0.0.9\nB=10.168.1.10\nC=10.168.1.100\nURL=http://10.0.0.9:8080/x"; s != want {
		t.Fatalf("remap =\n%s\nwant\n%s", s, want)
	}
	// No-ops.
	if _, n := RemapTextHostIP(in, "10.168.1.1", "10.168.1.1"); n != 0 {
		t.Errorf("equal IPs must be a no-op, got %d", n)
	}
	if _, n := RemapTextHostIP(in, "notanip", "10.0.0.9"); n != 0 {
		t.Errorf("invalid from-IP must be a no-op, got %d", n)
	}
}

func TestRemapContainerHostIP(t *testing.T) {
	insp := types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{
			HostConfig: &container.HostConfig{
				PortBindings: nat.PortMap{
					"8080/tcp": []nat.PortBinding{{HostIP: "10.168.1.10", HostPort: "8080"}},
					"9090/tcp": []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "9090"}}, // wildcard: untouched
				},
				ExtraHosts: []string{"gateway:10.168.1.10", "other:10.168.1.100"},
			},
		},
		Config: &container.Config{
			Env: []string{"ADVERTISE_ADDR=10.168.1.10", "DB_HOST=10.168.1.100", "PLAIN=hello"},
		},
	}
	raw, _ := json.Marshal(insp)

	out, n, err := RemapContainerHostIP(raw, "10.168.1.10", "10.20.30.40")
	if err != nil {
		t.Fatalf("remap: %v", err)
	}
	// port binding (1) + extra host (1) + env (1) = 3; the .100 entries stay put.
	if n != 3 {
		t.Fatalf("expected 3 changes, got %d", n)
	}
	var got types.ContainerJSON
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if got.HostConfig.PortBindings["8080/tcp"][0].HostIP != "10.20.30.40" {
		t.Errorf("port HostIP not remapped: %+v", got.HostConfig.PortBindings["8080/tcp"])
	}
	if got.HostConfig.PortBindings["9090/tcp"][0].HostIP != "0.0.0.0" {
		t.Errorf("wildcard bind must be untouched: %+v", got.HostConfig.PortBindings["9090/tcp"])
	}
	if got.HostConfig.ExtraHosts[0] != "gateway:10.20.30.40" || got.HostConfig.ExtraHosts[1] != "other:10.168.1.100" {
		t.Errorf("extra hosts = %v", got.HostConfig.ExtraHosts)
	}
	wantEnv := []string{"ADVERTISE_ADDR=10.20.30.40", "DB_HOST=10.168.1.100", "PLAIN=hello"}
	for i, w := range wantEnv {
		if got.Config.Env[i] != w {
			t.Errorf("env[%d] = %q; want %q", i, got.Config.Env[i], w)
		}
	}

	// Equal IPs → no-op, original bytes.
	if _, n, _ := RemapContainerHostIP(raw, "10.168.1.10", "10.168.1.10"); n != 0 {
		t.Errorf("equal IPs must be a no-op, got %d", n)
	}
	// No occurrences → 0 changes.
	if _, n, _ := RemapContainerHostIP(raw, "172.16.0.1", "10.20.30.40"); n != 0 {
		t.Errorf("absent IP must be a no-op, got %d", n)
	}
}
