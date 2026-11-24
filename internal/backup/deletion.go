package backup

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"dockback/internal/store"
)

// Deleting a backup (DBK-029). A delete removes every copy it can and then decides
// what the catalog may forget. A copy that is still stored, because it is locked or
// its destination refused or did not answer, keeps its row: the backup turns into
// store.BackupStatusDeleting, lists only what is left, and the delete is tried
// again without anyone asking. A row only goes once nothing it names is stored.

// ErrTargetHeld says the backup's target is under a tripwire hold (F69), which
// freezes every deletion of its backups until the operator clears the hold.
var ErrTargetHeld = errors.New("this container is on hold after a suspicious change, so its backups cannot be deleted until the hold is cleared on its page")

// BackupDeletion is what a delete did.
type BackupDeletion struct {
	// Removed says the backup left the catalog: no copy of it is stored any more.
	Removed bool `json:"removed"`
	// Copies is the outcome per copy, kept or not.
	Copies []CopyDeletion `json:"copies,omitempty"`
}

// Kept returns the copies that are still stored.
func (d BackupDeletion) Kept() []CopyDeletion {
	var kept []CopyDeletion
	for _, c := range d.Copies {
		if copyStillStored(c.Outcome) {
			kept = append(kept, c)
		}
	}
	return kept
}

// copyStillStored says whether a copy with this outcome still holds its archive
// where DockBack can find it. An unconfirmed copy does not count: its key is gone,
// and what a versioned bucket keeps behind it is nothing DockBack can name.
func copyStillStored(outcome string) bool {
	return outcome == CopyLocked || outcome == CopyFailed
}

// awaitingDeletion stamps a copy that outlived a delete with the reason, and with
// when to try again: at the end of its lock, or after the same growing wait a
// failed upload gets.
func awaitingDeletion(loc Location, deletion CopyDeletion, now time.Time) Location {
	loc.DeleteOutcome = deletion.Outcome
	loc.DeleteDetail = deletion.Detail
	if deletion.Outcome == CopyLocked {
		loc.NextAttemptAt = loc.LockUntil
		return loc
	}
	loc.Attempts++
	loc.NextAttemptAt = now.Add(offsiteRetryDelay(loc.Attempts)).Unix()
	return loc
}

// DeleteBackup deletes a backup everywhere it is stored. It is the only way a
// backup leaves the catalog, so manual deletes, retention and node removal agree
// on what happens to a copy that cannot be removed. Callers decide whether the
// backup may be deleted at all (chain dependents, something reading it); the hold
// is checked here because it must bind every one of them.
func (e *Engine) DeleteBackup(ctx context.Context, b *store.Backup) (BackupDeletion, error) {
	if e.retentionHeld(b.NodeID, b.TargetName) {
		return BackupDeletion{}, ErrTargetHeld
	}
	deletions, remaining := e.deleteCopies(ctx, b)
	if len(remaining) == 0 {
		return BackupDeletion{Removed: true, Copies: deletions}, e.Store.DeleteBackup(b.ID)
	}
	locations, err := json.Marshal(remaining)
	if err != nil {
		return BackupDeletion{Copies: deletions}, err
	}
	b.Status = store.BackupStatusDeleting
	b.LocationsJSON = string(locations)
	return BackupDeletion{Copies: deletions}, e.Store.UpdateBackup(b)
}

// deletionDue says whether any copy a delete left behind is worth trying now.
func deletionDue(remaining []Location, now time.Time) bool {
	for _, loc := range remaining {
		if loc.NextAttemptAt <= now.Unix() {
			return true
		}
	}
	return false
}

// RetryPendingDeletions finishes deletes that left a copy behind: a lock that has
// run out, a destination that answers again. It returns how many backups left the
// catalog. A backup whose target has since been put on hold waits for the hold.
func (e *Engine) RetryPendingDeletions(ctx context.Context, now time.Time) int {
	pending, err := e.Store.ListBackupsDeleting()
	if err != nil {
		return 0
	}
	finished := 0
	for _, b := range pending {
		if ctx.Err() != nil {
			return finished
		}
		if !deletionDue(e.locations(b), now) {
			continue
		}
		deletion, err := e.DeleteBackup(ctx, b)
		if err != nil || !deletion.Removed {
			continue
		}
		e.logf("", "INFO", "Finished deleting a backup of %s: its last copy is gone", b.TargetName)
		finished++
	}
	return finished
}

// DescribeRemainingCopies names the copies a backup still lists, each with why a
// delete left it, for the audit trail.
func (e *Engine) DescribeRemainingCopies(b *store.Backup) string {
	locations := e.locations(b)
	parts := make([]string, 0, len(locations))
	for _, loc := range locations {
		parts = append(parts, loc.Name+" ("+loc.DeleteDetail+")")
	}
	return strings.Join(parts, "; ")
}
