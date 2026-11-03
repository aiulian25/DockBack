package api

import (
	"cmp"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
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

// Secrets a save can remove outright, since a blank field means "keep" for all of them.
const (
	clearWebhookURL   = "webhook_url"
	clearHeartbeatURL = "heartbeat_url"
)

// notifySaveRequest is the config plus any stored URLs to remove.
type notifySaveRequest struct {
	notify.Config
	ClearSecrets []string `json:"clear_secrets,omitempty"`
}

// urlHost is the part of a stored URL that is safe to show, so an operator can tell
// what is configured. Everything else may be a credential. Pure.
func urlHost(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Host
}

// keepStoredSecrets fills every blank secret in c from what is stored, trimming first:
// a field holding only whitespace is the user clearing it or a stray paste, not a new
// secret, and a token with a trailing space or newline is the commonest cause of an
// authentication failure that looks exactly like a wrong token. Pure.
func keepStoredSecrets(c *notify.Config, stored notify.Config) {
	c.Gotify.URL = strings.TrimSpace(c.Gotify.URL)
	c.Gotify.Token = cmp.Or(strings.TrimSpace(c.Gotify.Token), stored.Gotify.Token)
	c.Email.Password = cmp.Or(strings.TrimSpace(c.Email.Password), stored.Email.Password)
	c.Webhook.URL = cmp.Or(strings.TrimSpace(c.Webhook.URL), stored.Webhook.URL)
	c.Heartbeat.URL = cmp.Or(strings.TrimSpace(c.Heartbeat.URL), stored.Heartbeat.URL)
}

// handleGetNotify returns the config with every secret MASKED: the gotify token, the
// SMTP password, and the webhook and heartbeat URLs, whose paths are bearer
// credentials. Any GET-capable token reaches this. `*_set` flags tell the UI a secret
// is stored, and `*_url_host` says where a URL points.
func (s *Server) handleGetNotify(w http.ResponseWriter, r *http.Request) {
	c, err := s.loadNotifyConfig()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "could not read notification config")
		return
	}
	response := map[string]any{
		"gotify_token_set":   c.Gotify.Token != "",
		"email_password_set": c.Email.Password != "",
		"webhook_url_set":    c.Webhook.URL != "",
		"webhook_url_host":   urlHost(c.Webhook.URL),
		"heartbeat_url_set":  c.Heartbeat.URL != "",
		"heartbeat_url_host": urlHost(c.Heartbeat.URL),
	}
	c.Gotify.Token = ""
	c.Email.Password = ""
	c.Webhook.URL = ""
	c.Heartbeat.URL = ""
	response["config"] = c
	writeJSON(w, http.StatusOK, response)
}

// handleSetNotify saves the config, keeping any existing secret when the submitted
// field is blank (so the UI never has to re-enter it). clear_secrets removes a URL.
func (s *Server) handleSetNotify(w http.ResponseWriter, r *http.Request) {
	var req notifySaveRequest
	if err := readJSON(r, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	in := req.Config
	cur, _ := s.loadNotifyConfig()
	keepStoredSecrets(&in, cur)
	if slices.Contains(req.ClearSecrets, clearWebhookURL) {
		in.Webhook.URL = ""
	}
	if slices.Contains(req.ClearSecrets, clearHeartbeatURL) {
		in.Heartbeat.URL = ""
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
	cur, _ := s.loadNotifyConfig()
	keepStoredSecrets(&req.Config, cur)
	if err := s.notifier.TestChannel(req.Config, req.Channel); err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}
