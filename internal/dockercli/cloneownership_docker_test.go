package dockercli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
)

// Against a real daemon: a running sentinel holds the name a clone asks for, and
// comes out of it the same container, still running, with its bytes intact.
func TestCloneNameCollisionLeavesTheSentinelUntouched(t *testing.T) {
	c := dockerForTest(t)
	ctx := context.Background()
	name := fmt.Sprintf("dback-clone-sentinel-%d", time.Now().UnixNano())
	const marker = "sentinel-bytes"

	created, err := c.ContainerCreate(ctx, &container.Config{
		Image: "alpine:3.20", Cmd: []string{"sh", "-c", "echo " + marker + " > /marker; exec sleep 300"},
	}, &container.HostConfig{}, nil, nil, name)
	if err != nil {
		t.Skipf("could not create the sentinel (image alpine:3.20 not present?): %v", err)
	}
	t.Cleanup(func() {
		_ = c.ContainerRemove(context.Background(), created.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
	})
	if err := c.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	before, err := c.ContainerInspect(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}

	inspect := fmt.Sprintf(`{"Name":"/source","Image":%q,"Config":{"Image":"alpine:3.20","Cmd":["sleep","1"]},"HostConfig":{}}`, before.Image)
	_, _, _, err = RecreateContainer(ctx, c, []byte(inspect), "", &CloneOptions{NewName: name}, nil, false)
	if err == nil {
		t.Fatal("a clone must not be created under a name in use")
	}

	after, err := c.ContainerInspect(ctx, name)
	if err != nil {
		t.Fatalf("the sentinel is gone: %v", err)
	}
	if after.ID != created.ID || !after.State.Running || after.State.StartedAt != before.State.StartedAt {
		t.Errorf("the sentinel changed: id %s running %v started %s (was %s)", after.ID, after.State.Running, after.State.StartedAt, before.State.StartedAt)
	}
	content, err := ExecCapture(ctx, c, created.ID, []string{"cat", "/marker"})
	if err != nil || !strings.Contains(string(content), marker) {
		t.Errorf("the sentinel's bytes changed: %q %v", content, err)
	}
}

// Against a real daemon: a create the daemon refuses leaves the running original
// exactly as it was, under its own name.
func TestAFailedRecreateLeavesTheOriginalRunning(t *testing.T) {
	c := dockerForTest(t)
	ctx := context.Background()
	name := fmt.Sprintf("dback-recreate-sentinel-%d", time.Now().UnixNano())
	created, err := c.ContainerCreate(ctx, &container.Config{Image: "alpine:3.20", Cmd: []string{"sleep", "300"}}, &container.HostConfig{}, nil, nil, name)
	if err != nil {
		t.Skipf("could not create the sentinel (image alpine:3.20 not present?): %v", err)
	}
	t.Cleanup(func() {
		_ = c.ContainerRemove(context.Background(), created.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
	})
	if err := c.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	before, err := c.ContainerInspect(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}

	unusable := fmt.Sprintf(`{"Name":"/%s","Image":%q,"Config":{"Image":"alpine:3.20"},"HostConfig":{"Binds":["not:a:valid:bind:spec"]}}`, name, before.Image)
	_, _, _, err = RecreateContainer(ctx, c, []byte(unusable), "", nil, nil, false)
	if !errors.Is(err, ErrNothingReplaced) {
		t.Fatalf("want ErrNothingReplaced, got %v", err)
	}

	after, err := c.ContainerInspect(ctx, name)
	if err != nil {
		t.Fatalf("the original is gone: %v", err)
	}
	if after.ID != created.ID || !after.State.Running || after.State.StartedAt != before.State.StartedAt {
		t.Errorf("the original changed: id %s running %v started %s (was %s)", after.ID, after.State.Running, after.State.StartedAt, before.State.StartedAt)
	}
}
