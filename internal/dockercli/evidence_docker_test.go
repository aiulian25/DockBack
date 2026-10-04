package dockercli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
)

func TestCollectEvidenceFromARealNode(t *testing.T) {
	c := dockerForTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := ensureSidecar(ctx, c); err != nil {
		t.Fatal(err)
	}
	created, err := c.ContainerCreate(ctx,
		&container.Config{Image: sidecarRef(), Cmd: []string{"sleep", "60"}, Env: []string{"EVIDENCE_PASSWORD=do-not-leak"}}, nil, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeContainer(c, created.ID) })

	ev, err := CollectEvidence(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, raw := range ev.Containers {
		text := string(raw)
		if strings.Contains(text, "do-not-leak") {
			t.Fatal("an environment value reached the evidence")
		}
		if strings.Contains(text, created.ID) && strings.Contains(text, "EVIDENCE_PASSWORD") {
			found = true
		}
	}
	if !found {
		t.Error("the container, with its environment names, must be in the evidence")
	}
	if len(ev.Networks) == 0 {
		t.Error("the node's networks must be recorded")
	}
}
