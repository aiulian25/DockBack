package dockercli

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

// dockerTestsEnv opts into the tests that drive a real Docker daemon. They create
// and remove containers, so `go test ./...` never runs them by accident.
const dockerTestsEnv = "DOCKBACK_DOCKER_TESTS"

func dockerForTest(t *testing.T) *client.Client {
	t.Helper()
	if os.Getenv(dockerTestsEnv) != "1" {
		t.Skipf("set %s=1 to run tests against the local Docker daemon", dockerTestsEnv)
	}
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Ping(context.Background()); err != nil {
		t.Skipf("no Docker daemon reachable: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func tarOf(t *testing.T, files map[string]string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

// The real failure from the 2026-10-04 recovery: a stack whose secrets file was bound
// :ro could not be restored ("tar: can't remove old file secrets/app.key:
// Read-only file system"), and the restore died half-way. Restoring must write
// into read-only mounts while leaving the container's own mounts read-only.
func TestRestoreWritesIntoReadOnlyMounts(t *testing.T) {
	c := dockerForTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	host := t.TempDir()
	secretsDir := filepath.Join(host, "secrets")
	keyFile := filepath.Join(host, "app.key")
	if err := os.MkdirAll(secretsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretsDir, "app.key"), []byte("damaged"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte("damaged"), 0o644); err != nil {
		t.Fatal(err)
	}
	volumeName := "dockback-test-ro-" + time.Now().Format("150405.000000")
	if _, err := c.VolumeCreate(ctx, volume.CreateOptions{Name: volumeName}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.VolumeRemove(context.Background(), volumeName, true) })

	if err := ensureSidecar(ctx, c); err != nil {
		t.Fatal(err)
	}
	target, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: []string{"sleep", "300"}},
		&container.HostConfig{Binds: []string{
			secretsDir + ":/secrets:ro",
			keyFile + ":/run/secrets/app.key:ro",
			volumeName + ":/vol:ro",
		}}, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeContainer(c, target.ID) })
	if err := c.ContainerStart(ctx, target.ID, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}

	// What a volumes archive really holds: directory binds and volumes. A
	// single-file bind is captured and written back separately, so it is not in
	// here — but it IS mounted on the container, and its presence must not stop
	// the sidecar from being created.
	archive := tarOf(t, map[string]string{
		"secrets/app.key": "restored-dir-key",
		"vol/state.db":    "restored-volume",
	})
	if err := UntarToVolumes(ctx, c, target.ID, archive); err != nil {
		t.Fatalf("restoring into read-only mounts failed: %v", err)
	}

	// Read back through the container's own view (as root, like the app): the
	// restored secret is root-owned and mode 600, exactly as a secret should be.
	for path, want := range map[string]string{
		"/secrets/app.key":     "restored-dir-key",
		"/run/secrets/app.key": "damaged", // not in the archive, untouched
		"/vol/state.db":        "restored-volume",
	} {
		out, err := CaptureSidecarRO(ctx, c, target.ID, []string{"cat", path})
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if string(out) != want {
			t.Errorf("%s = %q, want %q", path, out, want)
		}
	}

	// The container itself must still be read-only everywhere it was.
	info, err := c.ContainerInspect(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range info.Mounts {
		if m.RW {
			t.Errorf("the container's mount %s became writable", m.Destination)
		}
	}
}

// A deleted file bind's source comes back from Docker as an empty DIRECTORY
// when the container restarts — that is how a VAPID private key "came back as
// mode 755" in the 2026-10-04 recovery. Writing the backed-up file must replace that
// artefact, and must never swallow a directory that holds data.
func TestWritingAFileBindReplacesTheEmptyDirectoryDockerInvented(t *testing.T) {
	c := dockerForTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	host := t.TempDir()
	owner := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())

	artefact := filepath.Join(host, "vapid_private_key")
	if err := os.Mkdir(artefact, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteHostFile(ctx, c, artefact, []byte("the-real-key"), owner, "600"); err != nil {
		t.Fatalf("writing over the empty directory artefact failed: %v", err)
	}
	info, err := os.Stat(artefact)
	if err != nil {
		t.Fatal(err)
	}
	if info.IsDir() {
		t.Fatal("the key is still a directory")
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
	if got, _ := os.ReadFile(artefact); string(got) != "the-real-key" {
		t.Errorf("content = %q", got)
	}

	// A directory that holds something is not an artefact: refuse, keep it.
	occupied := filepath.Join(host, "occupied")
	if err := os.MkdirAll(filepath.Join(occupied, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteHostFile(ctx, c, occupied, []byte("x"), owner, "600"); err == nil {
		t.Error("a non-empty directory in the way must be refused")
	}
	if _, err := os.Stat(filepath.Join(occupied, "keep")); err != nil {
		t.Errorf("the directory's contents must survive the refusal: %v", err)
	}
}
