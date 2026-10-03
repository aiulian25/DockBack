package api

import (
	"testing"

	"dockback/internal/dockercli"
	"dockback/internal/notify"
)

// TestClassifyCrash locks in the F24 classification: OOM and non-zero die alert,
// a clean stop and unrelated events do not.
func TestClassifyCrash(t *testing.T) {
	cases := []struct {
		name     string
		ev       dockercli.ChangeEvent
		wantKind string // "" = no alert
	}{
		{"oom kill", dockercli.ChangeEvent{Type: "container", Action: "oom", Name: "app", OOMKilled: true}, notify.KindContainerOOM},
		{"crash exit 1", dockercli.ChangeEvent{Type: "container", Action: "die", Name: "app", ExitCode: 1}, notify.KindContainerCrashed},
		{"crash exit 137", dockercli.ChangeEvent{Type: "container", Action: "die", Name: "app", ExitCode: 137}, notify.KindContainerCrashed},
		{"clean stop", dockercli.ChangeEvent{Type: "container", Action: "die", Name: "app", ExitCode: 0}, ""},
		{"kill without code", dockercli.ChangeEvent{Type: "container", Action: "kill", Name: "app"}, ""},
		{"no name", dockercli.ChangeEvent{Type: "container", Action: "die", ExitCode: 1}, ""},
		{"image event", dockercli.ChangeEvent{Type: "image", Action: "pull", Image: "img:1"}, ""},
	}
	for _, c := range cases {
		kind, title, msg, ok := classifyCrash(c.ev)
		if c.wantKind == "" {
			if ok {
				t.Errorf("%s: expected no alert, got kind=%s", c.name, kind)
			}
			continue
		}
		if !ok || kind != c.wantKind {
			t.Errorf("%s: kind=%s ok=%v, want %s", c.name, kind, ok, c.wantKind)
		}
		if title == "" || msg == "" {
			t.Errorf("%s: title/message must be populated (title=%q msg=%q)", c.name, title, msg)
		}
	}
}
