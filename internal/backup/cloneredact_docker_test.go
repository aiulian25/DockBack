package backup

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"

	"dockback/internal/store"
)

const (
	dockerTestsVariable = "DOCKBACK_DOCKER_TESTS"
	plexProfileImage    = "linuxserver/plex:latest"
	redactionTestImage  = "alpine:3.20"
	invented            = "invented-token-value"
	preferencesFixture  = `<Preferences MachineIdentifier="keep-me" PlexOnlineToken="` + invented + `" PublishServerOnPlexOnlineKey="1"/>`
)

func redactionClient(t *testing.T) *client.Client {
	t.Helper()
	if os.Getenv(dockerTestsVariable) != "1" {
		t.Skipf("set %s=1 to run tests against a real Docker daemon", dockerTestsVariable)
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Ping(context.Background()); err != nil {
		t.Skipf("no Docker daemon reachable: %v", err)
	}
	t.Cleanup(func() { cli.Close() })
	return cli
}

// inVolume runs a script against the volume in a container of its own and returns
// its output, the way an operator looking at the files would.
func inVolume(t *testing.T, cli *client.Client, volumeName, script string) string {
	t.Helper()
	ctx := context.Background()
	created, err := cli.ContainerCreate(ctx,
		&container.Config{Image: redactionTestImage, Cmd: []string{"sh", "-c", script}, Tty: true},
		&container.HostConfig{Binds: []string{volumeName + ":/config"}}, nil, nil, "")
	if err != nil {
		t.Skipf("could not create a container (image %s not present?): %v", redactionTestImage, err)
	}
	defer cli.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})
	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	status, errs := cli.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	select {
	case <-status:
	case err := <-errs:
		t.Fatal(err)
	}
	logs, err := cli.ContainerLogs(ctx, created.ID, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	var out strings.Builder
	buffer := make([]byte, 4096)
	for {
		n, rerr := logs.Read(buffer)
		out.Write(buffer[:n])
		if rerr != nil {
			break
		}
	}
	return out.String()
}

type stoppedCopy struct {
	id, volume, preferences string
}

// aStoppedCopy is what a restore has at this point: a container that has never run,
// with its restored file in a volume of its own.
func aStoppedCopy(t *testing.T, cli *client.Client, extraSetup string) stoppedCopy {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("dback-redact-%d", time.Now().UnixNano())
	if _, err := cli.VolumeCreate(ctx, volume.CreateOptions{Name: name}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.VolumeRemove(context.Background(), name, true) })
	preferences := pmsDataDir + "/Preferences.xml"
	inVolume(t, cli, name, fmt.Sprintf(`mkdir -p '%s' && printf '%%s' '%s' > '%s' && chown 1000:1000 '%s' && chmod 640 '%s'; %s`,
		pmsDataDir, preferencesFixture, preferences, preferences, preferences, extraSetup))
	created, err := cli.ContainerCreate(ctx,
		&container.Config{Image: redactionTestImage, Cmd: []string{"sleep", "300"}},
		&container.HostConfig{Binds: []string{name + ":/config"}}, nil, nil, name)
	if err != nil {
		t.Skipf("could not create the copy: %v", err)
	}
	t.Cleanup(func() {
		_ = cli.ContainerRemove(context.Background(), created.ID, container.RemoveOptions{Force: true})
	})
	return stoppedCopy{id: created.ID, volume: name, preferences: preferences}
}

func neverStarted(t *testing.T, cli *client.Client, id string) {
	t.Helper()
	info, err := cli.ContainerInspect(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Running || !strings.HasPrefix(info.State.StartedAt, "0001-") {
		t.Fatalf("the copy ran: running %v, started %s", info.State.Running, info.State.StartedAt)
	}
}

func neutralize(cli *client.Client, target string) error {
	e := &Engine{Log: func(string, string, string) {}}
	return e.neutralizeClone(context.Background(), cli, &store.Backup{ID: "b1"},
		&Manifest{Image: plexProfileImage}, RestoreOptions{TargetID: target})
}

// The finding: this could never succeed, because a copy is stopped at this point.
func TestAStoppedPlexCopyIsNeutralisedWithoutEverRunning(t *testing.T) {
	cli := redactionClient(t)
	target := aStoppedCopy(t, cli, "")
	if err := neutralize(cli, target.id); err != nil {
		t.Fatalf("a stopped copy must be neutralised: %v", err)
	}
	neverStarted(t, cli, target.id)

	content := inVolume(t, cli, target.volume, fmt.Sprintf(`cat '%s'`, target.preferences))
	if strings.Contains(content, invented) {
		t.Fatalf("the token is still there: %s", content)
	}
	for _, want := range []string{`PlexOnlineToken=""`, `PublishServerOnPlexOnlineKey=""`, `MachineIdentifier="keep-me"`} {
		if !strings.Contains(content, want) {
			t.Errorf("%s missing from %s", want, content)
		}
	}
	listing := inVolume(t, cli, target.volume, fmt.Sprintf(`stat -c '%%u:%%g %%a' '%s'; ls -a '%s'`, target.preferences, pmsDataDir))
	if !strings.Contains(listing, "1000:1000 640") {
		t.Errorf("the file should keep its owner and mode: %s", listing)
	}
	if strings.Contains(listing, "dockback") {
		t.Errorf("working copies were left behind: %s", listing)
	}
}

func TestAMissingFileIsNothingToClear(t *testing.T) {
	cli := redactionClient(t)
	target := aStoppedCopy(t, cli, fmt.Sprintf(`rm -f '%s/Preferences.xml'`, pmsDataDir))
	if err := neutralize(cli, target.id); err != nil {
		t.Fatalf("no file holds no identity to clear: %v", err)
	}
	neverStarted(t, cli, target.id)
}

func TestARedactionThatFailsRefusesTheCopyAndLeavesItStopped(t *testing.T) {
	cli := redactionClient(t)
	// A directory where the working file goes makes the edit itself fail.
	target := aStoppedCopy(t, cli, fmt.Sprintf(`mkdir '%s/Preferences.xml.dockback-tmp'`, pmsDataDir))
	err := neutralize(cli, target.id)
	if err == nil || !strings.Contains(err.Error(), "was not started") {
		t.Fatalf("a required redaction that failed must refuse the copy: %v", err)
	}
	if strings.Contains(err.Error(), invented) {
		t.Fatalf("the error holds the value: %v", err)
	}
	neverStarted(t, cli, target.id)
	listing := inVolume(t, cli, target.volume, fmt.Sprintf(`ls -a '%s'`, pmsDataDir))
	if strings.Contains(listing, "pre-redact") {
		t.Errorf("the working copy of the token was left behind: %s", listing)
	}
}
