package api

import (
	"net/http"
	"strings"

	"dockback/internal/store"
)

// Signed-in device inventory (F202).
//
// "Sign out everywhere else" already existed, and it was all the operator had:
// a single button that ends every other session without ever showing what those
// sessions were. That is the wrong shape for the question actually being asked.
// Somebody who suspects one session is not theirs does not want to sign out
// their phone, their other laptop and the tab they left at work — they want to
// look at the list, recognise four of the five, and end the fifth.
//
// So each session now records where it signed in from and what browser it is,
// and can be ended on its own.
//
// ON NOT PUTTING THE SESSION TOKEN IN THE LIST OR THE URL
//
// The session token IS the credential: whoever holds it is signed in as that
// user. It must therefore never appear anywhere designed to be readable — and a
// URL path is exactly that, landing in browser history, access logs, proxy logs
// and Referer headers. Returning tokens in the list body is no better: one
// copied HAR file, one exported devtools trace, and every live session goes with
// it.
//
// Each session is identified instead by sessionPublicID — a truncated SHA-256 of
// the token. It is stable (the same session always has the same id), unique in
// practice, and reveals nothing: it cannot be turned back into a working
// credential. Revocation takes that id in a POST BODY, not a path.

const (
	// maxUserAgentLen bounds what is stored from a client-supplied header.
	maxUserAgentLen = 200

	// sessionPublicIDLen is how much of the hash identifies a session. 16 hex
	// characters is 64 bits — far beyond collision range for the handful of
	// sessions one account holds, while remaining short enough to read.
	sessionPublicIDLen = 16
)

// sessionPublicID derives a non-secret, stable identifier from a session token.
func sessionPublicID(sessionKey string) string {
	if len(sessionKey) < sessionPublicIDLen {
		return sessionKey
	}
	return sessionKey[:sessionPublicIDLen]
}

// sessionDevice is one signed-in session as the API reports it. There is
// deliberately no field for the token.
type sessionDevice struct {
	ID        string `json:"id"`
	Current   bool   `json:"current"`
	CreatedAt int64  `json:"created_at"`
	LastSeen  int64  `json:"last_seen"`
	ExpiresAt int64  `json:"expires_at"`
	IP        string `json:"ip"`
	UserAgent string `json:"user_agent"`
	// Device is the short human rendering ("Firefox on Linux"). Derived here so
	// the rule is testable in one place rather than reimplemented in the client,
	// and sent ALONGSIDE the raw value rather than instead of it — the operator
	// gets a readable row and can still see exactly what the browser sent.
	Device string `json:"device"`
}

// handleListSessions returns the caller's own live sessions (F202).
//
// Scoped to the calling user's id by construction — the list is built from
// ListUserSessions(uid), so there is no code path by which one account can
// enumerate another's devices, and no parameter that could be tampered with to
// ask for one.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	cur := readCookie(r, sessionCookie)
	if cur == "" {
		errJSON(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	uid, _, err := s.store.SessionUser(cur)
	if err != nil {
		errJSON(w, http.StatusUnauthorized, "session expired")
		return
	}
	rows, err := s.store.ListUserSessions(uid)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	curID := sessionPublicID(store.SessionKey(cur))
	out := make([]sessionDevice, 0, len(rows))
	for _, d := range rows {
		id := sessionPublicID(d.Key)
		out = append(out, sessionDevice{
			ID:        id,
			Current:   id == curID,
			CreatedAt: d.CreatedAt,
			LastSeen:  d.LastSeen,
			ExpiresAt: d.ExpiresAt,
			IP:        d.IP,
			UserAgent: d.UserAgent,
			Device:    describeUserAgent(d.UserAgent),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

// handleRevokeSession ends ONE session by its public id (F202).
//
// The id arrives in the body rather than the path so it stays out of request
// logs, and the lookup is confined to the caller's own sessions: an id that
// belongs to another account simply is not in the set being searched, so it
// reports "not found" without ever revealing that it exists elsewhere. That is
// the ownership guard and the enumeration defence in one — there is no branch
// where a foreign session is found and then refused, because a foreign session
// is never found at all.
func (s *Server) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		errJSON(w, http.StatusBadRequest, "which session? (id required)")
		return
	}

	cur := readCookie(r, sessionCookie)
	if cur == "" {
		errJSON(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	uid, username, err := s.store.SessionUser(cur)
	if err != nil {
		errJSON(w, http.StatusUnauthorized, "session expired")
		return
	}

	keys, err := s.store.ListUserSessionKeys(uid)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	target := ""
	for _, k := range keys {
		if sessionPublicID(k) == id {
			target = k
			break
		}
	}
	if target == "" {
		errJSON(w, http.StatusNotFound, "no such session — it may have already ended")
		return
	}

	// Ending the CURRENT session through this door would sign the operator out
	// mid-click with no explanation. "Sign out" is its own control and says so.
	if target == store.SessionKey(cur) {
		errJSON(w, http.StatusBadRequest, "that is this device — use Sign out to end this session")
		return
	}

	s.endSessionByKey(target)
	_ = s.store.Audit(username, "session.revoke_one", id, "ended one other signed-in session from "+s.clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"revoked": 1})
}

// endSession deletes a session and everything keyed to it (F202).
//
// The csrf: and stepup: entries are keyed by the token, so a session ended
// without them would leave a live step-up grant behind — a small window in
// which a re-authentication somebody else performed still counts. Kept in one
// place so every revocation path clears the same set.
func (s *Server) endSession(token string) {
	s.endSessionByKey(store.SessionKey(token))
}

// endSessionByKey is endSession for a session read out of the store, which
// yields the stored key and never the cookie value.
func (s *Server) endSessionByKey(key string) {
	_ = s.store.DeleteSessionByKey(key)
	_ = s.store.DeleteSetting(csrfKey(key))
	_ = s.store.DeleteSetting(stepUpKey(key))
}

// describeUserAgent renders a browser string as a short, human phrase.
//
// Deliberately crude: this exists so a row reads "Firefox on Linux" instead of
// 140 characters of version soup, and the full value is still shown on hover.
// Nothing depends on it being right — a string it cannot classify is returned
// as-is rather than guessed at.
func describeUserAgent(ua string) string {
	if strings.TrimSpace(ua) == "" {
		return "Unknown device"
	}
	browser := ""
	switch {
	case strings.Contains(ua, "Firefox/"):
		browser = "Firefox"
	case strings.Contains(ua, "Edg/"):
		browser = "Edge"
	case strings.Contains(ua, "OPR/"), strings.Contains(ua, "Opera"):
		browser = "Opera"
	case strings.Contains(ua, "Chrome/"):
		browser = "Chrome"
	case strings.Contains(ua, "Safari/"):
		browser = "Safari"
	case strings.Contains(ua, "curl/"):
		browser = "curl"
	}
	os := ""
	switch {
	case strings.Contains(ua, "Android"):
		os = "Android"
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"):
		os = "iOS"
	case strings.Contains(ua, "Windows"):
		os = "Windows"
	case strings.Contains(ua, "Mac OS X"), strings.Contains(ua, "Macintosh"):
		os = "macOS"
	case strings.Contains(ua, "Linux"):
		os = "Linux"
	}
	switch {
	case browser != "" && os != "":
		return browser + " on " + os
	case browser != "":
		return browser
	case os != "":
		return os
	}
	return safeLabel(ua, 60)
}
