package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"dockback/internal/crypto"
	"dockback/internal/store"
)

// Write-only mode (F86) — engine-side policy and the one place that decides
// whether this instance can read a given backup.
//
// The security property being bought: an attacker who owns the DockBack
// container owns the master key and every destination credential, and therefore
// could read the entire backup history wherever it is stored. Object Lock stops
// deletion, never reading. Write-only mode removes the read capability from the
// running process entirely by sealing each DEK to a public key whose private
// half never enters the container except by hand, for one restore.
//
// The honest cost, stated here because it is inherent and not a bug: an instance
// that cannot read its own backups cannot TEST them either. A tool cannot
// simultaneously prove it can restore a backup and be unable to open it. What
// remains for write-only backups is everything that doesn't need the payload —
// ciphertext SHA-256, size, and the master-key-signed manifest — plus the offline
// recovery path. What is lost is the deep decrypt-and-walk verification, restore
// drills, standby rehearsals, cross-backup file search, and incremental deltas
// (which diff against a previous archive's index, inside the ciphertext).
//
// Those are not silently degraded: each one reports write-only as its reason,
// incrementals fall back to full baselines, and the confidence grade refuses to
// award a drill-backed grade to a backup that cannot be drilled.

// ErrPrivateKeyRequired is returned by every read path when a backup is
// write-only encrypted and no offline private key was supplied for this
// operation. Callers surface it verbatim — it is written for the operator.
var ErrPrivateKeyRequired = errors.New("this backup is write-only encrypted — supply the offline private key to restore")

// writeOnlyPubSetting is the setting holding the ACTIVE public key. It is public
// material, so storing it in plain settings is correct: there is nothing here an
// attacker gains by reading, and sealing it under the master key would make it
// unavailable in exactly the recovery scenarios it exists for.
const writeOnlyPubSetting = "backup.write_only_pubkey"

// writeOnlyPub returns the active write-only public key, or "" for the default
// symmetric mode. Empty is the only "off" — there is no separate enabled flag to
// drift out of sync with the key.
func (e *Engine) writeOnlyPub() string {
	v, _ := e.Store.GetSetting(writeOnlyPubSetting, "")
	return v
}

// WriteOnlyEnabled reports whether NEW backups will be write-only encrypted.
// Existing backups are unaffected either way — the mode is recorded per backup
// in its manifest, so turning it on or off never changes what a stored backup is.
func (e *Engine) WriteOnlyEnabled() bool { return e.writeOnlyPub() != "" }

// IsWriteOnly reports whether a manifest describes a write-only backup.
func IsWriteOnly(man *Manifest) bool { return man != nil && man.WrappedKeyPub != "" }

// wrapDEK seals a fresh DEK for a new backup, choosing the envelope from the
// current mode, and records the result on the manifest. It is the ONLY place
// that decides which envelope a backup gets, so the two modes cannot diverge.
func (e *Engine) wrapDEK(dek []byte, man *Manifest) error {
	pub := e.writeOnlyPub()
	if pub == "" {
		wrapped, err := crypto.WrapKey(dek, e.MasterKey()) // today's symmetric envelope, unchanged
		if err != nil {
			return err
		}
		man.WrappedKey = wrapped
		return nil
	}
	wrapped, err := crypto.WrapKeyPub(dek, pub)
	if err != nil {
		return err
	}
	// WrappedKey stays EMPTY: there must be no master-key-openable copy of the
	// DEK anywhere, or the mode would be theatre.
	man.WrappedKeyPub = wrapped
	man.BackupPubFP = crypto.BackupPubFP(pub)
	return nil
}

// restorePrivFor returns the private key to use for the current operation, if
// one was supplied.
//
// F209: an ATOMIC load, never a lock. The caller is normally the restore's own
// goroutine — archiveKey reaching for the key it was given, or Verify deciding
// whether it may inspect a payload — and that goroutine already holds privGate
// for the whole restore. Taking the same mutex here parked it forever; reading
// the value atomically is memory-safe and re-entrant by construction.
func (e *Engine) restorePrivFor() string {
	if p := e.restorePriv.Load(); p != nil {
		return *p
	}
	return ""
}

// DrillSkipReason returns a human reason why a backup cannot be test-restored,
// or "" when a drill is possible. Callers MUST consult it before drilling: a
// write-only backup recorded as a "failed drill" would fire a critical alert and
// downgrade its confidence grade, when in fact nothing is wrong with it — the
// instance simply cannot open it, which is what the operator asked for.
func (e *Engine) DrillSkipReason(b *store.Backup) string {
	if b == nil || b.ManifestJSON == "" {
		return ""
	}
	var man Manifest
	if unmarshal(b.ManifestJSON, &man) != nil {
		return ""
	}
	return e.drillSkipReason(&man)
}

func (e *Engine) drillSkipReason(man *Manifest) string {
	if IsWriteOnly(man) && e.restorePrivFor() == "" {
		return "write-only encrypted — a drill would need the offline private key, so restores must be rehearsed by hand"
	}
	return ""
}

// ErrNotWriteOnly reports that a backup is NOT sealed to an offline keypair, so
// there is no recovery key to verify against it (F204). A distinct sentinel
// rather than a generic failure, because "this backup does not use a recovery
// key" and "your recovery key does not work" are opposite answers, and a caller
// that conflated them would tell an operator their key was bad when it was
// simply irrelevant to the backup they picked.
var ErrNotWriteOnly = errors.New("this backup is not write-only encrypted — it has no offline keypair to verify a recovery key against")

// VerifyRecoveryKey proves that an offline private key actually opens a
// write-only backup, without restoring it (F204).
//
// # THE GAP THIS CLOSES
//
// Write-only mode moves the only key that can read a backup out of the running
// system and onto a sheet of paper in someone's safe. Everything about that is
// deliberate — but it also means the single point of failure for the entire
// backup history is a string nothing ever checks. A transposed character, a
// truncated copy-paste, the key from a keypair that was rotated last year: none
// of these are visible until a real disaster restore, which is the worst
// possible moment to discover them. Structural verification cannot help here by
// construction; it proves the ciphertext is intact, and an intact archive whose
// key is wrong is exactly as unrecoverable as a corrupt one.
//
// # WHY THE FIRST FRAME IS ENOUGH
//
// One DEK encrypts the whole stream, so the key that authenticates frame 0
// authenticates every frame. Opening one frame proves the private key unwraps
// this backup's DEK and that the DEK decrypts this archive — which is the entire
// claim. Reading further would prove nothing more and would cost a full
// download, making it the kind of check nobody runs.
//
// The private key is a parameter and stays one: it is never written to the
// store, never logged, never assigned to e.restorePriv. That last point is not
// incidental — archiveKey() reads restorePriv, which is shared engine state that
// concurrent restores depend on, so a drill that set it would hand its key to
// whatever else happened to be running. This path calls UnwrapKeyPriv directly.
func (e *Engine) VerifyRecoveryKey(ctx context.Context, b *store.Backup, privB64 string) (fingerprint string, err error) {
	if b == nil {
		return "", errors.New("no backup to verify")
	}
	var man Manifest
	if b.ManifestJSON != "" {
		_ = unmarshal(b.ManifestJSON, &man)
	}
	if !IsWriteOnly(&man) {
		return "", ErrNotWriteOnly
	}
	if strings.TrimSpace(privB64) == "" {
		return "", ErrPrivateKeyRequired
	}
	privB64 = strings.TrimSpace(privB64)

	// The fingerprint the operator is holding, reported on every outcome so a
	// failure says WHICH key was tried. Derived from the private key's own public
	// half — public material, safe to return and to audit.
	pub, err := crypto.PublicKeyFromPrivate(privB64)
	if err != nil {
		return "", err
	}
	fingerprint = crypto.BackupPubFP(pub)

	// Cheap check first: a key from the wrong keypair is the commonest mistake
	// (more than one recovery sheet exists), and saying so costs no I/O and reads
	// nothing like corruption.
	if err := CheckPrivateKey(&man, privB64); err != nil {
		return fingerprint, err
	}

	dek, err := crypto.UnwrapKeyPriv(man.WrappedKeyPub, privB64)
	if err != nil {
		return fingerprint, err
	}
	defer crypto.Zero(dek)

	// Try each copy that should hold data, newest-preferred order, and stop at
	// the first that decrypts. A destination being unreachable is not a verdict
	// on the key, so it moves on rather than reporting a failure.
	var lastErr error
	tried := 0
	for _, loc := range e.orderLocations(b, "") {
		if noData(loc.Status) {
			continue
		}
		be, err := e.backendForLocation(loc)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", loc.Name, err)
			continue
		}
		rc, err := be.Get(ctx, b.StorageKey)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", loc.Name, err)
			continue
		}
		// Bounded: at most one frame is ever read, and the reader is closed
		// immediately — on a remote backend that aborts the transfer rather than
		// streaming a whole archive nobody asked for.
		derr := crypto.DecryptFirstFrame(io.LimitReader(rc, int64(crypto.MaxFirstFrameBytes)), dek)
		rc.Close()
		tried++
		if derr == nil {
			return fingerprint, nil
		}
		lastErr = fmt.Errorf("%s: %w", loc.Name, derr)
	}
	if lastErr == nil {
		lastErr = errors.New("this backup has no readable copy to test the key against")
	}
	if tried == 0 {
		// Never opened an archive: the key was not tested, so this must not read
		// as a failed key.
		return fingerprint, fmt.Errorf("could not reach a copy of this backup to test the key: %w", lastErr)
	}
	return fingerprint, lastErr
}

// CheckPrivateKey validates a pasted private key against a backup's recorded
// keypair fingerprint BEFORE any decryption is attempted, so the operator gets
// "that is the wrong key" instead of a GCM failure that reads like corruption.
//
// It compares PUBLIC fingerprints only — the private key is used solely to
// derive its own public half, locally, and is never stored or logged.
func CheckPrivateKey(man *Manifest, privB64 string) error {
	if !IsWriteOnly(man) {
		return nil
	}
	if privB64 == "" {
		return ErrPrivateKeyRequired
	}
	pub, err := crypto.PublicKeyFromPrivate(privB64)
	if err != nil {
		return err
	}
	if man.BackupPubFP != "" && crypto.BackupPubFP(pub) != man.BackupPubFP {
		return errors.New("that private key belongs to a different keypair than this backup — check which recovery sheet you are holding")
	}
	return nil
}
