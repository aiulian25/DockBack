package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"dockback/internal/crypto"
	"dockback/internal/storage"
	"dockback/internal/store"
)

// Location records one place an archive physically lives (PLAN §9.1/§9.3): the
// local volume or a specific external destination. Every backup tracks all of
// its locations so restore can fall back and retention can prune everywhere.
type Location struct {
	Kind   string `json:"kind"` // "local" | "dest"
	DestID string `json:"dest_id,omitempty"`
	Name   string `json:"name"`
	Type   string `json:"type"`             // local | smb | webdav | s3
	Status string `json:"status,omitempty"` // "" = ok/present; "failed" = intended copy that didn't upload (PLAN §6.5); LocationIncomplete = archive without a proven envelope
	Detail string `json:"detail,omitempty"` // short human reason when Status=="failed" (e.g. "no progress (stalled)")

	// WORM / object-lock (PLAN §9.1): this copy is immutable until LockUntil
	// (unix secs) — it cannot be deleted/overwritten, even by the app's creds.
	Immutable bool  `json:"immutable,omitempty"`
	LockUntil int64 `json:"lock_until,omitempty"`

	// Per-copy verification (F44). A scrub re-reads THIS copy and stamps its own
	// result here, so an offsite copy that silently rots is caught independently of
	// the local one. VerifiedAt is unix seconds (0 = never checked); VerifyOK is a
	// pointer so "never checked" (nil) is distinct from "checked, failed" (false).
	// omitempty keeps legacy rows byte-identical and lets old rows unmarshal cleanly.
	VerifiedAt int64 `json:"verified_at,omitempty"`
	VerifyOK   *bool `json:"verify_ok,omitempty"`

	// The offsite drain's state for a copy still to be sent. Attempts counts failures
	// in a row, NextAttemptAt (unix secs) is when the next may start, and GaveUp means
	// it cannot succeed, so only a deliberate send tries again.
	Attempts      int   `json:"attempts,omitempty"`
	NextAttemptAt int64 `json:"next_attempt_at,omitempty"`
	GaveUp        bool  `json:"gave_up,omitempty"`

	// The provider version this copy's archive was written as (DBK-030), from the
	// upload's own answer. VersionID names the exact bytes, so a delete marker or
	// a newer object at the same key can neither hide nor replace them, and a
	// delete removes exactly them. Unversioned records that the provider gave the
	// write no version, so deleting the key removes the bytes. A copy with
	// neither predates this record: it is read by key, and what a delete leaves
	// behind on a versioned bucket is unknown.
	VersionID   string `json:"version_id,omitempty"`
	Unversioned bool   `json:"unversioned,omitempty"`

	// Set on a copy that outlived its backup's deletion (DBK-029): CopyLocked or
	// CopyFailed, and the reason in words. Such a backup is in
	// store.BackupStatusDeleting, where Attempts and NextAttemptAt count the delete's
	// retries instead of an upload's.
	DeleteOutcome string `json:"delete_outcome,omitempty"`
	DeleteDetail  string `json:"delete_detail,omitempty"`
}

// Copy deletion outcomes (DBK-030): what deleting a backup did to one copy.
const (
	CopyRemoved     = "removed"     // the bytes are gone
	CopyUnconfirmed = "unconfirmed" // the key is gone; older versions may still be stored
	CopyLocked      = "locked"      // kept: its retention still runs
	CopyFailed      = "failed"      // the destination refused the delete or could not be reached
)

// CopyDeletion is the outcome of deleting one copy of a backup.
type CopyDeletion struct {
	Location string `json:"location"`
	Outcome  string `json:"outcome"`
	Detail   string `json:"detail,omitempty"`
}

// Reclaimed reports whether every copy's bytes are confirmed gone, which is
// the only time a deleted backup's size is storage actually freed.
func Reclaimed(deletions []CopyDeletion) bool {
	for _, d := range deletions {
		if d.Outcome != CopyRemoved {
			return false
		}
	}
	return true
}

// versionPinned reads one key, a copy's archive, by the version it was written
// as. Every other key, and every other call, goes to the backend unchanged.
type versionPinned struct {
	storage.Backend
	versioned storage.Versioned
	key       string
	versionID string
}

func (v versionPinned) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if key != v.key {
		return v.Backend.Get(ctx, key)
	}
	return v.versioned.GetVersion(ctx, key, v.versionID)
}

// backendForCopy is backendForLocation for reading b's archive from loc. A copy
// whose provider version is recorded is read by that version, so what a restore
// or a scrub reads is the bytes this backup wrote, wherever the key points now.
func (e *Engine) backendForCopy(loc Location, b *store.Backup) (storage.Backend, error) {
	be, err := e.backendForLocation(loc)
	if err != nil || loc.VersionID == "" {
		return be, err
	}
	versioned, ok := be.(storage.Versioned)
	if !ok {
		return be, nil
	}
	return versionPinned{Backend: be, versioned: versioned, key: b.StorageKey, versionID: loc.VersionID}, nil
}

// mergeLocations overlays freshly-mirrored destination results onto a backup's
// existing locations: a destination present in updated replaces the same
// destination in existing (so a now-succeeded copy overwrites its old "failed"
// entry), local and not-re-mirrored destinations are preserved, and any brand-new
// destination is appended. Used by MirrorExisting so an on-demand mirror only
// touches the destinations it actually uploaded to.
func mergeLocations(existing, updated []Location) []Location {
	upd := make(map[string]Location, len(updated))
	for _, l := range updated {
		if l.Kind == "dest" {
			upd[l.DestID] = l
		}
	}
	out := make([]Location, 0, len(existing)+len(updated))
	replaced := make(map[string]bool, len(updated))
	for _, l := range existing {
		if l.Kind == "dest" {
			if nl, ok := upd[l.DestID]; ok {
				out = append(out, nl)
				replaced[l.DestID] = true
				continue
			}
		}
		out = append(out, l)
	}
	for _, l := range updated {
		if l.Kind == "dest" && !replaced[l.DestID] {
			out = append(out, l)
		}
	}
	return out
}

// hasLiveImmutable reports whether a backup still has an immutable copy whose
// WORM lock hasn't expired — such a backup is retained whole by prune (PLAN §9.1).
func (e *Engine) hasLiveImmutable(b *store.Backup) bool {
	now := time.Now().Unix()
	for _, loc := range e.locations(b) {
		if loc.Immutable && loc.Status != "failed" && loc.LockUntil > now {
			return true
		}
	}
	return false
}

// locations parses a backup's stored locations, defaulting legacy backups
// (created before location tracking) to local-only.
// LocationsOf is locations for callers outside this package (F214) — the copies
// a backup lives in, as recorded. Exported rather than duplicated so the source
// picker and the restore's own fallback read the same list.
func (e *Engine) LocationsOf(b *store.Backup) []Location { return e.locations(b) }

func (e *Engine) locations(b *store.Backup) []Location {
	var locs []Location
	if b.LocationsJSON != "" {
		_ = json.Unmarshal([]byte(b.LocationsJSON), &locs)
	}
	if len(locs) == 0 {
		locs = []Location{{Kind: "local", Name: "local", Type: "local"}}
	}
	return locs
}

// storageFor resolves the backend a verification/scrub should read FROM (F44):
// ""|"local" → the local backend; a destination ID → that destination's backend
// (decrypting its stored config exactly as backendForLocation/MirrorExisting do).
// It also returns a human label for logs/alerts. A source that isn't a copy this
// backup actually has is an error, so a scrub can't be pointed at nothing.
func (e *Engine) storageFor(source string, b *store.Backup) (storage.Backend, string, error) {
	if source == "" || source == "local" {
		return e.Storage, "local", nil
	}
	for _, loc := range e.locations(b) {
		if loc.Kind == "dest" && loc.DestID == source {
			be, err := e.backendForCopy(loc, b)
			if err != nil {
				return nil, loc.Name, err
			}
			return be, loc.Name, nil
		}
	}
	return nil, source, fmt.Errorf("backup has no copy on destination %q", source)
}

// locationLabel names the copy a source refers to (for logs/alerts): "local" or
// the destination's display name.
func (e *Engine) locationLabel(source string, b *store.Backup) string {
	if source == "" || source == "local" {
		return "local"
	}
	for _, loc := range e.locations(b) {
		if loc.Kind == "dest" && loc.DestID == source {
			return loc.Name
		}
	}
	return source
}

// recordLocationVerify stamps a scrub result onto the matching Location in the
// backup's LocationsJSON (F44), leaving the others untouched. It mutates b in
// memory; the caller persists via UpdateBackup.
func (e *Engine) recordLocationVerify(b *store.Backup, source string, at int64, ok bool) {
	locs := e.locations(b)
	isLocal := source == "" || source == "local"
	for i := range locs {
		if (isLocal && locs[i].Kind == "local") || (!isLocal && locs[i].Kind == "dest" && locs[i].DestID == source) {
			locs[i].VerifiedAt = at
			v := ok
			locs[i].VerifyOK = &v
		}
	}
	b.LocationsJSON = mustJSON(locs)
}

// recordEnvelopeProblem turns a scrub's finding of a missing or unusable envelope
// into a status a repair acts on, which is how a copy recorded as good before the
// envelope was required gets caught. It mutates b in memory, like recordLocationVerify.
func (e *Engine) recordEnvelopeProblem(b *store.Backup, source string, rep *VerificationReport) {
	for _, check := range rep.Checks {
		if check.Name != replicaEnvelopeCheck || check.OK {
			continue
		}
		locs := e.locations(b)
		for i := range locs {
			if locs[i].Kind == "dest" && locs[i].DestID == source {
				locs[i].Status = LocationIncomplete
				locs[i].Detail = check.Info
			}
		}
		b.LocationsJSON = mustJSON(locs)
	}
}

// backendForLocation resolves a Location to a usable storage backend.
func (e *Engine) backendForLocation(loc Location) (storage.Backend, error) {
	if loc.Kind == "local" {
		return e.Storage, nil
	}
	d, err := e.Store.GetDestination(loc.DestID)
	if err != nil {
		return nil, fmt.Errorf("destination %q unavailable: %w", loc.Name, err)
	}
	return e.destBackend(d)
}

// destConfig decrypts a destination's stored credentials into its config map
// (also carries optional per-destination upload policy keys, F14).
func (e *Engine) destConfig(d *store.Destination) (map[string]string, error) {
	cfgJSON, err := crypto.OpenString(d.ConfigEnc, e.MasterKey())
	if err != nil {
		return nil, fmt.Errorf("decrypt credentials for %q: %w", d.Name, err)
	}
	var cfg map[string]string
	if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// destBackend decrypts a destination's stored credentials and builds its backend.
func (e *Engine) destBackend(d *store.Destination) (storage.Backend, error) {
	cfg, err := e.destConfig(d)
	if err != nil {
		return nil, err
	}
	return storage.NewFromConfig(d.Type, cfg)
}

// noData reports whether a destination location never actually received the
// archive — an intended offsite copy that FAILED, or one DEFERRED because it was
// outside its upload window (F14). Such a location is not a restore source, isn't
// counted as a good copy, and still needs a copy pushed to it.
func noData(status string) bool { return status == "failed" || status == "deferred" }

// matchesSource reports whether a location matches a user-chosen restore source
// ("local" or a destination ID).
func matchesSource(loc Location, source string) bool {
	if source == "local" {
		return loc.Kind == "local"
	}
	return loc.DestID == source
}

// orderLocations returns a backup's locations with the user's preferred source
// (if any and present) moved to the front. The rest follow as fallbacks so a
// restore still succeeds if the chosen copy is unreachable or corrupt.
func (e *Engine) orderLocations(b *store.Backup, source string) []Location {
	locs := e.locations(b)
	if source == "" {
		return locs
	}
	var preferred, rest []Location
	for _, loc := range locs {
		if matchesSource(loc, source) {
			preferred = append(preferred, loc)
		} else {
			rest = append(rest, loc)
		}
	}
	return append(preferred, rest...)
}

// bestLocation finds the first location whose stored ciphertext passes an
// integrity check (SHA-256 == manifest value), so a corrupt or unreachable
// copy is skipped in favour of a good one (PLAN §3.4/§9.1 — restore survives
// local loss). With source set the user's chosen copy is tried first; otherwise
// local is tried first for speed.
func (e *Engine) bestLocation(ctx context.Context, b *store.Backup, source string) (storage.Backend, Location, error) {
	var lastErr error
	for _, loc := range e.orderLocations(b, source) {
		if noData(loc.Status) {
			continue // an intended copy that never uploaded (failed/deferred) — nothing to read
		}
		be, err := e.backendForCopy(loc, b)
		if err != nil {
			lastErr = err
			continue
		}
		rc, err := be.Get(ctx, b.StorageKey)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", loc.Name, err)
			continue
		}
		sha, err := crypto.CipherSHA256(rc)
		rc.Close()
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", loc.Name, err)
			continue
		}
		if b.CipherSHA256 == "" || sha == b.CipherSHA256 {
			return be, loc, nil
		}
		lastErr = fmt.Errorf("%s: integrity mismatch (corrupt copy)", loc.Name)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no readable copy found")
	}
	return nil, Location{}, lastErr
}

// openVerified returns a fresh reader of the archive from the best integrity-
// verified location (the caller streams + decrypts it).
func (e *Engine) openVerified(ctx context.Context, b *store.Backup, source string) (io.ReadCloser, error) {
	be, loc, err := e.bestLocation(ctx, b, source)
	if err != nil {
		return nil, err
	}
	if source != "" && !matchesSource(loc, source) {
		e.warnCopyFallback(ctx, b, loc)
	}
	e.logf(b.ID, "INFO", "Restoring from %s (%s)", loc.Name, loc.Kind)
	return be.Get(ctx, b.StorageKey)
}

// stackRunLogKey carries a stack restore's log id to the reads its members make.
// A member logs under its own backup, which is not the console the operator is
// watching during a stack restore.
type stackRunLogKey struct{}

// warnCopyFallback says that a read did not come from the copy the operator chose.
// A copy that cannot be read must not cost the recovery, so the read goes on from
// another one, but somebody who chose "not this disk" has to be told it was used.
func (e *Engine) warnCopyFallback(ctx context.Context, b *store.Backup, loc Location) {
	msg := fmt.Sprintf("%s: no readable copy on the chosen source — restoring from %s (%s) instead", b.TargetName, loc.Name, loc.Kind)
	e.logf(b.ID, "WARN", "%s", msg)
	if stackLog, ok := ctx.Value(stackRunLogKey{}).(string); ok {
		e.logf(stackLog, "WARN", "%s", msg)
	}
}

// sidecarSuffixes are the files written beside an archive, under its key.
var sidecarSuffixes = []string{".manifest.json", ".manifest.json.sig", ".manifest.json.enc", ".verification.json"}

// deleteCopies removes a backup's archive and sidecars from every location it lives
// in (local and all destinations) and says what happened to each copy. remaining is
// the copies still stored afterwards, each stamped with why and with when trying
// again is worth it. A backup that never stored anything has no copies to remove.
func (e *Engine) deleteCopies(ctx context.Context, b *store.Backup) (deletions []CopyDeletion, remaining []Location) {
	if b.StorageKey == "" {
		return nil, nil
	}
	now := time.Now()
	for _, loc := range e.locations(b) {
		if noData(loc.Status) {
			continue // nothing was written there (failed/deferred)
		}
		deletion := e.deleteLocation(ctx, b, loc, now)
		deletions = append(deletions, deletion)
		if copyStillStored(deletion.Outcome) {
			remaining = append(remaining, awaitingDeletion(loc, deletion, now))
		}
	}
	return deletions, remaining
}

// deleteLocation removes one copy and its sidecars. The sidecars go only once the
// archive has: a copy that stays must still say what it is.
func (e *Engine) deleteLocation(ctx context.Context, b *store.Backup, loc Location, now time.Time) CopyDeletion {
	if loc.Immutable && loc.LockUntil > now.Unix() {
		// WORM-locked: the object store refuses deletion until expiry — that's
		// the whole point. Skip it (and don't generate noisy errors). PLAN §9.1.
		until := time.Unix(loc.LockUntil, 0).UTC().Format("2006-01-02")
		e.logf(b.ID, "INFO", "Keeping WORM-locked copy on %s (immutable until %s)", loc.Name, until)
		return CopyDeletion{Location: loc.Name, Outcome: CopyLocked, Detail: "immutable until " + until}
	}
	be, err := e.backendForLocation(loc)
	if err != nil {
		return CopyDeletion{Location: loc.Name, Outcome: CopyFailed, Detail: err.Error()}
	}
	deletion := deleteCopy(ctx, be, loc, b.StorageKey)
	switch deletion.Outcome {
	case CopyFailed:
		e.logf(b.ID, "WARN", "Could not remove the backup file from %s (%s). The backup stays listed as \"deletion incomplete\" and the delete is tried again", loc.Name, deletion.Detail)
		return deletion
	case CopyUnconfirmed:
		e.logf(b.ID, "INFO", "Removed the backup file from %s by name. %s", loc.Name, deletion.Detail)
	}
	for _, suffix := range sidecarSuffixes {
		_ = be.Delete(ctx, b.StorageKey+suffix)
	}
	return deletion
}

// deleteCopy removes one copy's archive as precisely as its recorded evidence
// allows. A known version is removed itself, which is the only delete on a
// versioned bucket that frees the bytes. A key on a bucket that gave the write no
// version is removed outright. A key with no evidence either way is removed, but
// on a versioned bucket that only adds a delete marker, so it is not called freed.
func deleteCopy(ctx context.Context, be storage.Backend, loc Location, key string) CopyDeletion {
	versioned, isVersioned := be.(storage.Versioned)
	if isVersioned && loc.VersionID != "" {
		if err := versioned.DeleteVersion(ctx, key, loc.VersionID); err != nil {
			return CopyDeletion{Location: loc.Name, Outcome: CopyFailed, Detail: fmt.Sprintf("version %s: %v", loc.VersionID, err)}
		}
		return CopyDeletion{Location: loc.Name, Outcome: CopyRemoved, Detail: "version " + loc.VersionID + " removed"}
	}
	if err := be.Delete(ctx, key); err != nil {
		return CopyDeletion{Location: loc.Name, Outcome: CopyFailed, Detail: err.Error()}
	}
	if isVersioned && !loc.Unversioned {
		return CopyDeletion{Location: loc.Name, Outcome: CopyUnconfirmed,
			Detail: "This copy predates version tracking, so if the bucket keeps versions, its earlier versions are still stored until the bucket's own lifecycle rules remove them."}
	}
	return CopyDeletion{Location: loc.Name, Outcome: CopyRemoved}
}

// enforceRetention prunes a container (node+target) per the GFS policy, removing
// pruned backups from ALL their locations (PLAN §4.6). Newest is always kept.
func (e *Engine) enforceRetention(ctx context.Context, nodeID, target string, cfg RetentionConfig) {
	if !cfg.Active() {
		return
	}
	all, err := e.Store.ListBackupsForTarget(nodeID, target, 10000) // newest first
	if err != nil {
		return
	}
	var mine []*store.Backup
	for _, b := range all {
		if b.Status == "success" {
			mine = append(mine, b)
		}
	}
	e.pruneTarget(ctx, nodeID, target, mine, cfg)
}

// applyRetention prunes after a successful backup if auto-pruning is enabled for
// this container — resolving the effective policy (container override → node →
// global, PLAN §4.13/§4.2).
func (e *Engine) applyRetention(ctx context.Context, nodeID, target string) {
	cfg, autoprune := EffectiveRetentionFor(e.Store, nodeID, target)
	if !autoprune {
		return
	}
	e.enforceRetention(ctx, nodeID, target, cfg)
}
