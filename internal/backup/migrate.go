package backup

import (
	"context"
	"fmt"

	"dockback/internal/storage"
	"dockback/internal/store"
)

// Layout migration (F77): move a legacy flat-key archive (plus its sidecars)
// into the canonical per-stack folder layout on EVERY location. The order is
// strictly copy → verify → repoint the catalog row → delete the old objects, so
// a crash at any point leaves at worst harmless duplicates, never a row whose
// key has no data behind it.

// migrateSidecarSuffixes are the per-archive sidecar objects that move with it.
// Only the ones that actually exist are copied (a sealed-manifest destination
// has .enc, a readable one has .json + .sig, etc.).
var migrateSidecarSuffixes = []string{".manifest.json", ".manifest.json.sig", ".manifest.json.enc", ".verification.json"}

// MigrateBackupLayout moves one backup from b.StorageKey to newKey on every
// location holding data. A live WORM (object-locked) copy skips the WHOLE
// backup — immutable objects cannot be moved, and one storage key spans all
// copies, so a partial move would split the row. On any copy failure the old
// objects are kept everywhere, the new copies are best-effort removed, and an
// error is returned; the row is repointed only after every location succeeded.
func (e *Engine) MigrateBackupLayout(ctx context.Context, b *store.Backup, newKey string) (moved, skippedWORM bool, err error) {
	if b.StorageKey == "" || newKey == "" || newKey == b.StorageKey {
		return false, false, nil
	}
	if e.hasLiveImmutable(b) {
		return false, true, nil
	}

	type target struct {
		loc Location
		be  storage.Backend
	}
	var targets []target
	for _, loc := range e.locations(b) {
		if noData(loc.Status) {
			continue // an intended copy that never uploaded — nothing to move
		}
		be, berr := e.backendForLocation(loc)
		if berr != nil {
			// Fail closed: if any data-holding location is unreachable we move
			// nothing — a half-moved backup would need per-location keys.
			return false, false, fmt.Errorf("location %q unreachable: %w", loc.Name, berr)
		}
		targets = append(targets, target{loc, be})
	}

	// Phase 1: copy archive + sidecars to the new key on every location.
	written := make([][]string, len(targets)) // per-target new keys, for rollback
	rollback := func() {
		for i, keys := range written {
			for _, k := range keys {
				_ = targets[i].be.Delete(ctx, k)
			}
		}
	}
	for i, t := range targets {
		if cerr := copyObject(ctx, t.be, b.StorageKey, newKey); cerr != nil {
			rollback()
			return false, false, fmt.Errorf("copy archive on %q: %w", t.loc.Name, cerr)
		}
		written[i] = append(written[i], newKey)
		for _, sfx := range migrateSidecarSuffixes {
			ok, serr := copyObjectIfExists(ctx, t.be, b.StorageKey+sfx, newKey+sfx)
			if serr != nil {
				rollback()
				return false, false, fmt.Errorf("copy sidecar %s on %q: %w", sfx, t.loc.Name, serr)
			}
			if ok {
				written[i] = append(written[i], newKey+sfx)
			}
		}
	}

	// Phase 2: repoint the catalog row — from here the new copies are canonical.
	oldKey := b.StorageKey
	if uerr := e.Store.SetBackupStorageKey(b.ID, newKey); uerr != nil {
		rollback()
		return false, false, uerr
	}
	b.StorageKey = newKey

	// Phase 3: remove the old objects (best-effort — a leftover is only a
	// duplicate; the row already points at the verified new copies).
	for _, t := range targets {
		_ = t.be.Delete(ctx, oldKey)
		for _, sfx := range migrateSidecarSuffixes {
			_ = t.be.Delete(ctx, oldKey+sfx)
		}
	}
	return true, false, nil
}

// copyObject streams oldKey → newKey on one backend and verifies the new
// object exists with the same size before reporting success.
func copyObject(ctx context.Context, be storage.Backend, oldKey, newKey string) error {
	rc, err := be.Get(ctx, oldKey)
	if err != nil {
		return err
	}
	_, perr := be.Put(ctx, newKey, rc)
	rc.Close()
	if perr != nil {
		return perr
	}
	newN, ok, serr := be.Stat(ctx, newKey)
	if serr != nil {
		return fmt.Errorf("verify after copy: could not check %s: %w", newKey, serr)
	}
	if !ok {
		return fmt.Errorf("verify after copy: new object missing")
	}
	if oldN, oldOK, _ := be.Stat(ctx, oldKey); oldOK && oldN != newN {
		return fmt.Errorf("verify after copy: size mismatch (%d != %d)", newN, oldN)
	}
	return nil
}

// copyObjectIfExists copies when the source exists. A source that is
// definitively absent is fine (ok=false, nil error) — a destination that seals
// its manifest has no .json, a readable one has no .enc.
//
// A stat we could not PERFORM is not an absent sidecar (Backend.Stat). Treating
// it as one was silent, permanent data loss: the sidecar was skipped in phase 1,
// phase 2 repointed the row anyway, and phase 3 deleted the old
// .manifest.json.sig that had never been copied. One network blip during
// "Organize now" stripped an archive's signature for good — verification then
// quietly skips the signature check, and "Scan & adopt" cannot re-import what no
// longer exists. Failing here instead aborts before phase 2, so the old objects
// are left exactly where they are and the operator can simply run it again.
func copyObjectIfExists(ctx context.Context, be storage.Backend, oldKey, newKey string) (bool, error) {
	_, ok, err := be.Stat(ctx, oldKey)
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", oldKey, err)
	}
	if !ok {
		return false, nil
	}
	if err := copyObject(ctx, be, oldKey, newKey); err != nil {
		return false, err
	}
	return true, nil
}
