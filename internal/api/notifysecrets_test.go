package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"dockback/internal/notify"
)

const (
	webhookSecret   = "T00000000-B00000000-webhooksecretpart"
	heartbeatSecret = "pushtokenheartbeatsecretpart"
	gotifySecret    = "gotifytokensecretpart"
	smtpSecret      = "smtppasswordsecretpart"
	secretHost      = "hooks.example.test"
	pingHost        = "ping.example.test"
)

func secretsFixture() notify.Config {
	var c notify.Config
	c.Webhook.Enabled = true
	c.Webhook.URL = "https://" + secretHost + "/services/" + webhookSecret
	c.Heartbeat.Enabled = true
	c.Heartbeat.URL = "https://" + pingHost + "/api/push/" + heartbeatSecret
	c.Gotify.Token = gotifySecret
	c.Email.Password = smtpSecret
	return c
}

func notifySecretsServer(t *testing.T) *Server {
	t.Helper()
	s, _, _ := twofaServer(t)
	s.notifier = notify.New(s.loadNotifyConfig, func(string, string) {})
	if err := s.saveNotifyConfig(secretsFixture()); err != nil {
		t.Fatal(err)
	}
	return s
}

func assertNoFixtureSecret(t *testing.T, where, text string) {
	t.Helper()
	for _, secret := range []string{webhookSecret, heartbeatSecret, gotifySecret, smtpSecret} {
		if strings.Contains(text, secret) {
			t.Errorf("%s holds a secret %q: %s", where, secret, text)
		}
	}
}

func notifyRequest(s *Server, handler http.HandlerFunc, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler(rec, r)
	return rec
}

// The finding: this GET is open to read and backup tokens, so what it returns is what
// such a token holds.
func TestNotificationViewsOmitEverySecret(t *testing.T) {
	for _, scope := range []string{"read", "backup"} {
		if !tokenAllows(scope, http.MethodGet, "/api/notifications") {
			t.Fatalf("the premise changed: a %s token cannot read this any more", scope)
		}
	}
	s := notifySecretsServer(t)
	get := notifyRequest(s, s.handleGetNotify, http.MethodGet, "/api/notifications", "")
	if get.Code != http.StatusOK {
		t.Fatalf("%d %s", get.Code, get.Body.String())
	}
	assertNoFixtureSecret(t, "GET", get.Body.String())

	var view struct {
		WebhookSet    bool   `json:"webhook_url_set"`
		WebhookHost   string `json:"webhook_url_host"`
		HeartbeatSet  bool   `json:"heartbeat_url_set"`
		HeartbeatHost string `json:"heartbeat_url_host"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.WebhookSet || view.WebhookHost != secretHost || !view.HeartbeatSet || view.HeartbeatHost != pingHost {
		t.Fatalf("the view must say what is configured: %+v", view)
	}

	put := notifyRequest(s, s.handleSetNotify, http.MethodPut, "/api/notifications", `{"webhook":{"enabled":true}}`)
	assertNoFixtureSecret(t, "PUT", put.Body.String())
}

func storedNotify(t *testing.T, s *Server) notify.Config {
	t.Helper()
	c, err := s.loadNotifyConfig()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Saving what GET returned is what the UI does on every save, so it must lose nothing.
func TestSavingTheMaskedViewKeepsEverySecret(t *testing.T) {
	s := notifySecretsServer(t)
	var view struct {
		Config json.RawMessage `json:"config"`
	}
	get := notifyRequest(s, s.handleGetNotify, http.MethodGet, "/api/notifications", "")
	if err := json.Unmarshal(get.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if rec := notifyRequest(s, s.handleSetNotify, http.MethodPut, "/api/notifications", string(view.Config)); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	got, want := storedNotify(t, s), secretsFixture()
	if got.Webhook.URL != want.Webhook.URL || got.Heartbeat.URL != want.Heartbeat.URL || got.Gotify.Token != gotifySecret || got.Email.Password != smtpSecret {
		t.Fatalf("a secret was lost: %+v", got)
	}
}

func TestANewURLReplacesAndClearSecretsRemoves(t *testing.T) {
	s := notifySecretsServer(t)
	const replacement = "https://other.example.test/new"
	notifyRequest(s, s.handleSetNotify, http.MethodPut, "/api/notifications", `{"webhook":{"url":"  `+replacement+`  "}}`)
	if got := storedNotify(t, s); got.Webhook.URL != replacement || got.Heartbeat.URL == "" {
		t.Fatalf("after a replacement: %+v", got)
	}

	notifyRequest(s, s.handleSetNotify, http.MethodPut, "/api/notifications", `{"clear_secrets":["webhook_url"]}`)
	if got := storedNotify(t, s); got.Webhook.URL != "" || got.Heartbeat.URL == "" {
		t.Fatalf("only the named URL goes: %+v", got)
	}
	notifyRequest(s, s.handleSetNotify, http.MethodPut, "/api/notifications", `{"clear_secrets":["heartbeat_url"]}`)
	rec := notifyRequest(s, s.handleGetNotify, http.MethodGet, "/api/notifications", "")
	if got := storedNotify(t, s); got.Heartbeat.URL != "" {
		t.Fatalf("heartbeat: %+v", got)
	}
	if !strings.Contains(rec.Body.String(), `"heartbeat_url_set":false`) {
		t.Fatalf("the view must show it gone: %s", rec.Body.String())
	}
}

func TestSendTestUsesTheStoredURLWithoutEchoingIt(t *testing.T) {
	s := notifySecretsServer(t)
	var hits atomic.Int32
	var seenPath atomic.Value
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		seenPath.Store(r.URL.Path)
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	defer receiver.Close()
	const storedPath = "/services/" + webhookSecret
	c := secretsFixture()
	c.Webhook.URL = receiver.URL + storedPath
	if err := s.saveNotifyConfig(c); err != nil {
		t.Fatal(err)
	}

	rec := notifyRequest(s, s.handleTestNotify, http.MethodPost, "/api/notifications/test", `{"channel":"webhook","config":{"webhook":{"enabled":true}}}`)
	if rec.Code != http.StatusOK || hits.Load() != 1 || seenPath.Load() != storedPath {
		t.Fatalf("%d, %d hits, path %v: %s", rec.Code, hits.Load(), seenPath.Load(), rec.Body.String())
	}
	assertNoFixtureSecret(t, "test reply", rec.Body.String())
}

// Go's HTTP client writes the full URL into its errors, and those reach both this
// reply and the notification log.
func TestAFailedSendDoesNotEchoTheURL(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	unreachable := closed.URL
	closed.Close()
	cases := map[string]func(*notify.Config){
		"webhook":   func(c *notify.Config) { c.Webhook.URL = unreachable + "/services/" + webhookSecret },
		"heartbeat": func(c *notify.Config) { c.Heartbeat.URL = unreachable + "/api/push/" + heartbeatSecret },
	}
	for channel, point := range cases {
		t.Run(channel, func(t *testing.T) {
			s := notifySecretsServer(t)
			c := secretsFixture()
			point(&c)
			if err := s.saveNotifyConfig(c); err != nil {
				t.Fatal(err)
			}
			rec := notifyRequest(s, s.handleTestNotify, http.MethodPost, "/api/notifications/test", `{"channel":"`+channel+`","config":{}}`)
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			assertNoFixtureSecret(t, "failure reply", rec.Body.String())
			if !strings.Contains(rec.Body.String(), "refused") {
				t.Errorf("the reason must survive: %s", rec.Body.String())
			}
		})
	}
}

func TestUrlHostShowsNothingElse(t *testing.T) {
	cases := map[string]string{
		"https://user:pass@hooks.example.test:8443/path?key=value": "hooks.example.test:8443",
		"":             "",
		"://not a url": "",
	}
	for raw, want := range cases {
		if got := urlHost(raw); got != want {
			t.Errorf("urlHost(%q) = %q, want %q", raw, got, want)
		}
	}
}
