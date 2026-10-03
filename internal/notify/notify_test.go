package notify

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsFailure(t *testing.T) {
	if IsFailure(KindBackupSuccess) {
		t.Error("backup.success should not be a failure")
	}
	for _, k := range []string{KindBackupFailed, KindVerifyFailed} {
		if !IsFailure(k) {
			t.Errorf("%s should be a failure", k)
		}
	}
}

func TestWantsRouting(t *testing.T) {
	cases := []struct {
		onSuccess, onFailure bool
		sev                  Severity
		want                 bool
	}{
		{onFailure: true, sev: SevCritical, want: true},  // failure subscriber gets criticals
		{onFailure: true, sev: SevWarning, want: true},   // …and operational warnings (§9.15)
		{onFailure: true, sev: SevInfo, want: false},     // …but not successes
		{onSuccess: true, sev: SevInfo, want: true},      // success subscriber gets successes
		{onSuccess: true, sev: SevWarning, want: false},  // …but not alerts
		{onSuccess: true, sev: SevCritical, want: false}, // …nor criticals
		{want: false}, // nothing subscribed
	}
	for i, c := range cases {
		if got := wants(c.onSuccess, c.onFailure, c.sev); got != c.want {
			t.Errorf("case %d: wants(%v,%v,%v)=%v want %v", i, c.onSuccess, c.onFailure, c.sev, got, c.want)
		}
	}
}

// F14: per-channel minimum-severity routing. meetsMin is a monotonic floor, and
// an empty MinSeverity must reproduce today's OnSuccess/OnFailure routing exactly.
func TestSeverityMinRouting(t *testing.T) {
	// meetsMin matrix: floor -> which severities pass.
	minCases := []struct {
		min  string
		info bool
		warn bool
		crit bool
	}{
		{"info", true, true, true},       // everything
		{"warning", false, true, true},   // alerts & failures
		{"critical", false, false, true}, // data-at-risk only
		{"", false, true, true},          // unset explicit → safe warning floor
		{"bogus", false, true, true},     // unknown explicit → safe warning floor
	}
	for _, c := range minCases {
		if got := meetsMin(SevInfo, c.min); got != c.info {
			t.Errorf("meetsMin(info,%q)=%v want %v", c.min, got, c.info)
		}
		if got := meetsMin(SevWarning, c.min); got != c.warn {
			t.Errorf("meetsMin(warning,%q)=%v want %v", c.min, got, c.warn)
		}
		if got := meetsMin(SevCritical, c.min); got != c.crit {
			t.Errorf("meetsMin(critical,%q)=%v want %v", c.min, got, c.crit)
		}
	}

	// Acceptance: email critical-only, Gotify warning-and-up.
	if meetsMin(SevWarning, "critical") {
		t.Error("a warning must NOT reach a critical-only channel")
	}
	if !meetsMin(SevWarning, "warning") || !meetsMin(SevCritical, "critical") {
		t.Error("a channel must receive events at or above its floor")
	}

	// Empty MinSeverity reproduces the legacy toggle routing for every severity.
	for _, sev := range []Severity{SevInfo, SevWarning, SevCritical} {
		for _, onS := range []bool{false, true} {
			for _, onF := range []bool{false, true} {
				got := subscribes("", onS, onF, sev)
				want := wants(onS, onF, sev)
				if got != want {
					t.Errorf("subscribes(\"\",%v,%v,%v)=%v want legacy %v", onS, onF, sev, got, want)
				}
			}
		}
	}

	// A set floor supersedes the toggles: critical-only ignores an on_success toggle.
	if subscribes("critical", true, true, SevWarning) {
		t.Error("an explicit floor must supersede the toggles (warning blocked by critical floor)")
	}
	if !subscribes("critical", false, false, SevCritical) {
		t.Error("an explicit floor must supersede the toggles (critical passes even with toggles off)")
	}
}

func TestSeverityOf(t *testing.T) {
	crit := []string{KindBackupFailed, KindVerifyFailed, KindScrubFailed}
	warn := []string{KindNoOffsite, KindDestFull, KindMissedSchedule, KindKeyUnescrowed, "something.new"}
	if SeverityOf(KindBackupSuccess) != SevInfo {
		t.Error("success must be info")
	}
	for _, k := range crit {
		if SeverityOf(k) != SevCritical {
			t.Errorf("%s must be critical", k)
		}
	}
	for _, k := range warn {
		if SeverityOf(k) != SevWarning {
			t.Errorf("%s must be warning", k)
		}
	}
}

// TestSendFansOutOnlyToSubscribed verifies the dispatcher consults config and
// doesn't try to send when no channel subscribes (Load is called once; no panics
// on empty config).
func TestSendNoChannels(t *testing.T) {
	called := 0
	d := New(func() (Config, error) { called++; return Config{}, nil }, nil)
	d.Send(KindVerifyFailed, "t", "m")
	if called != 1 {
		t.Fatalf("Load should be called once, got %d", called)
	}
}

// TestTestChannelUnknown returns an error for an unknown channel.
func TestTestChannelUnknown(t *testing.T) {
	d := New(func() (Config, error) { return Config{}, nil }, nil)
	if err := d.TestChannel(Config{}, "nope"); err == nil {
		t.Fatal("expected error for unknown channel")
	}
}

// Gotify authentication failures (investigating a live 401).
//
// Gotify answers every auth failure with the same sentence, so the raw status
// told an operator nothing about which of three different problems they had.

func TestGotifyAuthErrorNamesTheLikelyCause(t *testing.T) {
	resp := &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(""))}
	key := strings.Repeat("x", 43) // a 32-byte ed25519 seed, base64url, unpadded

	// A CLIENT token: parses fine on Gotify's side and is simply not in the
	// applications table, which is why it fails identically to a wrong token.
	got := gotifyAuthError(resp, "gtfyc."+key).Error()
	if !strings.Contains(got, "CLIENT token") || !strings.Contains(got, "Gotify -> Apps") {
		t.Fatalf("a gtfyc token must be identified as a client token: %s", got)
	}

	// The RIGHT kind of token that the server does not know — revoked,
	// regenerated, or from a different Gotify. Must NOT tell them to switch kinds.
	got = gotifyAuthError(resp, "gtfya."+key).Error()
	if !strings.Contains(got, "right kind") || strings.Contains(got, "CLIENT token") {
		t.Fatalf("a gtfya token must not be misdiagnosed as the wrong kind: %s", got)
	}

	// A pre-upgrade token — the commonest cause after a Gotify update, since the
	// server stops recognising the old short tokens.
	got = gotifyAuthError(resp, "AWyi5S1TPZH0Xnq").Error()
	if !strings.Contains(got, "BEFORE Gotify's token change") {
		t.Fatalf("a legacy token must be identified as pre-upgrade: %s", got)
	}

	// The token must NEVER appear — this string reaches the UI, the run log and
	// the alert inbox.
	for _, tok := range []string{"gtfyc." + key, "gtfya." + key, "AWyi5S1TPZH0Xnq"} {
		if strings.Contains(gotifyAuthError(resp, tok).Error(), tok) {
			t.Fatalf("the token must not be echoed into the error")
		}
	}
}

// A trailing newline on a pasted token is enough to fail authentication, and is
// invisible in the UI. It must never reach the wire.
func TestSendGotifyTrimsTheToken(t *testing.T) {
	var gotKey, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotPath = r.Header.Get("X-Gotify-Key"), r.URL.Path
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d := &Dispatcher{hc: srv.Client()}
	err := d.sendGotify(GotifyConfig{URL: srv.URL + "  ", Token: "  AbCdEf1234567\n"}, "t", "m", 5)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if gotKey != "AbCdEf1234567" {
		t.Fatalf("the token must reach Gotify trimmed, got %q", gotKey)
	}
	if gotPath != "/message" {
		t.Fatalf("path = %q, want /message", gotPath)
	}
	// Whitespace-only is not a token.
	if err := d.sendGotify(GotifyConfig{URL: srv.URL, Token: "   "}, "t", "m", 5); err == nil {
		t.Fatal("a whitespace-only token must be refused, not sent")
	}
}
