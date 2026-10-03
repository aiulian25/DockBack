package api

import (
	"errors"
	"net/http"
	"strings"

	"dockback/internal/backup"
	"dockback/internal/crypto"
)

// Recovery-key drill (F204).
//
// Write-only mode (F86) deliberately makes DockBack unable to read its own
// backups: each DEK is sealed to a public key whose private half lives offline,
// on a recovery sheet in someone's safe. That is the security property, and it
// is worth having. It also means the entire backup history now depends on a
// string that nothing in the system has ever checked.
//
// Every other failure mode here is already covered — the ciphertext hash catches
// corruption, the signed manifest catches tampering, drills catch a backup that
// won't boot. The one property with no check at all is the one that matters
// most: does the key the operator actually saved open the archive? A transposed
// character, a truncated paste, or a sheet from a keypair rotated last year all
// look identical to a correct key right up until a real disaster restore.
//
// This endpoint answers that question in seconds without restoring anything: it
// unwraps one backup's DEK with the pasted key and opens the first frame of the
// archive. Pass means the key works, for this backup and every other backup
// sealed to the same keypair.
//
// HANDLING OF THE KEY
//
// Identical to a restore (handlers.go RestoreOptions.PrivateKey): it arrives in
// a POST body, lives in a local variable, and is gone when the handler returns.
// It is never stored, never written to a log line, and never placed in the audit
// detail — the audit trail is readable by any admin and is exported inside app
// backups, so it is the last place an offline recovery key may appear. What IS
// recorded is the keypair FINGERPRINT, which is derived from public material and
// is the thing an operator needs when reading the trail later.
//
// The endpoint is step-up gated (F2/F199 precedent). It performs a decryption
// with an operator-supplied key, which is exactly the class of action for which
// this app already demands fresh proof of the password.

// verifyKeyRequest carries the pasted key and the step-up credentials.
type verifyKeyRequest struct {
	stepUpBody
	PrivateKey string `json:"private_key"`
}

// handleVerifyRecoveryKey runs the drill for one backup (F204).
//
// The response separates three things that would otherwise be conflated:
//
//	200 {ok:true}   — the key opened the archive.
//	200 {ok:false}  — the key was tested and did not work. A verdict, not an
//	                  error: the request was well-formed and the drill ran.
//	4xx/5xx         — the drill could NOT run (no such backup, not write-only,
//	                  no reachable copy). These stay real errors, because
//	                  reporting "your key failed" when a destination was merely
//	                  unreachable would be a false alarm about the single thing
//	                  this feature exists to give confidence in.
func (s *Server) handleVerifyRecoveryKey(w http.ResponseWriter, r *http.Request) {
	var req verifyKeyRequest
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	id := r.PathValue("id")
	b, err := s.store.GetBackup(id)
	if err != nil {
		// Before the password prompt, as the export grant does: asking someone to
		// re-authenticate for a backup that does not exist wastes their time.
		errJSON(w, http.StatusNotFound, "backup not found")
		return
	}
	if strings.TrimSpace(req.PrivateKey) == "" {
		errJSON(w, http.StatusBadRequest, "paste the offline private key from this backup's recovery sheet")
		return
	}
	if !s.requireFreshAuth(w, r, req.stepUpBody) {
		return
	}

	fp, verr := s.engine.VerifyRecoveryKey(r.Context(), b, req.PrivateKey)

	// Could not run at all — say so as an error rather than a verdict.
	if errors.Is(verr, backup.ErrNotWriteOnly) {
		errJSON(w, http.StatusBadRequest, verr.Error())
		return
	}
	if errors.Is(verr, backup.ErrPrivateKeyRequired) {
		errJSON(w, http.StatusBadRequest, verr.Error())
		return
	}

	if verr == nil {
		_ = s.store.Audit(userFrom(r), "recovery_key.verified", id, "pass; keypair "+fp+"; first frame decrypted")
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":          true,
			"fingerprint": fp,
			"message":     "This key opens this backup. It will open every backup sealed to keypair " + fp + ".",
		})
		return
	}

	// A verdict on the key. The reason comes from the layer that produced it —
	// "wrong keypair", "does not match this backup", "frame 0 auth failed" — each
	// already written for the operator.
	_ = s.store.Audit(userFrom(r), "recovery_key.verified", id, "FAIL; keypair "+safeLabel(fp, 32)+"; "+safeLabel(verr.Error(), 200))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          false,
		"fingerprint": fp,
		"error":       verr.Error(),
	})
}

// handleWriteOnlyKeyFingerprint reports the fingerprint of a pasted private key
// without touching any backup (F204).
//
// A small thing that removes a real dead end: an operator holding two recovery
// sheets and no idea which is which currently has to guess, and guessing wrong
// against a backup produces a scary "does not match" message. Deriving the
// fingerprint locally from the key's own public half tells them which sheet they
// are holding, using public material only. It is still step-up gated and still
// never stores the key, because the input is a live recovery key regardless of
// how little the output reveals.
func (s *Server) handleWriteOnlyKeyFingerprint(w http.ResponseWriter, r *http.Request) {
	var req verifyKeyRequest
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	if strings.TrimSpace(req.PrivateKey) == "" {
		errJSON(w, http.StatusBadRequest, "paste a private key to identify it")
		return
	}
	if !s.requireFreshAuth(w, r, req.stepUpBody) {
		return
	}
	pub, err := crypto.PublicKeyFromPrivate(strings.TrimSpace(req.PrivateKey))
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fingerprint": crypto.BackupPubFP(pub)})
}
