package dockercli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A restore lays a stack folder down by one rule for every file: identical is
// left alone, different is renamed aside and never overwritten, and every file
// is private from the first byte, because the operator's own compose file can
// hold passwords.
func TestStackFolderConvergesAndNeverOverwrites(t *testing.T) {
	c := dockerForTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	stackDir := filepath.Join(t.TempDir(), "mystack")
	owner := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	original := []byte("services: {app: {image: alpine, environment: {PASSWORD: inline}}}\n")
	env := []byte("TOKEN=abc\n")
	beside := []NamedFile{{Name: "docker-compose.dockback.yml", Content: []byte("services: {rebuilt: {}}\n")}}

	if _, err := ReconstructStackDirWithEnv(ctx, c, stackDir, "docker-compose.yml", original, env, owner, ReplaceExisting, beside...); err != nil {
		t.Fatalf("first write: %v", err)
	}
	for name, want := range map[string]string{
		"docker-compose.yml":          string(original),
		".env":                        string(env),
		"docker-compose.dockback.yml": "services: {rebuilt: {}}\n",
	} {
		p := filepath.Join(stackDir, name)
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
		if info, _ := os.Stat(p); info.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %o, want 600", name, info.Mode().Perm())
		}
	}

	again, err := ReconstructStackDirWithEnv(ctx, c, stackDir, "docker-compose.yml", original, env, owner, ReplaceExisting, beside...)
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if !again.Unchanged || len(again.BesideDisplaced) != 0 || again.EnvDisplaced != "" {
		t.Errorf("an identical re-run must change nothing, got %+v", again)
	}

	edited := []byte("services: {hand-edited: {}}\n")
	if err := os.WriteFile(filepath.Join(stackDir, "docker-compose.dockback.yml"), edited, 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := ReconstructStackDirWithEnv(ctx, c, stackDir, "docker-compose.yml", original, env, owner, ReplaceExisting, beside...)
	if err != nil {
		t.Fatalf("third write: %v", err)
	}
	if len(third.BesideDisplaced) != 1 {
		t.Fatalf("a different file in the way must be renamed aside, got %+v", third)
	}
	kept, err := os.ReadFile(third.BesideDisplaced[0])
	if err != nil || string(kept) != string(edited) {
		t.Errorf("the file that was in the way must survive intact as %s: %q, %v", third.BesideDisplaced[0], kept, err)
	}
	if got, _ := os.ReadFile(filepath.Join(stackDir, "docker-compose.dockback.yml")); string(got) != "services: {rebuilt: {}}\n" {
		t.Errorf("the new file must take the name, got %q", got)
	}
}

// Step 15: with no valid compose file to place, the folder gets its .env and
// the files beside, and whatever compose file is already there stays untouched.
func TestStackFolderWithoutAComposeFileLeavesTheExistingOne(t *testing.T) {
	c := dockerForTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	stackDir := filepath.Join(t.TempDir(), "mystack")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mine := []byte("services: {mine: {image: alpine}}\n")
	if err := os.WriteFile(filepath.Join(stackDir, "docker-compose.yml"), mine, 0o644); err != nil {
		t.Fatal(err)
	}
	owner := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	res, err := ReconstructStackDirWithEnv(ctx, c, stackDir, "docker-compose.yml", nil, []byte("TOKEN=abc\n"), owner, ReplaceExisting,
		NamedFile{Name: "docker-compose.dockback.yml", Content: []byte("services: broken\n")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != "" || res.Displaced != "" {
		t.Errorf("no compose file may be written or displaced: %+v", res)
	}
	if got, _ := os.ReadFile(filepath.Join(stackDir, "docker-compose.yml")); string(got) != string(mine) {
		t.Errorf("the operator's compose file must stay as it is, got %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(stackDir, "docker-compose.dockback.yml")); string(got) != "services: broken\n" {
		t.Errorf("the rejected reconstruction goes beside: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(stackDir, ".env")); string(got) != "TOKEN=abc\n" {
		t.Errorf("the .env still lands: %q", got)
	}
}

// A files-only restore promises that nothing existing changes. Every file already
// in the folder must keep its bytes, its name and its mode; only what is missing
// is written.
func TestKeepExistingChangesNothingThatIsThere(t *testing.T) {
	c := dockerForTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	stackDir := filepath.Join(t.TempDir(), "mystack")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const operatorMode = 0o644
	theirs := map[string]string{
		"compose.yaml":                "services: {edited-since: {image: alpine}}\n",
		".env":                        "APP_MODE=staging\n",
		"docker-compose.dockback.yml": "services: {hand-edited: {}}\n",
	}
	for name, body := range theirs {
		if err := os.WriteFile(filepath.Join(stackDir, name), []byte(body), operatorMode); err != nil {
			t.Fatal(err)
		}
	}
	owner := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	const missingName = "notes.original-from-backup"
	res, err := ReconstructStackDirWithEnv(ctx, c, stackDir, "docker-compose.yml",
		[]byte("services: {from-backup: {image: alpine}}\n"), []byte("APP_MODE=production\n"), owner, KeepExisting,
		NamedFile{Name: "docker-compose.dockback.yml", Content: []byte("services: {rebuilt: {}}\n")},
		NamedFile{Name: missingName, Content: []byte("restored\n")})
	if err != nil {
		t.Fatal(err)
	}

	for name, body := range theirs {
		p := filepath.Join(stackDir, name)
		if got, _ := os.ReadFile(p); string(got) != body {
			t.Errorf("%s was changed to %q", name, got)
		}
		if info, _ := os.Stat(p); info.Mode().Perm() != operatorMode {
			t.Errorf("%s had its mode changed to %o", name, info.Mode().Perm())
		}
	}
	if got, _ := os.ReadFile(filepath.Join(stackDir, missingName)); string(got) != "restored\n" {
		t.Errorf("the missing file was not written: %q", got)
	}
	if _, err := os.Stat(filepath.Join(stackDir, "docker-compose.yml")); !os.IsNotExist(err) {
		t.Errorf("a second compose file was added beside the one Compose reads: %v", err)
	}
	if displaced, _ := filepath.Glob(filepath.Join(stackDir, "*"+displacedSuffix+"*")); len(displaced) != 0 {
		t.Errorf("files were renamed aside: %v", displaced)
	}

	// What the result says.
	if len(res.Kept) != len(theirs) || len(res.Failed) != 0 {
		t.Errorf("want the %d existing files kept and none failed, got kept %v, failed %v", len(theirs), res.Kept, res.Failed)
	}
}

// A symlink at a name DockBack writes must never be written through, and the file
// that could not be written must be named, not skipped in silence.
func TestAWriteNeverFollowsASymlinkAtTheName(t *testing.T) {
	c := dockerForTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	parent := t.TempDir()
	stackDir := filepath.Join(parent, "mystack")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const besideName = "docker-compose.dockback.yml"
	if err := os.Symlink("../escape", filepath.Join(stackDir, besideName)); err != nil {
		t.Fatal(err)
	}
	owner := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	res, err := ReconstructStackDirWithEnv(ctx, c, stackDir, "docker-compose.yml",
		[]byte("services: {app: {image: alpine}}\n"), nil, owner, ReplaceExisting,
		NamedFile{Name: besideName, Content: []byte("services: {rebuilt: {}}\n")})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(filepath.Join(parent, "escape")); !os.IsNotExist(err) {
		t.Errorf("the write followed the symlink out of the stack folder: %v", err)
	}

	// What the result says.
	if len(res.Failed) != 1 || !strings.Contains(res.Failed[0], besideName) {
		t.Errorf("the file that could not be written must be named, got %v", res.Failed)
	}
}
