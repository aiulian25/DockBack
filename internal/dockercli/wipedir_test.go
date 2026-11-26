package dockercli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// SEC-006: the folder a restore wipes is handed to a shell. It must be treated as
// a name, whatever characters it holds.

func TestWipeDirCommandRefusesWhatIsNotAFolderInside(t *testing.T) {
	for _, dir := range []string{"/", "//", "/.", "/data/..", "data", "./data", "../etc"} {
		if cmd, err := wipeDirCommand(dir); err == nil {
			t.Errorf("%q must be refused, got command %q", dir, cmd)
		}
	}
}

func TestWipeDirCommandTreatsThePathAsAName(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "ran")
	// A folder whose name would run commands if a shell expanded it.
	hostile := filepath.Join(root, "data $(touch "+marker+") `touch "+marker+"` 'q' \"d\"")
	if err := os.MkdirAll(filepath.Join(hostile, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"file", ".hidden", "..odd", "sub/nested"} {
		if err := os.WriteFile(filepath.Join(hostile, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bystander := filepath.Join(root, "bystander")
	if err := os.WriteFile(bystander, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd, err := wipeDirCommand(hostile)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-c", cmd).CombinedOutput(); err != nil {
		t.Fatalf("the wipe must succeed: %v %s", err, out)
	}

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the folder's name was run as a command")
	}
	left, err := os.ReadDir(hostile)
	if err != nil || len(left) != 0 {
		t.Fatalf("the folder must be left empty, got %v (%v)", left, err)
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Fatal("nothing outside the folder may be touched")
	}
}
