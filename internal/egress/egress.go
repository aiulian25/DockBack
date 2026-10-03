// Package egress implements an optional, operator-configured outbound allow-list
// (PLAN §3.10). DockBack only ever connects out to destinations and
// notification endpoints the operator entered, but in locked-down environments
// an operator may want a belt-and-suspenders "default deny unknown destinations"
// control. When the allow-list is empty the policy is disabled and every
// operator-configured destination is permitted (unchanged behavior). When it is
// set the policy is default-deny: a host must match an allow-list entry to be
// dialed, enforced both when a destination is added/tested and at the real TCP
// dial (so an HTTP redirect or DNS rebind to a non-allowed host is also refused).
package egress

import (
	"context"
	"fmt"
	"net"
	neturl "net/url"
	"strings"
	"sync/atomic"
)

// Policy is an immutable set of allow-list rules. The zero value (and a Policy
// parsed from no entries) is disabled — it allows everything.
type Policy struct {
	hosts    map[string]struct{} // exact hostnames (lowercased) and bare IPs
	suffixes []string            // domain suffixes incl. leading dot, e.g. ".example.com"
	cidrs    []*net.IPNet        // IP networks
	raw      []string            // operator entries that produced a rule, as given (F39 display)
}

// Parse builds a Policy from operator entries. Each entry may be:
//   - an exact hostname            example.com
//   - a wildcard/suffix domain     *.example.com  or  .example.com  (matches the
//     apex example.com and any subdomain)
//   - a bare IP                    203.0.113.10
//   - a CIDR network               203.0.113.0/24 or 2001:db8::/32
//
// A pasted URL or host:port is tolerated — only the host portion is used.
// Blank entries and obvious junk are skipped.
func Parse(entries []string) *Policy {
	p := &Policy{hosts: map[string]struct{}{}}
	for _, raw := range entries {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		orig := e // preserve what the operator typed, for echo-back display (F39)
		// Tolerate a pasted URL.
		if strings.Contains(e, "://") {
			if u, err := neturl.Parse(e); err == nil && u.Hostname() != "" {
				e = u.Hostname()
			}
		}
		// CIDR network.
		if strings.Contains(e, "/") {
			if _, n, err := net.ParseCIDR(e); err == nil {
				p.cidrs = append(p.cidrs, n)
				p.raw = append(p.raw, orig)
				continue
			}
		}
		// Wildcard / suffix domain.
		if strings.HasPrefix(e, "*.") || strings.HasPrefix(e, ".") {
			suf := strings.TrimPrefix(e, "*")      // "*.example.com" -> ".example.com"
			suf = "." + strings.TrimLeft(suf, ".") // normalize to exactly one leading dot
			suf = strings.ToLower(suf)
			p.suffixes = append(p.suffixes, suf)
			// The apex (example.com) is also allowed for a domain rule.
			p.hosts[strings.ToLower(strings.TrimPrefix(suf, "."))] = struct{}{}
			p.raw = append(p.raw, orig)
			continue
		}
		// Strip a host:port if the operator pasted one (keep bare IPv6 intact).
		if h, _, err := net.SplitHostPort(e); err == nil && h != "" {
			e = h
		}
		p.hosts[strings.ToLower(e)] = struct{}{}
		p.raw = append(p.raw, orig)
	}
	if len(p.hosts) == 0 && len(p.suffixes) == 0 && len(p.cidrs) == 0 {
		return &Policy{} // disabled
	}
	return p
}

// Enabled reports whether the policy actively restricts egress. A disabled
// policy (no entries) allows everything.
func (p *Policy) Enabled() bool {
	if p == nil {
		return false
	}
	return len(p.hosts) > 0 || len(p.suffixes) > 0 || len(p.cidrs) > 0
}

// Entries returns the operator allow-list entries that produced a rule, as they
// were given — for display/echo-back in the UI (F39). nil for a disabled policy.
func (p *Policy) Entries() []string {
	if p == nil || len(p.raw) == 0 {
		return nil
	}
	out := make([]string, len(p.raw))
	copy(out, p.raw)
	return out
}

// AllowHost reports whether a hostname or IP literal is permitted.
func (p *Policy) AllowHost(host string) bool {
	if !p.Enabled() {
		return true
	}
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.Trim(host, "[]") // bare IPv6 may arrive bracketed
	if host == "" {
		return false
	}
	if _, ok := p.hosts[host]; ok {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		for _, n := range p.cidrs {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	}
	for _, suf := range p.suffixes {
		if strings.HasSuffix(host, suf) {
			return true
		}
	}
	return false
}

// HostFrom extracts a bare hostname from a bare host, host:port, or URL — the
// SAME normalization Check applies before matching, so callers that pre-derive a
// host for display/allow-listing (F54) agree with what the policy will check. It
// deliberately drops any URL path, port, and userinfo, so no credentials leak.
func HostFrom(s string) string { return hostFrom(s) }

// hostFrom extracts a hostname from a bare host, host:port, or URL.
func hostFrom(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.Contains(s, "://") {
		if u, err := neturl.Parse(s); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	if h, _, err := net.SplitHostPort(s); err == nil && h != "" {
		return h
	}
	return strings.Trim(s, "[]")
}

// Check returns a clear error if the host implied by target (a bare host,
// host:port, or URL) is not permitted; nil when the policy is disabled or the
// host is allowed.
func (p *Policy) Check(target string) error {
	if !p.Enabled() {
		return nil
	}
	host := hostFrom(target)
	if host == "" {
		return fmt.Errorf("egress: could not determine host for %q", target)
	}
	if !p.AllowHost(host) {
		return fmt.Errorf("egress denied: %q is not in the egress allow-list (DOCKBACK_EGRESS_ALLOW)", host)
	}
	return nil
}

// Audit mode (F207).
//
// Turning a default-deny allow-list on is all-or-nothing, and the failure is
// silent in the worst way: a destination the operator forgot to list stops
// working at the next scheduled backup, hours later, with an egress error nobody
// is watching for. There was no way to find out what a list WOULD block without
// finding out the hard way.
//
// In audit mode the policy still evaluates every outbound host exactly as it
// would when enforcing, and still reports the verdict truthfully — it simply
// does not act on a denial. Each would-be denial is handed to an observer the
// application layer records, so the operator can look at a real list of what
// enforcement would break before flipping it on.
//
// THE DISTINCTION THAT MATTERS: Check vs Enforce
//
// Check answers "does this policy allow this host". Enforce decides "does this
// connection proceed". They were the same function, and audit mode is exactly
// the case where they must differ — because the operator's Test-a-host button
// and the "would be blocked by the current list" markers in the suggestions list
// BOTH call Check, and both exist to tell the operator the truth while audit
// mode is on. A Check that returned nil in audit mode would report every host as
// permitted and destroy the one signal this feature exists to provide.
//
// So Check is never affected by audit mode. Enforce is the one that yields.

// auditMode is the process-wide dry-run flag, stored atomically so the dial path
// can read it without a lock.
var auditMode atomic.Bool

// observer receives each host that WOULD have been denied. Stored atomically;
// nil is the normal state and costs nothing to check.
var observer atomic.Pointer[func(host string)]

// SetAuditMode turns the dry run on or off process-wide (F207).
func SetAuditMode(on bool) { auditMode.Store(on) }

// AuditMode reports whether egress is currently observed rather than enforced.
func AuditMode() bool { return auditMode.Load() }

// SetObserver installs the recorder for would-be denials (F207). Passing nil
// removes it. The observer runs ON THE DIAL PATH, so it must be cheap and must
// not block — the recorder that satisfies this contract dedups in memory and
// persists only the first sighting of each host.
func SetObserver(fn func(host string)) {
	if fn == nil {
		observer.Store(nil)
		return
	}
	observer.Store(&fn)
}

// note hands a would-be-denied host to the observer, if one is installed.
func note(host string) {
	if fn := observer.Load(); fn != nil {
		(*fn)(host)
	}
}

// Enforce is Check with the audit-mode escape hatch — the function every path
// that actually STOPS a connection must call (F207).
//
// Identical to Check when enforcing. In audit mode a denial is recorded and
// nil is returned, so the connection proceeds and the operator learns what would
// have broken instead of discovering it at 3am when a scheduled upload fails.
func (p *Policy) Enforce(target string) error {
	err := p.Check(target)
	if err == nil {
		return nil
	}
	if !auditMode.Load() {
		return err
	}
	// Only a policy DENIAL is observable. A target whose host could not be
	// determined is a malformed address, not a would-be-denied host, and
	// recording it would put junk in a list the operator is meant to act on.
	if host := hostFrom(target); host != "" && !p.AllowHost(host) {
		note(host)
	}
	return nil
}

// DialFunc matches net.Dialer.DialContext / http.Transport.DialContext.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// GuardDial wraps a dialer so a connection to a non-allowed host is refused at
// the TCP layer — catching redirects and DNS rebinds that bypass the
// construction-time Check. A nil base falls back to a default dialer.
func (p *Policy) GuardDial(base DialFunc) DialFunc {
	if base == nil {
		var d net.Dialer
		base = d.DialContext
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if p.Enabled() {
			// F207: Enforce, not Check — in audit mode this records the host and
			// lets the dial through. This is the site that catches redirects and
			// DNS rebinds, so it is also the one whose observations may name a
			// host the operator never configured; the recorder marks those rather
			// than presenting them as safe to allow-list.
			if err := p.Enforce(address); err != nil {
				return nil, err
			}
		}
		return base(ctx, network, address)
	}
}

// defaultPolicy is the process-wide policy, configured once at startup from the
// environment. Stored atomically so it is safe to read from any goroutine.
var defaultPolicy atomic.Pointer[Policy]

// Configure installs the process-wide policy from operator entries. Called once
// from config.Load.
func Configure(entries []string) { defaultPolicy.Store(Parse(entries)) }

// Default returns the process-wide policy, or a disabled policy if none was
// configured yet.
func Default() *Policy {
	if p := defaultPolicy.Load(); p != nil {
		return p
	}
	return &Policy{}
}

// GuardDialDefault wraps a dialer against whatever the process-wide policy is
// AT DIAL TIME, rather than the policy that happened to be installed when the
// wrapper was built.
//
// Policy.GuardDial binds one immutable *Policy, which is right for a client
// constructed per operation. A LONG-LIVED client (the notification dispatcher
// is built once at boot) must not do that: Configure swaps the policy live
// from Settings, and a captured pointer keeps enforcing the list the operator
// has already replaced.
func GuardDialDefault(base DialFunc) DialFunc {
	if base == nil {
		var d net.Dialer
		base = d.DialContext
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		p := Default()
		if p.Enabled() {
			if err := p.Enforce(address); err != nil {
				return nil, err
			}
		}
		return base(ctx, network, address)
	}
}

// ParseCIDROrIP parses one allow-list entry as a network, widening a bare
// address to a single-host network (/32 or /128) — what an operator plainly
// means by writing "10.0.0.5". Returns ok=false when the entry is neither.
//
// The two callers differ in what they do with a bad entry — the trusted-proxy
// list skips it, the API-token source pin fails closed — so the DECISION stays
// with them and only the parsing rule lives here.
func ParseCIDROrIP(entry string) (*net.IPNet, bool) {
	e := strings.TrimSpace(entry)
	if e == "" {
		return nil, false
	}
	if !strings.Contains(e, "/") {
		ip := net.ParseIP(e)
		if ip == nil {
			return nil, false
		}
		if ip.To4() != nil {
			e += "/32"
		} else {
			e += "/128"
		}
	}
	_, n, err := net.ParseCIDR(e)
	if err != nil || n == nil {
		return nil, false
	}
	return n, true
}
