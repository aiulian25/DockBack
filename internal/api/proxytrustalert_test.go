package api

import (
	"net"
	"strings"
	"testing"

	"dockback/internal/config"
	"dockback/internal/notify"
	"dockback/internal/store"
)

func proxyAlertServer(t *testing.T, cfg *config.Config) *Server {
	t.Helper()
	return &Server{store: testStore(t), cfg: cfg}
}

func alertsOfKind(t *testing.T, s *Server, kind string) []*store.Alert {
	t.Helper()
	rows, err := s.store.ListAlerts(false, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []*store.Alert
	for _, a := range rows {
		if a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

// Trusting X-Forwarded-* from every peer defeats the login lockout, the API
// token source pins and the audit trail's IP column. The startup stderr warning
// is invisible on a NAS, so the condition has to reach the Alerts inbox.
func TestProxyTrustAlertFiresWhileTrustIsSpoofable(t *testing.T) {
	s := proxyAlertServer(t, &config.Config{TrustProxy: true}) // no allow-list
	s.checkProxyTrustAlert()

	rows := alertsOfKind(t, s, notify.KindConfigTrustAllProxies)
	if len(rows) != 1 {
		t.Fatalf("want one alert about forgeable client IPs, got %d", len(rows))
	}
	if rows[0].Severity != notify.SevCritical.String() {
		t.Errorf("severity = %q, want critical — this removes a protection rather than degrading one", rows[0].Severity)
	}
	if !strings.Contains(rows[0].Message, "DOCKBACK_TRUSTED_PROXIES") {
		t.Errorf("the alert must name the setting that fixes it: %q", rows[0].Message)
	}
	// Still misconfigured on the next hourly tick: the operator is not spammed
	// with a second unacknowledged row.
	s.checkProxyTrustAlert()
	if rows := alertsOfKind(t, s, notify.KindConfigTrustAllProxies); len(rows) != 1 {
		t.Errorf("an unacknowledged alert must not pile up, got %d rows", len(rows))
	}
}

// A configured allow-list, or proxy trust switched off entirely, is the correct
// setup and must stay silent.
func TestProxyTrustAlertSilentWhenConfigured(t *testing.T) {
	_, lan, err := net.ParseCIDR("172.16.0.0/12")
	if err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]*config.Config{
		"allow-list set":  {TrustProxy: true, TrustedProxies: []*net.IPNet{lan}},
		"proxy trust off": {TrustProxy: false},
		"off with a list": {TrustProxy: false, TrustedProxies: []*net.IPNet{lan}},
	} {
		t.Run(name, func(t *testing.T) {
			s := proxyAlertServer(t, cfg)
			s.checkProxyTrustAlert()
			if rows := alertsOfKind(t, s, notify.KindConfigTrustAllProxies); len(rows) != 0 {
				t.Errorf("a correctly configured deployment must not be alerted: %d rows", len(rows))
			}
		})
	}
}
