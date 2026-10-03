package api

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"dockback/internal/backup"
	"dockback/internal/crypto"
	"dockback/internal/notify"
	"dockback/internal/store"
)

// Encryption-key escrow & recovery (PLAN §9.2). AES-256-GCM means losing the
// master key permanently destroys every backup, so the UI forces the admin
// through a one-time "save your key" flow and warns persistently until done.

// handleKeyStatus reports whether the master key has been acknowledged as
// backed up, and whether it is ephemeral (no persistent key configured). It
// returns NO key material — it only drives the reminder banner.
func (s *Server) handleKeyStatus(w http.ResponseWriter, r *http.Request) {
	ack, _ := s.store.GetSetting("key.escrow_acknowledged", "false")
	atStr, _ := s.store.GetSetting("key.escrow_acknowledged_at", "0")
	at, _ := strconv.ParseInt(atStr, 10, 64)
	writeJSON(w, http.StatusOK, map[string]any{
		"acknowledged":    ack == "true",
		"acknowledged_at": at,
		"ephemeral":       s.cfg.EphemeralKey,
		"fingerprint":     s.engine.MasterKeyFP(),
		// The policy the keyfile passphrase is held to, so the form states the
		// real number rather than a hard-coded one that can disagree with it.
		"min_passphrase_len": s.minPasswordLength(),
	})
}

// handleKeyReveal returns the master key (hex) so the admin can save a recovery
// sheet. This is a deliberate, explicit action: auth + CSRF protected, audited,
// AND step-up gated (F64 — the account password, plus TOTP when enabled, must
// be re-entered); the key is never written to logs. The caller already supplied
// this key via the environment, so this only re-surfaces it for safe-keeping.
func (s *Server) handleKeyReveal(w http.ResponseWriter, r *http.Request) {
	var req stepUpBody
	_ = readJSON(r, &req) // body optional — absent creds just mean "no step-up yet"
	if !s.requireFreshAuth(w, r, req) {
		return
	}
	_ = s.store.Audit(userFrom(r), "key.revealed", "", "master encryption key revealed for a recovery sheet; step-up ok")
	// F198: the one secret that decrypts every backup this app has ever written
	// was just put on a screen. That is a legitimate action — it is how the
	// recovery sheet gets made — but it is also exactly what an attacker who
	// reached a session would do, and it was previously visible only to whoever
	// went and read the audit table. Not throttled: every reveal is reported,
	// because "it happened twice" is itself the thing worth knowing.
	s.notify(notify.KindKeyRevealed,
		"Master encryption key was revealed",
		fmt.Sprintf("The master key (fingerprint %s) was displayed to %s from %s. "+
			"If this was not you, every backup that key can decrypt should be considered exposed: rotate the key from Settings → Security and revoke other sessions.",
			s.engine.MasterKeyFP(), safeLabel(userFrom(r), 64), s.clientIP(r)))
	writeJSON(w, http.StatusOK, map[string]any{
		"key_hex":     hex.EncodeToString(s.engine.MasterKey()),
		"fingerprint": s.engine.MasterKeyFP(),
		"ephemeral":   s.cfg.EphemeralKey,
	})
}

// handleKeyKeyfile mints a passphrase-protected keyfile from the running master
// key so the operator can store the key encrypted at rest (PLAN §9.2): mount the
// file + set DOCKBACK_ENCRYPTION_KEYFILE/_PASSPHRASE, then remove the plaintext
// key. Deliberate action: auth + CSRF + audited; the key is never logged.
func (s *Server) handleKeyKeyfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Passphrase string `json:"passphrase"`
		stepUpBody
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	// F64: same step-up gate as key-reveal — this handler exports the master key
	// too, just passphrase-wrapped. Usually satisfied by the reveal's fresh grant.
	if !s.requireFreshAuth(w, r, req.stepUpBody) {
		return
	}
	// The same minimum the account password is held to. This passphrase wraps the
	// MASTER KEY, which opens every backup ever taken, and the file it protects is
	// by design kept beside the deployment — so it is the last thing that should
	// be allowed to be weaker than the login.
	if minLen := s.minPasswordLength(); len(req.Passphrase) < minLen {
		errJSON(w, http.StatusBadRequest, fmt.Sprintf("passphrase must be at least %d characters", minLen))
		return
	}
	kf, err := crypto.WrapKeyfile(s.engine.MasterKey(), req.Passphrase)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "key.keyfile", "", "passphrase-protected keyfile generated; step-up ok")
	writeJSON(w, http.StatusOK, map[string]string{"keyfile": string(kf)})
}

// handleKeyAcknowledge records that the admin has stored the key safely, which
// clears the persistent reminder (PLAN §9.2). Audited.
func (s *Server) handleKeyAcknowledge(w http.ResponseWriter, r *http.Request) {
	_ = s.store.SetSetting("key.escrow_acknowledged", "true")
	_ = s.store.SetSetting("key.escrow_acknowledged_at", strconv.FormatInt(time.Now().Unix(), 10))
	_ = s.store.Audit(userFrom(r), "key.escrow.ack", "", "admin confirmed the encryption key is backed up")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- F86 write-only backups: asymmetric envelope, offline private key ---

// manifestOf parses a backup row's manifest-of-record, returning an empty (not
// nil) manifest when there is none — so callers can test it without nil checks.
func manifestOf(b *store.Backup) *backup.Manifest {
	m := &backup.Manifest{}
	if b != nil && b.ManifestJSON != "" {
		_ = json.Unmarshal([]byte(b.ManifestJSON), m)
	}
	return m
}

// handleWriteOnlyStatus reports whether write-only mode is armed, and the
// fingerprint of the public key new backups are being sealed to. PUBLIC material
// only — there is no private key here to leak, because DockBack never had one.
func (s *Server) handleWriteOnlyStatus(w http.ResponseWriter, r *http.Request) {
	pub, _ := s.store.GetSetting("backup.write_only_pubkey", "")
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":     pub != "",
		"public_key":  pub,
		"fingerprint": crypto.BackupPubFP(pub),
	})
}

// handleWriteOnlyEnable generates an X25519 keypair, stores ONLY the public half,
// and returns the private key EXACTLY ONCE. Losing it means every backup taken
// while the mode is armed is unrecoverable, which is stated plainly in the UI.
//
// Step-up gated (F64), CSRF protected and audited, mirroring handleKeyReveal:
// arming this changes what a compromise of this instance is worth, so it must
// not be reachable from a merely-hijacked session.
func (s *Server) handleWriteOnlyEnable(w http.ResponseWriter, r *http.Request) {
	var req stepUpBody
	_ = readJSON(r, &req) // body optional — absent creds just mean "no step-up yet"
	if !s.requireFreshAuth(w, r, req) {
		return
	}
	if cur, _ := s.store.GetSetting("backup.write_only_pubkey", ""); cur != "" {
		// Refuse to silently supersede a live keypair: backups sealed to the old
		// public key would remain restorable ONLY with the old private key, and
		// quietly handing out a second sheet is how an operator ends up unable to
		// tell which key opens which backup.
		errJSON(w, http.StatusConflict, "write-only mode is already enabled — disable it first if you intend to start a NEW keypair (backups already taken stay tied to the current one)")
		return
	}
	pub, priv, err := crypto.NewBackupKeypair()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.store.SetSetting("backup.write_only_pubkey", pub); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	fp := crypto.BackupPubFP(pub)
	// The private key is NEVER written to the audit trail, a log line, or the
	// database — this row records only that the mode was armed, and to which
	// (public) keypair.
	_ = s.store.Audit(userFrom(r), "writeonly.enable", fp,
		"write-only backups armed; private key issued once and not retained; step-up ok")
	writeJSON(w, http.StatusOK, map[string]any{
		"public_key":  pub,
		"private_key": priv,
		"fingerprint": fp,
	})
}

// handleWriteOnlyDisable returns NEW backups to the symmetric envelope. It does
// not and cannot convert existing write-only backups: those stay sealed to the
// offline key, so the recovery sheet must be kept regardless. The response says
// how many are affected so the warning can be specific.
func (s *Server) handleWriteOnlyDisable(w http.ResponseWriter, r *http.Request) {
	var req stepUpBody
	_ = readJSON(r, &req)
	if !s.requireFreshAuth(w, r, req) {
		return
	}
	pub, _ := s.store.GetSetting("backup.write_only_pubkey", "")
	if pub == "" {
		errJSON(w, http.StatusBadRequest, "write-only mode is not enabled")
		return
	}
	if err := s.store.SetSetting("backup.write_only_pubkey", ""); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "writeonly.disable", crypto.BackupPubFP(pub),
		"new backups return to the master-key envelope; existing write-only backups still need the offline key; step-up ok")
	writeJSON(w, http.StatusOK, map[string]any{
		"disabled":            true,
		"still_write_only":    s.countWriteOnlyBackups(),
		"keep_recovery_sheet": true,
	})
}

// countWriteOnlyBackups counts stored backups that still need the offline key, so
// the UI can say exactly what the recovery sheet is still protecting.
func (s *Server) countWriteOnlyBackups() int {
	list, err := s.store.ListBackups("", 100000)
	if err != nil {
		return 0
	}
	n := 0
	for _, b := range list {
		if backup.IsWriteOnly(manifestOf(b)) {
			n++
		}
	}
	return n
}
