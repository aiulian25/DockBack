package api

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"dockback/internal/store"
)

// Passkeys: signing in with the device's own lock (fingerprint, face, PIN, or a
// security key) instead of the password and code. A passkey must verify the
// person, so one alone counts as both steps. Only public keys are kept, each
// bound to the host DOCKBACK_PUBLIC_URL names, and every challenge works once,
// within passkeyCeremonyTTL, for the purpose and user it was made for.
const (
	passkeyCeremonyTTL     = 5 * time.Minute
	passkeyCeremonyLimit   = 1000
	passkeysPerUser        = 20
	passkeyNameMaxRunes    = 64
	passkeyDefaultName     = "Passkey"
	passkeyRelyingPartyApp = "DockBack"
	passkeyFailureLabel    = "passkey"

	passkeyPurposeSignIn = "sign-in"
	passkeyPurposeAdd    = "add"

	localhostName = "localhost"
	schemeHTTPS   = "https"
	schemeHTTP    = "http"
)

// Why passkeys are off, in the words the sign-in page and Settings show.
const (
	passkeyReasonUnset    = "Set DOCKBACK_PUBLIC_URL to the address you open DockBack at (https://dockback.example.org, say) to use passkeys."
	passkeyReasonInvalid  = "DOCKBACK_PUBLIC_URL is not an http(s) address, so passkeys are off."
	passkeyReasonAddress  = "Passkeys need DockBack at a name, not an IP address: browsers don't allow them at an IP address."
	passkeyReasonInsecure = "Passkeys need DockBack over https (plain http works only for localhost)."
)

// What the passkey's own password manager calls itself, by the AAGUID it
// reports at registration: a display name only, never trusted for anything.
var passkeyProviders = map[string]string{
	"fbfc3007-154e-4ecc-8c0b-6e020557d7bd": "iCloud Keychain",
	"dd4ec289-e01d-41c9-bb89-70fa845d4bf2": "iCloud Keychain",
	"ea9b8d66-4d01-1d21-3ce4-b6b48cb575d4": "Google Password Manager",
	"adce0002-35bc-c60a-648b-0b25f1f05503": "Chrome on Mac",
	"08987058-cadc-4b81-b6e1-30de50dcbe96": "Windows Hello",
	"9ddd1817-af5a-4672-a2b9-3e3dd95000a9": "Windows Hello",
	"6028b017-b1d4-4c02-b4b3-afcdafc96bb2": "Windows Hello",
	"bada5566-a7aa-401f-bd96-45619a55120d": "1Password",
	"d548826e-79b4-db40-a3d8-11116f7e8349": "Bitwarden",
	"53414d53-554e-4700-0000-000000000000": "Samsung Pass",
	"fdb141b2-5d84-443e-8a35-4698c205a502": "KeePassXC",
	"50726f74-6f6e-5061-7373-50726f746f6e": "Proton Pass",
	"531126d6-e717-415c-9320-3d9aa6981239": "Dashlane",
	"b84e4048-15dc-4dd0-8640-f4f60813c8af": "NordPass",
}

type passkeyCeremony struct {
	purpose string
	userID  int64 // 0 for a sign-in, which doesn't know whose passkey will answer
	session webauthn.SessionData
	expires time.Time
}

// passkeyService holds the relying party and the ceremonies under way.
// ponytail: ceremonies live in memory, so a restart drops a sign-in under way;
// a table if DockBack ever runs as more than one instance.
type passkeyService struct {
	rpID        string
	origin      string
	unavailable string // why passkeys are off; "" when they are on
	webAuthn    *webauthn.WebAuthn

	mu         sync.Mutex
	ceremonies map[string]passkeyCeremony
	now        func() time.Time
}

// passkeyRelyingParty reads the host passkeys are bound to from the public URL,
// or why there is none. Browsers keep passkeys to a site reached by name, over
// https or on localhost.
func passkeyRelyingParty(publicURL string) (rpID, origin, unavailable string) {
	if publicURL == "" {
		return "", "", passkeyReasonUnset
	}
	u, err := url.Parse(publicURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != schemeHTTPS && u.Scheme != schemeHTTP) {
		return "", "", passkeyReasonInvalid
	}
	host := u.Hostname()
	if net.ParseIP(host) != nil {
		return "", "", passkeyReasonAddress
	}
	if u.Scheme == schemeHTTP && host != localhostName {
		return "", "", passkeyReasonInsecure
	}
	return host, u.Scheme + "://" + u.Host, ""
}

func newPasskeyService(publicURL string) *passkeyService {
	p := &passkeyService{ceremonies: map[string]passkeyCeremony{}, now: time.Now}
	p.rpID, p.origin, p.unavailable = passkeyRelyingParty(publicURL)
	if p.unavailable != "" {
		return p
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:                  p.rpID,
		RPDisplayName:         passkeyRelyingPartyApp,
		RPOrigins:             []string{p.origin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			RequireResidentKey: protocol.ResidentKeyRequired(),
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			UserVerification:   protocol.VerificationRequired,
		},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Enforce: true, Timeout: passkeyCeremonyTTL, TimeoutUVD: passkeyCeremonyTTL},
			Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: passkeyCeremonyTTL, TimeoutUVD: passkeyCeremonyTTL},
		},
	})
	if err != nil {
		p.unavailable = passkeyReasonInvalid
		return p
	}
	p.webAuthn = wa
	return p
}

// remember keeps a ceremony and returns the id its answer must name. Expired
// ones go first, and at the cap the one closest to expiry goes too, so a flood
// can only end its own ceremonies, never grow memory.
func (p *passkeyService) remember(purpose string, userID int64, session *webauthn.SessionData) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for id, c := range p.ceremonies {
		if !c.expires.After(now) {
			delete(p.ceremonies, id)
		}
	}
	// ponytail: an O(n) scan for the eviction, bounded by passkeyCeremonyLimit.
	if len(p.ceremonies) >= passkeyCeremonyLimit {
		oldest := ""
		for id, c := range p.ceremonies {
			if oldest == "" || c.expires.Before(p.ceremonies[oldest].expires) {
				oldest = id
			}
		}
		delete(p.ceremonies, oldest)
	}
	id := randToken()
	p.ceremonies[id] = passkeyCeremony{purpose: purpose, userID: userID, session: *session, expires: now.Add(passkeyCeremonyTTL)}
	return id
}

// take ends a ceremony before anything else about its answer is checked, so a
// refused answer ends it too. ok only when it was still open to this answer.
func (p *passkeyService) take(id, purpose string, userID int64) (webauthn.SessionData, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, found := p.ceremonies[id]
	delete(p.ceremonies, id)
	open := found && c.purpose == purpose && c.userID == userID && c.expires.After(p.now())
	return c.session, open
}

// passkeyUser presents a DockBack account to go-webauthn. Its handle is the
// user id, which is what says whose passkey answered.
type passkeyUser struct {
	user        *store.User
	credentials []webauthn.Credential
}

func passkeyHandle(userID int64) []byte                           { return []byte(strconv.FormatInt(userID, 10)) }
func (u *passkeyUser) WebAuthnID() []byte                         { return passkeyHandle(u.user.ID) }
func (u *passkeyUser) WebAuthnName() string                       { return u.user.Username }
func (u *passkeyUser) WebAuthnDisplayName() string                { return u.user.Username }
func (u *passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

// passkeyUserFor loads an account with its passkeys for this host.
func (s *Server) passkeyUserFor(u *store.User) (*passkeyUser, []*store.Passkey, error) {
	rows, err := s.store.ListPasskeys(u.ID)
	if err != nil {
		return nil, nil, err
	}
	pu := &passkeyUser{user: u}
	var here []*store.Passkey
	for _, row := range rows {
		if row.RPID != s.passkeys.rpID {
			continue
		}
		var c webauthn.Credential
		if err := json.Unmarshal([]byte(row.CredentialJSON), &c); err != nil {
			return nil, nil, fmt.Errorf("passkey %s: %w", row.ID, err)
		}
		pu.credentials = append(pu.credentials, c)
		here = append(here, row)
	}
	return pu, here, nil
}

func credentialIDOf(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }

// passkeyName is what a new passkey is called: its password manager's name when
// the AAGUID says, else the browser's own guess, else a plain default.
func passkeyName(aaguid []byte, suggested string) string {
	if provider, ok := passkeyProviders[formatAAGUID(aaguid)]; ok {
		return provider
	}
	if name, ok := cleanPasskeyName(suggested); ok {
		return name
	}
	return passkeyDefaultName
}

func formatAAGUID(b []byte) string {
	const aaguidBytes = 16
	if len(b) != aaguidBytes {
		return ""
	}
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// cleanPasskeyName trims a name and refuses an empty or over-long one, with
// control characters folded to spaces.
func cleanPasskeyName(name string) (string, bool) {
	name = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, name)), " ")
	ok := name != "" && utf8.RuneCountInString(name) <= passkeyNameMaxRunes
	return name, ok
}

// handlePasskeyStatus tells the sign-in page whether to offer a passkey: on at
// all, and anyone here has one. Public, and says nothing about whose.
func (s *Server) handlePasskeyStatus(w http.ResponseWriter, r *http.Request) {
	offered := false
	if s.passkeys.unavailable == "" {
		n, _ := s.store.CountPasskeysForRP(s.passkeys.rpID)
		offered = n > 0
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"usable": s.passkeys.unavailable == "", "offered": offered,
		"reason": s.passkeys.unavailable, "origin": s.passkeys.origin,
	})
}

// passkeysOff answers 409 with the reason when passkeys can't be used here.
func (s *Server) passkeysOff(w http.ResponseWriter) bool {
	if s.passkeys.unavailable == "" {
		return false
	}
	errJSON(w, http.StatusConflict, s.passkeys.unavailable)
	return true
}

// passkeyLockedOut answers 429 while this address is locked out, the same
// persisted lock a password sign-in uses.
func (s *Server) passkeyLockedOut(w http.ResponseWriter, ip string) bool {
	d := s.guard.blocked(ip, "")
	if d <= 0 {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
	_ = s.store.Audit(passkeyFailureLabel, "login.locked", "", "method=passkey ip="+ip)
	errJSON(w, http.StatusTooManyRequests, "too many failed attempts — try again in "+humanShort(d))
	return true
}

// handlePasskeySignInBegin hands out a challenge any passkey made here may
// answer, so no username is asked first.
func (s *Server) handlePasskeySignInBegin(w http.ResponseWriter, r *http.Request) {
	if s.passkeysOff(w) || s.passkeyLockedOut(w, s.clientIP(r)) {
		return
	}
	assertion, session, err := s.passkeys.webAuthn.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "could not start a passkey sign-in")
		return
	}
	ceremony := s.passkeys.remember(passkeyPurposeSignIn, 0, session)
	writeJSON(w, http.StatusOK, map[string]any{"ceremony": ceremony, "options": assertion.Response})
}

type passkeyAnswer struct {
	Ceremony   string          `json:"ceremony"`
	Credential json.RawMessage `json:"credential"`
	Name       string          `json:"name"` // the browser's guess, when adding one
}

// handlePasskeySignIn checks a passkey's answer and signs its owner in. Every
// refusal looks the same to the caller and counts against the address's lockout.
func (s *Server) handlePasskeySignIn(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if s.passkeysOff(w) || s.passkeyLockedOut(w, ip) {
		return
	}
	var req passkeyAnswer
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	u, row, err := s.verifyPasskeySignIn(req)
	if err != nil {
		out := s.guard.recordFail(ip, "")
		_ = s.store.Audit(passkeyFailureLabel, "login.failed", "", "method=passkey ip="+ip)
		s.noteAuthFailure(ip, passkeyFailureLabel, "passkey sign-ins", out)
		errJSON(w, http.StatusUnauthorized, "the passkey was not accepted")
		return
	}
	s.guard.reset(ip, "")
	csrf, err := s.issueSession(w, r, u)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "session error")
		return
	}
	_ = s.store.Audit(u.Username, "login.ok", "", "method=passkey passkey="+safeLabel(row.Name, passkeyNameMaxRunes))
	writeJSON(w, http.StatusOK, map[string]string{"username": u.Username, "csrf": csrf})
}

// errPasskeyRefused is any reason a sign-in answer is not accepted; the caller
// never learns which.
var errPasskeyRefused = errors.New("passkey refused")

// verifyPasskeySignIn returns the account a sign-in answer proves, and records
// the passkey's updated sign counter.
func (s *Server) verifyPasskeySignIn(req passkeyAnswer) (*store.User, *store.Passkey, error) {
	session, open := s.passkeys.take(req.Ceremony, passkeyPurposeSignIn, 0)
	if !open {
		return nil, nil, errPasskeyRefused
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(req.Credential)
	if err != nil {
		return nil, nil, errPasskeyRefused
	}
	var row *store.Passkey
	findOwner := func(rawID, userHandle []byte) (webauthn.User, error) {
		found, err := s.store.PasskeyByCredentialID(s.passkeys.rpID, credentialIDOf(rawID))
		if err != nil {
			return nil, errPasskeyRefused
		}
		owner, err := s.store.GetUserByID(found.UserID)
		if err != nil {
			return nil, errPasskeyRefused
		}
		pu, _, err := s.passkeyUserFor(owner)
		if err != nil {
			return nil, errPasskeyRefused
		}
		row = found
		return pu, nil
	}
	user, credential, err := s.passkeys.webAuthn.ValidatePasskeyLogin(findOwner, session, parsed)
	if err != nil {
		return nil, nil, errPasskeyRefused
	}
	pu := user.(*passkeyUser)
	updated, err := json.Marshal(credential)
	if err == nil {
		_ = s.store.RecordPasskeyUse(row.ID, string(updated), s.passkeys.now().Unix())
	}
	if credential.Authenticator.CloneWarning {
		_ = s.store.Audit(pu.user.Username, "security.passkey.clone_warning", "", "passkey="+safeLabel(row.Name, passkeyNameMaxRunes))
	}
	return pu.user, row, nil
}

// passkeyView is a stored passkey as Settings shows it.
type passkeyView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at"`
	Synced     bool   `json:"synced"`
	OtherHost  string `json:"other_host,omitempty"` // made for another address: browsers won't offer it here
}

func (s *Server) viewOfPasskey(row *store.Passkey) passkeyView {
	v := passkeyView{ID: row.ID, Name: row.Name, CreatedAt: row.CreatedAt, LastUsedAt: row.LastUsedAt, Synced: row.Synced}
	if row.RPID != s.passkeys.rpID {
		v.OtherHost = row.RPID
	}
	return v
}

// handleListPasskeys lists the signed-in account's passkeys, with whether new
// ones can be added here and why not.
func (s *Server) handleListPasskeys(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		errJSON(w, http.StatusUnauthorized, "session expired")
		return
	}
	rows, err := s.store.ListPasskeys(u.ID)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "could not list passkeys")
		return
	}
	views := make([]passkeyView, 0, len(rows))
	for _, row := range rows {
		views = append(views, s.viewOfPasskey(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"passkeys": views, "usable": s.passkeys.unavailable == "",
		"reason": s.passkeys.unavailable, "origin": s.passkeys.origin,
	})
}

// handleAddPasskeyBegin hands out the challenge for a new passkey, after a fresh
// password (and code) check, so a session left open can't plant one.
func (s *Server) handleAddPasskeyBegin(w http.ResponseWriter, r *http.Request) {
	var body stepUpBody
	_ = readJSON(r, &body)
	if s.passkeysOff(w) || !s.requireFreshAuth(w, r, body) {
		return
	}
	u, ok := s.currentUser(r)
	if !ok {
		errJSON(w, http.StatusUnauthorized, "session expired")
		return
	}
	pu, here, err := s.passkeyUserFor(u)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "could not read passkeys")
		return
	}
	if len(here) >= passkeysPerUser {
		errJSON(w, http.StatusConflict, fmt.Sprintf("an account can have at most %d passkeys — remove one first", passkeysPerUser))
		return
	}
	// The same device twice would be a second entry for one passkey: the browser
	// says it has one already instead.
	exclude := make([]protocol.CredentialDescriptor, 0, len(pu.credentials))
	for _, c := range pu.credentials {
		exclude = append(exclude, c.Descriptor())
	}
	creation, session, err := s.passkeys.webAuthn.BeginRegistration(pu, webauthn.WithExclusions(exclude))
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "could not start adding a passkey")
		return
	}
	ceremony := s.passkeys.remember(passkeyPurposeAdd, u.ID, session)
	writeJSON(w, http.StatusOK, map[string]any{"ceremony": ceremony, "options": creation.Response})
}

// handleAddPasskey keeps the passkey the browser made for the ceremony this
// account started.
func (s *Server) handleAddPasskey(w http.ResponseWriter, r *http.Request) {
	if s.passkeysOff(w) {
		return
	}
	u, ok := s.currentUser(r)
	if !ok {
		errJSON(w, http.StatusUnauthorized, "session expired")
		return
	}
	var req passkeyAnswer
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	session, open := s.passkeys.take(req.Ceremony, passkeyPurposeAdd, u.ID)
	if !open {
		errJSON(w, http.StatusBadRequest, "this passkey request expired or was already used — start again")
		return
	}
	pu, _, err := s.passkeyUserFor(u)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "could not read passkeys")
		return
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(req.Credential)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "the passkey was not added")
		return
	}
	credential, err := s.passkeys.webAuthn.CreateCredential(pu, session, parsed)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "the passkey was not added")
		return
	}
	stored, err := json.Marshal(credential)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "could not store the passkey")
		return
	}
	row := &store.Passkey{
		ID: randToken(), UserID: u.ID, RPID: s.passkeys.rpID,
		CredentialID: credentialIDOf(credential.ID), CredentialJSON: string(stored),
		Name:      passkeyName(credential.Authenticator.AAGUID, req.Name),
		Synced:    credential.Flags.BackupState,
		CreatedAt: s.passkeys.now().Unix(),
	}
	if err := s.store.CreatePasskey(row); err != nil {
		errJSON(w, http.StatusConflict, "this passkey is already added")
		return
	}
	_ = s.store.Audit(u.Username, "account.passkey.add", "", "passkey="+safeLabel(row.Name, passkeyNameMaxRunes))
	writeJSON(w, http.StatusOK, s.viewOfPasskey(row))
}

// handleRenamePasskey renames one of the signed-in account's passkeys.
func (s *Server) handleRenamePasskey(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		errJSON(w, http.StatusUnauthorized, "session expired")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	name, valid := cleanPasskeyName(req.Name)
	if !valid {
		errJSON(w, http.StatusBadRequest, fmt.Sprintf("a passkey name is 1 to %d characters", passkeyNameMaxRunes))
		return
	}
	if err := s.store.RenamePasskey(u.ID, r.PathValue("id"), name); err != nil {
		passkeyWriteFailed(w, err)
		return
	}
	_ = s.store.Audit(u.Username, "account.passkey.rename", "", "passkey="+safeLabel(name, passkeyNameMaxRunes))
	writeJSON(w, http.StatusOK, map[string]string{"status": "renamed", "name": name})
}

// handleRemovePasskey removes one of the signed-in account's passkeys. The
// password still signs in, so even the last one needs no extra check.
func (s *Server) handleRemovePasskey(w http.ResponseWriter, r *http.Request) {
	u, ok := s.currentUser(r)
	if !ok {
		errJSON(w, http.StatusUnauthorized, "session expired")
		return
	}
	if err := s.store.DeletePasskey(u.ID, r.PathValue("id")); err != nil {
		passkeyWriteFailed(w, err)
		return
	}
	_ = s.store.Audit(u.Username, "account.passkey.remove", "", "id="+safeLabel(r.PathValue("id"), passkeyNameMaxRunes))
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

func passkeyWriteFailed(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "passkey not found")
		return
	}
	errJSON(w, http.StatusInternalServerError, "could not update the passkey")
}
