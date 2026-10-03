package api

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dockback/internal/crypto"
	"dockback/internal/notify"

	"dockback/internal/store"
)

// Tamper-evident audit trail (F68). Every new audit row stores
// chain = HMAC(master, "audit-v1|" ts \x00 actor \x00 action \x00 target \x00 detail \x00 prev_chain)
// so a silent edit/delete/reorder of past rows breaks every later link.
// Pre-feature rows are grandfathered behind the recorded chain-start anchor
// (setting audit.chain_anchor = the newest unchained row's id); a key rotation
// re-anchors, since older links were minted under the old key.
//
// Known limitation (inherent to any self-contained chain): truncating the TAIL
// (deleting the newest rows) is indistinguishable from them never having been
// written. Everything at or before the surviving head stays verifiable.

const auditAnchorKey = "audit.chain_anchor"

// auditChainValue computes one row's chain link — the single definition used by
// BOTH the write path (the installed chainer) and verification, so they can
// never drift. Domain-separated from other manifest-HMAC uses by "audit-v1|"
// (the share.go "runbook-share|" pattern).
func auditChainValue(key []byte, prev string, fields ...string) string {
	return crypto.SignManifest([]byte("audit-v1|"+strings.Join(fields, "\x00")+"\x00"+prev), key)
}

// installAuditChain installs the store's audit chainer and records the
// chain-start anchor on the first run with the feature. Called once from New,
// before anything in the server's lifetime audits. The chainer reads
// s.engine.MasterKey() at call time, so a key rotation switches new links to the new
// key immediately (rotation also re-anchors — see handleKeyRotate).
func (s *Server) installAuditChain() {
	s.store.SetAuditChainer(func(prev string, fields ...string) string {
		return auditChainValue(s.engine.MasterKey(), prev, fields...)
	})
	if v, _ := s.store.GetSetting(auditAnchorKey, ""); v == "" {
		if maxID, err := s.store.MaxAuditID(); err == nil {
			_ = s.store.SetSetting(auditAnchorKey, strconv.FormatInt(maxID, 10))
		}
	}
}

// auditAnchor returns the recorded chain-start anchor id (0 = chain from the
// very first row).
func (s *Server) auditAnchor() int64 {
	v, _ := s.store.GetSetting(auditAnchorKey, "0")
	n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	return n
}

// auditChainVerify walks every row after the anchor recomputing the HMAC chain.
// Returns whether the chain is intact, how many rows were checked, and the id
// of the first broken row (0 when intact).
func (s *Server) auditChainVerify() (ok bool, checked int, firstBad int64, err error) {
	anchor := s.auditAnchor()
	prev, err := s.store.AuditChainAt(anchor)
	if err != nil {
		return false, 0, 0, err
	}
	// A page at a time, chaining as it goes. The trail is append-only and
	// unbounded, so loading it whole meant this check got slower and hungrier for
	// the whole life of the deployment — and failed outright, on the one operation
	// whose job is to tell you whether the record can be trusted.
	key := s.engine.MasterKey()
	broken := int64(0)
	werr := s.store.WalkAuditSince(anchor, func(r *store.AuditChainRow) bool {
		want := auditChainValue(key, prev, strconv.FormatInt(r.TS, 10), r.Actor, r.Action, r.Target, r.Detail)
		if subtle.ConstantTimeCompare([]byte(want), []byte(r.Chain)) != 1 {
			broken = r.ID
			return false // the first broken link is the answer; the rest cannot be judged
		}
		checked++
		prev = r.Chain
		return true
	})
	if werr != nil {
		return false, 0, 0, werr
	}
	if broken != 0 {
		return false, checked, broken, nil
	}
	return true, checked, 0, nil
}

// handleAuditVerify — GET /api/audit/verify (auth-only, read-only; no step-up).
func (s *Server) handleAuditVerify(w http.ResponseWriter, r *http.Request) {
	ok, checked, firstBad, err := s.auditChainVerify()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := map[string]any{"ok": ok, "checked": checked}
	if !ok {
		resp["first_bad_id"] = firstBad
	}
	// F200: what we last published, so the UI can point the operator at the
	// message to check against. Labelled "sent", never "verified" — this came out
	// of the same database the check is about, and proves nothing by itself.
	if id, chain, at, found := s.lastAuditBeacon(); found {
		resp["beacon_sent"] = map[string]any{"head_id": id, "chain": chain, "at": at}
	}
	writeJSON(w, http.StatusOK, resp)
}

// auditVerifySweep is the opportunistic daily background check (F68), run from
// the existing 5-minute sweep goroutine. A broken chain raises a
// KindScrubFailed-band alert (integrity failure severity) once per day.
func (s *Server) auditVerifySweep() {
	ok, checked, firstBad, err := s.auditChainVerify()
	if err != nil {
		s.logSink("security", "WARN", "Audit trail integrity check could not run: "+err.Error())
		return
	}
	if ok {
		s.logSink("security", "INFO", "Audit trail integrity check passed ("+strconv.Itoa(checked)+" chained rows)")
		return
	}
	s.notify(notify.KindScrubFailed, "Audit trail integrity check FAILED",
		"The audit trail's hash chain is broken at entry #"+strconv.FormatInt(firstBad, 10)+
			" — rows were modified, deleted, or reordered after being written. Treat the trail after that entry as untrustworthy and investigate who has file access to the database.")
}

// --- F200: off-host chain-head beacon -------------------------------------
//
// The chain above is tamper-EVIDENT, not tamper-proof, and the distinction
// matters against the threat model this app already takes seriously elsewhere:
// an attacker with a shell in the container. That attacker holds the master key
// (so they can recompute every link), the anchor (so they can move where
// verification starts) and the database itself. They can rewrite the trail end
// to end and the check above will pass. The evidence is defeated by exactly the
// compromise it exists to detect.
//
// No amount of additional cryptography fixes that, because the attacker has the
// key. What fixes it is keeping the answer somewhere they cannot reach: a chain
// head that LEFT the machine — in an inbox, a webhook receiver, a push
// notification — is a checkpoint DockBack itself can no longer alter. Comparing
// today's database against that copy is what turns "tamper-evident if the
// database is honest" into "tamper-evident against a compromise of this host".
//
// Two halves, and the difference between them is the whole feature:
//
//   - beaconAuditHead PUBLISHES the head. That is the part with security value.
//   - auditBeaconLastKey records what was published, for the UI to display.
//     It proves NOTHING on its own — an attacker rewrites that setting exactly
//     as they rewrite the anchor — and the code and the interface both say so,
//     because a green tick that a compromised instance passes is worse than no
//     tick at all.
//
// The check that counts is handleAuditVerifyBeacon, where the operator supplies
// the head from their own external copy.

const (
	auditBeaconLastKey      = "audit.beacon_last" // "<id>|<chain>|<unix ts>" of the last head we published
	auditBeaconEveryKey     = "audit.beacon_hours"
	defaultAuditBeaconHours = 24
)

// auditBeaconHours is the cadence, clamped so a misconfiguration cannot turn the
// beacon into a notification flood or disable it silently.
func (s *Server) auditBeaconHours(cfg notify.HeadBeaconConfig) int {
	h := cfg.IntervalHours
	if h <= 0 {
		h = s.settingInt(auditBeaconEveryKey, defaultAuditBeaconHours)
	}
	if h < 1 {
		h = 1
	}
	if h > 24*7 {
		h = 24 * 7
	}
	return h
}

// auditHead reads the current chain head: the newest audit row's id and its
// stored chain value.
func (s *Server) auditHead() (id int64, chain string, err error) {
	id, err = s.store.MaxAuditID()
	if err != nil || id == 0 {
		return 0, "", err
	}
	chain, err = s.store.AuditChainAt(id)
	return id, chain, err
}

// beaconAuditHead publishes the current chain head to the configured channels.
//
// The FULL chain value is sent, not a truncation. It is an HMAC digest, so
// publishing it reveals neither the key nor anything about the entries it
// covers — and a shortened value would be useless for the one thing this exists
// for: an operator later checking their own copy against the database. A
// checkpoint nobody can verify against is decoration.
func (s *Server) beaconAuditHead(reason string) {
	id, chain, err := s.auditHead()
	if err != nil || id == 0 || chain == "" {
		// No chained rows yet (a fresh install, or every row predates the
		// anchor). Nothing to attest to; say nothing rather than publish a head
		// that means nothing.
		return
	}
	_ = s.store.SetSetting(auditBeaconLastKey,
		fmt.Sprintf("%d|%s|%d", id, chain, time.Now().Unix()))

	s.notify(notify.KindAuditBeacon, "DockBack audit checkpoint",
		auditBeaconMessage(id, chain, time.Now(), reason))
}

// auditBeaconMessage is what the operator keeps. Split out so its contents are
// testable directly: everything that makes the checkpoint usable later — the
// entry number, the WHOLE chain value, and what to do with them — has to be in
// here, because this message is the only copy that survives a compromise of the
// machine that sent it.
func auditBeaconMessage(id int64, chain string, when time.Time, reason string) string {
	return fmt.Sprintf("Audit trail head at entry #%d:\n%s\n\n"+
		"Keep this message. It is a checkpoint of the audit trail as it stood at %s (%s). "+
		"To check the trail later, open Audit Trail → Verify against a checkpoint and paste the entry number and value above. "+
		"If they no longer match, entries at or before #%d were rewritten — something an attacker who reached this machine could do to the database and to its own records, but not to this message.",
		id, chain, when.UTC().Format(time.RFC3339), reason, id)
}

// lastAuditBeacon returns the head we last published, for display.
//
// Read from our own database, so it carries no proof — see the note above. The
// API and the UI both label it as a record of what was SENT.
func (s *Server) lastAuditBeacon() (id int64, chain string, at int64, ok bool) {
	v, _ := s.store.GetSetting(auditBeaconLastKey, "")
	parts := strings.Split(v, "|")
	if len(parts) != 3 {
		return 0, "", 0, false
	}
	id, err1 := strconv.ParseInt(parts[0], 10, 64)
	at, err2 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil || parts[1] == "" {
		return 0, "", 0, false
	}
	return id, parts[1], at, true
}

// startAuditBeacon runs the publication cadence.
func (s *Server) startAuditBeacon() {
	go func() {
		t := time.NewTicker(15 * time.Minute)
		defer t.Stop()
		for range t.C {
			s.auditBeaconTick()
		}
	}()
}

// auditBeaconTick publishes a head when the configured interval has elapsed.
func (s *Server) auditBeaconTick() {
	cfg, err := s.loadNotifyConfig()
	if err != nil || !cfg.HeadBeacon.Enabled {
		return
	}
	_, _, at, ok := s.lastAuditBeacon()
	if ok && time.Since(time.Unix(at, 0)) < time.Duration(s.auditBeaconHours(cfg.HeadBeacon))*time.Hour {
		return
	}
	s.beaconAuditHead("scheduled checkpoint")
}

// beaconVerdict is the outcome of checking the database against a head the
// operator kept outside it.
type beaconVerdict struct {
	OK      bool   `json:"ok"`
	Reason  string `json:"reason"`
	HeadID  int64  `json:"head_id"`
	Current int64  `json:"current_head_id"`
}

// verifyAgainstBeacon compares a supplied checkpoint against the stored trail.
//
// This is the check that survives a container compromise, because the value
// being compared came from outside the container. Three distinct outcomes, and
// they mean different things:
//
//   - the entry is gone            → the trail was truncated past the checkpoint
//   - the entry's link differs     → history at or before it was rewritten
//   - the link matches             → everything up to that point is as attested
//
// A match does NOT vouch for rows added after the checkpoint; the walk in
// auditChainVerify covers those, and the caller runs both.
func (s *Server) verifyAgainstBeacon(id int64, chain string) (beaconVerdict, error) {
	cur, err := s.store.MaxAuditID()
	if err != nil {
		return beaconVerdict{}, err
	}
	v := beaconVerdict{HeadID: id, Current: cur}

	if id <= 0 || strings.TrimSpace(chain) == "" {
		v.Reason = "Enter the entry number and the value from your checkpoint message."
		return v, nil
	}
	if cur < id {
		v.Reason = fmt.Sprintf("The trail now ends at entry #%d, before the checkpoint's #%d — entries after #%d were deleted.", cur, id, cur)
		return v, nil
	}
	stored, err := s.store.AuditChainAt(id)
	if err != nil {
		return beaconVerdict{}, err
	}
	if stored == "" {
		v.Reason = fmt.Sprintf("Entry #%d is no longer in the trail — it was deleted.", id)
		return v, nil
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(strings.TrimSpace(chain))) != 1 {
		v.Reason = fmt.Sprintf("Entry #%d no longer matches the checkpoint — entries at or before it were rewritten after that checkpoint was taken.", id)
		return v, nil
	}
	v.OK = true
	v.Reason = fmt.Sprintf("Entry #%d matches the checkpoint. Everything recorded up to that point is unchanged since it was taken.", id)
	return v, nil
}

// handleAuditVerifyBeacon — POST /api/audit/verify-beacon.
//
// Auth + CSRF, no step-up: this reads nothing and changes nothing, and putting
// a password in front of "check whether you have been tampered with" is the
// wrong kind of friction.
func (s *Server) handleAuditVerifyBeacon(w http.ResponseWriter, r *http.Request) {
	var req struct {
		HeadID int64  `json:"head_id"`
		Chain  string `json:"chain"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	v, err := s.verifyAgainstBeacon(req.HeadID, req.Chain)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Audited either way: someone checking for tampering, and what they found,
	// is exactly the kind of thing worth having a record of afterwards.
	outcome := "matched"
	if !v.OK {
		outcome = "MISMATCH: " + v.Reason
	}
	_ = s.store.Audit(userFrom(r), "audit.beacon_verify", strconv.FormatInt(req.HeadID, 10), outcome)
	writeJSON(w, http.StatusOK, v)
}
