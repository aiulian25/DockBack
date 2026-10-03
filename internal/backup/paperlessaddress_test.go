package backup

import (
	"encoding/json"
	"strings"
	"testing"
)

// F181 — Paperless-ngx records where it lives in three environment variables,
// and each one wants a different form of the same address.
//
// The names and the shapes were read out of the image's own
// src/paperless/settings/__init__.py, not from documentation:
//
//	CSRF_TRUSTED_ORIGINS = get_list_from_env("PAPERLESS_CSRF_TRUSTED_ORIGINS")
//	CORS_ALLOWED_ORIGINS = get_list_from_env("PAPERLESS_CORS_ALLOWED_HOSTS", …)
//	ALLOWED_HOSTS        = get_list_from_env("PAPERLESS_ALLOWED_HOSTS", default=["*"])
//	  … url = os.getenv("PAPERLESS_URL")
//	      CSRF_TRUSTED_ORIGINS.append(url)
//	      CORS_ALLOWED_ORIGINS.append(url)
//	      ALLOWED_HOSTS.append(urlparse(url).hostname)
//
// So the origin lists take a URL and the host list takes a hostname. Getting
// that backwards produces entries that never match anything, and the failure is
// a CSRF error at login rather than something that names the setting.
func TestPaperlessAddressBindings(t *testing.T) {
	p := ProfileFor("ghcr.io/paperless-ngx/paperless-ngx:latest")
	if p == nil || p.Name != "Paperless-ngx" {
		t.Fatalf("expected the Paperless profile, got %+v", p)
	}
	byKey := map[string]AddressBinding{}
	for _, b := range p.Address {
		for _, k := range b.Keys {
			byKey[k] = b
		}
	}

	// The full URL, exactly as supplied.
	if b, ok := byKey["PAPERLESS_URL"]; !ok {
		t.Error("PAPERLESS_URL is the setting that covers all three")
	} else {
		if b.Shape != EnvURL {
			t.Errorf("PAPERLESS_URL is parsed as a URL, got shape %q", b.Shape)
		}
		if !b.Blocking {
			t.Error("a wrong address here means nobody can log in — that is blocking")
		}
	}

	// Origins carry a scheme. Django rejects a trusted origin without one.
	for _, k := range []string{"PAPERLESS_CSRF_TRUSTED_ORIGINS", "PAPERLESS_CORS_ALLOWED_HOSTS"} {
		b, ok := byKey[k]
		if !ok {
			t.Errorf("%s must be bound — it is the one an operator asked for by name", k)
			continue
		}
		if !b.List {
			t.Errorf("%s is a list; replacing it wholesale would drop the addresses it already answers on", k)
		}
		if b.Shape != EnvOrigin {
			t.Errorf("%s needs scheme://host, got shape %q", k, b.Shape)
		}
	}

	// Allowed hosts are hostnames; a scheme here matches nothing.
	if b, ok := byKey["PAPERLESS_ALLOWED_HOSTS"]; !ok {
		t.Error("PAPERLESS_ALLOWED_HOSTS must be bound")
	} else if !b.List || b.Shape != EnvHost {
		t.Errorf("PAPERLESS_ALLOWED_HOSTS is a list of bare hosts, got list=%v shape=%q", b.List, b.Shape)
	}
}

// The three forms, produced from one supplied address.
func TestPaperlessAddressShapes(t *testing.T) {
	p := ProfileFor("ghcr.io/paperless-ngx/paperless-ngx:latest")
	got := map[string]string{}
	for _, b := range p.Address {
		for _, k := range b.Keys {
			got[k] = shapeAddress(b, "https://docs.example.uk")
		}
	}
	want := map[string]string{
		"PAPERLESS_URL":                  "https://docs.example.uk",
		"PAPERLESS_CSRF_TRUSTED_ORIGINS": "https://docs.example.uk",
		"PAPERLESS_CORS_ALLOWED_HOSTS":   "https://docs.example.uk",
		"PAPERLESS_ALLOWED_HOSTS":        "docs.example.uk",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}

	// An address given without a scheme still produces a valid origin — a bare
	// host in a trusted-origin list is rejected by Django at startup, so
	// defaulting to https is the only useful answer.
	for _, b := range p.Address {
		if b.Shape == EnvOrigin {
			if s := shapeAddress(b, "docs.example.uk"); !strings.HasPrefix(s, "https://") {
				t.Errorf("an origin must carry a scheme, got %q", s)
			}
			if s := shapeAddress(b, "http://docs.example.uk:8000"); s != "http://docs.example.uk:8000" {
				t.Errorf("the supplied scheme and port must be kept, got %q", s)
			}
		}
	}
}

// F183 — the wire contract for the one profile type that is serialized straight
// to the browser.
//
// RegenerablePath had no json tags, so Go named the fields Path/Label/Cost while
// the page read path/label/cost. Every field arrived undefined, and
// `label.toLowerCase()` threw during render — taking the whole container page
// down with "Unexpected Application Error" for any image that declares a
// regenerable directory. Reported against Tautulli; it was every one of them.
//
// Pinned as a test rather than a comment because the failure is invisible in Go:
// the struct compiles, the handler returns it, and only the browser can tell.
func TestRegenerablePathWireContract(t *testing.T) {
	raw, err := json.Marshal(RegenerablePathsFor("tautulli"))
	if err != nil {
		t.Fatal(err)
	}
	var back []map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back) == 0 {
		t.Fatal("Tautulli declares a regenerable directory")
	}
	for _, key := range []string{"path", "label", "cost"} {
		v, ok := back[0][key]
		if !ok {
			t.Errorf("the page reads %q; got keys %v", key, keysOf(back[0]))
			continue
		}
		if s, _ := v.(string); strings.TrimSpace(s) == "" {
			t.Errorf("%q is empty — the toggle would render a blank", key)
		}
	}
	// Every profile that declares one must fill all three: the label names it on
	// the toggle and the cost is what makes the trade legible.
	for _, image := range []string{"tautulli", "jellyfin", "fosrl/pangolin", "requarks/wiki"} {
		for _, r := range RegenerablePathsFor(image) {
			if r.Path == "" || r.Label == "" || r.Cost == "" {
				t.Errorf("%s declares an incomplete regenerable path: %+v", image, r)
			}
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// F186 — the restore that completed and left the application dead.
//
// A stack restore of Paperless finished, and the container then refused to boot:
//
//	SystemCheckError: … (4_0.E001) As of Django 4.0, the values in the
//	CSRF_TRUSTED_ORIGINS setting must start with a scheme (usually http:// or
//	https://) but found paper.example.net.
//
// The address field had been given a bare host — which is a reasonable thing to
// type and which the validator accepts, because for OVERWRITEHOST and every
// allowed-hosts list a bare host is exactly right. It is fatal for PAPERLESS_URL:
// Paperless parses it as a URL and appends it to CSRF_TRUSTED_ORIGINS itself, so
// a missing scheme takes the whole application down three layers from where it
// was typed.
//
// Every single-value binding in the registry is a URL variable, so this was never
// Paperless-specific.
func TestBareAddressNeverReachesAURLVariable(t *testing.T) {
	const bare = "paper.example.net"

	// The reported failure, exactly.
	p := ProfileFor("ghcr.io/paperless-ngx/paperless-ngx:latest")
	for _, b := range p.Address {
		for _, k := range b.Keys {
			got := shapeAddress(b, bare)
			if k == "PAPERLESS_URL" && got != "https://"+bare {
				t.Errorf("PAPERLESS_URL = %q — Django will refuse to start", got)
			}
			// The origin lists were already right and must stay right.
			if b.Shape == EnvOrigin && !strings.Contains(got, "://") {
				t.Errorf("%s = %q — a trusted origin needs a scheme", k, got)
			}
			// And the host list must NOT gain one.
			if b.Shape == EnvHost && strings.Contains(got, "://") {
				t.Errorf("%s = %q — an allowed host with a scheme matches nothing", k, got)
			}
		}
	}

	// The same hazard in every other application that records a URL: BookStack
	// boots with APP_URL, Mealie with BASE_URL, Nextcloud generates links from
	// OVERWRITECLIURL.
	for _, image := range []string{"solidnerd/bookstack", "nextcloud:31-apache", "mealie", "linux-update-dashboard"} {
		prof := ProfileFor(image)
		if prof == nil {
			continue
		}
		for _, b := range prof.Address {
			if b.Kind != BindEnv || b.List || b.Shape != EnvURL {
				continue
			}
			got := shapeAddress(b, bare)
			if !strings.HasPrefix(got, "https://") {
				t.Errorf("%s %v = %q — a URL variable must never be written without a scheme", image, b.Keys, got)
			}
		}
	}
}

// F188 — the one thing an otherwise perfect Paperless restore can leave broken.
//
// Asked after a successful cross-host restore: "what I didn't get to test is if
// the email document fetch still works." For a password account the answer is
// yes and needs no note — the credential is a column in the database that just
// came back. For an OAuth account it is not: the refresh token is issued to an
// app registration at the provider and expires, and re-authorising is a round
// trip through a consent screen no restore can perform. The documents are all
// there; the thing that fetches new ones has quietly stopped.
func TestPaperlessMailNote(t *testing.T) {
	notes := postRestoreNotesFor("ghcr.io/paperless-ngx/paperless-ngx:latest")
	if len(notes) != 1 {
		t.Fatalf("expected the mail note, got %d", len(notes))
	}
	n := notes[0]

	// Token accounts ONLY. Firing for every deployment that fetches mail is how a
	// note stops being read, and a password account genuinely needs nothing.
	if !strings.Contains(n.SQL, "is_token = true") {
		t.Errorf("a password account needs no action; only a token account does: %q", n.SQL)
	}
	if !strings.Contains(n.SQL, "paperless_mail_mailaccount") {
		t.Errorf("the table is Django's default name for that model: %q", n.SQL)
	}
	if n.Engine != "postgres" {
		t.Errorf("the dialect must be declared so it never runs against another engine: %q", n.Engine)
	}
	// Read-only, like every note: this runs against a table that also holds
	// credentials.
	up := strings.ToUpper(n.SQL)
	if !strings.HasPrefix(up, "SELECT") {
		t.Errorf("a note must be a SELECT: %q", n.SQL)
	}
	for _, forbidden := range []string{"UPDATE ", "DELETE ", "INSERT ", "DROP ", "ALTER ", ";"} {
		if strings.Contains(up, forbidden) {
			t.Errorf("a note must not contain %q: %q", forbidden, n.SQL)
		}
	}
	// It has to name the action, and say what does NOT need one — the question
	// this answers was about mail fetching in general, not about OAuth.
	if !strings.Contains(n.Note, "re-authorise") || !strings.Contains(n.Note, "Password-based") {
		t.Errorf("the note should name the action and rule out the common case: %q", n.Note)
	}
}
