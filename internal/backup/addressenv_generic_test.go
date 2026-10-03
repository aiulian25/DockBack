package backup

import (
	"strings"
	"testing"
)

// F175, the generic half: any container that records where it lives keeps
// recording the previous machine after a move, whether or not DockBack has a
// profile for the application.
//
// This reports and never rewrites, so the bar it has to clear is different from
// the profile bindings': it must not print a secret, and it must not be so noisy
// that the one line that matters is lost among service names and version
// strings.
func TestAddressLikeEnv(t *testing.T) {
	env := []string{
		// Recorded addresses — exactly what a move leaves stale.
		"PAPERLESS_URL=https://docs.old-machine.uk",
		"JELLYFIN_PublishedServerUrl=http://10.168.1.50:8096",
		"NEXTAUTH_URL=https://notes.old-machine.uk",
		"ADVERTISE_IP=http://10.168.1.50:32400/",
		"PUBLIC_ORIGIN=https://app.old-machine.uk",
		"SERVER_HOSTNAME=nas.old.lan",

		// Not addresses at all.
		"TZ=Europe/London",
		"PUID=1000",
		"NEXTCLOUD_VERSION=34.0.2",

		// A service name on the compose network. It resolves the same on the new
		// host, so reporting it is pure noise — and noise is what stops the real
		// line being read.
		"DB_HOST=mariadb",
		"REDIS_HOST=redis",

		// Addresses by shape, secrets in fact. None of these may ever be printed:
		// the run log is something operators paste into help requests.
		"DATABASE_URL=postgres://user:hunter2@db:5432/app",
		"S3_SECRET_KEY=AKIAIOSFODNN7EXAMPLE",
		"OIDC_CLIENT_SECRET=abc123",
		"API_TOKEN_URL=https://api.example.com/t?token=abc123",
		"NEXTAUTH_SECRET=abc123",
		"SMTP_PASSWORD=hunter2",
		"WEBHOOK_URL=https://hooks.example.com/services/T00/B00/XXXXXXXXXXXX",
	}

	got := strings.Join(addressLikeEnv(env, nil), "\n")

	for _, want := range []string{
		"PAPERLESS_URL", "JELLYFIN_PublishedServerUrl", "NEXTAUTH_URL",
		"ADVERTISE_IP", "PUBLIC_ORIGIN", "SERVER_HOSTNAME",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%s records an address and should be reported:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"TZ=", "PUID=", "NEXTCLOUD_VERSION=", "DB_HOST=", "REDIS_HOST="} {
		if strings.Contains(got, unwanted) {
			t.Errorf("%s is not an address a move invalidates — reporting it is noise:\n%s", unwanted, got)
		}
	}
	// The whole point of the value filter.
	for _, secret := range []string{"hunter2", "AKIAIOSFODNN7EXAMPLE", "abc123", "XXXXXXXXXXXX"} {
		if strings.Contains(got, secret) {
			t.Fatalf("a secret reached the run log (%q):\n%s", secret, got)
		}
	}

	// A key the profile already handles is not repeated: it has either been
	// corrected or been named already, and saying it twice buries the rest.
	declared := map[string]bool{"PAPERLESS_URL": true}
	if second := strings.Join(addressLikeEnv(env, declared), "\n"); strings.Contains(second, "PAPERLESS_URL") {
		t.Error("a declared key must not appear in the generic list as well")
	}
}

// The value filter, case by case — this is the half that decides what is safe to
// print. Only the scheme and the authority ever survive: a webhook URL is a
// credential carried in a PATH, and it lives under a key called WEBHOOK_URL.
func TestReportableAddress(t *testing.T) {
	yes := map[string]string{
		"https://cloud.example.com": "https://cloud.example.com",
		"http://10.168.1.50:8080":   "http://10.168.1.50:8080",
		"10.168.1.50":               "10.168.1.50",
		"nas.local":                 "nas.local",
		"[2001:db8::1]:8443":        "[2001:db8::1]:8443",
		"http://10.168.1.50:32400/": "http://10.168.1.50:32400/",
		// The path is dropped and its absence marked, so the line still says
		// "there was more here" without saying what.
		"https://hooks.example.com/services/T00/B00/XXXX": "https://hooks.example.com/…",
		"https://idp.example.com/realms/main?x=secret":    "https://idp.example.com/…",
	}
	for in, want := range yes {
		got, ok := reportableAddress(in)
		if !ok || got != want {
			t.Errorf("reportableAddress(%q) = %q,%v — want %q", in, got, ok, want)
		}
	}
	no := []string{
		"",                         // nothing recorded
		"mariadb",                  // a compose service name: unchanged by a move
		"https://u:p@host/",        // credentials in the authority
		"postgres://u:p@db:5432/",  // the same, as a connection string
		strings.Repeat("a.b/", 60), // too long to be an address anyone typed
		"https://x y",              // whitespace: not a single address
		"$SOME_VAR",                // unexpanded reference, not a value
	}
	for _, v := range no {
		if got, ok := reportableAddress(v); ok {
			t.Errorf("reportableAddress(%q) should be rejected, got %q", v, got)
		}
	}
}

// The name test, including the two that a substring match gets wrong in opposite
// directions: NEXTAUTH_URL is an ordinary address, and DISK_USAGE_URI_CACHE is
// not one at all.
func TestLastWord(t *testing.T) {
	cases := map[string]string{
		"PAPERLESS_URL":               "URL",
		"JELLYFIN_PublishedServerUrl": "URL",
		"ADVERTISE_IP":                "IP",
		"OVERWRITEHOST":               "OVERWRITEHOST", // run together, still ends in HOST
		"HOMEPAGE_ALLOWED_HOSTS":      "HOSTS",
	}
	for in, want := range cases {
		if got := lastWord(in); got != want {
			t.Errorf("lastWord(%q) = %q, want %q", in, got, want)
		}
	}
	if addressLikeKey("DISK_USAGE_URI_CACHE") {
		t.Error("a name that merely CONTAINS an address word is not an address")
	}
	if !addressLikeKey("NEXTAUTH_URL") {
		t.Error("NEXTAUTH_URL is an ordinary address, and one a move invalidates")
	}
	if addressLikeKey("NEXTAUTH_SECRET") {
		t.Error("the secret beside it must never be printed")
	}
}

// Secret-shaped names lose, whatever else they look like. Checked explicitly
// because the two hint lists overlap — AUTH_URL and TOKEN_URI are address names
// by one rule and secrets by the other, and the secret rule has to win.
func TestSecretKeysNeverReported(t *testing.T) {
	for _, k := range []string{
		"API_TOKEN_URI", "SIGNING_KEY_URL", "DATABASE_DSN",
		"PRIVATE_KEY_HOST", "LICENSE_SERVER_URL", "PASSWORD_RESET_URL",
	} {
		if addressLikeKey(k) {
			t.Errorf("%s may hold a credential and must never be printed", k)
		}
	}
	// An identity provider's URL IS reported, and safely: only the scheme and
	// host survive the value filter, so what prints is "https://idp.example.com"
	// — no realm, no client id, no query. It is also one of the addresses most
	// worth checking after a move, so excluding it would cost more than it saves.
	for _, k := range []string{"APP_URL", "BASE_URL", "OVERWRITEHOST", "PUBLIC_ORIGIN", "SERVER_FQDN", "OIDC_AUTH_URL"} {
		if !addressLikeKey(k) {
			t.Errorf("%s records an address and should be reported", k)
		}
	}
}

// The noise filter, measured rather than imagined.
//
// Every case here was produced by running the detector over the real
// environments of 25 containers on a working host. The first version reported
// something for 12 of them; with these rules it reports for 5, and every
// surviving line names a particular machine. That ratio is the point — the run
// log is where one stale address has to be NOTICED, and a list that is mostly
// irrelevant is a list that gets skimmed past.
func TestNoiseRejectedFromRealEnvironments(t *testing.T) {
	// Kept: each of these names one machine, and a move can invalidate it.
	keep := map[string]string{
		"APP_URL=https://car.example.uk":                            "APP_URL=https://car.example.uk",
		"BASE_URL=http://10.168.50.54":                              "BASE_URL=http://10.168.50.54",
		"OLLAMA_BASE_URL=http://10.168.1.80:11434":                  "OLLAMA_BASE_URL=http://10.168.1.80:11434",
		"CORS_ORIGINS=http://localhost:5000,https://car.example.uk": "CORS_ORIGINS=https://car.example.uk",
	}
	for in, want := range keep {
		got := addressLikeEnv([]string{in}, nil)
		if len(got) != 1 || got[0] != want {
			t.Errorf("addressLikeEnv(%q) = %v, want [%q]", in, got, want)
		}
	}

	// Dropped, each for its own reason.
	drop := []string{
		"HOST=0.0.0.0",                             // a bind address: correct on every machine
		"OLLAMA_HOST=0.0.0.0:11434",                // the same, with a port
		"SW_HOST=http://localhost:8889",            // loopback is loopback everywhere
		"DJANGO_ALLOWED_HOSTS=localhost,127.0.0.1", // a real list, none of it machine-specific
		"ES_URL=http://mediaapp-es:9200",           // a compose service name: docker resolves it the same on the new host
		"REDIS_URL=redis://redis:6379/0",           // the same
		"DOCKER_HOST=tcp://socket-proxy:2375",
	}
	for _, in := range drop {
		if got := addressLikeEnv([]string{in}, nil); len(got) != 0 {
			t.Errorf("%q does not change when the container moves — reporting it is noise, got %v", in, got)
		}
	}
}

// F177 — the pre-restore half, from the manifest's recorded env KEYS. Values are
// dropped at that boundary because a container environment holds passwords, so
// this can only ever name variables — which is still the thing that would have
// prompted somebody to fill in the new address before starting.
func TestAddressEnvKeys(t *testing.T) {
	man := &Manifest{ContainerEnvKeys: []string{
		"OVERWRITEHOST", "NEXTCLOUD_TRUSTED_DOMAINS", "APP_URL",
		"MYSQL_PASSWORD", "TZ", "PUID", "NEXTAUTH_SECRET", "DISK_USAGE_URI_CACHE",
	}}
	got := strings.Join(AddressEnvKeys(man), ",")
	for _, want := range []string{"OVERWRITEHOST", "NEXTCLOUD_TRUSTED_DOMAINS", "APP_URL"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s records an address and should be named: %q", want, got)
		}
	}
	for _, unwanted := range []string{"MYSQL_PASSWORD", "TZ", "PUID", "NEXTAUTH_SECRET", "DISK_USAGE_URI_CACHE"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("%s must not be named as an address: %q", unwanted, got)
		}
	}
	// A backup too old to record env keys says nothing. "No evidence" and
	// "nothing to check" are different claims, and only one of them is true.
	if got := AddressEnvKeys(&Manifest{}); len(got) != 0 {
		t.Errorf("a manifest with no recorded keys must report nothing, got %v", got)
	}
	if got := AddressEnvKeys(nil); len(got) != 0 {
		t.Errorf("a nil manifest must report nothing, got %v", got)
	}
}
