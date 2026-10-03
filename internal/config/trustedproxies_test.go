package config

import (
	"os"
	"strings"
	"testing"
)

// A typo in the trusted-proxy allow-list used to be skipped, which silently
// turned "trust these two proxies" into "trust every caller" — the opposite of
// what was written, announced only by a stderr line nobody reads on a NAS.
func TestParseCIDRsRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"nonsense", "172.16.0.0/12,nonsense", "10.0.0.0/33", "10.168.1.1/", "10.0.0.256"} {
		got, err := parseCIDRs(bad)
		if err == nil {
			t.Errorf("parseCIDRs(%q) accepted %d entries, want a refusal", bad, len(got))
			continue
		}
		if !strings.Contains(err.Error(), "DOCKBACK_TRUSTED_PROXIES") {
			t.Errorf("the error must name the variable to fix: %v", err)
		}
	}
}

// The valid spellings, and the documented empty case, must keep working — this
// runs on every boot of every existing deployment.
func TestParseCIDRsAcceptsValidLists(t *testing.T) {
	cases := map[string]int{
		"":                           0,
		"   ":                        0,
		"172.16.0.0/12":              1,
		"127.0.0.0/8,::1/128":        2,
		" 10.0.0.1 , 10.168.1.0/24 ": 2, // padded entries, as a .env file tends to have
		"2001:db8::/32":              1,
		"10.0.0.1":                   1, // a bare IP is a valid single-host entry
	}
	for in, want := range cases {
		got, err := parseCIDRs(in)
		if err != nil {
			t.Errorf("parseCIDRs(%q) refused a valid list: %v", in, err)
			continue
		}
		if len(got) != want {
			t.Errorf("parseCIDRs(%q) = %d entries, want %d", in, len(got), want)
		}
	}
}

// Load must refuse to boot rather than run with an allow-list it could not read.
func TestLoadRefusesAnUnparseableProxyList(t *testing.T) {
	t.Setenv("DOCKBACK_TRUSTED_PROXIES", "172.16.0.0/12,not-an-address")
	t.Setenv("DOCKBACK_ENCRYPTION_KEY", strings.Repeat("ab", 32))
	t.Setenv("DOCKBACK_DATA_DIR", t.TempDir())
	t.Setenv("DOCKBACK_BACKUPS_DIR", t.TempDir())

	_, err := Load()
	if err == nil {
		t.Fatal("a trusted-proxy list that cannot be parsed must stop the process, not be silently emptied")
	}
	if !strings.Contains(err.Error(), "not-an-address") {
		t.Errorf("the error must name the entry to fix: %v", err)
	}
	// Sanity: the same environment without the typo loads.
	_ = os.Setenv("DOCKBACK_TRUSTED_PROXIES", "172.16.0.0/12")
	if _, err := Load(); err != nil {
		t.Fatalf("a valid list must still load: %v", err)
	}
}
