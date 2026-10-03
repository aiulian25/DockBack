package backup

import (
	"strings"
	"testing"

	"dockback/internal/store"
)

// TestParseTrustedDomains: the index a domain sits at is the index occ writes
// to, so a parse that drifts by one would overwrite the WRONG entry — silently
// removing a domain the instance still answers on.
func TestParseTrustedDomains(t *testing.T) {
	got := parseTrustedDomains("localhost\ncloud.example.com\n10.168.1.50\n")
	want := []string{"localhost", "cloud.example.com", "10.168.1.50"}
	if len(got) != len(want) {
		t.Fatalf("parsed %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d = %q, want %q", i, got[i], want[i])
		}
	}
	// Blank lines and occ's list framing must not shift the indexes.
	got = parseTrustedDomains("\n  - localhost\n\n  - cloud.example.com  \n")
	if len(got) != 2 || got[0] != "localhost" || got[1] != "cloud.example.com" {
		t.Errorf("framing/blank lines shifted the indexes: %v", got)
	}
	if len(parseTrustedDomains("")) != 0 {
		t.Error("empty output must parse to no domains")
	}
}

// TestTrustedDomainSlotAlreadyPresent — re-running a restore must CONVERGE.
// Appending every time would grow the trust list without bound, quietly widening
// it on each run.
func TestTrustedDomainSlotAlreadyPresent(t *testing.T) {
	domains := []string{"localhost", "cloud.example.com"}
	idx, action := trustedDomainSlot(domains, "cloud.example.com", "")
	if action != trustedDomainAlready || idx != 1 {
		t.Fatalf("an already-trusted host must be a no-op at its own index, got idx=%d action=%v", idx, action)
	}
	// Case-insensitively, since hostnames are.
	if _, action := trustedDomainSlot(domains, "Cloud.Example.COM", ""); action != trustedDomainAlready {
		t.Error("hostname comparison must be case-insensitive")
	}
}

// TestTrustedDomainSlotReplacesStale is the honest operation for a move: the old
// machine's address is swapped for the new one, so the trust list ends up
// exactly as large as it started. Nothing is widened.
func TestTrustedDomainSlotReplacesStale(t *testing.T) {
	domains := []string{"localhost", "cloud.example.com", "10.168.1.50"}
	idx, action := trustedDomainSlot(domains, "10.168.1.99", "10.168.1.50")
	if action != trustedDomainReplace {
		t.Fatalf("the old address is present — it must be REPLACED, not appended (got %v)", action)
	}
	if idx != 2 {
		t.Errorf("must replace at the old address's index 2, got %d", idx)
	}
	// A URL-shaped old address still matches the bare host entry.
	if idx, action := trustedDomainSlot(domains, "new.example", "https://10.168.1.50/"); action != trustedDomainReplace || idx != 2 {
		t.Errorf("a scheme on the old address must not prevent the match, got idx=%d action=%v", idx, action)
	}
}

// TestTrustedDomainSlotNeverReplacesLocalhost — occ and cron reach the instance
// over localhost. Overwriting that entry to make a move work would break them,
// and the breakage would look unrelated.
func TestTrustedDomainSlotNeverReplacesLocalhost(t *testing.T) {
	domains := []string{"localhost", "cloud.example.com"}
	idx, action := trustedDomainSlot(domains, "new.example", "localhost")
	if action == trustedDomainReplace {
		t.Fatal("localhost must never be chosen as the entry to overwrite — occ and cron use it")
	}
	if idx != len(domains) {
		t.Errorf("expected an append at index %d, got %d", len(domains), idx)
	}
}

// TestTrustedDomainSlotAppends: with no stale entry to replace, adding is a
// widening — of exactly one named host. The caller reports it differently for
// that reason, so the distinction must survive here.
func TestTrustedDomainSlotAppends(t *testing.T) {
	domains := []string{"localhost", "cloud.example.com"}
	idx, action := trustedDomainSlot(domains, "new.example", "")
	if action != trustedDomainAdd {
		t.Fatalf("nothing to replace — must be reported as an ADD, got %v", action)
	}
	if idx != 2 {
		t.Errorf("must append at the next free index 2, got %d", idx)
	}
	// An empty list still appends at 0 rather than doing something odd.
	if idx, action := trustedDomainSlot(nil, "new.example", ""); action != trustedDomainAdd || idx != 0 {
		t.Errorf("an empty list must append at 0, got idx=%d action=%v", idx, action)
	}
}

// TestOccSetHint: every failure path prints a command that actually fixes the
// problem, so the operator is never left holding only an error.
func TestOccSetHint(t *testing.T) {
	h := occSetHint("cloud.example.com")
	for _, want := range []string{"occ", "config:system:set", "trusted_domains", "cloud.example.com"} {
		if !strings.Contains(h, want) {
			t.Errorf("the manual hint must contain %q, got %q", want, h)
		}
	}
}

// F173: the two optional address changes reach every member of a stack.
//
// The gap this closes was a mismatch between a promise and a field. The stack
// dialog's pre-restore panel printed Nextcloud's "enter it below" note and had
// nothing below it, and the options struct behind it carried no address at all
// — so even a filled-in field would have gone nowhere. It was worst exactly
// where it mattered most: an application that needs an address change on a move
// is usually a multi-service one, which is restored from the stack dialog.
func TestStackRestorePassesAddressesToEveryService(t *testing.T) {
	sopts := StackRestoreOptions{
		NewSiteAddress:     "https://cloud.example.com",
		NewUpstreamAddress: "http://10.0.0.5:32400",
	}
	for _, svc := range []string{"nextcloud", "db", "redis", "cron"} {
		s := &stackService{
			service: svc,
			backup:  &store.Backup{ID: "b-" + svc},
			man:     &Manifest{ContainerID: "c-" + svc},
		}
		got := stackServiceRestoreOptions(s, "node1", sopts)
		if got.NewSiteAddress != "https://cloud.example.com" {
			t.Errorf("%s: the site address must reach every member, got %q", svc, got.NewSiteAddress)
		}
		if got.NewUpstreamAddress != "http://10.0.0.5:32400" {
			t.Errorf("%s: the dependency address must reach every member, got %q", svc, got.NewUpstreamAddress)
		}
		// Only the service whose profile declares the binding acts on it; for the
		// rest it is inert, which is why passing it to all of them is safe.
		if !got.stackMember {
			t.Errorf("%s: must still be marked as a stack member", svc)
		}
	}

	// Blank is the normal case and must stay blank — a move that keeps the same
	// address needs nothing, and applying one anyway is its own way to break a
	// working restore.
	got := stackServiceRestoreOptions(
		&stackService{service: "x", backup: &store.Backup{ID: "b"}, man: &Manifest{}},
		"node1", StackRestoreOptions{})
	if got.NewSiteAddress != "" || got.NewUpstreamAddress != "" {
		t.Errorf("nothing supplied must carry nothing through: %q / %q", got.NewSiteAddress, got.NewUpstreamAddress)
	}
}
