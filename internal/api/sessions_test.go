package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dockback/internal/config"
	"dockback/internal/crypto"
	"dockback/internal/store"
)

// F202 — see the signed-in devices before deciding which one to end.
//
// "Sign out everywhere else" existed and was all there was: one button that ends
// every other session without showing what they are. Somebody who suspects one
// session is not theirs wants to keep the four they recognise.

func sessionServer(t *testing.T) (*Server, int64) {
	t.Helper()
	s := newTestServer(t)
	s.cfg = &config.Config{}
	hash, err := crypto.HashPassword("correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := s.store.CreateUser("admin", hash)
	if err != nil {
		t.Fatal(err)
	}
	return s, uid
}

func listSessions(t *testing.T, s *Server, cookie string) []sessionDevice {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/session/list", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	rec := httptest.NewRecorder()
	s.handleListSessions(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Sessions []sessionDevice `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Sessions
}

func revokeSession(s *Server, cookie, id string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/session/revoke", strings.NewReader(`{"id":"`+id+`"}`))
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	rec := httptest.NewRecorder()
	s.handleRevokeSession(rec, r)
	return rec
}

// AC1 — two sign-ins show as two rows carrying their own address and browser,
// with the caller's own marked.
func TestSessionListShowsEachDevice(t *testing.T) {
	s, uid := sessionServer(t)
	const laptopUA = "Mozilla/5.0 (X11; Linux x86_64) Gecko/20100101 Firefox/153.0"
	const phoneUA = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0) AppleWebKit/605.1 Safari/604.1"

	if err := s.store.CreateSessionFrom("tok-laptop", uid, time.Hour, "10.168.1.10", laptopUA); err != nil {
		t.Fatal(err)
	}
	if err := s.store.CreateSessionFrom("tok-phone", uid, time.Hour, "10.0.0.7", phoneUA); err != nil {
		t.Fatal(err)
	}

	rows := listSessions(t, s, "tok-laptop")
	if len(rows) != 2 {
		t.Fatalf("expected both devices, got %d", len(rows))
	}

	byIP := map[string]sessionDevice{}
	for _, d := range rows {
		byIP[d.IP] = d
	}
	laptop, phone := byIP["10.168.1.10"], byIP["10.0.0.7"]
	if laptop.ID == "" || phone.ID == "" {
		t.Fatalf("both rows need an id: %+v", rows)
	}
	if !laptop.Current {
		t.Error("the caller's own session must be marked as this device")
	}
	if phone.Current {
		t.Error("another device must not be marked as this one")
	}
	// The row has to be recognisable at a glance, and still carry the exact
	// value underneath it.
	if laptop.Device != "Firefox on Linux" || phone.Device != "Safari on iOS" {
		t.Errorf("device labels = %q / %q", laptop.Device, phone.Device)
	}
	if laptop.UserAgent != laptopUA {
		t.Error("the raw user agent must survive alongside the label")
	}

	// The credential itself must never leave the server. Assert on the wire
	// bytes, not the struct — a future field could reintroduce it.
	r := httptest.NewRequest("GET", "/api/session/list", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok-laptop"})
	rec := httptest.NewRecorder()
	s.handleListSessions(rec, r)
	for _, tok := range []string{"tok-laptop", "tok-phone"} {
		if strings.Contains(rec.Body.String(), tok) {
			t.Fatalf("a session token reached the response body (%q) — it is a live credential", tok)
		}
	}
}

// AC2 — revoking one session ends exactly that one; the caller keeps working.
func TestRevokeOneSessionLeavesTheOthers(t *testing.T) {
	s, uid := sessionServer(t)
	_ = s.store.CreateSessionFrom("tok-here", uid, time.Hour, "10.168.1.10", "Firefox/153.0")
	_ = s.store.CreateSessionFrom("tok-there", uid, time.Hour, "10.0.0.7", "Safari/604.1")
	_ = s.store.CreateSessionFrom("tok-third", uid, time.Hour, "10.0.0.9", "Chrome/120.0")
	// Keyed side-entries that must die with the session.
	_ = s.store.SetSetting("csrf:"+store.SessionKey("tok-there"), "csrfB")
	_ = s.store.SetSetting(stepUpKey(store.SessionKey("tok-there")), "1700000000")

	var targetID string
	for _, d := range listSessions(t, s, "tok-here") {
		if d.IP == "10.0.0.7" {
			targetID = d.ID
		}
	}
	if targetID == "" {
		t.Fatal("could not find the session to revoke")
	}

	if rec := revokeSession(s, "tok-here", targetID); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d: %s", rec.Code, rec.Body.String())
	}

	// The revoked one is gone…
	if _, _, err := s.store.SessionUser("tok-there"); err == nil {
		t.Error("the revoked session must no longer authenticate")
	}
	// …along with everything keyed to it. A live step-up grant outliving its
	// session is a window where somebody else's re-authentication still counts.
	if v, _ := s.store.GetSetting("csrf:"+store.SessionKey("tok-there"), ""); v != "" {
		t.Error("the csrf entry must be cleared with the session")
	}
	if v, _ := s.store.GetSetting(stepUpKey(store.SessionKey("tok-there")), ""); v != "" {
		t.Error("the step-up grant must be cleared with the session")
	}

	// …and nothing else was touched.
	for _, keep := range []string{"tok-here", "tok-third"} {
		if _, _, err := s.store.SessionUser(keep); err != nil {
			t.Errorf("%s should still be signed in", keep)
		}
	}
	if len(listSessions(t, s, "tok-here")) != 2 {
		t.Error("the list should now show two devices")
	}
}

// AC3 — the ownership guard. Another account's session is not in the set being
// searched, so it cannot be ended and its existence is never confirmed.
func TestCannotRevokeAnotherUsersSession(t *testing.T) {
	s, uid := sessionServer(t)
	hash, _ := crypto.HashPassword("second-account-password")
	otherUID, err := s.store.CreateUser("other", hash)
	if err != nil {
		t.Fatal(err)
	}
	if otherUID == uid {
		t.Fatal("need two distinct users")
	}
	_ = s.store.CreateSessionFrom("tok-mine", uid, time.Hour, "10.168.1.10", "Firefox/153.0")
	_ = s.store.CreateSessionFrom("tok-theirs", otherUID, time.Hour, "203.0.113.9", "Chrome/120.0")

	rec := revokeSession(s, "tok-mine", sessionPublicID(store.SessionKey("tok-theirs")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("revoking another user's session = %d, want 404", rec.Code)
	}
	// Refused AND unharmed.
	if _, _, err := s.store.SessionUser("tok-theirs"); err != nil {
		t.Error("the other user's session must survive")
	}
	// The list is scoped too — one account cannot enumerate another's devices.
	for _, d := range listSessions(t, s, "tok-mine") {
		if d.IP == "203.0.113.9" {
			t.Error("another user's device must not appear in this list")
		}
	}
}

// Ending the current session through this door would sign the operator out
// mid-click; "Sign out" is its own control and says what it does.
func TestCannotRevokeCurrentSessionHere(t *testing.T) {
	s, uid := sessionServer(t)
	_ = s.store.CreateSessionFrom("tok-here", uid, time.Hour, "10.168.1.10", "Firefox/153.0")

	rec := revokeSession(s, "tok-here", sessionPublicID(store.SessionKey("tok-here")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("revoking this device = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Sign out") {
		t.Errorf("the refusal should point at the right control: %s", rec.Body.String())
	}
	if _, _, err := s.store.SessionUser("tok-here"); err != nil {
		t.Error("the current session must survive the attempt")
	}
}

// The public id must be stable, non-reversible, and never the token. It is a
// prefix of the STORED key, which is already the SHA-256 of the cookie value —
// so the id an operator sees is the same one it always was.
func TestSessionPublicID(t *testing.T) {
	const tok = "a-very-secret-session-token"
	id := sessionPublicID(store.SessionKey(tok))
	if id == "" || id == tok || strings.Contains(tok, id) {
		t.Fatalf("id %q must not be derived visibly from the token", id)
	}
	if len(id) != sessionPublicIDLen {
		t.Errorf("id length = %d, want %d", len(id), sessionPublicIDLen)
	}
	if sessionPublicID(store.SessionKey(tok)) != id {
		t.Error("the same session must always have the same id")
	}
	if sessionPublicID(store.SessionKey("another-token")) == id {
		t.Error("different sessions must have different ids")
	}
	if sessionPublicID("") != "" {
		t.Error("no session, no id")
	}
}

// An expired or idled-out session is not a signed-in device.
func TestExpiredSessionsAreNotListed(t *testing.T) {
	s, uid := sessionServer(t)
	_ = s.store.CreateSessionFrom("tok-live", uid, time.Hour, "10.168.1.10", "Firefox/153.0")
	_ = s.store.CreateSessionFrom("tok-dead", uid, -time.Hour, "10.0.0.7", "Chrome/120.0")

	rows := listSessions(t, s, "tok-live")
	if len(rows) != 1 {
		t.Fatalf("only the live session is a device, got %d", len(rows))
	}
	if rows[0].IP != "10.168.1.10" {
		t.Errorf("wrong session listed: %+v", rows[0])
	}
}

// The label is a convenience, so it degrades to something honest rather than
// guessing.
func TestDescribeUserAgent(t *testing.T) {
	cases := map[string]string{
		"Mozilla/5.0 (X11; Linux x86_64) Gecko/20100101 Firefox/153.0":                  "Firefox on Linux",
		"Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537 Chrome/120.0 Safari/537":         "Chrome on Windows",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15) AppleWebKit/605 Safari/605":      "Safari on macOS",
		"Mozilla/5.0 (Linux; Android 14) AppleWebKit/537 Chrome/120 Mobile Safari/537":  "Chrome on Android",
		"Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537 Chrome/120 Safari/537 Edg/120.0": "Edge on Windows",
		"curl/8.14.1": "curl",
		"":            "Unknown device",
		"   ":         "Unknown device",
	}
	for ua, want := range cases {
		if got := describeUserAgent(ua); got != want {
			t.Errorf("describeUserAgent(%q) = %q, want %q", ua, got, want)
		}
	}
	// Something it cannot classify is passed through, bounded — never guessed at.
	odd := describeUserAgent(strings.Repeat("Z", 500))
	if len([]rune(odd)) > 61 {
		t.Errorf("an unclassifiable agent must still be bounded, got %d runes", len([]rune(odd)))
	}
}
