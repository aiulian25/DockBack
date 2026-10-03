package dockercli

import (
	"testing"

	"github.com/docker/docker/api/types/events"
)

func TestRelevantEvent(t *testing.T) {
	cases := []struct {
		typ  events.Type
		act  events.Action
		want bool
	}{
		// Container lifecycle changes the inventory.
		{events.ContainerEventType, "start", true},
		{events.ContainerEventType, "die", true},
		{events.ContainerEventType, "create", true},
		{events.ContainerEventType, "destroy", true},
		{events.ContainerEventType, "pause", true},
		{events.ContainerEventType, "rename", true},
		// Noise that must NOT trigger a refresh.
		{events.ContainerEventType, "health_status: healthy", false},
		{events.ContainerEventType, "exec_start", false},
		{events.ContainerEventType, "exec_create", false},
		{events.ContainerEventType, "top", false},
		// Volumes / networks / images.
		{events.VolumeEventType, "create", true},
		{events.VolumeEventType, "mount", false},
		{events.NetworkEventType, "connect", true},
		{events.ImageEventType, "pull", true},
		{events.ImageEventType, "untag", true},
		// Unrelated types.
		{events.DaemonEventType, "reload", false},
		{events.BuilderEventType, "build", false},
	}
	for _, c := range cases {
		got := relevantEvent(events.Message{Type: c.typ, Action: c.act})
		if got != c.want {
			t.Errorf("relevantEvent(%s/%s) = %v, want %v", c.typ, c.act, got, c.want)
		}
	}
}

// TestChangeEventFrom locks in the F24 crash/OOM plumbing: a die carries its exit
// code, an oom flags OOMKilled, and a clean stop stays exit 0.
func TestChangeEventFrom(t *testing.T) {
	die := func(name, code string) events.Message {
		return events.Message{
			Type:   events.ContainerEventType,
			Action: "die",
			Actor:  events.Actor{ID: "cid1", Attributes: map[string]string{"name": name, "image": "img:1", "exitCode": code}},
		}
	}

	// A crash: non-zero exit code is parsed onto ExitCode.
	if ev := changeEventFrom(die("app", "137")); ev.ExitCode != 137 || ev.OOMKilled || ev.Name != "app" {
		t.Errorf("die exit 137: got %+v, want ExitCode=137 OOMKilled=false Name=app", ev)
	}
	// A clean stop: exit 0, no OOM.
	if ev := changeEventFrom(die("app", "0")); ev.ExitCode != 0 || ev.OOMKilled {
		t.Errorf("die exit 0: got %+v, want ExitCode=0 OOMKilled=false", ev)
	}
	// A missing/garbage exitCode attribute must not panic and stays 0.
	if ev := changeEventFrom(die("app", "")); ev.ExitCode != 0 {
		t.Errorf("die no code: got ExitCode=%d, want 0", ev.ExitCode)
	}
	// An oom event flags OOMKilled (Docker sends no exit code with it).
	oom := events.Message{
		Type:   events.ContainerEventType,
		Action: "oom",
		Actor:  events.Actor{ID: "cid1", Attributes: map[string]string{"name": "app", "image": "img:1"}},
	}
	if ev := changeEventFrom(oom); !ev.OOMKilled || ev.Name != "app" {
		t.Errorf("oom: got %+v, want OOMKilled=true Name=app", ev)
	}
}
