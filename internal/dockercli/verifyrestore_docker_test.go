package dockercli

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
)

// A drill's throwaway app must run inside the drill budget on a real daemon. The app
// reads its own cgroup limits and the restored file, and exits — failing the drill —
// unless every limit was applied and the data came back.
func TestADrillRunsInsideItsBudget(t *testing.T) {
	c := dockerForTest(t)
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Skip("the budget check reads cgroup v2 files")
	}
	const bootWaitSeconds = 3
	SetDrillBootWait(bootWaitSeconds)
	t.Cleanup(func() { SetDrillBootWait(0) })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const expectedCPUMax = "200000 100000"
	insideBudget := `[ "$(cat /sys/fs/cgroup/pids.max)" = ` + strconv.Itoa(drillPidsLimit) + ` ] &&
		[ "$(cat /sys/fs/cgroup/memory.max)" = ` + strconv.Itoa(drillMemoryBytes) + ` ] &&
		[ "$(cat /sys/fs/cgroup/cpu.max)" = "` + expectedCPUMax + `" ] &&
		grep -q restored /data/probe && exec sleep 300; exit 3`
	inspect, err := json.Marshal(types.ContainerJSON{Config: &container.Config{Cmd: []string{"sh", "-c", insideBudget}}})
	if err != nil {
		t.Fatal(err)
	}

	summary, _, err := VerifyRestoreVolumes(ctx, c, sidecarRef(), inspect, []string{"/data"}, tarOf(t, map[string]string{"data/probe": "restored"}))
	if err != nil {
		t.Fatalf("the drill failed inside its budget: %v", err)
	}
	t.Log(summary)
}
