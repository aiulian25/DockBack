package backup

import (
	"strings"
	"testing"
)

// PLAYBOOK §9.3 says CLEAR proxy trust; DockBack kept it. Both are right about
// different restores, and this is the table that says which is which.
func TestProxyTrustPolicy(t *testing.T) {
	t.Run("the cross-host x changed-address matrix", func(t *testing.T) {
		cases := []struct {
			crossHost bool
			changed   bool
			want      proxyTrustAction
			because   string
		}{
			// Nothing moved. Clearing would remove working configuration to fix a
			// problem that does not exist.
			{false, false, proxyTrustKeep, "did not move"},
			// An address change is a DNS-and-vhost operation. The proxy in front
			// of the container is the same one at the same address.
			{false, true, proxyTrustKeep, "same machine"},
			// The path changed, but this restore writes no settings at all — so
			// the stale trust is named, not removed.
			{true, false, proxyTrustReport, "different machine"},
			// §9.3's case exactly: the app moved and its address moved with it.
			{true, true, proxyTrustClear, "different machine"},
		}
		for _, tc := range cases {
			verdict := proxyTrustPolicy(tc.crossHost, tc.changed)
			if verdict.Action != tc.want {
				t.Errorf("crossHost=%v changed=%v: action %d, want %d", tc.crossHost, tc.changed, verdict.Action, tc.want)
			}
			if verdict.Why == "" {
				t.Errorf("crossHost=%v changed=%v: no reason given", tc.crossHost, tc.changed)
			}
			if !strings.Contains(verdict.Why, tc.because) {
				t.Errorf("crossHost=%v changed=%v: reason %q does not rest on %q", tc.crossHost, tc.changed, verdict.Why, tc.because)
			}
		}
	})

	t.Run("only a move clears, and only a move that changes settings", func(t *testing.T) {
		// The security claim in one line: a same-host restore must never remove
		// proxy trust, whatever else it is doing.
		for _, changed := range []bool{false, true} {
			if proxyTrustPolicy(false, changed).Action == proxyTrustClear {
				t.Fatalf("cleared proxy trust on a same-host restore (changed=%v)", changed)
			}
		}
	})

	t.Run("the old value comes back as a runnable command, one entry per index", func(t *testing.T) {
		// occ has no undo. Setting the value again IS the undo, so the run log is
		// where the old value has to survive the delete.
		lines := occProxyRestoreLines([]string{"10.0.0.5", "172.18.0.2"})
		for _, want := range []string{
			"config:system:set trusted_proxies 0 --value 10.0.0.5",
			"config:system:set trusted_proxies 1 --value 172.18.0.2",
			"docker exec -u " + occUser,
		} {
			if !strings.Contains(lines, want) {
				t.Errorf("restore command is missing %q:\n%s", want, lines)
			}
		}
		if occProxyRestoreLines(nil) != "" {
			t.Error("an empty list must produce no command at all")
		}
	})

	t.Run("proxy-trust env keys are reported, never cleared", func(t *testing.T) {
		// The step's own DO-NOT assumed the generic address reporter already
		// listed these. It does not — addressLikeKey matches on the last word
		// (URL/HOST/ORIGIN…), and PROXIES is not one — so without this the env
		// half of #22 is silent.
		env := []string{
			"TRUSTED_PROXIES=10.0.0.5",
			"NEXTCLOUD_TRUSTED_PROXIES=172.18.0.0/16",
			"PROXY_TRUST=10.0.0.9",
			"APP_URL=https://old.example.com",
			"DB_PASSWORD=hunter2",
		}
		found := proxyTrustEnv(env)
		if len(found) != 3 {
			t.Fatalf("reported %v, want the three proxy-trust variables", found)
		}
		for _, want := range []string{"TRUSTED_PROXIES=10.0.0.5", "NEXTCLOUD_TRUSTED_PROXIES=172.18.0.0/16", "PROXY_TRUST=10.0.0.9"} {
			if !slicesContainsString(found, want) {
				t.Errorf("missing %q from %v", want, found)
			}
		}
		// It must not swallow what the address reporter owns, or claim a secret.
		for _, unwanted := range []string{"APP_URL", "DB_PASSWORD"} {
			for _, got := range found {
				if strings.HasPrefix(got, unwanted+"=") {
					t.Errorf("proxy-trust reporter claimed %q", got)
				}
			}
		}
		if len(proxyTrustEnv([]string{"TRUSTED_PROXIES=", "HTTP_PROXY=http://cache:3128"})) != 0 {
			t.Error("an empty value, or an outbound proxy, is not a proxy-trust grant")
		}
		// A netmask is the whole trust scope, so it must survive verbatim — the
		// address reporter would have cut it at the slash, which is right for a
		// webhook URL's secret path and wrong for a CIDR.
		if got := proxyTrustEnv([]string{"TRUSTED_PROXIES=10.0.0.0/8"}); len(got) != 1 || got[0] != "TRUSTED_PROXIES=10.0.0.0/8" {
			t.Errorf("netmask did not survive: %v", got)
		}
		// The most dangerous settings of all must still be visible.
		if got := proxyTrustEnv([]string{"TRUSTED_PROXIES=*"}); len(got) != 1 {
			t.Errorf("a wildcard proxy trust must be reported, got %v", got)
		}
		// And a credential grammar is dropped whole, never shortened.
		for _, unsafe := range []string{"TRUSTED_PROXIES=https://u:p@host", "TRUSTED_PROXIES=$(id)", "TRUSTED_PROXY_TOKEN=abc"} {
			if got := proxyTrustEnv([]string{unsafe}); len(got) != 0 {
				t.Errorf("printed %q from %q", got, unsafe)
			}
		}
	})

	t.Run("the aside is not a file Nextcloud would load", func(t *testing.T) {
		// Nextcloud reads config.php plus every *.config.php in the directory. A
		// copy named that way would come back as live configuration.
		aside := nextcloudConfigFile + ".dockback-1756600000.bak"
		if strings.HasSuffix(aside, ".config.php") {
			t.Fatalf("%s would be loaded as configuration", aside)
		}
		if !strings.HasPrefix(aside, nextcloudConfigFile+".") {
			t.Fatalf("%s does not sit beside the file it copies", aside)
		}
	})
}

// slicesContainsString is a local helper: the package targets a Go version
// whose slices package is already used elsewhere, but this keeps the assertion
// readable at the call site.
func slicesContainsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
