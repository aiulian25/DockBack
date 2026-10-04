package backup

import (
	"strings"
	"testing"

	"dockback/internal/dockercli"
)

// The restore's answer for a checked compose file, in every case it can be.
func TestComposeCheckSummary(t *testing.T) {
	valid := &dockercli.ComposeCheck{Version: "5.5.1", Valid: true, Hashes: map[string]string{"app": "h1", "db": "h2"}}
	cases := []struct {
		name    string
		check   *dockercli.ComposeCheck
		running map[string]string
		level   string
		want    string
	}{
		{"matches", valid, map[string]string{"app": "h1", "db": "h2"}, "INFO", "will change nothing"},
		{"one differs", valid, map[string]string{"app": "h1", "db": "old"}, "WARN", "would recreate db."},
		{"one missing", valid, map[string]string{"app": "h1"}, "WARN", "would create db (no container yet)"},
		{"nothing to compare", valid, nil, "INFO", "docker-compose.yml is valid."},
		{"invalid", &dockercli.ComposeCheck{Version: "5.5.1", Problem: "env file /srv/app/app.env not found"}, nil, "WARN", "is NOT valid — env file /srv/app/app.env not found"},
		{"warned", &dockercli.ComposeCheck{Valid: true, Warnings: []string{`The "TLS_CERT" variable is not set.`}, Hashes: map[string]string{"app": "h1"}},
			map[string]string{"app": "h1"}, "WARN", `will change nothing — every service matches its running container. Compose warned: The "TLS_CERT" variable is not set.`},
	}
	for _, c := range cases {
		level, message := composeCheckSummary("docker-compose.yml", c.check, c.running)
		if level != c.level || !strings.Contains(message, c.want) {
			t.Errorf("%s: %s %q, want %s containing %q", c.name, level, message, c.level, c.want)
		}
	}
}

// A reconstruction Compose rejects never takes the stack's compose name.
func TestAsideInvalidReconstruction(t *testing.T) {
	layout := asideInvalidReconstruction(stackFolderLayout{
		Primary: []byte("services: broken"),
		Beside:  []dockercli.NamedFile{{Name: "docker-compose.prod.yml.original-from-backup", Content: []byte("x")}},
	})
	if layout.Primary != nil || layout.PrimaryOriginal {
		t.Fatalf("nothing may be written under the compose name: %+v", layout)
	}
	if len(layout.Beside) != 2 || layout.Beside[0].Name != reconstructionComposeName || string(layout.Beside[0].Content) != "services: broken" {
		t.Errorf("the rejected file goes beside, for the operator to fix: %+v", layout.Beside)
	}
}

func TestComposeProjectName(t *testing.T) {
	for in, want := range map[string]string{"My.App 2": "my-app-2", "uptime-kuma": "uptime-kuma", "_x": "x", "!!!": "dockback"} {
		if got := composeProjectName(in); got != want {
			t.Errorf("composeProjectName(%q) = %q, want %q", in, got, want)
		}
	}
}
