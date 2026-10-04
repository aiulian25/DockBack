package dockercli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
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

	if _, err := ReconstructStackDirWithEnv(ctx, c, stackDir, "docker-compose.yml", original, env, owner, beside...); err != nil {
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

	again, err := ReconstructStackDirWithEnv(ctx, c, stackDir, "docker-compose.yml", original, env, owner, beside...)
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
	third, err := ReconstructStackDirWithEnv(ctx, c, stackDir, "docker-compose.yml", original, env, owner, beside...)
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
	res, err := ReconstructStackDirWithEnv(ctx, c, stackDir, "docker-compose.yml", nil, []byte("TOKEN=abc\n"), owner,
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
