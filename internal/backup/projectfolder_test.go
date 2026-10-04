package backup

import (
	"fmt"
	"testing"

	"github.com/docker/docker/api/types"
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
	mounts := []types.MountPoint{
		{Type: "bind", Source: "/srv/app/data"},
		{Type: "bind", Source: "/srv/app/config/app.ini"},
		{Type: "bind", Source: "/srv/app/data"},
		{Type: "bind", Source: "/mnt/media"},
		{Type: "bind", Source: "/srv/app"},
		{Type: "volume", Name: "app_db", Source: "/var/lib/docker/volumes/app_db/_data"},
	}
	if got := fmt.Sprint(bindPathsUnder(mounts, "/srv/app")); got != "[config/app.ini data]" {
		t.Errorf("only binds inside the folder, relative, once each: %s", got)
	}
}

func TestSelectProjectEntriesKeepsFoldersFirst(t *testing.T) {
	got := selectProjectEntries([]string{"scripts/run.sh", "app.log", "scripts", "README.md", "build/x", "build"}, []string{"*.log", "/build"})
	if fmt.Sprint(got) != "[README.md scripts scripts/run.sh]" {
		t.Errorf("got %v", got)
	}
}
