package api

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dockback/internal/backup"
	"dockback/internal/crypto"
	"dockback/internal/store"
)

// Master-key rotation (F16). Supplying the current + a new 64-hex master key
// re-wraps every backup's per-backup data key (no archive re-encryption) AND
// re-seals every OTHER secret sealed with the master key — node connection
// secrets, destination credentials, the notification config, and the 2FA secret —
// so the running process, and the next restart once DOCKBACK_ENCRYPTION_KEY is
// updated, keeps working end-to-end. (A backup-DEK-only rotation would strand all
// of those, breaking node access, offsite copies, notifications and 2FA.)
// Control-plane app-backups carry the same envelope as container archives, so
// rotation re-wraps them in place too (F37) — and, since F72, the remote copies
// on app-backup destinations as well (download → re-wrap header → re-upload),
// plus the app-destination credentials themselves. Only v1 (pre-envelope)
// app-backups are left on the old key — keep it to restore one of those; a v2
// file sealed under an unrelated key is reported as FAILED, never skipped.

// parseHexKey decodes a 64-hex-character master key into its 32 raw bytes.
func parseHexKey(s string) ([]byte, error) {
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("not valid hex")
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("must be 64 hex characters (32 bytes), got %d bytes", len(b))
	}
	return b, nil
}

func (s *Server) handleKeyRotate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CurrentHex string `json:"current_hex"`
		NewHex     string `json:"new_hex"`
		stepUpBody        // F64: password (+ TOTP code when 2FA is on)
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	u, ok := s.currentUser(r)
	if !ok {
		errJSON(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	// Step-up re-authentication (F64): this is a destructive, security-critical
	// action. Replaces the previous inline password check — same argon2 guard and
	// lockout, plus the second factor and the shared 5-minute grant window.
	if !s.requireFreshAuth(w, r, req.stepUpBody) {
		return
	}

	oldKey, err := parseHexKey(req.CurrentHex)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "current key: "+err.Error())
		return
	}
	newKey, err := parseHexKey(req.NewHex)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "new key: "+err.Error())
		return
	}
	// The supplied current key MUST match the running master key (constant-time), so
	// a rotation can never be driven from a wrong/guessed current key — which would
	// re-seal secrets so they can't be opened by the real key.
	if subtle.ConstantTimeCompare(oldKey, s.cfg.EncryptionKey) != 1 {
		_ = s.store.Audit(u.Username, "security.key.rotate_failed", "", "current key mismatch ip="+s.clientIP(r))
		errJSON(w, http.StatusBadRequest, "current key does not match the running encryption key")
		return
	}
	if subtle.ConstantTimeCompare(oldKey, newKey) == 1 {
		errJSON(w, http.StatusBadRequest, "new key must differ from the current key")
		return
	}

	// F16: nothing may be WRITING backups while their data keys are re-wrapped.
	// A run that straddles the swap seals its archive with the outgoing key and
	// records the incoming key's fingerprint — permanently unrestorable, and
	// invisible until someone tries. So the dispatcher is held first, and if
	// anything is already in flight the rotation is refused rather than raced:
	// the queue drains on its own and the operator simply tries again.
	releaseHold := s.holdBackupsForRotation()
	defer releaseHold()
	if n := s.backupsInFlight(); n > 0 {
		errJSON(w, http.StatusConflict, fmt.Sprintf("%d backup(s) queued or running — wait for them to finish, then rotate the key", n))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// 1) Re-wrap every backup DEK + rewrite manifest sidecars. MUST run while the
	//    engine's in-memory key is still the OLD key (it resolves destination
	//    backends — whose credentials are still old-key-sealed at this point).
	rr := s.engine.RewrapAll(ctx, oldKey, newKey)

	// 1b) Re-wrap control-plane app-backups too (F37) — same in-place envelope
	//     re-wrap, still on the OLD in-memory key. v1 (pre-envelope) archives are
	//     reported as skipped and stay recoverable with the old key.
	ar := s.engine.RewrapAppBackups(ctx, s.appBackupDir(), oldKey, newKey)

	// 2) Re-seal the other master-key-sealed secrets old -> new (explicit keys).
	nodesR, nodesF := s.resealNodeSecrets(oldKey, newKey)
	destsR, destsF := s.resealDestinations(oldKey, newKey)
	// F72: app-backup destinations live in their own table and were previously
	// missed — without this, pushing/restoring app backups on an external
	// destination breaks after rotation (credentials still old-key-sealed).
	appDestsR, appDestsF := s.resealAppDestinations(oldKey, newKey)
	notifyErr := s.resealNotifyConfig(oldKey, newKey)
	// F205: the per-container Redis passwords are sealed under the master key
	// too, and a rotation that skipped them would fail SILENTLY — the password
	// would stop opening, the dump would fall back to a raw file capture, and the
	// only symptom would be backups quietly grading lower again. Which is exactly
	// the loop that setting exists to close.
	redisR, redisF := s.resealRedisAuth(oldKey, newKey)
	totpR, totpErr := s.resealTOTP(u, oldKey, newKey)

	// 3) Switch the running process onto the new key so everything just re-sealed is
	//    immediately usable (before DOCKBACK_ENCRYPTION_KEY is updated for restart).
	s.cfg.EncryptionKey = newKey
	s.engine.SetKey(newKey, backup.KeyFingerprint(newKey))

	// F68: the audit hash chain is keyed by the master key. Re-anchor it so rows
	// chained under the OLD key are grandfathered (like pre-feature rows) instead
	// of being flagged broken; every row from here on links under the new key.
	if maxID, aerr := s.store.MaxAuditID(); aerr == nil {
		_ = s.store.SetSetting(auditAnchorKey, strconv.FormatInt(maxID, 10))
	}
	// F200: publish a checkpoint immediately at the new anchor. Rotation is
	// exactly when an operator's older checkpoints stop covering new rows, so
	// leaving the next one to the daily cadence would open a window with nothing
	// attested off-host.
	defer s.beaconAuditHead("after an encryption-key rotation")

	// 3c) F72: re-wrap the REMOTE app-config archives on the app-backup
	// destinations too (download → re-wrap header → re-upload in place). Runs
	// after the key switch so the just-re-sealed destination credentials open
	// with the current key; the archives themselves use the explicit old/new keys.
	adR, adF := s.rewrapAppDestBackups(ctx, oldKey, newKey)

	// 3b) End with one FRESH app-backup on the NEW key (F53). The switch above
	//     already flipped s.cfg.EncryptionKey, so this snapshot is a v2 envelope
	//     sealed under the new key — making the control-plane immediately
	//     recoverable under the new key even if every prior app-backup was a v1
	//     (pre-envelope) archive left on the old key. Best-effort: a failure here is
	//     a follow-up warning, never a rotation failure.
	freshEntry, freshErr := s.createAppBackup(u.Username)

	var warnings []string
	if rr.Failed > 0 {
		warnings = append(warnings, fmt.Sprintf("%d backup(s) belonged to the current key but their manifest could not be updated — re-run the rotation.", rr.Failed))
	}
	if rr.Skipped > 0 {
		warnings = append(warnings, fmt.Sprintf("%d backup(s) were left unchanged (encrypted with a different key, or a legacy direct-key archive) — they stay recoverable only with that key.", rr.Skipped))
	}
	if ar.Failed > 0 {
		warnings = append(warnings, fmt.Sprintf("%d app-backup(s) could not be re-wrapped (sealed under a different key, corrupt, or an I/O error — see the security log) — they will NOT open with the new key.", ar.Failed))
	}
	if ar.Skipped > 0 {
		warnings = append(warnings, fmt.Sprintf("%d older app-backup(s) predate envelope encryption and stay recoverable only with the old key — keep it archived.", ar.Skipped))
	}
	if adF > 0 {
		warnings = append(warnings, fmt.Sprintf("%d remote app-backup(s) on external destinations could not be re-wrapped — push a fresh one with \"Back up to destinations now\", or keep the old key for them.", adF))
	}
	if appDestsF > 0 {
		warnings = append(warnings, fmt.Sprintf("%d app-backup destination credential(s) could not be re-sealed — re-enter them.", appDestsF))
	}
	if nodesF > 0 {
		warnings = append(warnings, fmt.Sprintf("%d node secret(s) could not be re-sealed — re-enter their credentials.", nodesF))
	}
	if destsF > 0 {
		warnings = append(warnings, fmt.Sprintf("%d destination credential(s) could not be re-sealed — re-enter them.", destsF))
	}
	if notifyErr != nil {
		warnings = append(warnings, "the notification config could not be re-sealed — re-save it in Settings → Notifications.")
	}
	if redisF > 0 {
		warnings = append(warnings, fmt.Sprintf("%d per-container Redis password(s) could not be re-sealed — re-enter them on those containers, or their backups fall back to a file capture.", redisF))
	}
	if totpErr != nil {
		warnings = append(warnings, "two-factor auth could not be re-sealed — disable and re-enrol 2FA.")
	}
	if freshErr != nil {
		warnings = append(warnings, "a fresh app-backup could not be created automatically — create one in Settings → Application backup.")
	}

	freshFile := "-"
	if freshErr == nil {
		freshFile = freshEntry.File
	}
	_ = s.store.Audit(u.Username, "security.key.rotate", "",
		fmt.Sprintf("step-up ok; fp %s->%s; backups rewrapped=%d skipped=%d failed=%d; app-backups rewrapped=%d skipped=%d failed=%d; remote app-backups rewrapped=%d failed=%d; nodes=%d dests=%d appdests=%d totp=%d redis_auth=%d/%d fresh_app_backup=%s ip=%s",
			backup.KeyFingerprint(oldKey), s.engine.MasterKeyFP(), rr.Rewrapped, rr.Skipped, rr.Failed, ar.Rewrapped, ar.Skipped, ar.Failed, adR, adF, nodesR, destsR, appDestsR, totpR, redisR, redisF, freshFile, s.clientIP(r)))
	s.logSink("security", "WARN", "Master encryption key ROTATED to fingerprint "+s.engine.MasterKeyFP()+" — update DOCKBACK_ENCRYPTION_KEY to the new value before the next restart, and keep the OLD key archived until verified.")

	resp := map[string]any{
		"status":                "rotated",
		"new_fingerprint":       s.engine.MasterKeyFP(),
		"backups":               map[string]int{"rewrapped": rr.Rewrapped, "skipped": rr.Skipped, "failed": rr.Failed},
		"app_backups":           map[string]int{"rewrapped": ar.Rewrapped, "skipped": ar.Skipped, "failed": ar.Failed},
		"remote_app_backups":    map[string]int{"rewrapped": adR, "failed": adF}, // F72: archives on app-backup destinations
		"nodes_resealed":        nodesR,
		"destinations_resealed": destsR,
		"app_dests_resealed":    appDestsR,
		// F205: per-container Redis passwords carried onto the new key.
		"redis_auth_resealed": redisR,
		"totp_resealed":       totpR,
		"warnings":            warnings,
		"message":             "Key rotated. You MUST set DOCKBACK_ENCRYPTION_KEY to the NEW key before the next restart. Keep the OLD key archived until you have verified backups restore under the new key.",
	}
	// A fresh app-backup on the new key (F53) — reported only when it succeeded; a
	// failure is surfaced in warnings above instead.
	if freshErr == nil {
		resp["fresh_app_backup"] = map[string]string{"file": freshEntry.File}
	}
	writeJSON(w, http.StatusOK, resp)
}

// resealNodeSecrets re-seals every node's connection secret old -> new (F16). Empty
// or legacy-plaintext secrets carry no sealed material and are left as-is.
func (s *Server) resealNodeSecrets(oldKey, newKey []byte) (resealed, failed int) {
	nodes, err := s.store.ListNodes()
	if err != nil {
		return 0, 0
	}
	for _, n := range nodes {
		if !isSealedNodeSecret(n.SecretEnc) {
			continue
		}
		pt, err := crypto.OpenString(n.SecretEnc[len(nseMagic):], oldKey)
		if err != nil {
			failed++
			continue
		}
		n.SecretEnc = SealNodeSecret([]byte(pt), newKey)
		if err := s.store.UpsertNode(n); err != nil {
			failed++
			continue
		}
		resealed++
	}
	return
}

// resealDestinations re-seals every destination's stored credentials old -> new (F16).
func (s *Server) resealDestinations(oldKey, newKey []byte) (resealed, failed int) {
	dests, err := s.store.ListDestinations()
	if err != nil {
		return 0, 0
	}
	for _, d := range dests {
		if len(d.ConfigEnc) == 0 {
			continue
		}
		js, err := crypto.OpenString(d.ConfigEnc, oldKey)
		if err != nil {
			failed++
			continue
		}
		sealed, err := crypto.SealString(js, newKey)
		if err != nil {
			failed++
			continue
		}
		d.ConfigEnc = sealed
		if err := s.store.UpdateDestination(d); err != nil {
			failed++
			continue
		}
		resealed++
	}
	return
}

// resealAppDestinations re-seals every APP-BACKUP destination's stored
// credentials old -> new (F72). These live in their own table and were missed
// by the original F16 rotation — leaving external app-backup push/list/restore
// broken after a rotation. Mirrors resealDestinations.
func (s *Server) resealAppDestinations(oldKey, newKey []byte) (resealed, failed int) {
	ds, err := s.store.ListAppDestinations()
	if err != nil {
		return 0, 0
	}
	for _, d := range ds {
		if len(d.ConfigEnc) == 0 {
			continue
		}
		js, err := crypto.OpenString(d.ConfigEnc, oldKey)
		if err != nil {
			failed++
			continue
		}
		sealed, err := crypto.SealString(js, newKey)
		if err != nil {
			failed++
			continue
		}
		if err := s.store.UpdateAppDestinationConfig(d.ID, sealed); err != nil {
			failed++
			continue
		}
		resealed++
	}
	return
}

// resealNotifyConfig re-seals the encrypted notification config old -> new (F16).
func (s *Server) resealNotifyConfig(oldKey, newKey []byte) error {
	enc, _ := s.store.GetSetting(notifyConfigKey, "")
	if enc == "" {
		return nil
	}
	plain, err := crypto.OpenString([]byte(enc), oldKey)
	if err != nil {
		return err
	}
	sealed, err := crypto.SealString(plain, newKey)
	if err != nil {
		return err
	}
	return s.store.SetSetting(notifyConfigKey, string(sealed))
}

// resealRedisAuth re-seals every per-container Redis password old -> new (F205).
//
// Enumerated by settings PREFIX rather than by walking containers, because the
// containers a node reports today are not necessarily the ones a password was
// recorded for — a node that is down, or a container renamed since, would
// otherwise have its setting left behind on the old key with nothing to say so.
//
// A row that cannot be opened is COUNTED AND LEFT ALONE, never deleted. It was
// sealed under some other key; overwriting it would destroy the only copy, while
// leaving it costs nothing but a warning the operator can act on.
func (s *Server) resealRedisAuth(oldKey, newKey []byte) (resealed, failed int) {
	keys, err := s.store.SettingKeysWithPrefix(backup.RedisAuthKeyPrefix)
	if err != nil {
		return 0, 0
	}
	for _, k := range keys {
		stored, _ := s.store.GetSetting(k, "")
		if strings.TrimSpace(stored) == "" {
			continue // cleared entry — nothing sealed here
		}
		blob, derr := hex.DecodeString(stored)
		if derr != nil {
			failed++
			continue
		}
		pw, oerr := crypto.OpenString(blob, oldKey)
		if oerr != nil {
			failed++
			continue
		}
		sealed, serr := crypto.SealString(pw, newKey)
		if serr != nil {
			failed++
			continue
		}
		if err := s.store.SetSetting(k, hex.EncodeToString(sealed)); err != nil {
			failed++
			continue
		}
		resealed++
	}
	return resealed, failed
}

// resealTOTP re-seals the active 2FA secret old -> new (F16). Recovery codes are
// hashes (not sealed) and are untouched.
func (s *Server) resealTOTP(u *store.User, oldKey, newKey []byte) (int, error) {
	if u == nil || u.TOTPSecret == "" {
		return 0, nil
	}
	blob, err := hex.DecodeString(u.TOTPSecret)
	if err != nil {
		return 0, err
	}
	secret, err := crypto.OpenString(blob, oldKey)
	if err != nil {
		return 0, err
	}
	resealed, err := crypto.SealString(secret, newKey)
	if err != nil {
		return 0, err
	}
	if err := s.store.SetTOTPSecret(u.ID, hex.EncodeToString(resealed)); err != nil {
		return 0, err
	}
	return 1, nil
}
