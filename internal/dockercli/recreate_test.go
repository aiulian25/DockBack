package dockercli

import (
	"reflect"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	nat "github.com/docker/go-connections/nat"
)

func TestImageRefCandidates(t *testing.T) {
	const tag = "ghcr.io/karakeep-app/karakeep:0.16.0"
	const digest = "ghcr.io/karakeep-app/karakeep@sha256:1111111111111111111111111111111111111111111111111111111111111111"

	cases := []struct {
		name   string
		digest string
		tag    string
		want   []string
	}{
		{"digest preferred then tag", digest, tag, []string{digest, tag}},
		{"tag only when no digest", "", tag, []string{tag}},
		{"ignores malformed digest", "not-a-digest", tag, []string{tag}},
		{"ignores bare sha without ref", "sha256:abc", tag, []string{tag}},
		{"empty when nothing usable", "", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := imageRefCandidates(tc.digest, tc.tag); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("imageRefCandidates(%q, %q) = %v, want %v", tc.digest, tc.tag, got, tc.want)
			}
		})
	}
}

// F10: stripForClone must isolate a restore-as-copy so it can NEVER touch the
// original's data or ports — fresh empty volumes at every mount destination, no
// host-port bindings, no restart loop, no stack links.
func TestStripForClone(t *testing.T) {
	insp := &types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{
			HostConfig: &container.HostConfig{
				PortBindings:    nat.PortMap{"80/tcp": []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "8080"}}},
				PublishAllPorts: true,
				Binds:           []string{"appdata:/data", "/host/path:/config"},
				Mounts:          []mount.Mount{{Type: mount.TypeVolume, Source: "extra", Target: "/extra"}},
				VolumesFrom:     []string{"other"},
				Links:           []string{"db:db"},
				RestartPolicy:   container.RestartPolicy{Name: "always"},
			},
		},
		Config: &container.Config{Image: "app:1"},
		Mounts: []types.MountPoint{
			{Destination: "/data"},
			{Destination: "/config"},
			{Destination: "/var/run/docker.sock"},
		},
	}

	stripForClone(insp)
	hc := insp.HostConfig

	if len(hc.PortBindings) != 0 || hc.PublishAllPorts {
		t.Errorf("clone must publish no host ports, got bindings=%v publishAll=%v", hc.PortBindings, hc.PublishAllPorts)
	}
	if hc.Binds != nil || hc.Mounts != nil || hc.VolumesFrom != nil {
		t.Errorf("clone must drop all real volume/bind sources: binds=%v mounts=%v volumesFrom=%v", hc.Binds, hc.Mounts, hc.VolumesFrom)
	}
	if hc.Links != nil {
		t.Errorf("clone must drop stack links, got %v", hc.Links)
	}
	if hc.RestartPolicy.Name != "no" {
		t.Errorf("clone restart policy = %q, want \"no\"", hc.RestartPolicy.Name)
	}
	// Every original mount destination becomes a FRESH anonymous volume, so the
	// restore fills the clone's own volumes rather than the live container's.
	for _, dest := range []string{"/data", "/config", "/var/run/docker.sock"} {
		if _, ok := insp.Config.Volumes[dest]; !ok {
			t.Errorf("clone missing a fresh anonymous volume at %q: %v", dest, insp.Config.Volumes)
		}
	}
}

// F219: a clone must not inherit the labels that make it SOMEBODY ELSE. It used
// to keep the original's compose identity, so Docker — and DockBack reading
// Docker — saw a second member of the live stack: listed among its services,
// counted as a member with no backup, and a candidate for the next
// app-consistent snapshot. It kept the dockback.* policy labels too, which is
// how a throwaway copy acquires a backup schedule.
func TestStripForCloneDropsIdentityLabels(t *testing.T) {
	insp := &types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{HostConfig: &container.HostConfig{}},
		Config: &container.Config{Image: "app:1", Labels: map[string]string{
			"com.docker.compose.project":     "blog",
			"com.docker.compose.service":     "app",
			"dockback.enable":                "true",
			"dockback.schedule":              "nightly",
			"org.opencontainers.image.title": "App",
			"maintainer":                     "someone",
		}},
	}

	stripForClone(insp)

	for _, gone := range []string{"com.docker.compose.project", "com.docker.compose.service", "dockback.enable", "dockback.schedule"} {
		if _, still := insp.Config.Labels[gone]; still {
			t.Errorf("a clone must not inherit %q: %v", gone, insp.Config.Labels)
		}
	}
	// Everything else is the image's own metadata and says nothing about who this
	// container is to DockBack — dropping it would be vandalism, not isolation.
	for _, kept := range []string{"org.opencontainers.image.title", "maintainer"} {
		if _, ok := insp.Config.Labels[kept]; !ok {
			t.Errorf("label %q is not an identity claim and must survive: %v", kept, insp.Config.Labels)
		}
	}
}

// The test-clone marker is applied AFTER the strip, so the sweep that removes
// identity labels can never remove the one thing that makes the clone reapable.
func TestCloneLabelsSurviveTheStrip(t *testing.T) {
	insp := &types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{HostConfig: &container.HostConfig{}},
		Config:            &container.Config{Image: "app:1", Labels: map[string]string{"com.docker.compose.project": "blog"}},
	}
	stripForClone(insp)
	applyCloneLabels(insp, map[string]string{"com.dockback.test_clone": "1893456000"})

	if insp.Config.Labels["com.dockback.test_clone"] != "1893456000" {
		t.Errorf("the expiry marker must be on the created clone: %v", insp.Config.Labels)
	}
	// And a clone with no labels at all still gets a usable map rather than a
	// write to nil.
	bare := &types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{HostConfig: &container.HostConfig{}},
		Config:            &container.Config{Image: "app:1"},
	}
	applyCloneLabels(bare, map[string]string{"com.dockback.test_clone": "1"})
	if bare.Config.Labels["com.dockback.test_clone"] != "1" {
		t.Errorf("labels must be created when there were none: %v", bare.Config.Labels)
	}
	// No labels asked for: nothing invented.
	untouched := &types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{HostConfig: &container.HostConfig{}},
		Config:            &container.Config{Image: "app:1"},
	}
	applyCloneLabels(untouched, nil)
	if untouched.Config.Labels != nil {
		t.Errorf("an ordinary clone must gain no labels: %v", untouched.Config.Labels)
	}
}

// The comment said it dropped the auto-added short-id alias; the body did not.
//
// Every recreated container therefore carried the PREVIOUS container's short id
// as a working DNS alias, and they accumulate: the recorded inspect of a
// restored container includes the stale one, the next restore preserves it and
// adds its own. Confirmed in real recorded data during entry 3 — an
// app-consistent capture wrote `aliases: ["14eae861b80d", "nc-database"]`, where
// the first entry is the container's own short id.
func TestCleanAliasesDropsShortIDs(t *testing.T) {
	t.Run("the example from the plan", func(t *testing.T) {
		got := cleanAliases([]string{"wiki-js-db", "0123456789ab", "db"}, "wiki-js-db")
		want := []string{"wiki-js-db", "db"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("cleanAliases = %v, want %v", got, want)
		}
	})

	t.Run("the hostname is prepended and deduplicated", func(t *testing.T) {
		// The wikijs mechanism: Docker registers a container's hostname on every
		// user-defined network, which is why `DB_HOST=wiki-js-db` resolved at all.
		if got := cleanAliases(nil, "wiki-js-db"); !reflect.DeepEqual(got, []string{"wiki-js-db"}) {
			t.Fatalf("cleanAliases(nil, host) = %v", got)
		}
		if got := cleanAliases([]string{"wiki-js-db"}, "wiki-js-db"); len(got) != 1 {
			t.Fatalf("the hostname must not appear twice: %v", got)
		}
	})

	t.Run("names that only look like ids survive", func(t *testing.T) {
		for _, alias := range []string{
			"NotHex12Chars", // wrong shape and uppercase
			"abcdef",        // too short
			"0123456789abc", // 13 characters
			"0123456789a",   // 11 characters
			"0123456789AB",  // 12 chars, uppercase hex — not what Docker writes
			"g123456789ab",  // 12 chars, not hex
			"deadbeef-cafe", // 13 with a separator
			"web",           // an ordinary service alias
			"nc-database",   // the real alias from the entry-3 capture
		} {
			if got := cleanAliases([]string{alias}, ""); len(got) != 1 || got[0] != alias {
				t.Errorf("%q must survive, got %v", alias, got)
			}
		}
	})

	t.Run("every short-id shape is dropped", func(t *testing.T) {
		for _, alias := range []string{
			"0123456789ab",
			"14eae861b80d", // the one entry 3 found in a real manifest
			"bbe5dd40c7de",
			"000000000000",
			"ffffffffffff",
		} {
			if got := cleanAliases([]string{alias}, ""); len(got) != 0 {
				t.Errorf("%q is a short id and must be dropped, got %v", alias, got)
			}
		}
	})

	t.Run("stale ids accumulated across generations all go", func(t *testing.T) {
		// What a container restored three times looks like: its own id plus every
		// incarnation before it, with the one real name buried among them.
		got := cleanAliases([]string{"14eae861b80d", "bbe5dd40c7de", "nc-database", "0123456789ab"}, "wiki-js-db")
		want := []string{"wiki-js-db", "nc-database"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("cleanAliases = %v, want %v", got, want)
		}
	})

	t.Run("empty in, empty out", func(t *testing.T) {
		if got := cleanAliases(nil, ""); len(got) != 0 {
			t.Fatalf("cleanAliases(nil, \"\") = %v, want empty", got)
		}
		if got := cleanAliases([]string{"", "web"}, ""); !reflect.DeepEqual(got, []string{"web"}) {
			t.Fatalf("an empty alias must be skipped: %v", got)
		}
	})
}

func TestShortIDAlias(t *testing.T) {
	for _, yes := range []string{"0123456789ab", "abcdef012345", "000000000000", "ffffffffffff"} {
		if !ShortIDAlias(yes) {
			t.Errorf("%q is a 12-char lowercase hex short id", yes)
		}
	}
	for _, no := range []string{"", "abcdef", "0123456789abc", "0123456789AB", "g123456789ab", "web", "0123456789a-"} {
		if ShortIDAlias(no) {
			t.Errorf("%q is not a short id", no)
		}
	}
}
