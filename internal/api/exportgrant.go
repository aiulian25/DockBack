package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Step-up gated exports (F199).
//
// The asymmetry this closes was the sharpest one in the app. Revealing the
// master key demands a fresh password and a TOTP code (handleKeyReveal, F64) —
// while downloading a DECRYPTED archive, which is strictly more secret material
// than the key that opens it, needed nothing but a session cookie. A browser
// left open on an unlocked laptop could export every credential, database and
// private file DockBack has ever captured, and leave no record it happened.
//
// The obstacle is mechanical: a download is a browser NAVIGATION, so it is a GET
// with no request body and no CSRF header — there is nowhere to put a password.
// Hence a two-step exchange, which is the same shape as the runbook share links
// already in the app (F59):
//
//	POST /api/backups/{id}/export-grant   {password, code}  -> {ticket}
//	GET  /api/backups/{id}/download?ticket=…                -> the archive
//
// The POST carries the credentials and goes through requireFreshAuth; the GET
// carries a one-shot ticket. What makes the ticket safe to put in a URL — where
// it lands in browser history and possibly a proxy log — is that it is worth
// nothing after the first use, expires in two minutes, is stored only as a hash,
// and is bound to the session that asked for it.

const (
	// exportTicketTTL is short on purpose: it needs to survive the round trip
	// from "password accepted" to "browser starts the download", nothing more.
	exportTicketTTL = 2 * time.Minute

	// Purposes. A ticket is bound to one, so a whole-archive grant cannot be
	// replayed against the single-file extract or the control-plane export —
	// three very different amounts of data behind one confirmation.
	exportPurposeDownload  = "download"
	exportPurposeExtract   = "extract"
	exportPurposeAppBackup = "app-backup"
)

// exportTicketKey namespaces a ticket in the settings table, BY HASH.
//
// Only the SHA-256 of the ticket is stored, exactly as api_tokens does with
// token values: whoever can read the database still cannot mint a working
// download URL from what they find there.
func exportTicketKey(ticket string) string { return "export_ticket:" + sha256Hex(ticket) }

// exportGrant is the stored ticket record: what it authorises, for whom, and
// until when. Packed as a single delimited string because the settings table is
// a flat key/value store and this is one short, internal, never-user-supplied
// value — the same shape the lockout records use.
type exportGrant struct {
	Purpose   string
	Target    string // backup id, or the app-backup filename
	SessionFP string // hash of the session that requested it
	Expires   int64
}

func (g exportGrant) encode() string {
	return strings.Join([]string{g.Purpose, g.Target, g.SessionFP, strconv.FormatInt(g.Expires, 10)}, "|")
}

func decodeExportGrant(v string) (exportGrant, bool) {
	parts := strings.Split(v, "|")
	if len(parts) != 4 {
		return exportGrant{}, false
	}
	exp, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return exportGrant{}, false
	}
	return exportGrant{Purpose: parts[0], Target: parts[1], SessionFP: parts[2], Expires: exp}, true
}

// sessionFP identifies the caller's session without storing the session token.
func sessionFP(r *http.Request) string {
	tok := readCookie(r, sessionCookie)
	if tok == "" {
		return ""
	}
	return sha256Hex(tok)
}

// handleExportGrant re-authenticates the operator and issues a one-shot ticket
// for a decrypted export (F199).
//
// requireFreshAuth does the work and writes its own 401 envelope with
// step_up_required, so the browser's existing StepUpPrompt appears with no new
// client-side error handling. A recent step-up for any other protected action
// satisfies this too — one confirmation covers a few minutes of work, which is
// what keeps this from becoming a password prompt per file.
func (s *Server) handleExportGrant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		stepUpBody
		Purpose string `json:"purpose"`
	}
	_ = readJSON(r, &req) // body optional — absent credentials just mean "no step-up yet"

	purpose := strings.TrimSpace(req.Purpose)
	switch purpose {
	case exportPurposeDownload, exportPurposeExtract:
	case "":
		purpose = exportPurposeDownload
	default:
		errJSON(w, http.StatusBadRequest, "unknown export purpose")
		return
	}

	id := r.PathValue("id")
	if _, err := s.store.GetBackup(id); err != nil {
		// Checked BEFORE the password prompt: asking someone to re-authenticate
		// for a backup that does not exist wastes their time and tells an
		// attacker nothing either way.
		errJSON(w, http.StatusNotFound, "backup not found")
		return
	}

	if !s.requireFreshAuth(w, r, req.stepUpBody) {
		return
	}

	ticket := randToken()
	g := exportGrant{
		Purpose:   purpose,
		Target:    id,
		SessionFP: sessionFP(r),
		Expires:   time.Now().Add(exportTicketTTL).Unix(),
	}
	if err := s.store.SetSetting(exportTicketKey(ticket), g.encode()); err != nil {
		errJSON(w, http.StatusInternalServerError, "could not issue an export ticket")
		return
	}
	_ = s.store.Audit(userFrom(r), "export.grant", id,
		fmt.Sprintf("purpose=%s; one-shot ticket valid %s; step-up ok", purpose, humanShort(exportTicketTTL)))

	writeJSON(w, http.StatusOK, map[string]any{
		"ticket":     ticket,
		"expires_in": int(exportTicketTTL.Seconds()),
	})
}

// handleAppBackupExportGrant is the same exchange for the control-plane archive,
// which has no backup id in its path.
func (s *Server) handleAppBackupExportGrant(w http.ResponseWriter, r *http.Request) {
	var req stepUpBody
	_ = readJSON(r, &req)
	if !s.requireFreshAuth(w, r, req) {
		return
	}
	ticket := randToken()
	g := exportGrant{
		Purpose:   exportPurposeAppBackup,
		Target:    exportPurposeAppBackup, // one target: there is only this export
		SessionFP: sessionFP(r),
		Expires:   time.Now().Add(exportTicketTTL).Unix(),
	}
	if err := s.store.SetSetting(exportTicketKey(ticket), g.encode()); err != nil {
		errJSON(w, http.StatusInternalServerError, "could not issue an export ticket")
		return
	}
	_ = s.store.Audit(userFrom(r), "export.grant", "app-backup",
		fmt.Sprintf("purpose=%s; one-shot ticket valid %s; step-up ok", exportPurposeAppBackup, humanShort(exportTicketTTL)))
	writeJSON(w, http.StatusOK, map[string]any{
		"ticket":     ticket,
		"expires_in": int(exportTicketTTL.Seconds()),
	})
}

// requireExportTicket redeems the ticket on an export request, or refuses the
// request outright (F199).
//
// Every failure is a 403 with the same wording. The distinctions an attacker
// might want — does this backup exist, was the ticket real but expired, was it
// minted for a different file — are exactly the ones not worth handing over,
// and the operator never sees this text anyway: the UI mints a ticket and uses
// it immediately, so reaching here means something is wrong or someone is
// probing.
//
// Consumed BEFORE any decryption starts. A ticket that fails validation is
// already spent by then, so a wrong guess cannot be retried against the same
// ticket.
func (s *Server) requireExportTicket(w http.ResponseWriter, r *http.Request, purpose, target string) bool {
	const refusal = "this export needs re-authentication — start the download again from the DockBack interface"

	ticket := strings.TrimSpace(r.URL.Query().Get("ticket"))
	if ticket == "" {
		errJSON(w, http.StatusForbidden, refusal)
		return false
	}
	raw, found, err := s.store.ConsumeSetting(exportTicketKey(ticket))
	if err != nil || !found {
		errJSON(w, http.StatusForbidden, refusal)
		return false
	}
	g, ok := decodeExportGrant(raw)
	if !ok || g.Purpose != purpose || g.Target != target {
		errJSON(w, http.StatusForbidden, refusal)
		return false
	}
	if time.Now().Unix() > g.Expires {
		errJSON(w, http.StatusForbidden, refusal)
		return false
	}
	// Bound to the session that asked for it: a ticket recovered from browser
	// history on a shared machine is useless to anyone else.
	if g.SessionFP != "" && g.SessionFP != sessionFP(r) {
		_ = s.store.Audit(userFrom(r), "export.ticket_rejected", target, "ticket presented by a different session")
		errJSON(w, http.StatusForbidden, refusal)
		return false
	}
	return true
}

// pruneExportTickets deletes expired tickets that were never redeemed (F199).
//
// A ticket is worthless once expired — requireExportTicket checks the deadline —
// so this is hygiene rather than security: without it, every abandoned download
// leaves a row in the settings table forever.
func (s *Server) pruneExportTickets() {
	keys, err := s.store.SettingKeysWithPrefix("export_ticket:")
	if err != nil {
		return
	}
	now := time.Now().Unix()
	for _, k := range keys {
		v, _ := s.store.GetSetting(k, "")
		if g, ok := decodeExportGrant(v); !ok || now > g.Expires {
			_ = s.store.DeleteSetting(k)
		}
	}
}
