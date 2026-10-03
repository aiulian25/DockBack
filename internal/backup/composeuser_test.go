package backup

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// R3 §Issue 24 and R4's repeat of it. The third UID model — a `user:` line in
// the stack file — "is the easiest to miss because it lives in the stack file
// rather than in the image or environment" (PLAYBOOK §7.2).
func TestComposeUserFinding(t *testing.T) {
	// engine collects findings without a store or a daemon.
	newEngine := func() *Engine { return &Engine{Log: func(string, string, string) {}} }

	inspect := func(user string, mounts []types.MountPoint) types.ContainerJSON {
		return types.ContainerJSON{
			ContainerJSONBase: &types.ContainerJSONBase{Name: "/Nextcloud-REDIS"},
			Mounts:            mounts,
			Config:            &container.Config{Image: "redis:7", User: user},
		}
	}
	dataVolume := []types.MountPoint{{Type: "bind", Source: "/volume1/docker/nextcloud/redis", Destination: "/data", RW: true}}

	// findingsFor runs the capture reporter and returns what it recorded.
	findingsFor := func(t *testing.T, user string, mounts []types.MountPoint, imageUser string) []Finding {
		t.Helper()
		man := &Manifest{}
		if imageUser != "" {
			man.ImageConfig = &ImageConfig{User: imageUser}
		}
		newEngine().reportComposeUser(man, "b1", "Nextcloud-REDIS", inspect(user, mounts))
		return man.Findings
	}

	t.Run("a host-specific id on a container with data is a finding", func(t *testing.T) {
		got := findingsFor(t, "1026:100", dataVolume, "redis")
		if len(got) != 1 {
			t.Fatalf("got %d findings, want 1: %+v", len(got), got)
		}
		if got[0].Code != findingComposeUserHostSpecific || got[0].Severity != FindingWarn {
			t.Fatalf("wrong code/severity: %+v", got[0])
		}
		if got[0].Subject != "Nextcloud-REDIS" {
			t.Errorf("subject = %q", got[0].Subject)
		}
		// It has to name the id, recognise the convention, name the pin as the
		// fix, and name the image's own user as the other way out — R3 took the
		// second one.
		for _, want := range []string{"1026:100", "Synology", "restore ownership", "the image runs as redis"} {
			if !strings.Contains(got[0].Message, want) {
				t.Errorf("message is missing %q:\n%s", want, got[0].Message)
			}
		}
	})

	t.Run("no volumes makes it removable noise", func(t *testing.T) {
		// R4: "The `user: 1026:100` override on tika and gotenberg bought nothing
		// — neither has a volume — but would still have failed on a host without
		// uid 1026."
		got := findingsFor(t, "1026:100", nil, "")
		if len(got) != 1 {
			t.Fatalf("got %d findings, want 1", len(got))
		}
		if !strings.Contains(got[0].Message, "buys nothing") || !strings.Contains(got[0].Message, "delete the `user:` line") {
			t.Errorf("the no-volume variant must say to drop it:\n%s", got[0].Message)
		}
		if strings.Contains(got[0].Message, "restore ownership") {
			t.Errorf("nothing to own means the pin is not the fix:\n%s", got[0].Message)
		}
	})

	t.Run("a named user is portable and says nothing", func(t *testing.T) {
		// The step's DO-NOT.
		for _, user := range []string{"redis", "postgres", "www-data", "1000:users"} {
			if got := findingsFor(t, user, dataVolume, ""); len(got) != 0 {
				t.Errorf("%q produced a finding: %+v", user, got)
			}
		}
	})

	t.Run("root and no override say nothing", func(t *testing.T) {
		// uid 0 is uid 0 on every machine, and a container with no `user:` line is
		// running the image's own user — the model that is already portable.
		if got := findingsFor(t, "0:0", dataVolume, ""); len(got) != 0 {
			t.Errorf("root produced a finding: %+v", got)
		}
		if got := findingsFor(t, "", dataVolume, ""); len(got) != 0 {
			t.Errorf("no override produced a finding: %+v", got)
		}
	})

	t.Run("restating the image's own user says nothing", func(t *testing.T) {
		// A compose file repeating what the image already declares is not a
		// host-specific id — that number is the same everywhere.
		if got := findingsFor(t, "999:999", dataVolume, "999"); len(got) != 0 {
			t.Errorf("restating the image's uid produced a finding: %+v", got)
		}
		if got := findingsFor(t, "999", dataVolume, "999"); len(got) != 0 {
			t.Errorf("the same id written two ways produced a finding: %+v", got)
		}
		// But a DIFFERENT number from the image's own is exactly R3's case.
		if got := findingsFor(t, "1026:100", dataVolume, "999"); len(got) != 1 {
			t.Errorf("an override that differs from the image must be reported: %+v", got)
		}
	})

	t.Run("a named volume counts as data to own", func(t *testing.T) {
		named := []types.MountPoint{{Type: "volume", Name: "redis-data", Destination: "/data", RW: true}}
		got := findingsFor(t, "1026:100", named, "")
		if len(got) != 1 || strings.Contains(got[0].Message, "buys nothing") {
			t.Errorf("a named volume is data: %+v", got)
		}
	})

	t.Run("a system bind is not data to own", func(t *testing.T) {
		// /etc/localtime is in half the containers on a host and owns nothing.
		sys := []types.MountPoint{{Type: "bind", Source: "/etc/localtime", Destination: "/etc/localtime"}}
		got := findingsFor(t, "1026:100", sys, "")
		if len(got) != 1 || !strings.Contains(got[0].Message, "buys nothing") {
			t.Errorf("a system bind must not count as data: %+v", got)
		}
	})

	t.Run("an image that declares no user is not guessed at", func(t *testing.T) {
		// Most images drop privileges in their own entrypoint rather than with a
		// USER instruction, so naming a number here would be a guess dressed as a
		// fact. The message says how to find out instead.
		got := findingsFor(t, "1026:100", dataVolume, "")
		if len(got) != 1 {
			t.Fatalf("got %d findings", len(got))
		}
		if strings.Contains(got[0].Message, "the image runs as ") {
			t.Errorf("named a user the image never declared:\n%s", got[0].Message)
		}
		if !strings.Contains(got[0].Message, "docker exec") {
			t.Errorf("must say how to find the real user:\n%s", got[0].Message)
		}
	})

	t.Run("known id conventions are named", func(t *testing.T) {
		// PLAYBOOK §7.2: "Known id conventions that never match a stock Linux
		// host: Synology 1026+/100 (users), Unraid 99:100, QNAP. Detect and flag."
		if !strings.Contains(idConvention(1026, 100), "Synology") {
			t.Error("1026:100 is the Synology pair R3 and R4 both found")
		}
		if !strings.Contains(idConvention(99, 100), "Unraid") {
			t.Error("99:100 is Unraid's nobody/users")
		}
		if idConvention(1000, 1000) != "" {
			t.Error("an ordinary desktop id is not a NAS convention")
		}
	})
}

// The half F189 never covered. `applyRunAsIDs` returned early when the container
// declared no PUID/PGID pair — and redis, tika and gotenberg declare none: their
// id comes from the stack file. The pin chowned their data and left the process
// running as the NAS's uid, which is the same mismatch pointing the other way.
func TestApplyRunAsUserRewritesTheComposeOverride(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := &Engine{Store: st, Log: func(string, string, string) {}}
	b := &store.Backup{ID: "b1", TargetName: "Nextcloud-REDIS"}
	opts := RestoreOptions{NodeID: "n1"}

	inspect := func(user string, env ...string) []byte {
		insp := types.ContainerJSON{
			ContainerJSONBase: &types.ContainerJSONBase{Name: "/Nextcloud-REDIS"},
			Config:            &container.Config{Image: "redis:7", User: user, Env: env},
		}
		raw, _ := json.Marshal(insp)
		return raw
	}
	// R3's container: a `user:` override and no user-mapping variable anywhere.
	nas := inspect("1026:100", "TZ=Europe/London")

	// No pin: reproduced verbatim. #31 — the operator may well have created that
	// account here, and rewriting an id nobody asked about is the deviation.
	if got := dockercli.ConfigUser(e.applyRunAsIDs(b, nil, nas, opts)); got != "1026:100" {
		t.Errorf("without a pin the captured id must survive verbatim, got %q", got)
	}

	if err := e.SetRestoreOwnership("n1", "Nextcloud-REDIS", "999:999"); err != nil {
		t.Fatal(err)
	}
	out := e.applyRunAsIDs(b, nil, nas, opts)
	if got := dockercli.ConfigUser(out); got != "999:999" {
		t.Fatalf("the `user:` override must be rewritten to the pin, got %q", got)
	}
	if !strings.Contains(string(out), "TZ=Europe/London") {
		t.Error("unrelated configuration was disturbed")
	}
	// No environment pair was invented alongside it.
	if env := dockercli.ContainerEnv(out); len(env) != 1 {
		t.Errorf("a user-mapping variable was added to an image that never declared one: %v", env)
	}

	// A named user is left alone even with a pin in place: it resolves inside the
	// image, and its group memberships came with that account.
	named := inspect("redis")
	if got := dockercli.ConfigUser(e.applyRunAsIDs(b, nil, named, opts)); got != "redis" {
		t.Errorf("a named user must survive a pin, got %q", got)
	}

	// Both models at once — R4's stack used them together.
	both := inspect("1026:100", "PUID=1026", "PGID=100")
	out = e.applyRunAsIDs(b, nil, both, opts)
	joined := strings.Join(dockercli.ContainerEnv(out), " ")
	if dockercli.ConfigUser(out) != "999:999" || !strings.Contains(joined, "PUID=999") {
		t.Errorf("both models must be corrected together: user=%q env=%s", dockercli.ConfigUser(out), joined)
	}
}

// #5 — the env-configurable model works end to end, and can still be carrying a
// NAS's numbers. PLAYBOOK §7.2: "Known id conventions that never match a stock
// Linux host: Synology 1026+/100 (`users`), Unraid 99:100, QNAP."
func TestRunAsConventionFinding(t *testing.T) {
	findingsFor := func(t *testing.T, env ...string) []Finding {
		t.Helper()
		man := &Manifest{}
		insp := types.ContainerJSON{
			ContainerJSONBase: &types.ContainerJSONBase{Name: "/PaperlessNGX"},
			Config:            &container.Config{Image: "paperless-ngx", Env: env},
		}
		(&Engine{Log: func(string, string, string) {}}).reportRunAsConvention(man, "b1", "PaperlessNGX", insp)
		return man.Findings
	}

	t.Run("a Synology pair is named", func(t *testing.T) {
		got := findingsFor(t, "USERMAP_UID=1026", "USERMAP_GID=100", "TZ=UTC")
		if len(got) != 1 {
			t.Fatalf("got %d findings: %+v", len(got), got)
		}
		if got[0].Code != findingRunAsConventionForeign || got[0].Severity != FindingInfo {
			t.Fatalf("wrong code/severity: %+v", got[0])
		}
		// The subject is the variable pair, so the finding points at what to change.
		if got[0].Subject != "USERMAP_UID/USERMAP_GID" {
			t.Errorf("subject = %q", got[0].Subject)
		}
		for _, want := range []string{"1026:100", "Synology", "restore ownership", "Nothing is wrong"} {
			if !strings.Contains(got[0].Message, want) {
				t.Errorf("message is missing %q:\n%s", want, got[0].Message)
			}
		}
	})

	t.Run("a convention found through the semantic scan is reported too", func(t *testing.T) {
		// #5's whole point: the pair no list knew about still reaches this.
		if got := findingsFor(t, "RUN_AS_UID=1026", "RUN_AS_GID=100"); len(got) != 1 {
			t.Fatalf("a generic pair must be reported: %+v", got)
		} else if got[0].Subject != "RUN_AS_UID/RUN_AS_GID" {
			t.Errorf("subject = %q", got[0].Subject)
		}
	})

	t.Run("an ordinary id says nothing", func(t *testing.T) {
		// A finding on every container that sets PUID=1000 would bury the ones
		// that mean something.
		for _, env := range [][]string{
			{"PUID=1000", "PGID=1000"},
			{"PUID=33", "PGID=33"},
			{"PUID=0", "PGID=0"},
			{"TZ=UTC"},
			{"SQUID=3128"},
			{"PUID=1026"}, // half a pair declares nothing
		} {
			if got := findingsFor(t, env...); len(got) != 0 {
				t.Errorf("%v produced a finding: %+v", env, got)
			}
		}
	})
}

// The env-pair model's own restore-time warning (#N8).
//
// `reportCarriedComposeUser` had covered the `user:` model since #24; the
// PUID/PGID model — the one the EACCES incident was actually using — had
// nothing. A cross-host restore reproduced 1026:100 and said not a word.
func TestCarriedRunAsEnvWarning(t *testing.T) {
	// warningsFor captures the engine's log sink so the message itself can be
	// asserted, not just its absence.
	warningsFor := func(t *testing.T, sourceNode, targetNode string, pin string, env ...string) []string {
		t.Helper()
		st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		var lines []string
		e := &Engine{Store: st, Log: func(_, level, msg string) {
			if level == "WARN" {
				lines = append(lines, msg)
			}
		}}
		b := &store.Backup{ID: "b1", TargetName: "wikijs", NodeID: sourceNode}
		man := &Manifest{NodeID: sourceNode}
		opts := RestoreOptions{NodeID: targetNode}
		if pin != "" {
			if err := e.SetRestoreOwnership(targetNode, "wikijs", pin); err != nil {
				t.Fatal(err)
			}
		}
		insp := types.ContainerJSON{
			ContainerJSONBase: &types.ContainerJSONBase{Name: "/wikijs"},
			Config:            &container.Config{Image: "lscr.io/linuxserver/wikijs", Env: env},
		}
		raw, _ := json.Marshal(insp)
		e.applyRunAsIDs(b, man, raw, opts)
		return lines
	}

	t.Run("cross-host and unpinned warns, naming the ids and the fix", func(t *testing.T) {
		got := warningsFor(t, "nas01", "server2", "", "USERMAP_UID=1026", "USERMAP_GID=100")
		if len(got) != 1 {
			t.Fatalf("want exactly one warning, got %v", got)
		}
		for _, want := range []string{"1026:100", "USERMAP_UID/USERMAP_GID", "Synology", "restore ownership"} {
			if !strings.Contains(got[0], want) {
				t.Errorf("warning is missing %q:\n%s", want, got[0])
			}
		}
		// It must also name the trap, not just the ids — that is the whole reason
		// the incident went unnoticed until somebody edited PUID/PGID.
		if !strings.Contains(got[0], "permission") && !strings.Contains(got[0], "unreadable") {
			t.Errorf("the warning must say what goes wrong later:\n%s", got[0])
		}
	})

	t.Run("PUID/PGID is covered too, not just USERMAP", func(t *testing.T) {
		if got := warningsFor(t, "nas01", "server2", "", "PUID=1026", "PGID=100"); len(got) != 1 {
			t.Fatalf("want one warning, got %v", got)
		}
	})

	t.Run("same host stays silent", func(t *testing.T) {
		// The account is very likely the operator's own — the rule
		// reportCarriedComposeUser already documents.
		if got := warningsFor(t, "server2", "server2", "", "USERMAP_UID=1026", "USERMAP_GID=100"); len(got) != 0 {
			t.Fatalf("same-host restore must not warn: %v", got)
		}
	})

	t.Run("a pin stays silent", func(t *testing.T) {
		// Already answered, and the rewrite logs its own line.
		if got := warningsFor(t, "nas01", "server2", "1000:1000", "USERMAP_UID=1026", "USERMAP_GID=100"); len(got) != 0 {
			t.Fatalf("a pinned restore must not warn: %v", got)
		}
	})

	t.Run("an ordinary id is not news on any machine", func(t *testing.T) {
		if got := warningsFor(t, "nas01", "server2", "", "PUID=1000", "PGID=1000"); len(got) != 0 {
			t.Fatalf("1000:1000 must not warn: %v", got)
		}
	})

	t.Run("no declared pair, nothing to say", func(t *testing.T) {
		if got := warningsFor(t, "nas01", "server2", "", "TZ=Europe/London"); len(got) != 0 {
			t.Fatalf("a container declaring no pair must not warn: %v", got)
		}
	})
}
