package backup

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
	"dockback/internal/notify"
	"dockback/internal/store"
)

// The hold journal. Letting go of a paused or stopped container is a goroutine
// defer, which does not run when the process dies, and an upgrade during a hold is
// enough for that. So the intent is written before the Docker call, marked acquired
// after it, and cleared only once the container is confirmed released. Whatever is
// left at the next start is reconciled against a fresh inspect.

const (
	holdKindPause   = "pause"
	holdKindStop    = "stop"
	holdKindRestore = "restore"
	holdKindHooks   = "hooks"

	holdStateIntent   = "intent"
	holdStateAcquired = "acquired"

	// holdStopWindow is how long after an intent a stop can still be the one that
	// intent describes. ContainerStop waits for a container's grace period at most.
	holdStopWindow = 2 * time.Minute
	// holdClockTolerance allows for the node's clock differing from this one's.
	holdClockTolerance = time.Minute
	// holdInspectTimeout bounds one row, so an unreachable node cannot hold up start.
	holdInspectTimeout = 15 * time.Second
	// holdSettleTimeout bounds settling a hold whose own context was canceled.
	holdSettleTimeout = 30 * time.Second
)

func holdRowID(opID, containerID string) string { return opID + ":" + containerID }

// journalHoldIntent records that this operation is about to pause or stop a running
// container. A container that is not running gets no row, so it can never be
// mistaken for one this operation took down. A journal that cannot be written costs
// the crash protection, not the backup, and is said out loud.
func (e *Engine) journalHoldIntent(ctx context.Context, cli *client.Client, nodeID, opID, containerID, kind string) {
	insp, err := cli.ContainerInspect(ctx, containerID)
	if err != nil || insp.ContainerJSONBase == nil || insp.State == nil || !insp.State.Running || insp.State.Paused {
		return
	}
	row := store.WorkloadHold{
		ID: holdRowID(opID, containerID), NodeID: nodeID, ContainerID: containerID,
		Name: strings.TrimPrefix(insp.Name, "/"), Kind: kind, State: holdStateIntent,
		PriorStartedAt: insp.State.StartedAt, CreatedAt: time.Now().Unix(),
	}
	if werr := e.Store.AddWorkloadHold(row); werr != nil {
		e.logf(opID, "WARN", "Could not journal this hold (%v) — if DockBack dies during it, %s stays %sd until released by hand", werr, row.Name, kind)
	}
}

func (e *Engine) journalHoldAcquired(opID, containerID string) {
	_ = e.Store.MarkWorkloadHoldAcquired(holdRowID(opID, containerID))
}

func (e *Engine) clearHold(opID, containerID string) {
	_ = e.Store.DeleteWorkloadHold(holdRowID(opID, containerID))
}

// settleUnconfirmedHold deals with a pause or stop that returned an error. Its fate
// is unknown when that error is a canceled context, so ask rather than assume: the
// run carries on without a hold, and must not leave one behind.
func (e *Engine) settleUnconfirmedHold(cli *client.Client, opID, containerID string) {
	rows, err := e.Store.ListWorkloadHolds()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), holdSettleTimeout)
	defer cancel()
	for _, row := range rows {
		if row.ID == holdRowID(opID, containerID) {
			e.reconcileHold(ctx, cli, row)
		}
	}
}

// holdAction is what reconciliation does about one journal row.
type holdAction int

const (
	holdClear   holdAction = iota // nothing of ours is left to undo
	holdRelease                   // provably ours: let it go
	holdReport                    // cannot be proved ours: say so and leave it alone
)

// holdObservation is the container as a fresh inspect found it.
type holdObservation struct {
	exists, running, paused bool
	startedAt               string
	finishedAt              time.Time
}

// decideHoldRecovery is the decision that starts or unpauses somebody's container,
// so it is pure. It releases only what the row proves: an operator's stopped
// container stays stopped.
//
//   - A pause leaves nothing to compare, but the row says the container was running
//     unpaused a moment before, so a paused one is this operation's.
//   - A stop is this operation's if the container has not run since. An acquired
//     stop proves it by the start time alone. An intent does not prove the stop
//     happened, so the exit must also fall where this stop would have.
//   - An interrupted restore is never started: its data may be half written.
func decideHoldRecovery(row store.WorkloadHold, seen holdObservation) holdAction {
	if row.Kind == holdKindRestore {
		return holdReport
	}
	if !seen.exists {
		return holdClear
	}
	if row.Kind == holdKindPause {
		if seen.paused {
			return holdRelease
		}
		return holdClear
	}
	if seen.running || seen.startedAt != row.PriorStartedAt {
		return holdClear
	}
	if row.State == holdStateAcquired {
		return holdRelease
	}
	intentAt := time.Unix(row.CreatedAt, 0)
	if seen.finishedAt.After(intentAt.Add(-holdClockTolerance)) && seen.finishedAt.Before(intentAt.Add(holdStopWindow+holdClockTolerance)) {
		return holdRelease
	}
	return holdReport
}

// ReconcileWorkloadHolds settles rows left by a previous process and returns those
// it could not reach, to try again. Run before anything else claims work, and only
// on rows from before this start, so it never touches a live hold. Repeating it is
// harmless: a settled row is gone, and an unpaused container has nothing to undo.
//
// Hook windows go last: a post-hook execs in the container, which cannot happen
// until a pause from the same run has been released.
func (e *Engine) ReconcileWorkloadHolds(ctx context.Context, rows []store.WorkloadHold) (unreachable []store.WorkloadHold) {
	var hookRows []store.WorkloadHold
	for _, row := range rows {
		switch row.Kind {
		case holdKindRestore:
			e.reportInterruptedRestore(ctx, row)
		case holdKindHooks:
			hookRows = append(hookRows, row)
		default:
			if !e.reconcileOnNode(ctx, row, e.reconcileHold) {
				unreachable = append(unreachable, row)
			}
		}
	}
	for _, row := range hookRows {
		if !e.reconcileOnNode(ctx, row, e.reconcileHookWindow) {
			unreachable = append(unreachable, row)
		}
	}
	return unreachable
}

func (e *Engine) reconcileOnNode(ctx context.Context, row store.WorkloadHold, settle func(context.Context, *client.Client, store.WorkloadHold) bool) bool {
	cli, err := e.Reg.Get(row.NodeID)
	if err != nil {
		return false
	}
	rowCtx, cancel := context.WithTimeout(ctx, holdInspectTimeout)
	defer cancel()
	return settle(rowCtx, cli, row)
}

// reconcileHookWindow finishes a hook window a dead process left open. A hook that
// ran again on an application already out of the window is the same line the
// operator chose to run after every backup, so it is run rather than guessed at.
// false means it cannot be run yet: unreachable, or still paused.
func (e *Engine) reconcileHookWindow(ctx context.Context, cli *client.Client, row store.WorkloadHold) bool {
	insp, err := cli.ContainerInspect(ctx, row.ContainerID)
	if client.IsErrNotFound(err) {
		_ = e.Store.DeleteWorkloadHold(row.ID)
		return true
	}
	if err != nil || insp.ContainerJSONBase == nil || insp.State == nil || insp.State.Paused {
		return false
	}
	if !insp.State.Running {
		e.logf("", "WARN", "%s has a backup window DockBack could not close: it is stopped, so its post-hooks did not run", row.Name)
		e.notify(notify.KindBackupFailed, "Post-backup hooks not run: "+row.Name,
			fmt.Sprintf("DockBack stopped during %s's backup window, and the container is stopped now, so its post-hooks could not run. It may start up in the state its pre-hooks left it in, such as maintenance mode.", row.Name))
		_ = e.Store.DeleteWorkloadHold(row.ID)
		return true
	}
	_, post := e.gatherHooks(row.Detail, row.NodeID, row.Name)
	e.logf("", "WARN", "%s's backup window was left open when DockBack stopped — running its post-hooks now", row.Name)
	bracket := e.newHookBracket(cli, row.NodeID, row.ContainerID, row.Name, row.Detail, strings.SplitN(row.ID, ":", 2)[0], nil, post)
	bracket.armed = true
	bracket.release()
	return true
}

// reconcileHold settles one row; false means the daemon could not answer, so it
// stays for a later attempt.
func (e *Engine) reconcileHold(ctx context.Context, cli *client.Client, row store.WorkloadHold) bool {
	var seen holdObservation
	insp, err := cli.ContainerInspect(ctx, row.ContainerID)
	switch {
	case client.IsErrNotFound(err):
	case err != nil || insp.ContainerJSONBase == nil || insp.State == nil:
		return false
	default:
		seen = holdObservation{exists: true, running: insp.State.Running && !insp.State.Paused, paused: insp.State.Paused, startedAt: insp.State.StartedAt}
		seen.finishedAt, _ = time.Parse(time.RFC3339Nano, insp.State.FinishedAt)
	}
	switch decideHoldRecovery(row, seen) {
	case holdClear:
		_ = e.Store.DeleteWorkloadHold(row.ID)
	case holdRelease:
		return e.releaseOrphanedHold(ctx, cli, row)
	case holdReport:
		e.logf("", "WARN", "%s is stopped and DockBack cannot prove that it did this, so it has been left stopped — start it if it should be running", row.Name)
		e.notify(notify.KindRestoreFailed, "Container left stopped: "+row.Name,
			fmt.Sprintf("DockBack stopped unexpectedly while about to stop %s for a backup. It is stopped now, but that may not have been DockBack, so it was left alone. Start it if it should be running.", row.Name))
		_ = e.Store.DeleteWorkloadHold(row.ID)
	}
	return true
}

func (e *Engine) releaseOrphanedHold(ctx context.Context, cli *client.Client, row store.WorkloadHold) bool {
	var err error
	if row.Kind == holdKindPause {
		err = cli.ContainerUnpause(ctx, row.ContainerID)
	} else {
		err = cli.ContainerStart(ctx, row.ContainerID, container.StartOptions{})
	}
	if err != nil {
		e.logf("", "ERR", "Could not release %s after DockBack stopped during its %s: %v", row.Name, row.Kind, err)
		return false
	}
	_ = e.Store.DeleteWorkloadHold(row.ID)
	e.logf("", "WARN", "%s was left %sd when DockBack stopped during a backup, and has been released", row.Name, row.Kind)
	e.notify(notify.KindBackupFailed, "Container released after an interrupted backup: "+row.Name,
		fmt.Sprintf("DockBack stopped while it held %s (%s), and let it go on starting again. The container was down or frozen for that whole time.", row.Name, row.Kind))
	return true
}

// journalRestore records a restore for as long as it runs. Its data may be half
// written if it never finishes, so nothing starts the container afterwards: the row
// exists to keep the explanation, which a restart used to erase.
func (e *Engine) journalRestore(b *store.Backup, nodeID string) (clear func()) {
	id := holdRowID(b.ID, holdKindRestore)
	_ = e.Store.AddWorkloadHold(store.WorkloadHold{
		ID: id, NodeID: nodeID, Name: b.TargetName, Kind: holdKindRestore, State: holdStateIntent, CreatedAt: time.Now().Unix(),
	})
	return func() { _ = e.Store.DeleteWorkloadHold(id) }
}

// noteRestoreSnapshot adds the rollback point to a journaled restore.
func (e *Engine) noteRestoreSnapshot(b *store.Backup, snapID string) {
	_ = e.Store.SetWorkloadHoldDetail(holdRowID(b.ID, holdKindRestore), snapID)
}

func (e *Engine) reportInterruptedRestore(ctx context.Context, row store.WorkloadHold) {
	undo := "No safety snapshot had been taken."
	if row.Detail != "" {
		undo = fmt.Sprintf("Restore the safety snapshot %s to undo it.", short(row.Detail))
	}
	if cli, err := e.Reg.Get(row.NodeID); err == nil {
		rowCtx, cancel := context.WithTimeout(ctx, holdInspectTimeout)
		if dockercli.RestorePriorName(rowCtx, cli, row.Name) {
			undo += fmt.Sprintf(" The original container had been set aside, and is back under the name %s.", row.Name)
		}
		cancel()
	}
	e.logf("", "ERR", "A restore of %s was interrupted when DockBack stopped. It was not resumed, and %s was not started, because its data may be partly restored. %s", row.Name, row.Name, undo)
	e.notify(notify.KindRestoreFailed, "Restore interrupted: "+row.Name,
		fmt.Sprintf("DockBack stopped during a restore of %s, so it may be stopped with partly restored data. It was left as it is. %s", row.Name, undo))
	_ = e.Store.DeleteWorkloadHold(row.ID)
}

// notifyHoldNotReleased raises the failure that matters most in a backup that has
// otherwise worked: an application still down because of it. The journal row stays,
// so the next start tries again.
func (e *Engine) notifyHoldNotReleased(containerID, state string, cause error) {
	e.notify(notify.KindBackupFailed, "Container left "+state+" after a backup",
		fmt.Sprintf("DockBack could not bring container %s back after holding it for a backup, so it is still %s. Start it by hand. %v", short(containerID), state, cause))
}
