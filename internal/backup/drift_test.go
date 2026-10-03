package backup

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"

	"dockback/internal/dockercli"
)

// F73 config drift: identical configs never drift; each material field mutation
// names exactly its section; cosmetic runtime fields are ignored.

func driftInspect(mut func(i *types.ContainerJSON)) []byte {
	i := types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{
			ID:      "aaaa1111",
			Created: "2026-01-01T00:00:00Z",
			Image:   "sha256:img-one",
			HostConfig: &container.HostConfig{
				RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
				PortBindings:  nat.PortMap{"80/tcp": {{HostIP: "", HostPort: "8080"}}},
			},
		},
		Config: &container.Config{
			Image:      "nginx:1.25",
			Env:        []string{"PATH=/usr/local/bin", "HOSTNAME=aaaa1111", "TZ=UTC", "MODE=a"},
			Cmd:        []string{"nginx", "-g", "daemon off;"},
			Entrypoint: []string{"/docker-entrypoint.sh"},
		},
		Mounts: []types.MountPoint{
			{Type: "volume", Name: "data", Source: "/var/lib/docker/volumes/data/_data", Destination: "/data", RW: true},
			{Type: "bind", Source: "/srv/cfg", Destination: "/cfg", RW: true},
		},
		NetworkSettings: &types.NetworkSettings{
			NetworkSettingsBase: types.NetworkSettingsBase{SandboxKey: "/var/run/docker/netns/one"},
		},
	}
	if mut != nil {
		mut(&i)
	}
	b, _ := json.Marshal(i)
	return b
}

func TestConfigDriftFields(t *testing.T) {
	base := driftInspect(nil)
	cases := []struct {
		name string
		mut  func(i *types.ContainerJSON)
		want string // expected single drift field ("" = no drift)
	}{
		{"identical", nil, ""},
		{"image tag changed", func(i *types.ContainerJSON) { i.Config.Image = "nginx:1.26" }, "image"},
		{"image updated same tag", func(i *types.ContainerJSON) { i.Image = "sha256:img-two" }, "image"},
		{"env var added", func(i *types.ContainerJSON) { i.Config.Env = append(i.Config.Env, "NEW=1") }, "environment"},
		{"env var removed", func(i *types.ContainerJSON) { i.Config.Env = []string{"PATH=/usr/local/bin", "TZ=UTC"} }, "environment"},
		{"mount added", func(i *types.ContainerJSON) {
			i.Mounts = append(i.Mounts, types.MountPoint{Type: "volume", Name: "extra", Destination: "/extra", RW: true})
		}, "mounts"},
		{"port remapped", func(i *types.ContainerJSON) {
			i.HostConfig.PortBindings = nat.PortMap{"80/tcp": {{HostPort: "9090"}}}
		}, "ports"},
		{"restart policy changed", func(i *types.ContainerJSON) { i.HostConfig.RestartPolicy.Name = "always" }, "restart policy"},
		{"command changed", func(i *types.ContainerJSON) { i.Config.Cmd = []string{"nginx"} }, "command"},

		// Ignored cosmetic/runtime fields — never drift.
		{"container id changed", func(i *types.ContainerJSON) { i.ID = "bbbb2222" }, ""},
		{"created timestamp changed", func(i *types.ContainerJSON) { i.Created = "2026-06-30T12:00:00Z" }, ""},
		{"sandbox key changed", func(i *types.ContainerJSON) { i.NetworkSettings.SandboxKey = "/var/run/docker/netns/two" }, ""},
		{"PATH/HOSTNAME noise ignored", func(i *types.ContainerJSON) {
			i.Config.Env = []string{"MODE=a", "TZ=UTC", "PATH=/bin", "HOSTNAME=zzzz9999"}
		}, ""},
		{"env order irrelevant", func(i *types.ContainerJSON) {
			i.Config.Env = []string{"TZ=UTC", "MODE=a", "HOSTNAME=aaaa1111", "PATH=/usr/local/bin"}
		}, ""},
		{"mount order irrelevant", func(i *types.ContainerJSON) {
			i.Mounts = []types.MountPoint{
				{Type: "bind", Source: "/srv/cfg", Destination: "/cfg", RW: true},
				{Type: "volume", Name: "data", Source: "/var/lib/docker/volumes/data/_data", Destination: "/data", RW: true},
			}
		}, ""},
	}
	for _, c := range cases {
		rep := ConfigDrift(base, driftInspect(c.mut))
		if c.want == "" {
			if rep.Changed {
				t.Errorf("%s: unexpected drift %v", c.name, rep.Fields)
			}
			continue
		}
		if !rep.Changed || len(rep.Fields) != 1 || rep.Fields[0] != c.want {
			t.Errorf("%s: fields=%v want [%s]", c.name, rep.Fields, c.want)
		}
	}

	// Multiple mutations report every changed section.
	rep := ConfigDrift(base, driftInspect(func(i *types.ContainerJSON) {
		i.Config.Image = "nginx:1.26"
		i.Config.Env = append(i.Config.Env, "NEW=1")
	}))
	if !rep.Changed || strings.Join(rep.Fields, ",") != "image,environment" {
		t.Errorf("multi-field drift wrong: %v", rep.Fields)
	}

	// Unusable input never claims drift.
	if rep := ConfigDrift([]byte("not json"), base); rep.Changed {
		t.Error("broken stored inspect must not claim drift")
	}
	if rep := ConfigDrift(base, nil); rep.Changed {
		t.Error("missing live inspect must not claim drift")
	}
}

func TestConfigDriftFingerprint(t *testing.T) {
	base := driftInspect(nil)
	same := driftInspect(func(i *types.ContainerJSON) { i.ID = "bbbb2222"; i.Created = "2027-01-01T00:00:00Z" })
	changed := driftInspect(func(i *types.ContainerJSON) { i.Config.Env = append(i.Config.Env, "NEW=1") })

	if fp := ConfigFingerprint(base); fp == "" || fp != ConfigFingerprint(same) {
		t.Fatal("fingerprint must ignore cosmetic fields and be stable")
	}
	if ConfigFingerprint(base) == ConfigFingerprint(changed) {
		t.Fatal("a material change must change the fingerprint")
	}
	if ConfigFingerprint([]byte("garbage")) != "" {
		t.Fatal("unusable inspect must yield an empty fingerprint")
	}

	// Lite fingerprint: stable across mount order, changes on image/mount change.
	mounts := mountPointsToMounts([]types.MountPoint{
		{Type: "volume", Name: "data", Destination: "/data", RW: true},
		{Type: "bind", Source: "/srv/cfg", Destination: "/cfg", RW: true},
	})
	reordered := []dockercli.Mount{mounts[1], mounts[0]}
	a := ConfigFingerprintLite("nginx:1.25", "sha256:img-one", mounts)
	b := ConfigFingerprintLite("nginx:1.25", "sha256:img-one", reordered)
	if a == "" || a != b {
		t.Fatal("lite fingerprint must be order-independent")
	}
	if a == ConfigFingerprintLite("nginx:1.26", "sha256:img-one", mounts) {
		t.Fatal("lite fingerprint must change with the image")
	}
}
