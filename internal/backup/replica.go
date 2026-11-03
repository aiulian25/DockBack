package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"dockback/internal/crypto"
	"dockback/internal/storage"
)

// A replica is only worth calling offsite protection if it can be opened without
// this instance. A modern archive is encrypted under a per-backup key whose only
// wrapped form is in the manifest beside it, and the manifest inside the archive
// cannot unlock the archive holding it. So the archive and that envelope are one
// publication, and the envelope is read back before the copy counts.

const (
	// LocationIncomplete is a copy whose archive is on the destination but whose
	// recovery envelope is not proven there. The bytes exist, so it can be restored
	// from while the catalog lives and must be pruned like any copy, but it is not
	// protection, and "Send offsite now" repairs it without resending the archive.
	LocationIncomplete = "incomplete"

	sidecarManifest  = ".manifest.json"
	sidecarSignature = ".manifest.json.sig"
	sidecarSealed    = ".manifest.json.enc"

	// envelopePublishTimeout bounds writing and reading back a few small objects, so
	// a destination that hangs fails the copy rather than the whole mirror.
	envelopePublishTimeout = 5 * time.Minute

	replicaEnvelopeCheck = "replica-envelope"
)

// needsPublishing reports whether a location still needs something sent to it.
func needsPublishing(status string) bool { return noData(status) || status == LocationIncomplete }

type manifestSidecar struct {
	suffix string
	body   []byte
}

// manifestSidecars is the envelope for one sealing preference: an AES-256-GCM
// sealed manifest, or a readable manifest with an HMAC signature, last so that a
// signature in place means its manifest already is.
func (e *Engine) manifestSidecars(manBytes []byte, seal bool) ([]manifestSidecar, error) {
	if !seal {
		return []manifestSidecar{
			{sidecarManifest, manBytes},
			{sidecarSignature, []byte(crypto.SignManifest(manBytes, e.MasterKey()))},
		}, nil
	}
	sealed, err := crypto.SealString(string(manBytes), e.MasterKey())
	if err != nil {
		return nil, err
	}
	return []manifestSidecar{{sidecarSealed, sealed}}, nil
}

// requiredSidecarSuffixes names the objects a sealing preference requires.
func requiredSidecarSuffixes(seal bool) []string {
	if seal {
		return []string{sidecarSealed}
	}
	return []string{sidecarManifest, sidecarSignature}
}

// checkEnvelope proves the destination's envelope opens under the current master
// key and carries the wrapped key the catalog does. Read back, because a write that
// returned success is the claim being tested.
func (e *Engine) checkEnvelope(ctx context.Context, be storage.Backend, key string, manBytes []byte, seal bool) error {
	first := key + requiredSidecarSuffixes(seal)[0]
	plaintext, _, ok := e.readVerifiedManifest(ctx, be, first, key)
	if !ok {
		return fmt.Errorf("%s is missing or does not verify under the current key", strings.TrimPrefix(first, key))
	}
	var stored, want Manifest
	if err := errors.Join(json.Unmarshal(plaintext, &stored), json.Unmarshal(manBytes, &want)); err != nil {
		return fmt.Errorf("reading the envelope: %w", err)
	}
	if stored.WrappedKey != want.WrappedKey || stored.WrappedKeyPub != want.WrappedKeyPub {
		return errors.New("its wrapped key is not the one this backup records")
	}
	return nil
}

// publishEnvelope makes the envelope on be complete, writing only what is not there
// yet: a repair may find some of it already locked in place.
func (e *Engine) publishEnvelope(ctx context.Context, be storage.Backend, key string, manBytes []byte, seal bool) error {
	if e.checkEnvelope(ctx, be, key, manBytes, seal) == nil {
		return nil
	}
	sidecars, err := e.manifestSidecars(manBytes, seal)
	if err != nil {
		return err
	}
	for _, sidecar := range sidecars {
		if rc, gerr := be.Get(ctx, key+sidecar.suffix); gerr == nil {
			existing, rerr := readSidecar(rc, key+sidecar.suffix)
			if rerr == nil && bytes.Equal(existing, sidecar.body) {
				continue
			}
		}
		if _, err := be.Put(ctx, key+sidecar.suffix, bytes.NewReader(sidecar.body)); err != nil {
			return fmt.Errorf("writing %s: %w", sidecar.suffix, err)
		}
	}
	return e.checkEnvelope(ctx, be, key, manBytes, seal)
}

// publishEnvelopeWithRetry gives the envelope the transient-failure handling the
// archive's upload has, without sending the archive again.
func (e *Engine) publishEnvelopeWithRetry(ctx context.Context, id, destName string, be storage.Backend, key string, manBytes []byte, seal bool) error {
	ctx, cancel := context.WithTimeout(ctx, envelopePublishTimeout)
	defer cancel()
	backoff := mirrorRetryBackoff
	var err error
	for attempt := 1; attempt <= maxMirrorAttempts; attempt++ {
		if err = e.publishEnvelope(ctx, be, key, manBytes, seal); err == nil {
			return nil
		}
		if attempt == maxMirrorAttempts || !isTransientErr(err) {
			return err
		}
		e.logf(id, "INFO", "Mirror to %s: recovery envelope failed (%v) — retry %d/%d in %s", destName, err, attempt, maxMirrorAttempts-1, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 3
	}
	return err
}

// archiveGone reports whether the destination answered that the archive is not
// there. An unanswered question is not that, and the envelope step will say so.
func archiveGone(ctx context.Context, be storage.Backend, key string) bool {
	_, present, err := be.Stat(ctx, key)
	return err == nil && !present
}

// recordReplicaLock applies and records a destination's retention lock. A
// filesystem lock waits for the whole publication, since locking part of an envelope
// could stop it being completed. An object store locked the archive when it was
// written, whatever became of the envelope, and that has to be on record or prune
// would try to delete it. prior is the copy being repaired: its archive's lock did
// not restart.
func (e *Engine) recordReplicaLock(ctx context.Context, id string, backend storage.Backend, key string, required []string, prior Location, loc *Location) {
	immutable, ok := backend.(storage.Immutabler)
	if !ok || !immutable.Immutable() {
		return
	}
	until := time.Now().Add(time.Duration(immutable.LockDays()) * 24 * time.Hour)
	if prior.LockUntil > 0 {
		until = time.Unix(prior.LockUntil, 0)
	}
	record := func() {
		loc.Immutable = true
		loc.LockUntil = until.Unix()
		e.logf(id, "INFO", "The copy on %s is immutable / WORM-locked for ~%dd", loc.Name, immutable.LockDays())
	}
	locker, filesystemLock := backend.(storage.RetentionLocker)
	if !filesystemLock {
		record()
		return
	}
	if loc.Status == LocationIncomplete {
		return
	}
	if prior.Immutable {
		for _, suffix := range required {
			if lerr := locker.LockUntil(ctx, key+suffix, until); lerr != nil {
				e.logf(id, "WARN", "Repaired %s but could not lock %s (%v) — the archive stays locked", loc.Name, suffix, lerr)
			}
		}
		record()
		return
	}
	if lerr := e.applyRetentionLock(ctx, locker, key, required, until); lerr != nil {
		e.logf(id, "WARN", "Mirrored to %s but could not apply its retention lock (%v) — this copy is NOT protected and prune may remove it on schedule", loc.Name, lerr)
		return
	}
	record()
}

// replicaEnvelopeProblem says what is wrong with a replica's envelope, or nothing
// when it is sound, is not needed, or could not be asked about. A legacy archive
// opens with the master key alone, so only one with a wrapped key depends on it.
func (e *Engine) replicaEnvelopeProblem(ctx context.Context, be storage.Backend, key string, man *Manifest, manBytes []byte) string {
	if man.WrappedKey == "" && man.WrappedKeyPub == "" {
		return ""
	}
	_, sealed, sealedErr := be.Stat(ctx, key+sidecarSealed)
	_, readable, readableErr := be.Stat(ctx, key+sidecarManifest)
	_, signed, signedErr := be.Stat(ctx, key+sidecarSignature)
	if errors.Join(sealedErr, readableErr, signedErr) != nil {
		return ""
	}
	if !sealed && !(readable && signed) {
		return "this copy's recovery envelope is missing, so it cannot be opened without this instance's catalog"
	}
	if err := e.checkEnvelope(ctx, be, key, manBytes, sealed); err != nil {
		return "this copy's recovery envelope is unusable: " + err.Error()
	}
	return ""
}
