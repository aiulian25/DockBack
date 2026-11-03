package dockercli

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

func startForCloneTest(t *testing.T, c *client.Client, name string, cfg *container.Config, hc *container.HostConfig) string {
	t.Helper()
	created, err := c.ContainerCreate(context.Background(), cfg, hc, nil, nil, name)
	if err != nil {
		t.Skipf("could not create %s (image alpine:3.20 not present?): %v", name, err)
	}
	t.Cleanup(func() {
		_ = c.ContainerRemove(context.Background(), created.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
	})
	if err := c.ContainerStart(context.Background(), created.ID, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	return created.ID
}

// Against a real daemon, with a source holding extra powers: the copy has none of
// them, and cannot reach a listener that a control container on the ordinary
// bridge can, so the failure is the copy's and not the listener's.
func TestCloneCannotUseHostPowersOrTheNetwork(t *testing.T) {
	c := dockerForTest(t)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	sleep := []string{"sleep", "300"}

	listenerID := startForCloneTest(t, c, fmt.Sprintf("dback-sandbox-listener-%d", suffix),
		&container.Config{Image: "alpine:3.20", Cmd: []string{"sh", "-c", "while true; do echo reached | nc -l -p 8080; done"}},
		&container.HostConfig{})
	listener, err := c.ContainerInspect(ctx, listenerID)
	if err != nil {
		t.Fatal(err)
	}
	connect := []string{"nc", "-w", "2", listener.NetworkSettings.IPAddress, "8080"}

	controlID := startForCloneTest(t, c, fmt.Sprintf("dback-sandbox-control-%d", suffix),
		&container.Config{Image: "alpine:3.20", Cmd: sleep}, &container.HostConfig{})
	var reached []byte
	for attempt := 0; attempt < 10; attempt++ {
		if reached, err = ExecCapture(ctx, c, controlID, connect); err == nil && strings.Contains(string(reached), "reached") {
			break
		}
		time.Sleep(time.Second)
	}
	if !strings.Contains(string(reached), "reached") {
		t.Skipf("the control container cannot reach the listener either, so this proves nothing: %v", err)
	}

	source := fmt.Sprintf(`{"Name":"/source","Image":%q,"Config":{"Image":"alpine:3.20","Cmd":["sleep","300"]},`+
		`"HostConfig":{"CapAdd":["SYS_ADMIN"],"PidMode":"host","NetworkMode":"host","Devices":[{"PathOnHost":"/dev/null","PathInContainer":"/dev/sentinel-device","CgroupPermissions":"rwm"}]}}`, listener.Image)
	cloneName := fmt.Sprintf("dback-sandbox-clone-%d", suffix)
	cloneID, _, _, err := RecreateContainer(ctx, c, []byte(source), "", &CloneOptions{NewName: cloneName}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = RemoveCloneContainer(context.Background(), c, cloneID) })
	if err := c.ContainerStart(ctx, cloneID, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}

	if out, err := ExecCapture(ctx, c, cloneID, connect); err == nil && strings.Contains(string(out), "reached") {
		t.Error("the copy reached a listener on the bridge")
	}
	if out, _ := ExecCapture(ctx, c, cloneID, []string{"ls", "/dev/sentinel-device"}); strings.TrimSpace(string(out)) == "/dev/sentinel-device" {
		t.Error("the copy has the source's device")
	}
	interfaces, err := ExecCapture(ctx, c, cloneID, []string{"cat", "/proc/net/dev"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(interfaces), "eth0") {
		t.Errorf("the copy has a network interface: %s", interfaces)
	}
	status, err := ExecCapture(ctx, c, cloneID, []string{"cat", "/proc/1/comm"})
	if err != nil || strings.TrimSpace(string(status)) != "sleep" {
		t.Errorf("the copy shares the host's PID namespace: %q %v", status, err)
	}
	after, err := c.ContainerInspect(ctx, cloneID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.HostConfig.CapAdd) != 0 || len(after.HostConfig.Devices) != 0 || after.HostConfig.PidMode.IsHost() {
		t.Errorf("host powers survived: caps %v devices %v pid %q", after.HostConfig.CapAdd, after.HostConfig.Devices, after.HostConfig.PidMode)
	}
}
