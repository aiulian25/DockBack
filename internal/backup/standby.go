package backup

import (
	"context"
	"fmt"
	"time"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// Pilot-light standby rehearsal (F62). Every warm-standby primitive already
// exists — cross-node restore, isolated clones that can't collide with
// production, and the post-restore health gate — but nothing composes them into a
// continuous proof of "if node A dies, this container comes up on node B".
// RehearseStandby does exactly that: restore the newest verified backup as an
// ISOLATED clone (<name>-standby) onto a chosen target node, wait for it to come
// up healthy, record pass/fail + boot time, and ALWAYS tear the clone down.

// standbySuffix names the throwaway clone a rehearsal brings up on the standby node.
const standbySuffix = "-standby"

// StandbyCloneName returns the isolated-clone container name a rehearsal uses for a
// container (shared with the API so validation/teardown agree).
func StandbyCloneName(target string) string { return target + standbySuffix }

// RehearseStandby restores backup b as an isolated clone onto targetNodeID, waits
// for the clone to boot healthy (the clone path skips the normal health gate, so
// we gate here with the container's effective restore-health timeout), and returns
// (ok, bootMs, detail). The clone — container + its throwaway anonymous volumes —
// is ALWAYS removed, on every path, so a rehearsal leaves zero residue on the
// standby node. The clone is isolated (fresh empty volumes, no published ports,
// throwaway network), so it can never collide with or read the live stack there.
func (e *Engine) RehearseStandby(ctx context.Context, b *store.Backup, targetNodeID string) (bool, int64, string) {
	cloneName := StandbyCloneName(b.TargetName)
	cli, err := e.Reg.Get(targetNodeID)
	if err != nil {
		return false, 0, "standby node is unreachable: " + err.Error()
	}
	timeout := e.restoreHealthTimeout(targetNodeID, b.TargetName)
	start := time.Now()

	teardown := func() {
		// Own context: teardown must run even if ctx was cancelled/timed out.
		tctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if id, found := dockercli.FindContainerByName(tctx, cli, cloneName); found {
			if rerr := dockercli.RemoveCloneContainer(tctx, cli, id); rerr != nil {
				e.logf(b.ID, "WARN", "Standby rehearsal: could not remove clone %q on the standby node: %v", cloneName, rerr)
			} else {
				e.logf(b.ID, "INFO", "Standby rehearsal: removed clone %q and its throwaway volumes", cloneName)
			}
		}
	}
	restore := func() error {
		e.logf(b.ID, "INFO", "Standby rehearsal: restoring %q as an isolated clone %q on the standby node", b.TargetName, cloneName)
		return e.Restore(ctx, RestoreOptions{
			BackupID: b.ID, NodeID: targetNodeID,
			AsName: cloneName, Isolated: true, Volumes: true, Database: true,
		})
	}
	health := func() (bool, int64) {
		id, found := dockercli.FindContainerByName(ctx, cli, cloneName)
		if !found {
			return false, time.Since(start).Milliseconds()
		}
		return dockercli.WaitForHealthy(ctx, cli, id, timeout), time.Since(start).Milliseconds()
	}
	return rehearseStandby(cloneName, timeout, restore, health, teardown)
}

// rehearseStandby is the injectable, Docker-free control flow behind
// RehearseStandby: run the restore, gate on health, and ALWAYS tear down — so the
// teardown-always guarantee is unit-testable without a Docker daemon. bootMs is the
// wall-clock stand-up time (restore + boot) on the standby node.
func rehearseStandby(cloneName string, timeout time.Duration, restore func() error, health func() (bool, int64), teardown func()) (ok bool, bootMs int64, detail string) {
	defer teardown()
	if err := restore(); err != nil {
		return false, 0, "restore onto the standby node failed: " + err.Error()
	}
	healthy, bootMs := health()
	if !healthy {
		return false, bootMs, fmt.Sprintf("the clone %q did not come up healthy within %s on the standby node — an isolated clone can't reach its dependencies, so a multi-service app may need its whole stack to prove standby", cloneName, timeout)
	}
	return true, bootMs, fmt.Sprintf("restored and booted healthy on the standby node in %s", (time.Duration(bootMs) * time.Millisecond).Round(time.Second))
}
