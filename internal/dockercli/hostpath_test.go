package dockercli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

// F81 text remap: exact base-prefix matches only, with token boundaries on
// both sides — sibling dirs, container-side bind paths, and variable-suffixed
// paths are never rewritten.
func TestRemapTextHostPath(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		from, to  string
		want      string
		wantCount int
	}{
		{"simple compose volume", "- /opt/docker/app/data:/data\n", "/opt/docker", "/opt/stacks",
			"- /opt/stacks/app/data:/data\n", 1},
		{"exact base match", "dir: /opt/docker\n", "/opt/docker", "/opt/stacks", "dir: /opt/stacks\n", 1},
		{"sibling dir untouched", "- /opt/dockerx/data:/data\n", "/opt/docker", "/opt/stacks",
			"- /opt/dockerx/data:/data\n", 0},
		{"container path after colon untouched", "- /srv/data:/opt/docker/inside\n", "/opt/docker", "/opt/stacks",
			"- /srv/data:/opt/docker/inside\n", 0},
		{"variable-prefixed path untouched", "- ${HOME}/opt/docker/x:/data\n", "/opt/docker", "/opt/stacks",
			"- ${HOME}/opt/docker/x:/data\n", 0},
		{"deeper absolute path untouched", "- /mnt/opt/docker/x:/data\n", "/opt/docker", "/opt/stacks",
			"- /mnt/opt/docker/x:/data\n", 0},
		{"multiple occurrences", "- /opt/docker/a:/a\n- /opt/docker/b:/b\n", "/opt/docker", "/opt/stacks",
			"- /opt/stacks/a:/a\n- /opt/stacks/b:/b\n", 2},
		{"quoted path", `x: "/opt/docker/app"` + "\n", "/opt/docker", "/opt/stacks", `x: "/opt/stacks/app"` + "\n", 1},
		{"equal bases no-op", "- /opt/docker/a:/a\n", "/opt/docker", "/opt/docker", "- /opt/docker/a:/a\n", 0},
		{"root base refused", "- /opt/docker/a:/a\n", "/", "/opt/stacks", "- /opt/docker/a:/a\n", 0},
		{"relative base refused", "- /opt/docker/a:/a\n", "opt/docker", "/opt/stacks", "- /opt/docker/a:/a\n", 0},
	}
	for _, c := range cases {
		got, n := RemapTextHostPath([]byte(c.in), c.from, c.to)
		if string(got) != c.want || n != c.wantCount {
			t.Errorf("%s: got (%q, %d), want (%q, %d)", c.name, got, n, c.want, c.wantCount)
		}
	}
}

// F81 inspect-JSON remap: bind sources (2- and 3-part specs) and bind
// MountPoints move to the new base; named volumes and non-matching paths stay.
func TestRemapContainerHostPath(t *testing.T) {
	insp := types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{
			HostConfig: &container.HostConfig{
				Binds: []string{
					"/home/alice/docker/mealie/data:/app/data", // 2-part: remap
					"/home/alice/docker/mealie/cfg:/config:ro", // 3-part: remap, keep opts
					"appvol:/var/lib/app",                      // named volume: untouched
					"/srv/other/data:/other",                   // non-matching: untouched
					"/home/alice/dockerx/data:/x",              // sibling dir: untouched
				},
			},
		},
		Mounts: []types.MountPoint{
			{Type: "bind", Source: "/home/alice/docker/mealie/data", Destination: "/app/data"},
			{Type: "volume", Source: "/var/lib/docker/volumes/appvol/_data", Name: "appvol", Destination: "/var/lib/app"},
		},
	}
	raw, _ := json.Marshal(&insp)

	out, n, err := RemapContainerHostPath(raw, "/home/alice/docker", "/opt/stacks")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 { // two binds + one mount point
		t.Fatalf("substitutions = %d, want 3", n)
	}
	var got types.ContainerJSON
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	wantBinds := []string{
		"/opt/stacks/mealie/data:/app/data",
		"/opt/stacks/mealie/cfg:/config:ro",
		"appvol:/var/lib/app",
		"/srv/other/data:/other",
		"/home/alice/dockerx/data:/x",
	}
	for i, w := range wantBinds {
		if got.HostConfig.Binds[i] != w {
			t.Fatalf("bind[%d] = %q, want %q", i, got.HostConfig.Binds[i], w)
		}
	}
	if got.Mounts[0].Source != "/opt/stacks/mealie/data" {
		t.Fatalf("mount source = %q", got.Mounts[0].Source)
	}
	if got.Mounts[1].Source != "/var/lib/docker/volumes/appvol/_data" {
		t.Fatalf("volume mount must be untouched: %q", got.Mounts[1].Source)
	}
	if strings.Contains(string(out), "/home/alice/docker/mealie") {
		t.Fatal("old base still present in remapped JSON")
	}

	// No matches → original bytes, zero count.
	out2, n2, err := RemapContainerHostPath(raw, "/nonexistent/base", "/opt/stacks")
	if err != nil || n2 != 0 || string(out2) != string(raw) {
		t.Fatalf("no-match must be a byte-identical no-op (n=%d err=%v)", n2, err)
	}
	// Invalid bases → error, original bytes.
	if _, n3, err := RemapContainerHostPath(raw, "/", "/opt/stacks"); err == nil || n3 != 0 {
		t.Fatal("root base must be refused")
	}
}

// A file bind's contents are written to the host BY PATH, so the single-path
// remap has to agree exactly with the document-wide one — otherwise the key
// lands beside the source machine's layout while its directories land beside the
// target's.
func TestRemapHostPath(t *testing.T) {
	const from, to = "/volume1/docker/webapp", "/home/user/docker/webapp"
	cases := []struct{ name, in, want string }{
		{"under the base", from + "/secrets/vapid_private_key", to + "/secrets/vapid_private_key"},
		{"the base itself", from, to},
		{"outside the base", "/srv/other/key", "/srv/other/key"},
		// A sibling directory whose name merely starts with the base must not move.
		{"sibling prefix", "/volume1/docker/webapp-old/key", "/volume1/docker/webapp-old/key"},
	}
	for _, c := range cases {
		if got := RemapHostPath(c.in, from, to); got != c.want {
			t.Errorf("%s: RemapHostPath(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}

	// No remap configured, or a nonsensical one, must leave the path alone rather
	// than write the file somewhere the operator never named.
	const p = "/volume1/docker/webapp/secrets/vapid_private_key"
	for _, c := range []struct{ name, from, to string }{
		{"no remap", "", ""},
		{"only a from", from, ""},
		{"only a to", "", to},
		{"root base", "/", to},
		{"relative base", "volume1/docker", to},
		{"identical bases", from, from},
	} {
		if got := RemapHostPath(p, c.from, c.to); got != p {
			t.Errorf("%s: must leave the path untouched, got %q", c.name, got)
		}
	}
}

// The bug that let a cross-host restore report every path remapped, create them
// all at the new location, and still be refused by the daemon asking for the old
// one.
//
// A container's binds live in up to THREE places in one inspect, and only two
// were rewritten. The one that was missed — HostConfig.Mounts — is the one
// ContainerCreate actually reads; the top-level Mounts list is read-only OUTPUT
// from inspect, so rewriting it changes nothing about what gets created. A
// single `--mount`-declared secret was enough to expose it.
func TestRemapContainerHostPathRewritesHostConfigMounts(t *testing.T) {
	insp := types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{
			HostConfig: &container.HostConfig{
				Binds: []string{"/volume1/docker/gc/volumes/uploads:/app/uploads:rw"},
				// compose long-syntax / `--mount` lands here.
				Mounts: []mount.Mount{
					{Type: mount.TypeBind, Source: "/volume1/docker/gc/secrets/vapid_private_key", Target: "/run/secrets/vapid_private_key", ReadOnly: true},
					{Type: mount.TypeVolume, Source: "appvol", Target: "/var/lib/app"},
					{Type: mount.TypeBind, Source: "/srv/elsewhere/x", Target: "/x"},
				},
			},
		},
		Mounts: []types.MountPoint{
			{Type: "bind", Source: "/volume1/docker/gc/volumes/uploads", Destination: "/app/uploads"},
			{Type: "bind", Source: "/volume1/docker/gc/secrets/vapid_private_key", Destination: "/run/secrets/vapid_private_key"},
		},
	}
	raw, err := json.Marshal(&insp)
	if err != nil {
		t.Fatal(err)
	}
	out, n, err := RemapContainerHostPath(raw, "/volume1/docker/gc", "/home/user/docker/gc")
	if err != nil {
		t.Fatal(err)
	}
	// 1 Binds + 1 HostConfig.Mounts + 2 top-level Mounts.
	if n != 4 {
		t.Errorf("substitutions = %d, want 4 (the HostConfig.Mounts entry was the one being missed)", n)
	}
	var got types.ContainerJSON
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.HostConfig.Mounts[0].Source != "/home/user/docker/gc/secrets/vapid_private_key" {
		t.Errorf("the field ContainerCreate reads must be remapped, got %q", got.HostConfig.Mounts[0].Source)
	}
	// A named volume has no host path, and a bind outside the base is not ours.
	if got.HostConfig.Mounts[1].Source != "appvol" {
		t.Errorf("a named volume source must never be rewritten, got %q", got.HostConfig.Mounts[1].Source)
	}
	if got.HostConfig.Mounts[2].Source != "/srv/elsewhere/x" {
		t.Errorf("a bind outside the base must be untouched, got %q", got.HostConfig.Mounts[2].Source)
	}
}

// And the reader that decides which paths to prepare has to see that bind too,
// or it prepares a path the daemon never asks for.
func TestContainerBindMountsFindsHostConfigMounts(t *testing.T) {
	insp := types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{
			HostConfig: &container.HostConfig{
				Binds: []string{"/srv/gc/uploads:/app/uploads:rw"},
				Mounts: []mount.Mount{
					{Type: mount.TypeBind, Source: "/srv/gc/secrets/key", Target: "/run/secrets/key", ReadOnly: true},
					{Type: mount.TypeVolume, Source: "appvol", Target: "/var/lib/app"},
				},
			},
		},
		// Deliberately EMPTY: an inspect that records the bind only in HostConfig
		// must still be understood.
	}
	raw, err := json.Marshal(&insp)
	if err != nil {
		t.Fatal(err)
	}
	got := ContainerBindMounts(raw)
	if len(got) != 2 {
		t.Fatalf("both host binds must be found, got %+v", got)
	}
	byDest := map[string]string{}
	for _, b := range got {
		byDest[b.Destination] = b.Source
	}
	if byDest["/run/secrets/key"] != "/srv/gc/secrets/key" {
		t.Errorf("the HostConfig.Mounts bind must carry its target as the destination: %+v", got)
	}
	if byDest["/app/uploads"] != "/srv/gc/uploads" {
		t.Errorf("the Binds entry must still be found: %+v", got)
	}
}
