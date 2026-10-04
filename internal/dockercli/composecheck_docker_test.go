package dockercli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Step 15: what was checked by hand in the 2026-10-04 recovery, now done by the
// restore — valid or not, what Compose warns about, and the hashes it compares.
func TestComposeCheckerJudgesFoldersAndFiles(t *testing.T) {
	c := dockerForTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	checker, err := OpenComposeChecker(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer checker.Close()

	good := filepath.Join(t.TempDir(), "stack")
	writeFiles(t, good, map[string]string{
		"docker-compose.yml": "services:\n  app:\n    image: alpine\n    environment:\n      TOKEN: ${TOKEN}\n      LOST: ${NOT_DEFINED}\n",
		".env":               "TOKEN=abc\n",
	})
	check, err := checker.CheckFolder(ctx, good, "docker-compose.yml", "stack")
	if err != nil {
		t.Fatal(err)
	}
	if !check.Valid || check.Hashes["app"] == "" || check.Version == "" {
		t.Fatalf("a valid file must pass with a hash and the Compose version: %+v", check)
	}
	if !strings.Contains(strings.Join(check.Warnings, " "), `"NOT_DEFINED" variable is not set`) {
		t.Errorf("Compose's warning about a blank variable must be kept: %v", check.Warnings)
	}

	bad := filepath.Join(t.TempDir(), "stack")
	writeFiles(t, bad, map[string]string{"docker-compose.yml": "services:\n  app:\n    image: alpine\n    env_file: ./app.env\n"})
	check, err = checker.CheckFolder(ctx, bad, "docker-compose.yml", "stack")
	if err != nil {
		t.Fatal(err)
	}
	if check.Valid || !strings.Contains(check.Problem, "app.env not found") {
		t.Errorf("a missing env_file must be reported in Compose's words: %+v", check)
	}

	check, err = checker.CheckFiles(ctx, []byte("services:\n  app:\n    image: alpine\n    environment:\n      PASSWORD: ${DB_PASSWORD}\n"), []byte("DB_PASSWORD=x\n"), "rebuilt")
	if err != nil {
		t.Fatal(err)
	}
	if !check.Valid || len(check.Warnings) != 0 {
		t.Errorf("a file not on the host yet is checked with its .env beside it: %+v", check)
	}
	check, err = checker.CheckFiles(ctx, []byte("services:\n  app:\n    networks: [missing]\n"), nil, "rebuilt")
	if err != nil {
		t.Fatal(err)
	}
	if check.Valid {
		t.Error("a service with no image and an undeclared network must be rejected")
	}
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
