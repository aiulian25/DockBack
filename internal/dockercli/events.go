package dockercli

import (
	"context"
	"strconv"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/client"
)

// ChangeEvent is the minimal, docker-type-free view of an inventory-changing
// Docker event that callers react to (an inventory refresh, or an event-triggered
// protective snapshot — F7). It carries just enough to identify the affected
// container/image without leaking the docker events API to callers.
type ChangeEvent struct {
	Type      string // "container" | "image" | "volume" | "network"
	Action    string // e.g. "create" | "destroy" | "die" | "kill" | "stop" | "pull"
	ID        string // Actor.ID: container id (container events) or image id (image events)
	Name      string // container name (container events)
	Image     string // the container's image (container events) / the image ref (image events)
	ExitCode  int    // container "die" exit code (0 = clean stop; non-zero = crash) — F24
	OOMKilled bool   // container was OOM-killed (an "oom" event) — F24
}

// StreamRelevantEvents subscribes to a node's Docker event stream and invokes
// onChange for every event that should trigger an inventory refresh, until ctx
// is canceled or the stream errors (PLAN §4.13: refresh on events, not constant
// polling). It returns the stream error so the caller can reconnect with
// back-off. Noisy, inventory-irrelevant events (health checks, exec, container
// stats/top) are filtered out so a steady-state node stays quiet and a debounced
// caller doesn't churn.
func StreamRelevantEvents(ctx context.Context, c *client.Client, onChange func(ChangeEvent)) error {
	msgs, errs := c.Events(ctx, events.ListOptions{})
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errs:
			return err
		case m, ok := <-msgs:
			if !ok {
				return nil
			}
			if relevantEvent(m) {
				onChange(changeEventFrom(m))
			}
		}
	}
}

// changeEventFrom projects a Docker event into the caller-facing ChangeEvent.
func changeEventFrom(m events.Message) ChangeEvent {
	ev := ChangeEvent{Action: string(m.Action), ID: m.Actor.ID}
	switch m.Type {
	case events.ContainerEventType:
		ev.Type = "container"
		if m.Actor.Attributes != nil {
			ev.Name = m.Actor.Attributes["name"]
			ev.Image = m.Actor.Attributes["image"]
			// F24: a "die" carries the process exit code; 0 is a clean stop, any
			// other value is a crash. Docker only sets this attribute on die.
			if code, err := strconv.Atoi(m.Actor.Attributes["exitCode"]); err == nil {
				ev.ExitCode = code
			}
		}
		// F24: Docker emits a dedicated "oom" event (no exit code) just before the
		// container's die when the kernel OOM-kills it.
		if m.Action == "oom" {
			ev.OOMKilled = true
		}
	case events.ImageEventType:
		ev.Type = "image"
		ev.Image = m.Actor.ID // image events use the ref/id as the actor id
		if m.Actor.Attributes != nil {
			if n := m.Actor.Attributes["name"]; n != "" {
				ev.Image = n
			}
		}
	case events.VolumeEventType:
		ev.Type = "volume"
	case events.NetworkEventType:
		ev.Type = "network"
	}
	return ev
}

// relevantEvent reports whether a Docker event changes the cached inventory
// (containers/stacks/volumes/images/networks). High-frequency, state-neutral
// events are ignored so the event-driven refresh isn't triggered constantly.
func relevantEvent(m events.Message) bool {
	switch m.Type {
	case events.ContainerEventType:
		switch m.Action {
		// Lifecycle/state transitions that change what the inventory shows.
		case "create", "destroy", "start", "die", "stop", "kill",
			"pause", "unpause", "restart", "rename", "update", "oom":
			return true
		default:
			// Ignore exec_*, health_status, top, attach, resize, commit, export,
			// and the per-second stats noise.
			return false
		}
	case events.VolumeEventType:
		switch m.Action {
		case "create", "destroy":
			return true
		}
		return false
	case events.NetworkEventType:
		switch m.Action {
		case "create", "destroy", "connect", "disconnect":
			return true
		}
		return false
	case events.ImageEventType:
		switch m.Action {
		case "pull", "import", "load", "delete", "tag", "untag":
			return true
		}
		return false
	default:
		return false
	}
}
