package backup

import (
	"strings"
	"testing"
)

// A recreate restores the OLD configuration into whatever image actually runs.
// Paperless-ngx 3.0.0 added a hard startup gate the older configuration had no
// reason to satisfy, and the container died in a loop with an application error
// DockBack never surfaced. These tests pin what the diff reports — and, just as
// importantly, what it stays quiet about.

// THE acceptance case: a key the new image declares and the container does not
// set is a new requirement, and must be named.
func TestImageDriftNewEnvKeyIsNamed(t *testing.T) {
	old := ImageConfig{EnvKeys: []string{"PAPERLESS_REDIS", "PATH"}}
	next := ImageConfig{EnvKeys: []string{"PAPERLESS_REDIS", "PAPERLESS_SECRET_KEY", "PATH"}}

	got := ImageConfigDrift(old, next, []string{"PAPERLESS_REDIS", "PATH"})
	if len(got) != 1 {
		t.Fatalf("want exactly one line, got %v", got)
	}
	if !strings.Contains(got[0], "PAPERLESS_SECRET_KEY") {
		t.Fatalf("the new key must be named: %s", got[0])
	}
}

// A changed default VALUE says nothing about whether the container will start —
// only keys matter, and reporting values would also risk printing a secret.
func TestImageDriftIgnoresChangedValues(t *testing.T) {
	old := ImageConfig{EnvKeys: []string{"TZ", "PATH"}}
	next := ImageConfig{EnvKeys: []string{"TZ", "PATH"}}
	if got := ImageConfigDrift(old, next, nil); len(got) != 0 {
		t.Fatalf("identical KEY sets must produce nothing: %v", got)
	}
}

// A new image default the container ALREADY sets is not a new requirement.
// Without this filter every routine bump produces a wall nobody reads.
func TestImageDriftSkipsKeysTheContainerAlreadySets(t *testing.T) {
	old := ImageConfig{EnvKeys: []string{"PATH"}}
	next := ImageConfig{EnvKeys: []string{"PATH", "PAPERLESS_URL"}}

	if got := ImageConfigDrift(old, next, []string{"PATH", "PAPERLESS_URL"}); len(got) != 0 {
		t.Fatalf("a key the container already provides is not a new requirement: %v", got)
	}
	// …but it IS reported when the container does not set it.
	if got := ImageConfigDrift(old, next, []string{"PATH"}); len(got) != 1 {
		t.Fatalf("an unset new key must be reported: %v", got)
	}
}

func TestImageDriftIdenticalConfigsAreSilent(t *testing.T) {
	cfg := ImageConfig{
		EnvKeys: []string{"PATH", "TZ"}, Entrypoint: []string{"/init"},
		Cmd: []string{"serve"}, Volumes: []string{"/data"}, Healthcheck: true,
	}
	if got := ImageConfigDrift(cfg, cfg, []string{"PATH"}); len(got) != 0 {
		t.Fatalf("an unchanged image must produce nothing: %v", got)
	}
}

func TestImageDriftStructuralChanges(t *testing.T) {
	old := ImageConfig{Entrypoint: []string{"/old-init"}, Cmd: []string{"serve"}, Volumes: []string{"/data"}}
	next := ImageConfig{
		Entrypoint: []string{"/new-init", "--flag"}, Cmd: []string{"run"},
		Volumes: []string{"/data", "/consume"}, Healthcheck: true,
	}
	got := strings.Join(ImageConfigDrift(old, next, nil), "\n")
	for _, want := range []string{"ENTRYPOINT changed", "CMD changed", "/consume", "adds a HEALTHCHECK"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in:\n%s", want, got)
		}
	}
	// A REMOVED healthcheck matters too: the restore's health gate loses the only
	// thing that could confirm the container actually works.
	back := ImageConfigDrift(next, old, nil)
	if !strings.Contains(strings.Join(back, "\n"), "removes its HEALTHCHECK") {
		t.Fatalf("a removed healthcheck must be reported: %v", back)
	}
}

// Config that used to come from the image now has to come from somewhere else.
func TestImageDriftRemovedEnvDefault(t *testing.T) {
	old := ImageConfig{EnvKeys: []string{"PATH", "APP_HOME"}}
	next := ImageConfig{EnvKeys: []string{"PATH"}}

	got := ImageConfigDrift(old, next, []string{"PATH"})
	if len(got) != 1 || !strings.Contains(got[0], "APP_HOME") {
		t.Fatalf("a removed default must be reported: %v", got)
	}
	// Unless the container sets it itself, in which case nothing changes for it.
	if got := ImageConfigDrift(old, next, []string{"PATH", "APP_HOME"}); len(got) != 0 {
		t.Fatalf("a default the container supplies is not a loss: %v", got)
	}
}

// The change note is the honest headline when the declared config shows nothing
// — precisely what happened in the incident behind this feature: a hard
// requirement added with no default to declare.
func TestImageChangeNote(t *testing.T) {
	got := ImageChangeNote("3.0.0", "3.3.1")
	for _, want := range []string{"3.0.0", "3.3.1", "release notes"} {
		if !strings.Contains(got, want) {
			t.Fatalf("a version change must name %q: %s", want, got)
		}
	}
	// With a floating tag neither build declares a version, so nothing can be
	// named — but the change itself must still be reported.
	for _, c := range [][2]string{{"", ""}, {"3.0.0", ""}, {"", "3.3.1"}, {"3.0.0", "3.0.0"}} {
		got := ImageChangeNote(c[0], c[1])
		if !strings.Contains(got, "no longer available") {
			t.Fatalf("ImageChangeNote(%q,%q) must still report the change: %s", c[0], c[1], got)
		}
		if strings.Contains(got, "→") {
			t.Fatalf("ImageChangeNote(%q,%q) must not invent a version arrow: %s", c[0], c[1], got)
		}
	}
}
