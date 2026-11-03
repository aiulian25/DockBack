package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"

	"dockback/internal/crypto"
)

const (
	passkeyTestURL      = "https://dockback.test"
	passkeyTestPassword = "correct-horse-battery"
	passkeyTestSession  = "sess1"
	onePasswordAAGUID   = "bada5566-a7aa-401f-bd96-45619a55120d"
)

// WebAuthn's authenticator-data flags.
const (
	flagUserPresent    = 0x01
	flagUserVerified   = 0x04
	flagBackupEligible = 0x08
	flagBackedUp       = 0x10
	flagAttestedData   = 0x40
)

var b64 = base64.RawURLEncoding

// softwarePasskey is a passkey kept in memory, answering as a phone or a
// password manager would: an ES256 key, attestation "none".
type softwarePasskey struct {
	key          *ecdsa.PrivateKey
	id           []byte
	counter      uint32
	userHandle   string
	origin       string // where it believes it is
	userVerified bool   // whether it checked the person
	synced       bool
	aaguid       []byte
}

func newSoftwarePasskey(t *testing.T) *softwarePasskey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	aaguid, _ := hex.DecodeString(strings.ReplaceAll(onePasswordAAGUID, "-", ""))
	return &softwarePasskey{key: key, id: id, origin: passkeyTestURL, userVerified: true, synced: true, aaguid: aaguid}
}

func (p *softwarePasskey) header(t *testing.T, rpID string, extra byte) []byte {
	t.Helper()
	flags := byte(flagUserPresent) | extra
	if p.userVerified {
		flags |= flagUserVerified
	}
	if p.synced {
		flags |= flagBackupEligible | flagBackedUp
	}
	rpHash := sha256.Sum256([]byte(rpID))
	counter := make([]byte, 4)
	binary.BigEndian.PutUint32(counter, p.counter)
	return append(append(rpHash[:], flags), counter...)
}

func (p *softwarePasskey) clientData(t *testing.T, kind, challenge string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"type": kind, "challenge": challenge, "origin": p.origin, "crossOrigin": false})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// register answers a creation challenge the way a browser hands it back.
func (p *softwarePasskey) register(t *testing.T, options map[string]any) json.RawMessage {
	t.Helper()
	rp := options["rp"].(map[string]any)
	p.userHandle = options["user"].(map[string]any)["id"].(string)
	point, err := p.key.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	const coordinate = 32
	cose, err := webauthncbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: point[1 : 1+coordinate], -3: point[1+coordinate:]})
	if err != nil {
		t.Fatal(err)
	}
	length := make([]byte, 2)
	binary.BigEndian.PutUint16(length, uint16(len(p.id)))
	authData := append(p.header(t, rp["id"].(string), flagAttestedData), p.aaguid...)
	authData = append(append(append(authData, length...), p.id...), cose...)
	attestation, err := webauthncbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": authData})
	if err != nil {
		t.Fatal(err)
	}
	return p.credential(t, map[string]any{
		"clientDataJSON":    b64.EncodeToString(p.clientData(t, "webauthn.create", options["challenge"].(string))),
		"attestationObject": b64.EncodeToString(attestation),
		"transports":        []string{"internal", "hybrid"},
	})
}

// signIn answers a sign-in challenge.
func (p *softwarePasskey) signIn(t *testing.T, options map[string]any) json.RawMessage {
	t.Helper()
	p.counter++
	authData := p.header(t, options["rpId"].(string), 0)
	clientData := p.clientData(t, "webauthn.get", options["challenge"].(string))
	digest := sha256.Sum256(clientData)
	signed := sha256.Sum256(append(append([]byte{}, authData...), digest[:]...))
	signature, err := ecdsa.SignASN1(rand.Reader, p.key, signed[:])
	if err != nil {
		t.Fatal(err)
	}
	response := map[string]any{
		"clientDataJSON":    b64.EncodeToString(clientData),
		"authenticatorData": b64.EncodeToString(authData),
		"signature":         b64.EncodeToString(signature),
	}
	if p.userHandle != "" {
		response["userHandle"] = p.userHandle
	}
	return p.credential(t, response)
}

func (p *softwarePasskey) credential(t *testing.T, response map[string]any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"id": b64.EncodeToString(p.id), "rawId": b64.EncodeToString(p.id), "type": "public-key",
		"authenticatorAttachment": "platform", "clientExtensionResults": map[string]any{}, "response": response,
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// passkeyServer has one account with a password and a live session, and passkeys
// bound to passkeyTestURL.
func passkeyServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	hash, err := crypto.HashPassword(passkeyTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := s.store.CreateUser("admin", hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.CreateSession(passkeyTestSession, uid, time.Hour); err != nil {
		t.Fatal(err)
	}
	s.passkeys = newPasskeyService(passkeyTestURL)
	return s
}

type passkeyCall struct {
	handler  http.HandlerFunc
	method   string
	body     any
	signedIn bool
	id       string
}

func call(t *testing.T, c passkeyCall) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	body, err := json.Marshal(c.body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(c.method, "/api/passkeys", strings.NewReader(string(body)))
	if c.signedIn {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: passkeyTestSession})
	}
	if c.id != "" {
		r.SetPathValue("id", c.id)
	}
	rec := httptest.NewRecorder()
	c.handler(rec, r)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// addPasskey runs the whole add flow and returns the stored passkey's view.
func addPasskey(t *testing.T, s *Server, p *softwarePasskey) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec, begun := call(t, passkeyCall{handler: s.handleAddPasskeyBegin, method: "POST", signedIn: true, body: map[string]string{"password": passkeyTestPassword}})
	if rec.Code != http.StatusOK {
		t.Fatalf("add begin: %d %v", rec.Code, begun)
	}
	return call(t, passkeyCall{handler: s.handleAddPasskey, method: "POST", signedIn: true, body: map[string]any{
		"ceremony": begun["ceremony"], "credential": p.register(t, begun["options"].(map[string]any)), "name": "Chrome on Linux",
	}})
}

func signInWith(t *testing.T, s *Server, p *softwarePasskey) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec, begun := call(t, passkeyCall{handler: s.handlePasskeySignInBegin, method: "POST"})
	if rec.Code != http.StatusOK {
		t.Fatalf("sign-in begin: %d %v", rec.Code, begun)
	}
	return call(t, passkeyCall{handler: s.handlePasskeySignIn, method: "POST", body: map[string]any{
		"ceremony": begun["ceremony"], "credential": p.signIn(t, begun["options"].(map[string]any)),
	}})
}

func TestPasskeysNeedANameOverHTTPS(t *testing.T) {
	cases := map[string]struct{ rpID, origin, unavailable string }{
		"":                                  {unavailable: passkeyReasonUnset},
		"https://10.0.0.5":                  {unavailable: passkeyReasonAddress},
		"https://[::1]:28734":               {unavailable: passkeyReasonAddress},
		"http://dockback.lan":               {unavailable: passkeyReasonInsecure},
		"ftp://dockback.example.org":        {unavailable: passkeyReasonInvalid},
		"http://localhost:28734":            {rpID: "localhost", origin: "http://localhost:28734"},
		"https://dockback.example.org/app/": {rpID: "dockback.example.org", origin: "https://dockback.example.org"},
	}
	for url, want := range cases {
		rpID, origin, unavailable := passkeyRelyingParty(url)
		if rpID != want.rpID || origin != want.origin || unavailable != want.unavailable {
			t.Errorf("%q: got (%q, %q, %q), want %+v", url, rpID, origin, unavailable, want)
		}
	}
}

// The whole round trip: a passkey can only be added after a fresh password, is
// named after its password manager, and then signs its owner in on its own.
func TestAddingAPasskeyAndSigningInWithIt(t *testing.T) {
	s := passkeyServer(t)
	p := newSoftwarePasskey(t)

	rec, out := call(t, passkeyCall{handler: s.handleAddPasskeyBegin, method: "POST", signedIn: true, body: map[string]string{}})
	if rec.Code != http.StatusUnauthorized || out["step_up_required"] != true {
		t.Fatalf("adding a passkey must need a fresh password: %d %v", rec.Code, out)
	}

	rec, added := addPasskey(t, s, p)
	if rec.Code != http.StatusOK || added["name"] != "1Password" || added["synced"] != true {
		t.Fatalf("add: %d %v", rec.Code, added)
	}
	_, status := call(t, passkeyCall{handler: s.handlePasskeyStatus, method: "GET"})
	if status["usable"] != true || status["offered"] != true {
		t.Fatalf("the sign-in page must offer the passkey now: %v", status)
	}

	rec, signedIn := signInWith(t, s, p)
	if rec.Code != http.StatusOK || signedIn["username"] != "admin" || signedIn["csrf"] == "" {
		t.Fatalf("sign-in: %d %v", rec.Code, signedIn)
	}
	if !strings.Contains(rec.Header().Get("Set-Cookie"), sessionCookie) {
		t.Fatalf("a passkey sign-in must set the session cookie: %v", rec.Header())
	}
	rows, _ := s.store.ListPasskeys(1)
	if len(rows) != 1 || rows[0].LastUsedAt == 0 || !strings.Contains(rows[0].CredentialJSON, `"signCount":1`) {
		t.Fatalf("the use and the sign counter must be recorded: %+v", rows)
	}
	audited := false
	audit, _ := s.store.ListAudit(50)
	for _, a := range audit {
		audited = audited || (a.Action == "login.ok" && strings.Contains(a.Detail, "method=passkey"))
	}
	if !audited {
		t.Error("a passkey sign-in must be audited as one")
	}
}

// A challenge works once, and only for what it was made for.
func TestAPasskeyChallengeWorksOnceForItsPurpose(t *testing.T) {
	s := passkeyServer(t)
	p := newSoftwarePasskey(t)
	if rec, out := addPasskey(t, s, p); rec.Code != http.StatusOK {
		t.Fatalf("add: %d %v", rec.Code, out)
	}

	_, begun := call(t, passkeyCall{handler: s.handlePasskeySignInBegin, method: "POST"})
	answer := map[string]any{"ceremony": begun["ceremony"], "credential": p.signIn(t, begun["options"].(map[string]any))}
	if rec, _ := call(t, passkeyCall{handler: s.handlePasskeySignIn, method: "POST", body: answer}); rec.Code != http.StatusOK {
		t.Fatalf("first answer: %d", rec.Code)
	}
	if rec, _ := call(t, passkeyCall{handler: s.handlePasskeySignIn, method: "POST", body: answer}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a replayed answer must be refused, got %d", rec.Code)
	}

	_, begun = call(t, passkeyCall{handler: s.handlePasskeySignInBegin, method: "POST"})
	rec, _ := call(t, passkeyCall{handler: s.handleAddPasskey, method: "POST", signedIn: true, body: map[string]any{
		"ceremony": begun["ceremony"], "credential": newSoftwarePasskey(t).register(t, map[string]any{
			"rp": map[string]any{"id": "dockback.test"}, "user": map[string]any{"id": "MQ"}, "challenge": begun["options"].(map[string]any)["challenge"],
		}),
	}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a sign-in challenge must not add a passkey, got %d", rec.Code)
	}
}

// A page elsewhere answering for DockBack, and a passkey that didn't check the
// person, are refused, and the refusals count against the address's lockout.
func TestRefusedPasskeysCountAgainstTheLockout(t *testing.T) {
	s := passkeyServer(t)
	p := newSoftwarePasskey(t)
	if rec, out := addPasskey(t, s, p); rec.Code != http.StatusOK {
		t.Fatalf("add: %d %v", rec.Code, out)
	}

	p.origin = "https://dockback.test.evil.example"
	if rec, _ := signInWith(t, s, p); rec.Code != http.StatusUnauthorized {
		t.Fatalf("another origin must be refused, got %d", rec.Code)
	}
	p.origin, p.userVerified = passkeyTestURL, false
	if rec, _ := signInWith(t, s, p); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a passkey that didn't verify the person must be refused, got %d", rec.Code)
	}
	ip := remoteHost(httptest.NewRequest("POST", "/", nil))
	if fails := s.guard.load(lockKey("ip", ip)).Fails; fails != 2 {
		t.Fatalf("each refusal must count against the address, fails=%d", fails)
	}

	for range ipLockPolicy.max {
		s.guard.recordFail(ip, "")
	}
	if rec, _ := call(t, passkeyCall{handler: s.handlePasskeySignInBegin, method: "POST"}); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a locked-out address must not get a challenge, got %d", rec.Code)
	}
}

// A passkey that didn't check the person can't be added either.
func TestAPasskeyMustVerifyThePersonToBeAdded(t *testing.T) {
	s := passkeyServer(t)
	p := newSoftwarePasskey(t)
	p.userVerified = false
	if rec, _ := addPasskey(t, s, p); rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want the passkey refused", rec.Code)
	}
}

// Renaming and removing reach only the account's own passkeys, and a removed
// passkey signs no one in.
func TestRemovingAPasskeyEndsItsSignIns(t *testing.T) {
	s := passkeyServer(t)
	p := newSoftwarePasskey(t)
	_, added := addPasskey(t, s, p)
	id := added["id"].(string)

	if rec, out := call(t, passkeyCall{handler: s.handleRenamePasskey, method: "PATCH", signedIn: true, id: id, body: map[string]string{"name": "  Work\nlaptop "}}); rec.Code != http.StatusOK || out["name"] != "Work laptop" {
		t.Fatalf("rename: %d %v", rec.Code, out)
	}
	if rec, _ := call(t, passkeyCall{handler: s.handleRenamePasskey, method: "PATCH", signedIn: true, id: id, body: map[string]string{"name": strings.Repeat("x", passkeyNameMaxRunes+1)}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("an over-long name must be refused, got %d", rec.Code)
	}
	if rec, _ := call(t, passkeyCall{handler: s.handleRemovePasskey, method: "DELETE", signedIn: true, id: "not-mine"}); rec.Code != http.StatusNotFound {
		t.Fatalf("an unknown passkey: got %d", rec.Code)
	}
	if rec, _ := call(t, passkeyCall{handler: s.handleRemovePasskey, method: "DELETE", signedIn: true, id: id}); rec.Code != http.StatusOK {
		t.Fatalf("remove: %d", rec.Code)
	}
	if rec, _ := signInWith(t, s, p); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a removed passkey must not sign in, got %d", rec.Code)
	}
}

// With no usable public address, nothing is offered and nothing starts.
func TestPasskeysAreOffWithoutAPublicName(t *testing.T) {
	s := passkeyServer(t)
	s.passkeys = newPasskeyService("https://10.0.0.5")
	_, status := call(t, passkeyCall{handler: s.handlePasskeyStatus, method: "GET"})
	if status["usable"] != false || status["reason"] != passkeyReasonAddress {
		t.Fatalf("status: %v", status)
	}
	if rec, _ := call(t, passkeyCall{handler: s.handlePasskeySignInBegin, method: "POST"}); rec.Code != http.StatusConflict {
		t.Fatalf("sign-in begin: %d", rec.Code)
	}
	if rec, _ := call(t, passkeyCall{handler: s.handleAddPasskeyBegin, method: "POST", signedIn: true, body: map[string]string{"password": passkeyTestPassword}}); rec.Code != http.StatusConflict {
		t.Fatalf("add begin: %d", rec.Code)
	}
}

// A flood of sign-in challenges can't grow memory past the cap.
func TestPasskeyCeremoniesStayBounded(t *testing.T) {
	p := newPasskeyService(passkeyTestURL)
	for range passkeyCeremonyLimit + 50 {
		_, session, err := p.webAuthn.BeginDiscoverableLogin()
		if err != nil {
			t.Fatal(err)
		}
		p.remember(passkeyPurposeSignIn, 0, session)
	}
	if n := len(p.ceremonies); n != passkeyCeremonyLimit {
		t.Fatalf("%d ceremonies kept, want at most %d", n, passkeyCeremonyLimit)
	}
}
