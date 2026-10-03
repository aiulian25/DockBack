package backup

import (
	"testing"

	"dockback/internal/dockercli"
)

// F175 — one address, written into several variables that each hold a different
// part of it.
//
// The failure this comes from: a Nextcloud restored onto a new machine kept
// answering at the old one, because the official image reads OVERWRITEHOST from
// the environment on every request (through its own reverse-proxy.config.php,
// loaded after config.php) and the restore had carried the previous machine's
// value over untouched. occ set the right thing into config.php and the
// environment quietly outranked it.
func TestShapeAddress(t *testing.T) {
	cases := []struct {
		name string
		bind AddressBinding
		in   string
		want string
	}{
		// The host alone. A scheme here is how "the fix did nothing" happens:
		// Nextcloud compares OVERWRITEHOST against the request's host, and
		// "https://cloud.example.com" is not a host.
		{"host strips the scheme", AddressBinding{Shape: EnvHost}, "https://cloud.example.com", "cloud.example.com"},
		{"host keeps a port", AddressBinding{Shape: EnvHost}, "http://10.168.1.50:8080", "10.168.1.50:8080"},
		{"host from a bare name", AddressBinding{Shape: EnvHost}, "nas", "nas"},

		// The full URL, exactly as supplied.
		{"a URL keeps its scheme", AddressBinding{Shape: EnvURL}, "https://cloud.example.com", "https://cloud.example.com"},
		// F186: the case that broke a restore. A bare host typed into the address
		// field is reasonable and the validator accepts it — but a URL variable
		// with no scheme is fatal to the app that reads it, not merely untidy.
		{"a URL gains a scheme when none was typed", AddressBinding{Shape: EnvURL}, "docs.example.uk", "https://docs.example.uk"},
		{"a URL keeps http when that is what was typed", AddressBinding{Shape: EnvURL}, "http://docs.example.uk:8000", "http://docs.example.uk:8000"},

		// Just the protocol word.
		{"scheme from https", AddressBinding{Shape: EnvScheme}, "https://cloud.example.com", "https"},
		{"scheme from http", AddressBinding{Shape: EnvScheme}, "http://10.168.1.50:8080", "http"},
		// No scheme supplied is https: an address given bare is one someone
		// reaches over the web, and guessing http would downgrade it.
		{"scheme defaults to https", AddressBinding{Shape: EnvScheme}, "cloud.example.com", "https"},

		// A list of accepted hosts is hosts, whether or not it says so — this is
		// the pre-existing behaviour every other profile relies on.
		{"a list defaults to the host", AddressBinding{List: true}, "https://cloud.example.com", "cloud.example.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shapeAddress(c.bind, c.in); got != c.want {
				t.Errorf("shapeAddress(%+v, %q) = %q, want %q", c.bind, c.in, got, c.want)
			}
		})
	}
}

// The separator is not cosmetic. Nextcloud's entrypoint word-splits
// NEXTCLOUD_TRUSTED_DOMAINS, so joining with a comma does not add a domain — it
// makes the two into one entry that matches no request at all.
func TestAddToEnvListSeparator(t *testing.T) {
	got, changed := dockercli.AddToEnvListSep("vps.example.uk 203.0.113.41", "cloud.new.uk", " ")
	if !changed {
		t.Fatal("a new host must be added")
	}
	if got != "vps.example.uk 203.0.113.41 cloud.new.uk" {
		t.Errorf("space-separated list = %q", got)
	}
	// Existing entries are kept: the instance must keep answering everywhere it
	// already answers, or the move locks somebody out of a working address.
	if already, changed := dockercli.AddToEnvListSep("a.uk b.uk", "B.UK", " "); changed || already != "a.uk b.uk" {
		t.Errorf("an entry already present must not be added again, got %q changed=%v", already, changed)
	}
	// The comma default is unchanged for every profile that used it.
	if got, _ := dockercli.AddToEnvList("a.uk,b.uk", "c.uk"); got != "a.uk,b.uk,c.uk" {
		t.Errorf("comma-separated default = %q", got)
	}
}
