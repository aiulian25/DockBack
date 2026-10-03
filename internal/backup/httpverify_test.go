package backup

import (
	"strings"
	"testing"
)

// R2 §4.1's measurement, verbatim: requesting the source by IP answers
// `302 https://bookstack.example.com/login`. Following that is how an operator
// "signs off on a restore they never actually tested".
func TestRedirectVerdict(t *testing.T) {
	own := ownAddressesFor("http://10.168.1.149:6875", "/bookstack-restored")

	for _, tc := range []struct {
		name     string
		status   int
		location string
		wantOff  bool
		wantHost string
	}{
		{"R2's off-host 302 is the danger", 302, "https://bookstack.example.com/login", true, "bookstack.example.com"},
		{"301 counts the same", 301, "http://old-nas.local/", true, "old-nas.local"},
		{"a redirect to the new address the operator gave is fine", 302, "http://10.168.1.149:6875/login", false, "10.168.1.149"},
		{"a redirect to loopback is the app routing within itself", 302, "http://127.0.0.1:6875/login", false, "127.0.0.1"},
		{"a redirect to its own container name is fine", 302, "http://bookstack-restored/login", false, "bookstack-restored"},
		{"a RELATIVE redirect is not off-host", 302, "/login", false, ""},
		{"a 200 is not a redirect", 200, "", false, ""},
		{"a 302 with no Location says nothing", 302, "", false, ""},
		{"a 404 is not a redirect either", 404, "https://elsewhere.example/", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			off, host := redirectVerdict(tc.status, tc.location, own)
			if off != tc.wantOff {
				t.Errorf("offHost = %v, want %v", off, tc.wantOff)
			}
			if tc.wantHost != "" && host != tc.wantHost {
				t.Errorf("host = %q, want %q", host, tc.wantHost)
			}
		})
	}

	t.Run("the raw response is parsed without a client that would follow it", func(t *testing.T) {
		raw := "HTTP/1.1 302 Found\r\n" +
			"Location: https://bookstack.example.com/login\r\n" +
			"Set-Cookie: session=abc; Path=/; HttpOnly\r\n" +
			"Content-Length: 0\r\n\r\n"
		resp, ok := parseHTTPResponse(raw)
		if !ok || resp.Status != 302 {
			t.Fatalf("got %+v ok=%v", resp, ok)
		}
		if resp.Location != "https://bookstack.example.com/login" {
			t.Errorf("location = %q", resp.Location)
		}
		if len(resp.Cookies) != 1 || resp.Cookies[0] != "session=abc" {
			t.Errorf("cookies = %v", resp.Cookies)
		}
	})

	t.Run("a non-HTTP answer yields no verdict", func(t *testing.T) {
		for _, raw := range []string{"", "SSH-2.0-OpenSSH_9.6\r\n", "garbage"} {
			if _, ok := parseHTTPResponse(raw); ok {
				t.Errorf("must not read %q as HTTP", raw)
			}
		}
	})

	t.Run("the request never asks for a redirect to be followed", func(t *testing.T) {
		// busybox wget follows redirects unconditionally — measured, it went on to
		// connect to the real production host and report 200. The probe speaks raw
		// HTTP so exactly one request is made.
		script := rawRequestScript(6875, "GET", "/", nil, "")
		if strings.Contains(script, "wget") || strings.Contains(script, "curl") {
			t.Error("an HTTP client would follow the redirect this exists to catch")
		}
		if !strings.Contains(script, "nc -w") || !strings.Contains(script, "GET / HTTP/1.0") {
			t.Errorf("script = %s", script)
		}
	})
}

// R4 §Issue 33's exact discriminator: "POST /accounts/login/ -> HTTP 200 (form
// re-rendered = CSRF accepted); HTTP 403 would mean CSRF rejected."
func TestCSRFVerdict(t *testing.T) {
	for _, tc := range []struct {
		status       int
		wantRejected bool
	}{
		{403, true},  // origin rejected — the clone renders but cannot be used
		{200, false}, // form re-rendered: accepted
		{302, false}, // redirected after the attempt: accepted
		{401, false}, // wrong credentials, which is what was sent
		{500, false}, // the app disliked it for its own reasons; not a verdict
	} {
		if got := csrfVerdict(tc.status); got != tc.wantRejected {
			t.Errorf("status %d: rejected = %v, want %v", tc.status, got, tc.wantRejected)
		}
	}

	t.Run("the token is taken from the form, never guessed", func(t *testing.T) {
		spec := ProfileFor("ghcr.io/paperless-ngx/paperless-ngx:2.13").HTTPVerify
		if spec == nil || spec.TokenField != "csrfmiddlewaretoken" {
			t.Fatalf("the Paperless profile must carry R4's recipe: %+v", spec)
		}
		body := `<form method="post"><input type="hidden" name="csrfmiddlewaretoken" value="Xy9zAbC123"><input name="username"></form>`
		token, ok := extractFormToken(body, spec.TokenPattern)
		if !ok || token != "Xy9zAbC123" {
			t.Errorf("token = %q ok=%v", token, ok)
		}
		// A page with no such field must NOT produce a round trip: POSTing without
		// a token earns the 403 that means "no token", which would be reported as
		// an origin rejection — a false danger finding about the thing being tested.
		if _, ok := extractFormToken(`<form><input name="other"></form>`, spec.TokenPattern); ok {
			t.Error("no token must mean no verdict")
		}
		if _, ok := extractFormToken(body, "(("); ok {
			t.Error("an unusable pattern must not claim a token")
		}
	})

	t.Run("only deliberately-wrong credentials are ever sent, under a recognisable name", func(t *testing.T) {
		body := loginFormBody("csrfmiddlewaretoken", "tok")
		if !strings.Contains(body, "username="+probeUsername) {
			t.Errorf("body = %s", body)
		}
		if !strings.Contains(probeUsername, "dockback") {
			t.Error("the attempt must be identifiable in the application's own auth log")
		}
		if !strings.Contains(body, "csrfmiddlewaretoken=tok") {
			t.Errorf("the extracted token must go back: %s", body)
		}
	})

	t.Run("the POST carries the origin being tested", func(t *testing.T) {
		script := rawRequestScript(8000, "POST", "/accounts/login/",
			[]string{"Origin: https://new.example", "Cookie: csrftoken=abc"},
			loginFormBody("csrfmiddlewaretoken", "tok"))
		for _, want := range []string{"POST /accounts/login/ HTTP/1.0", "Origin: https://new.example", "Cookie: csrftoken=abc", "Content-Length: "} {
			if !strings.Contains(script, want) {
				t.Errorf("script must contain %q: %s", want, script)
			}
		}
	})

	t.Run("only the app's own port is probed, discovered from the container", func(t *testing.T) {
		if got := httpProbePort(nil, []string{"9090/tcp", "8000/tcp", "53/udp"}); got != 8000 {
			t.Errorf("port = %d, want the lowest TCP one", got)
		}
		if got := httpProbePort(&HTTPVerifySpec{Port: 3000}, []string{"8000/tcp"}); got != 3000 {
			t.Errorf("an explicit port wins: %d", got)
		}
		if got := httpProbePort(nil, []string{"53/udp"}); got != 0 {
			t.Errorf("nothing to probe = %d", got)
		}
	})
}
