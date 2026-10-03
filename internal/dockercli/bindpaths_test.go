package dockercli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
)

// The preflight only gets to warn about paths it can see, so the reader has to
// find every host bind in BOTH places the daemon records them, and none of the
// things that merely look like one.
func TestContainerBindMounts(t *testing.T) {
	insp := types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{
			HostConfig: &container.HostConfig{
				Binds: []string{
					"/volume1/docker/app/data:/app/data",              // 2-part
					"/volume1/docker/app/secrets/key:/run/secrets:ro", // 3-part: options dropped
					"appvol:/var/lib/app",                             // named volume: not a host path
					"malformed-no-colon",
				},
			},
		},
		Mounts: []types.MountPoint{
			{Type: "bind", Source: "/volume1/docker/app/logs", Destination: "/app/logs"},
			{Type: "volume", Source: "/var/lib/docker/volumes/x/_data", Destination: "/x", Name: "x"},
			{Type: "bind", Source: "/volume1/docker/app/data", Destination: "/app/data"}, // dup of a Binds entry
		},
	}
	raw, err := json.Marshal(&insp)
	if err != nil {
		t.Fatal(err)
	}
	got := ContainerBindMounts(raw)

	want := []BindMount{
		{Source: "/volume1/docker/app/data", Destination: "/app/data"},
		{Source: "/volume1/docker/app/logs", Destination: "/app/logs"},
		{Source: "/volume1/docker/app/secrets/key", Destination: "/run/secrets"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d binds %v, want %d", len(got), got, len(want))
	}
	// Sorted by source, so the report an operator reads is stable run to run.
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bind %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Unparseable JSON must yield no binds rather than a panic — a restore is never
// blocked by the preflight failing to read something.
func TestContainerBindMountsRejectsGarbage(t *testing.T) {
	if got := ContainerBindMounts([]byte("not json")); got != nil {
		t.Errorf("garbage inspect JSON should produce no binds, got %v", got)
	}
}

// A path with a space or a quote in it has to survive the trip through `sh -c`
// intact, or the probe reports a verdict about a different path.
func TestHostProbeScriptQuotesPaths(t *testing.T) {
	script := hostProbeScript([]string{"/srv/my data", "/srv/it's"})
	for _, want := range []string{`'/srv/my data'`, `'/srv/it'\''s'`} {
		if !strings.Contains(script, want) {
			t.Errorf("script must contain %s\ngot: %s", want, script)
		}
	}
	if !strings.Contains(script, hostProbeMount+"$p") {
		t.Errorf("paths must be probed under the read-only host mount, got: %s", script)
	}
}

// An unreadable line must produce NO verdict, not a wrong one: the caller treats
// "missing" as grounds to stop a restore, so inventing that verdict from noise
// would refuse restores that should have run.
func TestParseHostProbe(t *testing.T) {
	out := strings.Join([]string{
		"directory /volume1/docker/app/data",
		"file /volume1/docker/app/secrets/key",
		"missing /volume1/docker/app/logs",
		"directory /srv/my data", // a path containing a space survives
		"garbage-kind /srv/other",
		"nospace",
		"",
	}, "\n")
	kinds := parseHostProbe(out)

	want := map[string]HostPathKind{
		"/volume1/docker/app/data":        HostPathDir,
		"/volume1/docker/app/secrets/key": HostPathFile,
		"/volume1/docker/app/logs":        HostPathMissing,
		"/srv/my data":                    HostPathDir,
	}
	for path, kind := range want {
		if kinds[path] != kind {
			t.Errorf("%s: got %q, want %q", path, kinds[path], kind)
		}
	}
	for _, unknown := range []string{"/srv/other", "nospace"} {
		if _, ok := kinds[unknown]; ok {
			t.Errorf("%s: an unreadable line must leave no verdict, got %q", unknown, kinds[unknown])
		}
	}
	if len(kinds) != len(want) {
		t.Errorf("got %d verdicts, want %d: %v", len(kinds), len(want), kinds)
	}
}

// One bind per PARENT, not one bind of /. A mistake in path construction can
// then only reach a directory the container already binds, which is the property
// the rest of the host-write code has and the reason this does not take the
// shorter route.
func TestGroupHostPathsFoldsByParentAndRefusesTheRest(t *testing.T) {
	groups := groupHostPaths([]HostPathSpec{
		{Path: "/home/user/docker/gc/volumes/uploads", Kind: MountKindDir},
		{Path: "/home/user/docker/gc/volumes/pgdata", Kind: MountKindDir, Owner: "101:104", Mode: "700"},
		{Path: "/home/user/docker/gc/secrets/vapid_private_key", Kind: MountKindFile, Owner: "1026:100", Mode: "600"},
		// Refused: a system location, too shallow, unknown kind, bind-spec injection.
		{Path: "/etc/ssl/private/app.key", Kind: MountKindFile},
		{Path: "/key.pem", Kind: MountKindFile},
		{Path: "/srv/app/data", Kind: ""},
		{Path: "/srv/app:/etc/x", Kind: MountKindDir},
	})

	if len(groups) != 2 {
		t.Fatalf("two distinct parents, got %d: %+v", len(groups), groups)
	}
	byParent := map[string]int{}
	for _, g := range groups {
		byParent[g.parent] = len(g.specs)
	}
	if byParent["/home/user/docker/gc/volumes"] != 2 {
		t.Errorf("siblings must share one bind, got %v", byParent)
	}
	if byParent["/home/user/docker/gc/secrets"] != 1 {
		t.Errorf("the secrets parent must be bound on its own, got %v", byParent)
	}
	for _, g := range groups {
		if g.parent == "/etc/ssl/private" || g.parent == "/" || g.parent == "/srv/app" {
			t.Errorf("%q must never be bound read-write", g.parent)
		}
	}
}

// The script is the only thing that runs as root against a host filesystem, so
// what it does to each kind — and how it quotes — is pinned.
func TestHostEnsureScript(t *testing.T) {
	script := hostEnsureScript(groupHostPaths([]HostPathSpec{
		{Path: "/srv/app/data", Kind: MountKindDir, Owner: "101:104", Mode: "700"},
		{Path: "/srv/app/plain", Kind: MountKindDir},
		{Path: "/srv/secrets/key", Kind: MountKindFile, Owner: "1026:100", Mode: "600"},
	}))

	// A directory is made; a file is created only if absent, and never truncated
	// — one that turned up since the probe is the real one.
	if !strings.Contains(script, "mkdir -p '/dockback-ensure0/data'") {
		t.Errorf("a directory must be created with mkdir -p, got: %s", script)
	}
	if !strings.Contains(script, "[ -e '/dockback-ensure1/key' ] || (umask 077; : > '/dockback-ensure1/key')") {
		t.Errorf("a file placeholder must be guarded and private, got: %s", script)
	}
	if strings.Contains(script, "mkdir -p '/dockback-ensure1/key'") {
		t.Error("a file bind must never be created as a directory — that is the failure this exists to prevent")
	}
	// Recorded ownership is applied; unrecorded is left alone rather than guessed.
	for _, want := range []string{
		"chown 101:104 '/dockback-ensure0/data'",
		"chmod 700 '/dockback-ensure0/data'",
		"chown 1026:100 '/dockback-ensure1/key'",
		"chmod 600 '/dockback-ensure1/key'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script must contain %q\ngot: %s", want, script)
		}
	}
	if strings.Contains(script, "chown  ") || strings.Contains(script, "chmod  ") {
		t.Errorf("an unrecorded owner or mode must produce no command at all, got: %s", script)
	}
	// One failure must not take the rest of the paths down with it.
	if strings.Contains(script, "set -e") {
		t.Error("the script must not abort on the first path that cannot be created")
	}
}

// A malformed owner or mode must never reach a chown/chmod — they are the only
// values in the script that do not go through shQuote.
func TestHostEnsureScriptRejectsMalformedOwnerAndMode(t *testing.T) {
	script := hostEnsureScript(groupHostPaths([]HostPathSpec{
		{Path: "/srv/app/data", Kind: MountKindDir, Owner: "root:root; rm -rf /", Mode: "7ç0"},
		{Path: "/srv/app/other", Kind: MountKindDir, Owner: "$(id -u)", Mode: "999999"},
	}))
	for _, bad := range []string{"rm -rf", "root:root", "$(id -u)", "7ç0", "999999"} {
		if strings.Contains(script, bad) {
			t.Errorf("%q must never reach the script, got: %s", bad, script)
		}
	}
	if !strings.Contains(script, "mkdir -p '/dockback-ensure0/data'") {
		t.Error("the directory itself must still be created when only its metadata is unusable")
	}
}

// The leaf DockBack creates comes from an allow-list grammar, so a name outside
// it is refused rather than escaped around. That ordering is the point: quoting
// makes a hostile name safe to pass, whereas refusing means it is never created
// at all — and a path this cannot describe is one the operator should be told
// about, not one it invents a close approximation of.
func TestHostEnsureScriptRefusesUnsafeLeafNames(t *testing.T) {
	for _, p := range []string{
		"/srv/app/it's",    // quote
		"/srv/app/a b",     // space
		"/srv/app/-rf",     // leading dash reads as a flag
		"/srv/app/.hidden", // leading dot is outside the grammar
	} {
		if script := hostEnsureScript(groupHostPaths([]HostPathSpec{{Path: p, Kind: MountKindDir}})); script != "" {
			t.Errorf("%q must be refused, not created; got script: %s", p, script)
		}
	}

	// A space in a PARENT is fine and must still work: the parent travels as one
	// element of the bind list, which is never shell-split, and the leaf under it
	// is an ordinary name.
	groups := groupHostPaths([]HostPathSpec{{Path: "/srv/my data/uploads", Kind: MountKindDir}})
	if len(groups) != 1 || groups[0].parent != "/srv/my data" {
		t.Fatalf("a parent containing a space must still be bound, got %+v", groups)
	}
	if got := hostEnsureScript(groups); !strings.Contains(got, "mkdir -p '/dockback-ensure0/uploads'") {
		t.Errorf("the leaf under it must still be created, got: %s", got)
	}
}
