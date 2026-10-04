package dockercli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// forgejo's scripts and README had to come back from a guide on the 2026-10-04 recovery
// night. The project folder is captured without its bind-mounted data and its
// noise, and restored without replacing anything already there.
func TestProjectFolderRoundTrip(t *testing.T) {
	c := dockerForTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	project := filepath.Join(t.TempDir(), "forgejo")
	for rel, body := range map[string]string{
		"scripts/backup.sh":   "#!/bin/sh\necho backup\n",
		"README.md":           "how to run this stack\n",
		"data/repo.db":        "bind-mounted data, owned by the volume capture\n",
		"node_modules/x/i.js": "rebuilt by npm\n",
	} {
		p := filepath.Join(project, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(project, "scripts"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(project, "scripts/backup.sh"), 0o755); err != nil {
		t.Fatal(err)
	}

	folder, err := OpenProjectFolder(ctx, c, project)
	if err != nil {
		t.Fatal(err)
	}
	defer folder.Close()
	entries, err := folder.Entries(ctx, []string{"data"}, []string{"node_modules"})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(entries)
	if got := strings.Join(entries, " "); got != "README.md scripts scripts/backup.sh" {
		t.Fatalf("the bind-mounted data and node_modules must not even be walked: %s", got)
	}
	var archive bytes.Buffer
	if _, err := folder.Tar(ctx, entries, &archive, 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := folder.Tar(ctx, entries, &bytes.Buffer{}, 100); !errors.Is(err, ErrProjectFolderTooBig) {
		t.Errorf("a capture past its cap must stop: %v", err)
	}

	restored := filepath.Join(t.TempDir(), "forgejo")
	if err := os.MkdirAll(restored, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(restored, "README.md"), []byte("the operator's newer README\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	kept, err := RestoreProjectFolder(ctx, c, restored, &archive)
	if err != nil {
		t.Fatal(err)
	}
	if kept != 1 {
		t.Errorf("one existing file must be reported as kept, got %d", kept)
	}
	if got, _ := os.ReadFile(filepath.Join(restored, "README.md")); string(got) != "the operator's newer README\n" {
		t.Errorf("an existing file is never replaced, got %q", got)
	}
	script, err := os.Stat(filepath.Join(restored, "scripts/backup.sh"))
	if err != nil || script.Mode().Perm() != 0o755 {
		t.Fatalf("the missing script must come back executable: %v %v", script, err)
	}
	if dir, _ := os.Stat(filepath.Join(restored, "scripts")); dir == nil || dir.Mode().Perm() != 0o750 {
		t.Errorf("a folder comes back with its own mode, not root's default: %v", dir)
	}
	if _, err := os.Stat(filepath.Join(restored, "data")); !os.IsNotExist(err) {
		t.Errorf("bind-mounted data is the volume capture's, not the project folder's: %v", err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(restored), ".*"+projectStagingSuffix)); len(leftovers) > 0 {
		t.Errorf("the staging folder must be removed: %v", leftovers)
	}
}

// Reading a project must never create its folder.
func TestOpenProjectFolderRefusesAMissingFolder(t *testing.T) {
	c := dockerForTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	missing := filepath.Join(t.TempDir(), "gone", "stack")
	if folder, err := OpenProjectFolder(ctx, c, missing); err == nil {
		folder.Close()
		t.Fatal("a missing folder must be refused")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("the attempt created the folder it was reading: %v", err)
	}
}
