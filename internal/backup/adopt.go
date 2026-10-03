package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"dockback/internal/crypto"
	"dockback/internal/storage"
	"dockback/internal/store"
)

// Adopt a destination (F20): reconcile orphaned archives back into the catalog.
//
// After a SQLite loss (or restoring an old app-backup), newer `.dback` archives on
// a destination are orphaned and invisible to the UI even though the archive + its
// manifest sidecar sit right there. AdoptFromBackend scans the destination, finds
// every `.dback` whose manifest sidecar VERIFIES under the current master key but
// has no catalog row, and re-creates the `store.Backup` row from the manifest — so
// "my DockBack DB is gone but my S3 bucket is intact" becomes one click.
//
// Security: it only adopts what it can cryptographically confirm is ours — a sealed
// `.manifest.json.enc` that decrypts with the master key, or a readable
// `.manifest.json` whose `.sig` HMAC verifies under the master key. A manifest
// sealed/signed with a DIFFERENT key (the F16 third-key case) is skipped, never
// adopted, so we never register a backup we couldn't restore. Existing rows are
// never overwritten.

// Skip reasons (F102). Stable identifiers so the UI can phrase each one for a
// human without re-deriving the cause from a sentence.
const (
	// SkipForeignKey: the manifest is real but was sealed or signed under a
	// DIFFERENT master key — the F16 third-key case, and the one an operator can
	// usually act on (re-import the old key, or look at which instance wrote it).
	SkipForeignKey = "foreign key"
	// SkipUnreadable: the sidecar could not be read or parsed at all.
	SkipUnreadable = "unreadable manifest"
	// SkipCatalogued: already in the catalog — the ordinary, uninteresting case.
	SkipCatalogued = "already in catalog"
	// SkipMissingArchive: the manifest verified, but its `.dback` is gone.
	SkipMissingArchive = "archive missing"
	// SkipStoreError: the catalog itself refused the row.
	SkipStoreError = "catalog error"
)

// AdoptSkip names one archive that was NOT adopted, and why (F102).
//
// "42 adopted, 7 skipped" is unactionable after a key rotation or a two-instance
// mix-up: the operator cannot tell whether the 7 are harmless duplicates or
// backups they can no longer read. This says which object, for which reason, and
// — where it could be read — which master key it belongs to.
type AdoptSkip struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
	// KeyFingerprint is read from an UNVERIFIED manifest, so it is deliberately
	// the ONLY field taken from one. A foreign manifest may belong to someone
	// else's instance; its container names, stacks and volume paths are not ours
	// to display. The fingerprint is a key identifier the UI already shows and is
	// exactly what makes the skip actionable.
	KeyFingerprint string `json:"key_fingerprint,omitempty"`
}

// AdoptFromBackend scans be for orphaned container archives and re-registers them,
// returning how many were adopted and a per-archive report of what was skipped
// (F102). destName/destID/destType identify the location recorded on each
// adopted backup.
func (e *Engine) AdoptFromBackend(ctx context.Context, be storage.Backend, destName, destID, destType string) (adopted int, skipped []AdoptSkip, err error) {
	skipped = []AdoptSkip{}
	skip := func(key, reason, fp string) {
		if len(skipped) < maxAdoptSkipReport {
			skipped = append(skipped, AdoptSkip{Key: key, Reason: reason, KeyFingerprint: fp})
		}
	}
	keys, err := storage.ListKeys(ctx, be, "")
	if err != nil {
		return 0, skipped, err
	}
	for _, key := range keys {
		if ctx.Err() != nil {
			return adopted, skipped, ctx.Err()
		}
		// Only manifest sidecars drive adoption. `.sig` is fetched on demand; the
		// archive + verification sidecars are found via the manifest's archive key.
		var archiveKey string
		switch {
		case strings.HasSuffix(key, ".manifest.json.enc"):
			archiveKey = strings.TrimSuffix(key, ".manifest.json.enc")
		case strings.HasSuffix(key, ".manifest.json"):
			archiveKey = strings.TrimSuffix(key, ".manifest.json")
		default:
			continue
		}
		// Container archives are `<key>.dback`; anything else (e.g. an app-backup)
		// isn't ours to adopt here.
		if !strings.HasSuffix(archiveKey, ".dback") {
			continue
		}

		manBytes, raw, ok := e.readVerifiedManifest(ctx, be, key, archiveKey)
		if !ok {
			// Never adopt blindly. Say WHY, and — when the sidecar was readable but
			// simply not ours — which key it belongs to, so the operator can decide
			// whether to re-import that key or investigate a mixed-up instance.
			skip(key, adoptSkipReason(raw), foreignKeyFingerprint(raw))
			continue
		}
		var man Manifest
		if json.Unmarshal(manBytes, &man) != nil || man.BackupID == "" {
			skip(key, SkipUnreadable, "")
			continue
		}

		// Already catalogued? Never overwrite an existing row.
		if _, gerr := e.Store.GetBackup(man.BackupID); gerr == nil {
			skip(key, SkipCatalogued, man.KeyFingerprint)
			continue
		} else if !errors.Is(gerr, store.ErrNotFound) {
			skip(key, SkipStoreError, man.KeyFingerprint) // a real DB error — don't risk a partial/duplicate
			continue
		}

		// Confirm the archive itself is present and get its true on-disk size.
		size, exists, serr := be.Stat(ctx, archiveKey)
		if serr != nil || !exists {
			skip(key, SkipMissingArchive, man.KeyFingerprint)
			continue
		}

		b := &store.Backup{
			ID:         man.BackupID,
			NodeID:     man.NodeID,
			Stack:      man.Stack,
			TargetName: man.TargetName,
			Status:     "success",
			Verified:   "unverified", // re-registered from the manifest, not test-restored
		}
		if t, terr := time.Parse(time.RFC3339, man.CreatedAt); terr == nil {
			b.CreatedAt = t.Unix()
		}
		if e.Store.CreateBackup(b) != nil {
			skip(key, SkipStoreError, man.KeyFingerprint)
			continue
		}
		// Persist the rest of the row (CreateBackup only sets id/node/stack/target/…).
		b.SizeBytes = size
		b.CipherSHA256 = man.CipherSHA256
		b.StorageKey = archiveKey
		b.ManifestJSON = string(manBytes)
		b.CompletedAt = b.CreatedAt
		b.LocationsJSON = mustJSON([]Location{{Kind: "dest", DestID: destID, Name: destName, Type: destType}})
		_ = e.Store.UpdateBackup(b)
		e.logf(b.ID, "INFO", "Adopted orphaned backup %q from %s (%s)", man.TargetName, destName, humanBytes(size))
		adopted++
	}
	return adopted, skipped, nil
}

// maxSidecarBytes bounds a manifest or signature sidecar read from a
// destination. These are KILOBYTES — a manifest describing a hundred volumes is
// still well under one megabyte — so this is far past any real one and far below
// what the container is allowed to hold (mem_limit: 1g).
//
// The bound is the point. Reading a remote object with no limit means a corrupt
// or hostile destination can answer a request for a manifest with gigabytes, and
// the process is OOM-killed while the operator is merely scanning for orphaned
// backups. Nothing has to be malicious for it to happen: a destination that
// serves an HTML error page for every path, or a key that collided with a large
// object, is enough.
const maxSidecarBytes = 8 << 20 // 8 MiB

// readSidecar reads a sidecar object whole, refusing one that is implausibly
// large, and closes rc. The key is named in the error so the operator knows
// which object on which destination to go and look at.
func readSidecar(rc io.ReadCloser, key string) ([]byte, error) {
	defer rc.Close()
	// One byte past the limit, so a file exactly at it still reads and anything
	// larger is detectable without holding the whole thing.
	data, err := io.ReadAll(io.LimitReader(rc, maxSidecarBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSidecarBytes {
		return nil, fmt.Errorf("sidecar %q is larger than %s — refusing to read it into memory", key, humanBytes(maxSidecarBytes))
	}
	return data, nil
}

// readVerifiedManifest fetches a manifest sidecar and returns its plaintext bytes
// ONLY if it is cryptographically ours under the current master key: a sealed
// `.enc` that decrypts, or a readable manifest whose HMAC `.sig` verifies. Anything
// else (wrong key, missing/invalid signature) returns ok=false so the caller skips.
// It also returns the RAW sidecar bytes so the caller can report why a rejected
// archive was rejected. Those bytes are unverified and must never be trusted for
// anything but that report — see AdoptSkip.KeyFingerprint.
func (e *Engine) readVerifiedManifest(ctx context.Context, be storage.Backend, key, archiveKey string) (plaintext, raw []byte, ok bool) {
	rc, err := be.Get(ctx, key)
	if err != nil {
		return nil, nil, false
	}
	raw, rerr := readSidecar(rc, key)
	if rerr != nil {
		return nil, nil, false
	}
	if strings.HasSuffix(key, ".enc") {
		pt, derr := crypto.OpenString(raw, e.MasterKey())
		if derr != nil {
			return nil, raw, false // sealed with a different key (F16 third-key) or corrupt
		}
		return []byte(pt), raw, true
	}
	// Readable manifest: require its detached HMAC signature to verify under our key.
	sigrc, serr := be.Get(ctx, archiveKey+".manifest.json.sig")
	if serr != nil {
		return nil, raw, false // no signature — can't confirm it's ours; skip
	}
	// The signature comes from the same untrusted destination, in the same call.
	// An oversized one is unreadable rather than unsigned, and both mean skip.
	sig, sigErr := readSidecar(sigrc, archiveKey+".manifest.json.sig")
	if sigErr != nil {
		return nil, raw, false
	}
	if !crypto.VerifyManifest(raw, e.MasterKey(), strings.TrimSpace(string(sig))) {
		return nil, raw, false
	}
	return raw, raw, true
}

// maxAdoptSkipReport bounds the per-archive report. A bucket holding another
// instance's whole history would otherwise produce a response as large as its
// object listing — and a table of ten thousand rows tells an operator nothing
// that the first fifty do not.
const maxAdoptSkipReport = 200

// adoptSkipReason classifies a rejected sidecar from its raw bytes.
//
// A sidecar that PARSES as a manifest was written by a DockBack — it simply is
// not ours, which is the actionable case. Anything else is genuinely unreadable.
func adoptSkipReason(raw []byte) string {
	if looksLikeManifest(raw) {
		return SkipForeignKey
	}
	return SkipUnreadable
}

// foreignKeyFingerprint extracts ONLY the key fingerprint from an unverified
// manifest, and only when the bytes really look like one.
//
// This is the one place DockBack reads an unauthenticated manifest, so the
// narrowness is the point: a foreign sidecar may belong to another operator's
// instance, and its container names, stack names and volume paths are not ours
// to surface. The fingerprint identifies a KEY, is already shown throughout the
// UI, and is exactly what makes "skipped" actionable.
//
// A sealed (.enc) sidecar is opaque ciphertext, so nothing is readable — those
// skips carry no fingerprint, which is honest rather than a gap.
func foreignKeyFingerprint(raw []byte) string {
	if !looksLikeManifest(raw) {
		return ""
	}
	var probe struct {
		KeyFingerprint string `json:"key_fingerprint"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return ""
	}
	fp := strings.TrimSpace(probe.KeyFingerprint)
	// Fingerprints are short hex. Anything else came from a file that is not a
	// manifest we should be quoting back into the UI.
	if len(fp) > 64 || strings.ContainsAny(fp, "\n\r\x00") {
		return ""
	}
	return fp
}

// looksLikeManifest reports whether raw parses as a DockBack manifest — used to
// tell "another instance's backup" from "not a manifest at all".
func looksLikeManifest(raw []byte) bool {
	if len(raw) == 0 || len(raw) > maxManifestProbeBytes {
		return false
	}
	var probe struct {
		BackupID string `json:"backup_id"`
		Version  int    `json:"version"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return false
	}
	return probe.BackupID != "" || probe.Version > 0
}

// maxManifestProbeBytes bounds the unverified parse. A manifest is small; a
// multi-megabyte "sidecar" is not one, and is not worth the allocation.
const maxManifestProbeBytes = 1 << 20
