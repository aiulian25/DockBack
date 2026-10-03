package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"dockback/internal/store"
)

// ipRateLimiter is a cheap per-IP token bucket (F59) for the ONE unauthenticated
// dynamic route — the public runbook share view. It bounds abuse (scraping,
// brute-forcing tokens) without a dependency. Refills `perMin` tokens/minute up to
// `burst`. The map is crudely capped so it can't grow without bound.
type ipRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rlBucket
}

type rlBucket struct {
	tokens float64
	last   time.Time
}

func newIPRateLimiter() *ipRateLimiter { return &ipRateLimiter{buckets: map[string]*rlBucket{}} }

// allow reports whether a request from ip is permitted right now, consuming a token.
func (l *ipRateLimiter) allow(ip string, perMin, burst float64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.buckets) > 10000 { // crude bound: reset rather than grow unboundedly
		l.buckets = map[string]*rlBucket{}
	}
	b := l.buckets[ip]
	if b == nil {
		b = &rlBucket{tokens: burst, last: now}
		l.buckets[ip] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * (perMin / 60)
	if b.tokens > burst {
		b.tokens = burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// shareRL rate-limits the public runbook share route (30 requests/min per IP).
var shareRL = newIPRateLimiter()

// Login brute-force protection (PLAN §10.1 / Security.md §4).
//
// Two complementary defenses:
//   - A persisted sliding-window lockout keyed by client IP (escalating) and by
//     account name (short, fixed). Persisted in the settings table so it
//     survives a restart — an attacker can't reset it by forcing a crash.
//   - A small semaphore capping concurrent argon2 verifications, since each one
//     allocates 64 MiB; without it, a flood of guesses is a memory-DoS vector.
//
// Design note (account-lockout DoS): a *hard* per-account lock can be abused to
// lock the legitimate admin out from a different IP. So the per-IP policy is the
// escalating primary defense, and the per-account policy is intentionally short
// and non-escalating (bounds worst-case admin self/forced-lockout to 15 min).
const (
	argonMaxConcurrent = 2               // peak argon2 memory ≈ 2 × 64 MiB
	argonAcquireWait   = 2 * time.Second // shed load past this with 429

	// maxEscalationShift bounds the doubling of a repeat lockout. Unbounded, the
	// shift silently wraps: at 2^63 the duration goes NEGATIVE and at 2^64 it
	// becomes zero, either of which puts the lock's expiry in the past and hands
	// the lockout back to the one address patient enough to earn it. 2^20 × base
	// is already centuries, so every strike below 21 behaves exactly as before
	// and the maxLock clamp does the real work.
	maxEscalationShift = 20
)

type lockPolicy struct {
	max      int           // failures within window before locking
	window   time.Duration // sliding window for counting failures
	base     time.Duration // base lock duration
	maxLock  time.Duration // cap on (escalated) lock duration
	escalate bool          // double the lock each repeat lockout
}

var (
	ipLockPolicy   = lockPolicy{max: 5, window: 15 * time.Minute, base: 15 * time.Minute, maxLock: time.Hour, escalate: true}
	userLockPolicy = lockPolicy{max: 10, window: 15 * time.Minute, base: 15 * time.Minute, maxLock: 15 * time.Minute, escalate: false}
)

// lockRecord is the persisted per-key failure/lock state.
type lockRecord struct {
	Fails       int   `json:"fails"`
	WindowStart int64 `json:"window_start"`
	Until       int64 `json:"until"`   // locked until (unix seconds)
	Strikes     int   `json:"strikes"` // consecutive lockouts (escalation)
}

// loginGuard throttles and locks out abusive logins and bounds argon2 cost.
type loginGuard struct {
	store    *store.Store
	mu       sync.Mutex // serialize read-modify-write of lock records
	argonSem chan struct{}
}

func newLoginGuard(st *store.Store) *loginGuard {
	return &loginGuard{store: st, argonSem: make(chan struct{}, argonMaxConcurrent)}
}

func lockKey(kind, id string) string { return "lockout:" + kind + ":" + strings.ToLower(id) }

func (g *loginGuard) load(key string) lockRecord {
	var rec lockRecord
	if v, _ := g.store.GetSetting(key, ""); v != "" {
		_ = json.Unmarshal([]byte(v), &rec)
	}
	return rec
}

func (g *loginGuard) save(key string, rec lockRecord) {
	b, _ := json.Marshal(rec)
	_ = g.store.SetSetting(key, string(b))
}

// blocked returns the remaining lock duration if the IP or account is currently
// locked, else 0.
func (g *loginGuard) blocked(ip, user string) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now().Unix()
	keys := []string{lockKey("ip", ip)}
	if user != "" {
		keys = append(keys, lockKey("user", user))
	}
	var maxLeft int64
	for _, k := range keys {
		if rec := g.load(k); rec.Until > now && rec.Until-now > maxLeft {
			maxLeft = rec.Until - now
		}
	}
	if maxLeft <= 0 {
		return 0
	}
	return time.Duration(maxLeft) * time.Second
}

// failOutcome is what a recorded failure did to the IP's lock state (F198).
//
// Returned rather than queried afterwards because the difference between "this
// attempt locked the address" and "the address is locked" is the difference
// between one alert and one alert per retry. Reading it back with a second call
// would also race: two concurrent failures could both observe the locked state
// and both report the transition.
type failOutcome struct {
	// IPFails is the failure count now standing against this address inside the
	// current window. It resets to 0 at the moment a lock is applied, so it only
	// ever describes the run-up to a lockout.
	IPFails int
	// IPLocked is true only on the attempt that CAUSED the lock — the edge, not
	// the level.
	IPLocked bool
	// LockFor is how long that lock will hold, meaningful only with IPLocked.
	LockFor time.Duration
}

// recordFail counts a failed attempt against both the IP and the account,
// locking either when its policy threshold is crossed, and reports what that did
// to the IP so the caller can alert on the transition (F198).
func (g *loginGuard) recordFail(ip, user string) failOutcome {
	g.mu.Lock()
	defer g.mu.Unlock()
	fails, lockedFor := g.bump(lockKey("ip", ip), ipLockPolicy)
	if user != "" {
		g.bump(lockKey("user", user), userLockPolicy)
	}
	return failOutcome{IPFails: fails, IPLocked: lockedFor > 0, LockFor: lockedFor}
}

// bump records one failure against a key, returning the post-bump failure count
// and — when this call crossed the threshold — the duration of the lock it just
// applied (0 when it did not lock).
func (g *loginGuard) bump(key string, p lockPolicy) (fails int, lockedFor time.Duration) {
	now := time.Now().Unix()
	rec := g.load(key)
	// Already locked: count nothing and report nothing (F198).
	//
	// Before this, attempts kept accumulating during an active lock, so every
	// further p.max guesses re-locked the key and escalated its strike count.
	// Two consequences, one cosmetic and one not: the caller saw a fresh "lock
	// transition" every few retries, and a grinding attacker inflated their own
	// escalation — which sounds like a feature until you notice it also means the
	// operator's alert fires again and again for one ongoing incident.
	//
	// The lock is already doing its job here; the attempt is refused by blocked()
	// before any argon2 work. Leaving the record untouched keeps the strike count
	// meaning "how many times this address got itself locked", not "how many
	// times it kept knocking afterwards".
	if rec.Until > now {
		return 0, 0
	}
	// Reset the counting window if it has elapsed.
	if rec.WindowStart == 0 || now-rec.WindowStart > int64(p.window.Seconds()) {
		rec.WindowStart = now
		rec.Fails = 0
	}
	rec.Fails++
	if rec.Fails >= p.max {
		rec.Strikes++
		dur := p.base
		if p.escalate && rec.Strikes > 1 {
			shift := rec.Strikes - 1
			if shift > maxEscalationShift {
				shift = maxEscalationShift
			}
			dur = p.base * time.Duration(int64(1)<<shift)
		}
		if dur > p.maxLock {
			dur = p.maxLock
		}
		rec.Until = now + int64(dur.Seconds())
		rec.Fails = 0
		rec.WindowStart = now
		lockedFor = dur
	}
	g.save(key, rec)
	return rec.Fails, lockedFor
}

// lockoutRetention is how long a quiet lockout record is kept.
//
// NOT the policy window, deliberately. The record carries the strike count that
// makes a repeat lockout longer than the last, and dropping it fifteen minutes
// after the window closes would hand a patient attacker a fresh escalation
// ladder every quarter of an hour — undoing the escalation entirely. A month of
// complete silence is a different campaign, and by then the row is only cost.
const lockoutRetention = 30 * 24 * time.Hour

// pruneLockouts deletes lockout records that have been quiet for
// lockoutRetention. One row exists per address that has ever failed a sign-in,
// so on an internet-adjacent deployment the key space is however many addresses
// have knocked — unbounded, and copied into every application backup.
func (s *Server) pruneLockouts() {
	keys, err := s.store.SettingKeysWithPrefix("lockout:")
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-lockoutRetention).Unix()
	for _, k := range keys {
		raw, _ := s.store.GetSetting(k, "")
		var rec lockRecord
		if raw != "" && json.Unmarshal([]byte(raw), &rec) != nil {
			continue // unreadable: leave it rather than guess
		}
		// Still locked, or its counting window is still open? Keep it.
		if rec.Until > cutoff || rec.WindowStart > cutoff {
			continue
		}
		_ = s.store.DeleteSetting(k)
	}
}

// reset clears lock state for the IP and account after a successful sign-in.
func (g *loginGuard) reset(ip, user string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	_ = g.store.DeleteSetting(lockKey("ip", ip))
	if user != "" {
		_ = g.store.DeleteSetting(lockKey("user", user))
	}
}

// acquireArgon reserves an argon2 slot, returning false if the pool stays
// saturated past argonAcquireWait (caller should shed with 429).
func (g *loginGuard) acquireArgon(ctx context.Context) bool {
	t := time.NewTimer(argonAcquireWait)
	defer t.Stop()
	select {
	case g.argonSem <- struct{}{}:
		return true
	case <-t.C:
		return false
	case <-ctx.Done():
		return false
	}
}

func (g *loginGuard) releaseArgon() { <-g.argonSem }

// remoteHost returns the direct peer's IP (no port).
func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// trustForwarded reports whether this request's X-Forwarded-* headers may be
// trusted: TrustProxy must be on, and — when a TrustedProxies allowlist is set —
// the direct peer must be in it. This stops a directly-reachable (e.g. LAN HTTP)
// client from spoofing X-Forwarded-For to dodge the login lockout (§10.1).
func (s *Server) trustForwarded(r *http.Request) bool {
	// Nil-safe: this now runs on the API-token authentication path (F201), and a
	// nil dereference in the auth middleware is a crash on every request rather
	// than a failure of one. No config means no proxy trust, which is also the
	// safe answer — the direct peer address is believed instead of a header.
	if s == nil || s.cfg == nil || !s.cfg.TrustProxy {
		return false
	}
	if len(s.cfg.TrustedProxies) == 0 {
		return true // legacy trust-all (warned at startup)
	}
	ip := net.ParseIP(remoteHost(r))
	if ip == nil {
		return false
	}
	return s.trustedProxy(ip)
}

// trustedProxy reports whether ip is one of our own configured reverse proxies.
// Callers must have established that s.cfg is non-nil (trustForwarded does).
func (s *Server) trustedProxy(ip net.IP) bool {
	for _, n := range s.cfg.TrustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// forwardedIP parses one X-Forwarded-For entry to a canonical IP, or nil.
// Most proxies write a bare address; some append a port or bracket an IPv6
// address, and dropping those entries would silently key the login lockout on
// the proxy instead of the caller.
func forwardedIP(hop string) net.IP {
	hop = strings.TrimSpace(hop)
	if ip := net.ParseIP(hop); ip != nil {
		return ip
	}
	host, _, err := net.SplitHostPort(hop)
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

// forwardedClient picks the caller out of an X-Forwarded-For chain, returning ""
// when the header holds nothing usable. Only meaningful once the direct peer has
// been established as a trusted proxy.
//
// The chain is read from the RIGHT: each proxy APPENDS the peer it actually saw,
// so the rightmost entry is the one our own infrastructure wrote and everything
// to the left is, ultimately, whatever the client chose to send. Walking left
// past our own proxies lands on the first address none of them vouched for —
// the real caller.
func (s *Server) forwardedClient(xff string) string {
	hops := strings.Split(xff, ",")
	for i := len(hops) - 1; i >= 0; i-- {
		ip := forwardedIP(hops[i])
		if ip == nil {
			continue
		}
		// Legacy trust-all (no allow-list, warned at startup): no proxy can be
		// recognised as ours, so the last usable entry is the best answer there is.
		if len(s.cfg.TrustedProxies) == 0 {
			return ip.String()
		}
		if s.trustedProxy(ip) {
			continue // one of ours — keep walking left for the caller it forwarded
		}
		return ip.String()
	}
	return ""
}

// clientIP returns the caller's IP — the forwarded client IP when the request
// came through a trusted proxy, otherwise the direct peer.
func (s *Server) clientIP(r *http.Request) string {
	if !s.trustForwarded(r) {
		return remoteHost(r)
	}
	if client := s.forwardedClient(r.Header.Get("X-Forwarded-For")); client != "" {
		return client
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		return xr
	}
	return remoteHost(r)
}

// humanShort renders a lock duration compactly for the user-facing message.
func humanShort(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds())+1)
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes())+1)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}
