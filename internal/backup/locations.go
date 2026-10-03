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
	Status string `json:"status,omitempty"` // "" = ok/present; "failed" = intended copy that didn't upload (PLAN §6.5)
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
			be, err := e.backendForLocation(loc)
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
		be, err := e.backendForLocation(loc)
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
	e.logf(b.ID, "INFO", "Restoring from %s (%s)", loc.Name, loc.Kind)
	return be.Get(ctx, b.StorageKey)
}

// DeleteArtifacts removes a backup's archive + manifest from EVERY location it
// lives in (local + all destinations).
func (e *Engine) DeleteArtifacts(ctx context.Context, b *store.Backup) {
	now := time.Now().Unix()
	for _, loc := range e.locations(b) {
		if noData(loc.Status) {
			continue // nothing was written there (failed/deferred)
		}
		if loc.Immutable && loc.LockUntil > now {
			// WORM-locked: the object store refuses deletion until expiry — that's
			// the whole point. Skip it (and don't generate noisy errors). PLAN §9.1.
			e.logf(b.ID, "INFO", "Keeping WORM-locked copy on %s (immutable until %s)", loc.Name, time.Unix(loc.LockUntil, 0).UTC().Format("2006-01-02"))
			continue
		}
		be, err := e.backendForLocation(loc)
		if err != nil {
			continue
		}
		// Surface a failure to remove the main archive: the catalog row is deleted
		// regardless (so it leaves the list), which would otherwise silently orphan
		// the file and waste space. The WARN + "Find orphaned files" sweep make it
		// reclaimable instead of invisible (PLAN §4.6).
		if derr := be.Delete(ctx, b.StorageKey); derr != nil {
			e.logf(b.ID, "WARN", "Could not remove backup file from %s (%v) — it may remain as reclaimable space; use Settings → Find orphaned files to clean it up", loc.Name, derr)
		}
		_ = be.Delete(ctx, b.StorageKey+".manifest.json")
		_ = be.Delete(ctx, b.StorageKey+".manifest.json.sig")
		_ = be.Delete(ctx, b.StorageKey+".manifest.json.enc")
		_ = be.Delete(ctx, b.StorageKey+".verification.json")
	}
}

// enforceRetention prunes a container (node+target) per the GFS policy, removing
// pruned backups from ALL their locations (PLAN §4.6). Newest is always kept.
func (e *Engine) enforceRetention(ctx context.Context, nodeID, target string, cfg RetentionConfig) {
	if !cfg.Active() {
		return
	}
	// F69: a tripwire hold freezes pruning for this target (both this per-backup
	// path and the global PruneAll sweep) until the operator clears it.
	if e.retentionHeld(nodeID, target) {
		e.logf("", "WARN", "Retention: hold active for %s (possible mass-change event) — skipping its prune until cleared", target)
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
	keep, toPrune := SelectForRetention(mine, cfg)
	if len(toPrune) == 0 {
		return
	}
	protected, perr := e.chainProtected(nodeID, target, keep)
	if perr != nil {
		e.logf("", "WARN", "Retention: could not read chain parents for %s — skipping its prune this cycle: %v", target, perr)
		return
	}
	for _, old := range toPrune {
		if protected[old.ID] {
			e.logf(old.ID, "INFO", "Retention: keeping %s — a newer incremental backup depends on it", target)
			continue
		}
		// A backup with a still-locked immutable copy is retained whole until the
		// WORM lock expires — the immutable offsite copy is the ransomware
		// protection, and the local copy is kept restorable alongside it (§9.1).
		if e.hasLiveImmutable(old) {
			e.logf(old.ID, "INFO", "Retention: keeping %s — immutable copy still WORM-locked", target)
			continue
		}
		e.logf(old.ID, "INFO", "Retention: pruning old backup of %s (GFS policy)", target)
		e.DeleteArtifacts(ctx, old)
		if err := e.Store.DeleteBackup(old.ID); err != nil {
			e.logf(old.ID, "ERR", "Retention prune failed: %v", err)
		}
	}
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
