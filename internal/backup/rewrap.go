package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"dockback/internal/appbackup"
	"dockback/internal/crypto"
	"dockback/internal/store"
)

// Master-key rotation (F16). Rotating the master key only re-WRAPS each backup's
// per-backup data key (DEK): the DEK — and therefore the archive it encrypts — is
// never changed, so ciphertext stays byte-for-byte identical and no re-upload is
// needed. For each backup we unwrap the DEK with the OLD key and re-wrap it with
// the NEW one, then rewrite the manifest-of-record (SQLite) plus the per-location
// sidecars (readable manifest + HMAC signature, or the sealed .enc form) so
// verification and hand-restore keep working under the new key. A backup that was
// wrapped by an unrelated key (or a legacy archive encrypted directly with the
// master key, which has no wrapped DEK) is SKIPPED and left completely untouched —
// never corrupted.

// RewrapResult reports how a rotation pass went.
type RewrapResult struct {
	Rewrapped int // DEK successfully re-wrapped old -> new
	Skipped   int // not ours to rotate (third-key or legacy direct-key) — left intact
	Failed    int // belonged to old key but the manifest-of-record couldn't be updated
}

// RewrapAll re-wraps every backup's DEK from oldKey to newKey (F16). It must be
// called while the engine's in-memory key is still the OLD key, because it
// resolves destination backends (to rewrite their sidecars) using the current
// master key. Archives are not read or rewritten. Best-effort per backup: a
// sidecar it can't rewrite on one location is logged and skipped, since the
// authoritative wrapped key lives in the manifest-of-record it always updates.
func (e *Engine) RewrapAll(ctx context.Context, oldKey, newKey []byte) RewrapResult {
	var res RewrapResult
	list, err := e.Store.ListBackups("", 1000000)
	if err != nil {
		return res
	}
	newFP := KeyFingerprint(newKey)
	for _, b := range list {
		if b.ManifestJSON == "" {
			res.Skipped++
			continue
		}
		var man Manifest
		if err := unmarshal(b.ManifestJSON, &man); err != nil {
			res.Skipped++
			continue
		}
		if IsWriteOnly(&man) {
			// F86 write-only: the DEK is sealed to an OFFLINE public key, not to the
			// master key, so master-key rotation simply doesn't apply — there is
			// nothing here for the old key to have wrapped. Skipping is correct and
			// complete, not a limitation: the backup stays restorable with the same
			// offline private key before and after rotation.
			res.Skipped++
			continue
		}
		if man.WrappedKey == "" {
			// Legacy backup whose ARCHIVE is encrypted directly with the master key —
			// it can't be rotated without re-encrypting the whole archive (out of
			// scope). Leave it exactly as-is.
			res.Skipped++
			continue
		}
		newWrapped, err := crypto.RewrapDEK(man.WrappedKey, oldKey, newKey)
		if err != nil {
			// Wrapped by a different (third) key — not ours to rotate. Untouched.
			res.Skipped++
			continue
		}
		man.WrappedKey = newWrapped
		man.KeyFingerprint = newFP
		manBytes, err := json.MarshalIndent(&man, "", "  ")
		if err != nil {
			res.Failed++
			continue
		}
		// Authoritative first: the manifest-of-record (SQLite) is what restore reads
		// to unwrap the DEK, so a rotation is durable the moment this succeeds.
		if err := e.Store.UpdateBackupManifest(b.ID, string(manBytes)); err != nil {
			res.Failed++
			continue
		}
		// Then rewrite the sidecars on every location so verification + hand-restore
		// work under the new key. Best-effort — never fails the rotation.
		e.rewrapSidecars(ctx, b, manBytes, newKey)
		res.Rewrapped++
	}
	return res
}

// RewrapAppBackups re-wraps every stored application-backup's DEK from oldKey to
// newKey in place (F37), so master-key rotation covers DockBack's own control-plane
// history too — the one archive you need when DockBack itself is lost. Like
// RewrapAll, only the wrapped key (a tiny plaintext header) is rewritten; the
// encrypted archive body is copied byte-for-byte, so no re-encryption or re-upload
// happens. v1 (pre-envelope) app-backups have no wrapped key and are counted as
// skipped, left byte-for-byte intact — still recoverable with the OLD key. Must run
// while the in-memory key is still the OLD key (the caller passes explicit keys).
func (e *Engine) RewrapAppBackups(ctx context.Context, dir string, oldKey, newKey []byte) RewrapResult {
	var res RewrapResult
	list, err := appbackup.List(dir)
	if err != nil {
		return res
	}
	newFP := KeyFingerprint(newKey)
	for _, ent := range list {
		rewrapped, err := appbackup.RewrapFile(dir, ent.File, newFP, oldKey, newKey)
		switch {
		case err != nil:
			e.logf(ent.File, "WARN", "Key rotation: could not re-wrap app-backup %s: %v", ent.File, err)
			res.Failed++
		case rewrapped:
			res.Rewrapped++
		default:
			res.Skipped++
		}
	}
	return res
}

// rewrapSidecars rewrites a backup's manifest sidecars on every location with the
// NEW key: the sealed .manifest.json.enc when that's what exists, otherwise the
// readable .manifest.json plus a fresh HMAC .sig. Detection is per-location (not
// from the current global setting) so a backup written under a different
// manifest-encryption setting is still rewritten in its own form. Backend access
// still uses the engine's current (old) master key, which is why RewrapAll must
// run before the caller swaps the in-memory key.
func (e *Engine) rewrapSidecars(ctx context.Context, b *store.Backup, manBytes, newKey []byte) {
	for _, loc := range e.locations(b) {
		if noData(loc.Status) {
			continue // an intended copy that never uploaded — no sidecar to rewrite
		}
		be, err := e.backendForLocation(loc)
		if err != nil {
			e.logf(b.ID, "WARN", "Key rotation: cannot reach %s to rewrite manifest sidecar: %v", loc.Name, err)
			continue
		}
		key := b.StorageKey
		if rc, gerr := be.Get(ctx, key+".manifest.json.enc"); gerr == nil {
			rc.Close()
			sealed, serr := crypto.SealString(string(manBytes), newKey)
			if serr != nil {
				continue
			}
			if _, perr := be.Put(ctx, key+".manifest.json.enc", bytes.NewReader(sealed)); perr != nil {
				e.logf(b.ID, "WARN", "Key rotation: could not rewrite sealed manifest on %s: %v", loc.Name, perr)
			}
			continue
		}
		if _, perr := be.Put(ctx, key+".manifest.json", bytes.NewReader(manBytes)); perr != nil {
			e.logf(b.ID, "WARN", "Key rotation: could not rewrite manifest on %s: %v", loc.Name, perr)
			continue
		}
		if _, perr := be.Put(ctx, key+".manifest.json.sig", strings.NewReader(crypto.SignManifest(manBytes, newKey))); perr != nil {
			e.logf(b.ID, "WARN", "Key rotation: could not rewrite manifest signature on %s: %v", loc.Name, perr)
		}
	}
}
