package egress

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
)

// F207 — audit mode: evaluate the allow-list exactly as enforcement would, but
// record instead of refusing.
//
// The property that makes this safe to ship is the one asserted hardest below:
// audit mode changes what ENFORCEMENT does and never changes what the policy
// SAYS. The operator's "test a host" button and the "would be blocked" markers
// both ask Check, and both exist to tell the truth while the dry run is on.

func withAudit(t *testing.T, on bool) {
	t.Helper()
	prev := AuditMode()
	SetAuditMode(on)
	t.Cleanup(func() { SetAuditMode(prev) })
}

func recorder(t *testing.T) *[]string {
	t.Helper()
	var mu sync.Mutex
	seen := []string{}
	SetObserver(func(h string) {
		mu.Lock()
		seen = append(seen, h)
		mu.Unlock()
	})
	t.Cleanup(func() { SetObserver(nil) })
	return &seen
}

// AC1 + AC2 at the policy level: audit mode lets a non-listed host through and
// records it; with audit mode off the same host is refused.
func TestEnforceObservesInAuditModeAndBlocksOtherwise(t *testing.T) {
	p := Parse([]string{"allowed.example.com"})
	seen := recorder(t)

	// Enforcing: refused, and nothing recorded — the record is for the dry run.
	withAudit(t, false)
	if err := p.Enforce("blocked.example.net:443"); err == nil {
		t.Fatal("with audit mode off, a non-listed host must be refused")
	}
	if len(*seen) != 0 {
		t.Errorf("enforcement must not populate the audit record: %v", *seen)
	}

	// Auditing: allowed through, and recorded as a bare host.
	withAudit(t, true)
	if err := p.Enforce("blocked.example.net:443"); err != nil {
		t.Fatalf("audit mode must allow the connection: %v", err)
	}
	if len(*seen) != 1 || (*seen)[0] != "blocked.example.net" {
		t.Fatalf("observed = %v, want the bare host once", *seen)
	}
	// A permitted host is never recorded — the list is what would BREAK.
	if err := p.Enforce("allowed.example.com:443"); err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 1 {
		t.Errorf("an allowed host must not be recorded: %v", *seen)
	}
}

// The property the whole design turns on: Check keeps telling the truth.
func TestAuditModeNeverChangesTheVerdict(t *testing.T) {
	p := Parse([]string{"allowed.example.com"})
	_ = recorder(t)
	withAudit(t, true)

	if err := p.Check("blocked.example.net"); err == nil {
		t.Fatal("Check must still report a non-listed host as denied in audit mode")
	}
	if !strings.Contains(p.Check("blocked.example.net").Error(), "not in the egress allow-list") {
		t.Error("and it must still say why")
	}
	if p.AllowHost("blocked.example.net") {
		t.Error("AllowHost must be unaffected by audit mode")
	}
	if err := p.Check("allowed.example.com"); err != nil {
		t.Errorf("an allowed host is still allowed: %v", err)
	}
}

// A disabled policy has nothing to observe — audit mode must not manufacture
// observations for a deployment that never configured a list.
func TestAuditModeOnADisabledPolicyObservesNothing(t *testing.T) {
	p := Parse(nil)
	seen := recorder(t)
	withAudit(t, true)

	if err := p.Enforce("anything.example.com"); err != nil {
		t.Fatalf("a disabled policy allows everything: %v", err)
	}
	if len(*seen) != 0 {
		t.Errorf("nothing to observe when no list is configured: %v", *seen)
	}
}

// A malformed target is not a would-be-denied host, and recording it would put
// junk in a list the operator is supposed to act on.
func TestMalformedTargetIsNotRecorded(t *testing.T) {
	p := Parse([]string{"allowed.example.com"})
	seen := recorder(t)
	withAudit(t, true)

	_ = p.Enforce("")
	if len(*seen) != 0 {
		t.Errorf("an empty target is not an observation: %v", *seen)
	}
}

// The dial guard is the site that catches redirects and rebinds, so it must
// honour audit mode too — otherwise the dry run would still break the exact
// connections it exists to survey.
func TestGuardDialHonoursAuditMode(t *testing.T) {
	p := Parse([]string{"allowed.example.com"})
	seen := recorder(t)

	dialed := []string{}
	base := func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		return nil, nil
	}
	guard := p.GuardDial(base)

	withAudit(t, false)
	if _, err := guard(context.Background(), "tcp", "blocked.example.net:443"); err == nil {
		t.Fatal("enforcing: the dial must be refused")
	}
	if len(dialed) != 0 {
		t.Fatal("enforcing: the base dialer must never be reached")
	}

	withAudit(t, true)
	if _, err := guard(context.Background(), "tcp", "blocked.example.net:443"); err != nil {
		t.Fatalf("auditing: the dial must proceed: %v", err)
	}
	if len(dialed) != 1 || dialed[0] != "blocked.example.net:443" {
		t.Errorf("auditing: the real dial should have happened, got %v", dialed)
	}
	if len(*seen) != 1 || (*seen)[0] != "blocked.example.net" {
		t.Errorf("auditing: the host should have been recorded, got %v", *seen)
	}
}

// No observer installed is the normal state and must not panic on the dial path.
func TestNoObserverIsSafe(t *testing.T) {
	SetObserver(nil)
	p := Parse([]string{"allowed.example.com"})
	withAudit(t, true)
	if err := p.Enforce("blocked.example.net"); err != nil {
		t.Fatalf("audit mode with no observer still allows: %v", err)
	}
}

// Audit mode is process-wide state read from the dial path; concurrent dials and
// toggles must not race. Run with -race.
func TestAuditModeIsConcurrencySafe(t *testing.T) {
	p := Parse([]string{"allowed.example.com"})
	_ = recorder(t)
	withAudit(t, true)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = p.Enforce("blocked.example.net")
			if n%10 == 0 {
				SetAuditMode(true)
			}
		}(i)
	}
	wg.Wait()
}
