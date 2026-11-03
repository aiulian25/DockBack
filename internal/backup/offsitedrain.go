package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"dockback/internal/notify"
	"dockback/internal/store"
)

// The offsite drain. A copy that is deferred, failed or incomplete is sent later,
// without a new backup and without anyone asking. The copy's own Location is the
// queue entry: it is already durable, already one per backup and destination, and
// cannot disagree with what the UI shows. A restart loses nothing, because a copy
// only stops being pending once its publication is recorded.

const (
	// offsiteRetryBase is the wait after a copy's first failure, doubled each time.
	offsiteRetryBase = 15 * time.Minute
	// offsiteRetryMax is the longest wait: a destination that comes back after a long
	// outage is still found, within a day, at a cost of one try a day per copy.
	offsiteRetryMax = 24 * time.Hour
	// offsiteAlertAfterAttempts is when a copy that keeps failing is worth telling
	// someone about, once.
	offsiteAlertAfterAttempts = 8
)

// ErrMirrorBusy says this backup is already being mirrored by someone else.
var ErrMirrorBusy = errors.New("this backup is already being mirrored")

// ErrNothingToMirror says every destination asked about already holds a good copy.
var ErrNothingToMirror = errors.New("all enabled destinations already have a copy of this backup")

// ErrMirrorSourceGone says the archive a mirror would read from no longer exists,
// so trying again cannot help.
var ErrMirrorSourceGone = errors.New("the local archive for this backup is missing — cannot mirror")

// offsiteRetryDelay is the wait after a copy's nth failed attempt. Pure.
func offsiteRetryDelay(attempts int) time.Duration {
	delay := offsiteRetryBase
	for i := 1; i < attempts && delay < offsiteRetryMax; i++ {
		delay *= 2
	}
	return min(delay, offsiteRetryMax)
}

// stampOffsiteAttempts records another failure on each copy that just failed,
// counting on from what prior says, and returns the copies that have now failed
// often enough to report. Pure.
func stampOffsiteAttempts(fresh, prior []Location, now time.Time) (alert []Location) {
	attempts := make(map[string]int, len(prior))
	for _, l := range prior {
		attempts[l.DestID] = l.Attempts
	}
	for i := range fresh {
		l := &fresh[i]
		if l.Kind != "dest" || (l.Status != "failed" && l.Status != LocationIncomplete) {
			continue
		}
		l.Attempts = attempts[l.DestID] + 1
		l.NextAttemptAt = now.Add(offsiteRetryDelay(l.Attempts)).Unix()
		if l.Attempts == offsiteAlertAfterAttempts {
			alert = append(alert, *l)
		}
	}
	return alert
}

// goodCopyOn says whether a backup holds a finished copy on this destination.
func goodCopyOn(locs []Location, destID string) bool {
	for _, l := range locs {
		if l.Kind == "dest" && l.DestID == destID && !needsPublishing(l.Status) {
			return true
		}
	}
	return false
}

// parentBlocking returns why a delta must not be published to this destination yet,
// or nothing when it may. A delta is useless without the generations under it, so
// a green copy of one is only true once its parent's copy is.
func (e *Engine) parentBlocking(manBytes []byte, destID string) string {
	var man Manifest
	if err := json.Unmarshal(manBytes, &man); err != nil || !man.Incremental || man.Parent == "" {
		return ""
	}
	parent, err := e.Store.GetBackup(man.Parent)
	if err != nil {
		return fmt.Sprintf("its parent generation %s is no longer in the catalog", short(man.Parent))
	}
	if goodCopyOn(e.locations(parent), destID) {
		return ""
	}
	return fmt.Sprintf("waiting for parent generation %s to reach this destination", short(man.Parent))
}

// mirrorMissingAncestors sends a delta's generations to these destinations before
// the delta itself, oldest first by recursion. A generation that still does not get
// there is reported by the delta's own copy, through parentBlocking.
func (e *Engine) mirrorMissingAncestors(ctx context.Context, b *store.Backup, destIDs []string, overrideWindow bool) {
	var man Manifest
	if err := json.Unmarshal([]byte(b.ManifestJSON), &man); err != nil || !man.Incremental || man.Parent == "" {
		return
	}
	parent, err := e.Store.GetBackup(man.Parent)
	if err != nil {
		return
	}
	for _, destID := range destIDs {
		if goodCopyOn(e.locations(parent), destID) {
			continue
		}
		if merr := e.MirrorExisting(ctx, parent.ID, []string{destID}, overrideWindow); merr != nil && !errors.Is(merr, ErrNothingToMirror) {
			e.logf(b.ID, "WARN", "Could not send parent generation %s first: %v", short(parent.ID), merr)
		}
	}
}

// DrainOffsite sends every pending copy that is due, oldest backup first so a parent
// goes before its delta, one upload at a time so a small server is not swamped. It
// returns how many mirrors it ran.
func (e *Engine) DrainOffsite(ctx context.Context, now time.Time) int {
	pending, err := e.Store.ListBackupsAwaitingOffsite()
	if err != nil || len(pending) == 0 {
		return 0
	}
	open := e.destinationsOpenNow(now)
	ran := 0
	for _, b := range pending {
		for _, destID := range e.offsiteDue(b, open, now) {
			if ctx.Err() != nil {
				return ran
			}
			e.drainOne(ctx, b, destID)
			ran++
		}
	}
	return ran
}

// destinationsOpenNow lists the enabled destinations inside their upload window. A
// destination whose settings cannot be read counts as open, so the attempt fails
// and records why instead of the copy waiting without a word.
func (e *Engine) destinationsOpenNow(now time.Time) map[string]bool {
	open := map[string]bool{}
	dests, err := e.Store.ListDestinations()
	if err != nil {
		return open
	}
	for _, d := range dests {
		if !d.Enabled {
			continue
		}
		cfg, cerr := e.destConfig(d)
		if cerr != nil {
			open[d.ID] = true
			continue
		}
		policy := parseUploadPolicy(cfg)
		open[d.ID] = inUploadWindow(policy.Start, policy.End, now)
	}
	return open
}

// offsiteDue lists the destinations this backup should be sent to now. A source that
// failed verification is not sent anywhere. A delta whose parent has a pending copy
// of its own waits for it, using no attempt, and one whose parent has no entry at all
// goes ahead so the walk can send the parent too.
func (e *Engine) offsiteDue(b *store.Backup, open map[string]bool, now time.Time) []string {
	if b.Verified == "failed" {
		return nil
	}
	var due []string
	for _, l := range e.locations(b) {
		pending := l.Kind == "dest" && needsPublishing(l.Status) && !l.GaveUp && l.NextAttemptAt <= now.Unix()
		if !pending || !open[l.DestID] || e.parentHoldsBack(b, l.DestID) {
			continue
		}
		due = append(due, l.DestID)
	}
	return due
}

// parentHoldsBack says whether a delta has to wait for its parent to move first.
func (e *Engine) parentHoldsBack(b *store.Backup, destID string) bool {
	if e.parentBlocking([]byte(b.ManifestJSON), destID) == "" {
		return false
	}
	var man Manifest
	if json.Unmarshal([]byte(b.ManifestJSON), &man) != nil {
		return true
	}
	parent, err := e.Store.GetBackup(man.Parent)
	if err != nil {
		return true
	}
	for _, l := range e.locations(parent) {
		if l.Kind == "dest" && l.DestID == destID {
			return true
		}
	}
	return false
}

// drainOne runs one automatic attempt. Someone else mirroring this backup, or having
// finished it since the list was read, is not a failure of this copy.
func (e *Engine) drainOne(ctx context.Context, b *store.Backup, destID string) {
	err := e.MirrorExisting(ctx, b.ID, []string{destID}, false)
	switch {
	case err == nil, errors.Is(err, ErrMirrorBusy), errors.Is(err, ErrNothingToMirror):
		return
	case errors.Is(err, ErrMirrorSourceGone):
		e.giveUpOnCopy(b.ID, destID, err)
	default:
		e.logf(b.ID, "WARN", "Automatic offsite send failed: %v", err)
	}
}

// giveUpOnCopy stops retrying a copy that cannot succeed, and says so where the copy
// is shown. Send offsite now can still try it deliberately.
func (e *Engine) giveUpOnCopy(backupID, destID string, cause error) {
	b, err := e.Store.GetBackup(backupID)
	if err != nil {
		return
	}
	locs := e.locations(b)
	for i := range locs {
		if locs[i].Kind == "dest" && locs[i].DestID == destID {
			locs[i].GaveUp = true
			locs[i].Detail = cause.Error()
			e.logf(backupID, "ERR", "Stopped trying to send this backup to %s: %v", locs[i].Name, cause)
			e.notify(notify.KindNoOffsite, "Offsite copy cannot be sent: "+b.TargetName,
				fmt.Sprintf("%s cannot be sent to %s: %v. This destination has no copy of this backup.", b.TargetName, locs[i].Name, cause))
		}
	}
	b.LocationsJSON = mustJSON(locs)
	if uerr := e.Store.UpdateBackup(b); uerr != nil {
		e.logf(backupID, "WARN", "Could not record that the offsite copy was given up: %v", uerr)
	}
}

// alertOnRepeatedFailures reports each copy that has now failed often enough.
func (e *Engine) alertOnRepeatedFailures(targetName string, alert []Location) {
	for _, l := range alert {
		e.notify(notify.KindNoOffsite, "Offsite copy still failing: "+targetName,
			fmt.Sprintf("The copy of %s on %s has failed %d times (%s). DockBack keeps trying, at most once a day.", targetName, l.Name, l.Attempts, l.Detail))
	}
}
