package api

import (
	"testing"

	"dockback/internal/dockercli"
)

func TestAutosnapEventQualifies(t *testing.T) {
	yes := []dockercli.ChangeEvent{
		{Type: "container", Action: "die"},
		{Type: "container", Action: "kill"},
		{Type: "container", Action: "stop"},
		{Type: "container", Action: "destroy"},
		{Type: "image", Action: "pull"},
	}
	for _, ev := range yes {
		if !autosnapEventQualifies(ev) {
			t.Errorf("%s/%s should qualify", ev.Type, ev.Action)
		}
	}
	no := []dockercli.ChangeEvent{
		{Type: "container", Action: "start"},
		{Type: "container", Action: "create"},
		{Type: "container", Action: "health_status"},
		{Type: "image", Action: "tag"},
		{Type: "volume", Action: "destroy"},
		{Type: "network", Action: "disconnect"},
	}
	for _, ev := range no {
		if autosnapEventQualifies(ev) {
			t.Errorf("%s/%s should NOT qualify", ev.Type, ev.Action)
		}
	}
}

// F7: a destructive event for a protected, autosnap-enabled container yields a
// snapshot target; a non-enabled or non-destructive one does not.
func TestAutosnapTargetsContainerEvent(t *testing.T) {
	enabled := map[string]bool{autosnapKey("n1", "immich-server"): true}
	inv := []*dockercli.Container{
		{ID: "cid-live", Name: "immich-server", Image: "immich/immich-server:v1.2"},
	}

	// Enabled container being destroyed → one target, preferring the live inv id.
	ev := dockercli.ChangeEvent{Type: "container", Action: "destroy", ID: "cid-old", Name: "immich-server"}
	got := autosnapTargets(ev, enabled, inv, "n1")
	if len(got) != 1 || got[0].Name != "immich-server" || got[0].ContainerID != "cid-live" {
		t.Fatalf("want one target immich-server/cid-live, got %+v", got)
	}

	// Same event on a DIFFERENT node (not enabled there) → no target.
	if got := autosnapTargets(ev, enabled, inv, "n2"); len(got) != 0 {
		t.Errorf("autosnap must be node-scoped; got %+v", got)
	}

	// A container that is not enabled → no target.
	ev2 := dockercli.ChangeEvent{Type: "container", Action: "die", ID: "x", Name: "caddy"}
	if got := autosnapTargets(ev2, enabled, inv, "n1"); len(got) != 0 {
		t.Errorf("non-enabled container must not snapshot; got %+v", got)
	}

	// A non-destructive event (start) → no target even for an enabled container.
	ev3 := dockercli.ChangeEvent{Type: "container", Action: "start", Name: "immich-server"}
	if got := autosnapTargets(ev3, enabled, inv, "n1"); len(got) != 0 {
		t.Errorf("start must not snapshot; got %+v", got)
	}
}

// F7: an image pull of the image a protected container runs snapshots that
// container (the incoming-update case); an unrelated pull does not.
func TestAutosnapTargetsImagePull(t *testing.T) {
	enabled := map[string]bool{autosnapKey("n1", "app"): true}
	inv := []*dockercli.Container{
		{ID: "cid-app", Name: "app", Image: "ghcr.io/acme/app:latest"},
		{ID: "cid-db", Name: "db", Image: "postgres:16"}, // not enabled
	}

	// Pull of the same repo (tag moved) → snapshot the app.
	ev := dockercli.ChangeEvent{Type: "image", Action: "pull", Image: "ghcr.io/acme/app:v2"}
	got := autosnapTargets(ev, enabled, inv, "n1")
	if len(got) != 1 || got[0].Name != "app" || got[0].ContainerID != "cid-app" {
		t.Fatalf("want app/cid-app, got %+v", got)
	}

	// Pull of an unrelated image → nothing.
	ev2 := dockercli.ChangeEvent{Type: "image", Action: "pull", Image: "nginx:alpine"}
	if got := autosnapTargets(ev2, enabled, inv, "n1"); len(got) != 0 {
		t.Errorf("unrelated pull must not snapshot; got %+v", got)
	}

	// Pull of the (non-enabled) db image → nothing.
	ev3 := dockercli.ChangeEvent{Type: "image", Action: "pull", Image: "postgres:16"}
	if got := autosnapTargets(ev3, enabled, inv, "n1"); len(got) != 0 {
		t.Errorf("pull for non-enabled container must not snapshot; got %+v", got)
	}
}

func TestImageRepo(t *testing.T) {
	cases := map[string]string{
		"immich/immich-server:v1.2":       "immich/immich-server",
		"ghcr.io/acme/app:latest":         "ghcr.io/acme/app",
		"postgres:16":                     "postgres",
		"registry:5000/team/svc:1.0":      "registry:5000/team/svc",
		"nginx@sha256:abcd":               "nginx",
		"registry:5000/team/svc@sha256:d": "registry:5000/team/svc",
	}
	for in, want := range cases {
		if got := imageRepo(in); got != want {
			t.Errorf("imageRepo(%q) = %q, want %q", in, got, want)
		}
	}
}
