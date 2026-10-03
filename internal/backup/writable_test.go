package backup

import (
	"os/exec"
	"strings"
	"testing"

	"dockback/internal/dockercli"
)

func TestWritabilityScript(t *testing.T) {
	t.Run("it is valid POSIX shell", func(t *testing.T) {
		// The same bar hostEnsureScript is held to: a script that only fails when
		// it runs inside a sidecar is a script nobody sees fail.
		script := writabilityScript([]string{"/data", "/config"})
		cmd := exec.Command("sh", "-n")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sh -n rejected the script: %v\n%s\n%s", err, out, script)
		}
	})

	t.Run("every exit path clears the marker", func(t *testing.T) {
		script := writabilityScript([]string{"/data"})
		// Inline, after each attempt...
		if !strings.Contains(script, `rm -f "$f" 2>/dev/null`) {
			t.Error("the marker must be removed straight after the attempt")
		}
		// ...and again from a trap, for the paths that never got that far.
		if !strings.Contains(script, "trap '") || !strings.Contains(script, "EXIT INT TERM") {
			t.Errorf("a trap must cover the interrupted cases: %s", script)
		}
		if !strings.Contains(script, `rm -f "$p"/`+writabilityMarker) {
			t.Error("the trap must remove the marker")
		}
	})

	t.Run("it never truncates an existing file and never leaves one readable", func(t *testing.T) {
		script := writabilityScript([]string{"/data"})
		if !strings.Contains(script, "umask 077") {
			t.Error("a marker must not be born world-readable")
		}
		// A distinct name per run, so two restores cannot collide.
		if !strings.Contains(script, writabilityMarker+`.$$`) {
			t.Error("the marker must be unique to this run")
		}
	})

	t.Run("destinations are quoted and survive spaces", func(t *testing.T) {
		script := writabilityScript([]string{"/var/www/my data", "/it's here"})
		if !strings.Contains(script, `'/var/www/my data'`) {
			t.Errorf("a path with a space must be quoted: %s", script)
		}
		if !strings.Contains(script, `'/it'\''s here'`) {
			t.Errorf("a path with a quote must be escaped: %s", script)
		}
		// The loop reads "$@", not an unquoted list, or a space would split a path
		// into two and report a real directory as absent.
		if !strings.Contains(script, `for d in "$@"`) {
			t.Error("the loop must iterate the quoted argument list")
		}
	})

	t.Run("no destinations, no script", func(t *testing.T) {
		if writabilityScript(nil) != "" {
			t.Error("nothing to test is not a script to run")
		}
	})

	t.Run("parse: silence is not a pass", func(t *testing.T) {
		writable, refused, absent := parseWritability(
			"WOK|/data\nWFAIL|/config\nWSKIP|/media\nnoise\n")
		if len(writable) != 1 || writable[0] != "/data" {
			t.Errorf("writable = %v", writable)
		}
		if len(refused) != 1 || refused[0] != "/config" {
			t.Errorf("refused = %v", refused)
		}
		if len(absent) != 1 || absent[0] != "/media" {
			t.Errorf("absent = %v", absent)
		}
		w, r, a := parseWritability("")
		if len(w)+len(r)+len(a) != 0 {
			t.Error("no output is no verdict")
		}
	})

	t.Run("only mounts the application should be able to write are tested", func(t *testing.T) {
		dests := writableDestinations(&Manifest{Volumes: []VolumeRef{
			{Destination: "/data", Type: "bind"},
			{Destination: "/config", Type: "volume"},
			// Failing to write to a read-only mount is CORRECT, not a finding.
			{Destination: "/reference", Type: "bind", ReadOnly: true},
			// A file-rooted bind is a file, not a directory to put a marker in.
			{Destination: "/run/secrets/key", Type: "bind", Kind: dockercli.MountKindFile, Archive: "bind-files/0.bin"},
			{Destination: "/"},
		}})
		if len(dests) != 2 || dests[0] != "/config" || dests[1] != "/data" {
			t.Errorf("dests = %v", dests)
		}
	})

	t.Run("the fix names the host path the operator would actually type", func(t *testing.T) {
		man := &Manifest{
			MountedBinds: []VolumeRef{{Destination: "/data", Source: "/srv/app/data", Type: "bind"}},
			Volumes:      []VolumeRef{{Destination: "/config", Type: "volume", Name: "appconfig"}},
		}
		if got := hostSourceFor(man, "/data"); got != "/srv/app/data" {
			t.Errorf("host path = %q", got)
		}
		// A named volume has no host path worth printing, and the message says so
		// rather than printing a docker-internal one.
		if got := hostSourceFor(man, "/config"); got != "" {
			t.Errorf("named volume host path = %q, want empty", got)
		}
	})
}
