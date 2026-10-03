package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"dockback/internal/crypto"
	"dockback/internal/notify"
)

const notifyConfigKey = "notify.config.enc"

// notifyInfoDismissedKey persists whether the user dismissed the "set up
// notifications" nudge shown on the backup page (PLAN §4.10 UX).
const notifyInfoDismissedKey = "notify.info_dismissed"

// notifyConfigured reports whether at least one failure/verification alert
// channel is enabled. Heartbeat is a dead-man's-switch (a liveness ping), not a
// failure notifier, so it deliberately doesn't count here.
func notifyConfigured(c notify.Config) bool {
	return c.Gotify.Enabled || c.Email.Enabled || c.Webhook.Enabled
}

// loadNotifyConfig reads and decrypts the notification config (secrets sealed
// with the master key, PLAN §3.8). Returns a zero config when unset.
func (s *Server) loadNotifyConfig() (notify.Config, error) {
	var c notify.Config
	enc, _ := s.store.GetSetting(notifyConfigKey, "")
	if enc == "" {
		return c, nil
	}
	plain, err := crypto.OpenString([]byte(enc), s.cfg.EncryptionKey)
	if err != nil {
		return c, err
	}
	_ = json.Unmarshal([]byte(plain), &c)
	return c, nil
}

// saveNotifyConfig seals the config with the master key and persists it.
func (s *Server) saveNotifyConfig(c notify.Config) error {
	b, _ := json.Marshal(c)
	sealed, err := crypto.SealString(string(b), s.cfg.EncryptionKey)
	if err != nil {
		return err
	}
	return s.store.SetSetting(notifyConfigKey, string(sealed))
}

// handleGetNotify returns the config with secrets MASKED (never sends the gotify
// token or SMTP password back); `*_set` flags tell the UI a secret is stored.
func (s *Server) handleGetNotify(w http.ResponseWriter, r *http.Request) {
	c, err := s.loadNotifyConfig()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "could not read notification config")
		return
	}
	tokenSet := c.Gotify.Token != ""
	pwSet := c.Email.Password != ""
	c.Gotify.Token = ""
	c.Email.Password = ""
	writeJSON(w, http.StatusOK, map[string]any{
		"config":             c,
		"gotify_token_set":   tokenSet,
		"email_password_set": pwSet,
	})
}

// handleSetNotify saves the config, keeping any existing secret when the
// submitted secret field is blank (so the UI never has to re-enter it).
func (s *Server) handleSetNotify(w http.ResponseWriter, r *http.Request) {
	var in notify.Config
	if err := readJSON(r, &in); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	// Trim before the blank-means-keep check: a field holding only whitespace is
	// the user clearing it or a stray paste, not a new secret — and a token with a
	// trailing space or newline is the commonest cause of an authentication
	// failure that looks exactly like a wrong token.
	in.Gotify.URL = strings.TrimSpace(in.Gotify.URL)
	in.Gotify.Token = strings.TrimSpace(in.Gotify.Token)
	in.Email.Password = strings.TrimSpace(in.Email.Password)

	cur, _ := s.loadNotifyConfig()
	if in.Gotify.Token == "" {
		in.Gotify.Token = cur.Gotify.Token
	}
	if in.Email.Password == "" {
		in.Email.Password = cur.Email.Password
	}
	if err := s.saveNotifyConfig(in); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// If the user now has at least one channel, clear any prior dismissal of the
	// "set up notifications" nudge, so it returns if they later remove all
	// channels (PLAN §4.10 UX).
	if notifyConfigured(in) {
		_ = s.store.SetSetting(notifyInfoDismissedKey, "false")
	}
	_ = s.store.Audit(userFrom(r), "notify.update", "", "")
	s.handleGetNotify(w, r)
}

// handleNotifyHint tells the backup UI whether to show the "no notifications set
// up" nudge: shown only when no alert channel is configured AND the user hasn't
// dismissed it. Returns booleans only (no config/secrets).
func (s *Server) handleNotifyHint(w http.ResponseWriter, r *http.Request) {
	c, _ := s.loadNotifyConfig()
	configured := notifyConfigured(c)
	dismissed, _ := s.store.GetSetting(notifyInfoDismissedKey, "false")
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": configured,
		"show":       !configured && dismissed != "true",
	})
}

// handleDismissNotifyHint hides the nudge. It's re-shown automatically if the
// user later adds a channel and then removes all of them (handleSetNotify clears
// this flag whenever a channel is enabled).
func (s *Server) handleDismissNotifyHint(w http.ResponseWriter, r *http.Request) {
	_ = s.store.SetSetting(notifyInfoDismissedKey, "true")
	_ = s.store.Audit(userFrom(r), "notify.hint.dismiss", "", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleTestNotify sends a test notification to one channel using the SAVED
// secrets merged with the submitted (non-secret) fields, so "Send test" works
// without re-typing the token/password.
func (s *Server) handleTestNotify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channel string        `json:"channel"`
		Config  notify.Config `json:"config"`
	}
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	req.Config.Gotify.URL = strings.TrimSpace(req.Config.Gotify.URL)
	req.Config.Gotify.Token = strings.TrimSpace(req.Config.Gotify.Token)
	req.Config.Email.Password = strings.TrimSpace(req.Config.Email.Password)

	cur, _ := s.loadNotifyConfig()
	if req.Config.Gotify.Token == "" {
		req.Config.Gotify.Token = cur.Gotify.Token
	}
	if req.Config.Email.Password == "" {
		req.Config.Email.Password = cur.Email.Password
	}
	if err := s.notifier.TestChannel(req.Config, req.Channel); err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}
