package backup

import (
	"fmt"
	"testing"

	"dockback/internal/dockercli"
)

func TestParseProjectIgnore(t *testing.T) {
	got := parseProjectIgnore([]byte("# build output\n\n*.log\n  /build/  \ncache/data\n"))
	if fmt.Sprint(got) != "[*.log /build/ cache/data]" {
		t.Errorf("got %v", got)
	}
}

// .dockbackignore reads like .gitignore: a bare name matches at any depth, a
// pattern with a slash, or a leading one, matches from the project root.
func TestIgnoredBy(t *testing.T) {
	patterns := []string{"*.log", "/build/", "cache/data", "secrets.env"}
	for rel, want := range map[string]bool{
		"app.log":                true,
		"logs/today.log":         true,
		"build":                  true,
		"build/out.bin":          true,
		"tools/build":            false, // anchored: only the root's build
		"cache/data":             true,
		"cache/data/x":           true,
		"cache":                  false,
		"deep/secrets.env":       true,
		"scripts/backup.sh":      false,
		"README.md":              false,
		"scripts/logrotate.conf": false,
	} {
		if got := ignoredBy(rel, patterns); got != want {
			t.Errorf("ignoredBy(%q) = %v, want %v", rel, got, want)
		}
	}
	if !ignoredBy("web/node_modules/x/index.js", projectFolderDefaultIgnores) {
		t.Error("node_modules is never worth carrying, at any depth")
	}
}

// The volume capture owns every bind-mounted path inside the project folder,
// whether it captured it or left it out on purpose (a media library).
func TestBindPathsUnder(t *testing.T) {
	sources := []string{"/srv/app/data", "/srv/app/config/app.ini", "/srv/app/data", "/mnt/media", "/srv/app"}
	if got := fmt.Sprint(bindPathsUnder(sources, "/srv/app")); got != "[config/app.ini data]" {
		t.Errorf("only binds inside the folder, relative, once each: %s", got)
	}
}

// A side-car that mounts nothing — a PDF converter beside a document manager —
// walked the whole project folder, siblings' data included, and tripped the
// size cap. Every service's binds are left out, and only binds count.
func TestDockerBindSourcesTakesOnlyBinds(t *testing.T) {
	mounts := []dockercli.Mount{
		{Type: "bind", Source: "/srv/app/media"},
		{Type: "volume", Name: "app_db", Source: "/var/lib/docker/volumes/app_db/_data"},
		{Type: "tmpfs", Destination: "/tmp"},
	}
	if got := fmt.Sprint(dockerBindSources(mounts)); got != "[/srv/app/media]" {
		t.Errorf("only bind sources: %s", got)
	}
}

func TestSelectProjectEntriesKeepsFoldersFirst(t *testing.T) {
	got := selectProjectEntries([]string{"scripts/run.sh", "app.log", "scripts", "README.md", "build/x", "build"}, []string{"*.log", "/build"})
	if fmt.Sprint(got) != "[README.md scripts scripts/run.sh]" {
		t.Errorf("got %v", got)
	}
}
